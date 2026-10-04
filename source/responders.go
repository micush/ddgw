package main

import (
	"net/netip"
	"sync/atomic"
	"syscall"
	"time"
)

// rawResponder owns an AF_PACKET socket and a goroutine that answers ARP
// requests / IPv6 neighbor solicitations for the VIP with the vMAC of the AFN
// chosen by the engine's load-balancing method.  Only the AGC runs one.
type rawResponder struct {
	fd   int
	stop atomic.Bool
}

// Stop signals the goroutine; it closes the socket on its next wake-up
// (SO_RCVTIMEO bounds that to ~1s).  Deliberately does not wait: the loop
// takes the engine lock, and Stop is called with that lock held.
func (r *rawResponder) Stop() { r.stop.Store(true) }

func (r *rawResponder) loop(handle func(fd int, frame []byte)) {
	defer syscall.Close(r.fd)
	buf := make([]byte, 4096)
	for !r.stop.Load() {
		n, _, err := syscall.Recvfrom(r.fd, buf, 0)
		if err != nil {
			if err == syscall.EAGAIN || err == syscall.EINTR {
				continue
			}
			return
		}
		if r.stop.Load() {
			return
		}
		handle(r.fd, buf[:n])
	}
}

func (e *Engine) stopRespondersLocked() {
	if e.arp != nil {
		e.arp.Stop()
		e.arp = nil
	}
	if e.ns != nil {
		e.ns.Stop()
		e.ns = nil
	}
}

// ── ARP (IPv4) ───────────────────────────────────────────────────────────────

func (e *Engine) startARPResponderLocked() {
	if e.arp != nil {
		return
	}
	vip, err := vipAddr(e.cfg.VIP4)
	if err != nil {
		return
	}
	fd, err := openPacketSocket(e.cfg.Interface, syscall.ETH_P_ARP, time.Second)
	if err != nil {
		errorf("Failed to start ARP responder: %v", err)
		return
	}
	r := &rawResponder{fd: fd}
	e.arp = r
	vip4 := vip.As4()
	gid := e.cfg.GroupID
	// The answer names the slot's virtual MAC in its payload, but is sent from this node's own MAC.  With the
	// slot's MAC as the Ethernet source, every answer made the switch learn that MAC on the controller's port,
	// and the queries sent to the forwarder that really owns it were then delivered to the wrong node until it
	// next spoke.
	selfMAC := ifaceMAC(e.cfg.Interface)
	go r.loop(func(fd int, frame []byte) {
		// Ethernet(14) + ARP(28): op@20, sender mac@22, sender ip@28, target ip@38
		if len(frame) < 42 {
			return
		}
		arp := frame[14:]
		if arp[6] != 0 || arp[7] != 1 { // only requests
			return
		}
		if [4]byte(arp[24:28]) != vip4 {
			return
		}
		senderMAC := [6]byte(frame[6:12])
		senderIP := [4]byte(arp[14:18])
		requester := netip.AddrFrom4(senderIP).String()

		slot := e.PickAFN(requester)
		mac := vmacBytes(gid, slot)
		debugf("ARP req for VIP from %s — replying with slot %d vMAC %s", requester, slot, vmacStr(gid, slot))

		out := buildARPReply(senderMAC, selfMAC, mac, vip4, senderIP)
		if _, err := syscall.Write(fd, out); err != nil {
			debugf("ARP reply send failed: %v", err)
		}
	})
	infof("ARP responder started (group=%d vip=%s)", gid, e.cfg.VIP4)
}

// ── NS (IPv6) ────────────────────────────────────────────────────────────────

func (e *Engine) startNSResponderLocked() {
	if e.ns != nil {
		return
	}
	vip, err := vipAddr(e.cfg.VIP6)
	if err != nil {
		return
	}
	fd, err := openPacketSocket(e.cfg.Interface, syscall.ETH_P_IPV6, time.Second)
	if err != nil {
		errorf("Failed to start NS responder: %v", err)
		return
	}
	r := &rawResponder{fd: fd}
	e.ns = r
	vip6 := vip.As16()
	gid := e.cfg.GroupID
	selfMAC := ifaceMAC(e.cfg.Interface) // see the ARP responder: the answer is sent from this node's own MAC
	go r.loop(func(fd int, frame []byte) {
		const eth, ip6 = 14, 40
		if len(frame) < eth+ip6+24 {
			return
		}
		h := frame[eth:]
		if h[0]>>4 != 6 || h[6] != 58 { // IPv6, next header ICMPv6
			return
		}
		icmp := h[ip6:]
		if icmp[0] != 135 || [16]byte(icmp[8:24]) != vip6 { // NS for our VIP
			return
		}
		src := [16]byte(h[8:24])
		srcMAC := [6]byte(frame[6:12])
		requester := netip.AddrFrom16(src).String()

		slot := e.PickAFN(requester)
		debugf("NS for VIP6 from %s — replying with slot %d vMAC %s", requester, slot, vmacStr(gid, slot))

		// Solicited + Override, unicast back to the solicitor.
		vm := vmacBytes(gid, slot)
		out := buildNA(srcMAC, [6]byte(ethSource(selfMAC, vm)), vm, vip6, src, 0x60000000)
		if _, err := syscall.Write(fd, out); err != nil {
			debugf("NA reply send failed: %v", err)
		}
	})
	infof("NS responder started (group=%d vip=%s)", gid, e.cfg.VIP6)
}

// ethSource is the Ethernet source of an answer sent on behalf of a slot: this node's own MAC, or the slot's MAC
// if the interface's cannot be read.
func ethSource(self, slot [6]byte) []byte {
	if self != ([6]byte{}) {
		return self[:]
	}
	return slot[:]
}

// buildARPReply answers an ARP request for the VIP: the payload says the VIP is at mac, the Ethernet source is this
// node's own MAC (see ethSource).
func buildARPReply(reqMAC, self, mac [6]byte, vip4, reqIP [4]byte) []byte {
	out := make([]byte, 0, 42)
	out = append(out, reqMAC[:]...)
	out = append(out, ethSource(self, mac)...)
	out = append(out, 0x08, 0x06)
	out = append(out, 0x00, 0x01, 0x08, 0x00, 6, 4, 0x00, 0x02)
	out = append(out, mac[:]...)
	out = append(out, vip4[:]...)
	out = append(out, reqMAC[:]...)
	out = append(out, reqIP[:]...)
	return out
}
