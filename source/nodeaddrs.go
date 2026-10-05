package main

import (
	"net"
	"net/netip"
	"os"
	"sort"
	"strings"
)

// NodeIface is one Ethernet interface of a node with the addresses the Topology drawing shows in the node's tooltip:
// its IPv4 addresses and its IPv6 global unicast (GUA) addresses, each with its prefix length.
type NodeIface struct {
	Name string   `json:"name"`
	V4   []string `json:"v4,omitempty"`
	V6   []string `json:"v6,omitempty"`
}

var gua6 = netip.MustParsePrefix("2000::/3")

// nodeIfacesFn lists the node's interfaces (replaceable in tests).
var nodeIfacesFn = func() []nodeIfaceRaw {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []nodeIfaceRaw
	for _, ifc := range ifs {
		r := nodeIfaceRaw{Name: ifc.Name, Loopback: ifc.Flags&net.FlagLoopback != 0, Ethernet: isEthernetIface(ifc.Name)}
		r.Addrs, _ = ifc.Addrs()
		out = append(out, r)
	}
	return out
}

type nodeIfaceRaw struct {
	Name     string
	Loopback bool
	Ethernet bool
	Addrs    []net.Addr
}

// isEthernetIface says whether name is a real Ethernet-type interface of the host: ARPHRD_ETHER, and either a physical
// device, a bridge or a bond.  Virtual links (veth, macvlan such as ddgw's own ddgwN.M, tunnels, containers' bridges
// excepted) are left out.
func isEthernetIface(name string) bool {
	base := "/sys/class/net/" + name + "/"
	if b, err := os.ReadFile(base + "type"); err != nil || strings.TrimSpace(string(b)) != "1" {
		return false
	}
	if strings.HasPrefix(name, "ddgw") {
		return false
	}
	if _, err := os.Stat("/sys/devices/virtual/net/" + name); err != nil {
		return true // a physical device
	}
	for _, kind := range []string{"bridge", "bonding"} {
		if _, err := os.Stat(base + kind); err == nil {
			return true
		}
	}
	return false
}

// ethernetAddrs lists the Ethernet interfaces that hold an IPv4 address or an IPv6 GUA, sorted by name.
func ethernetAddrs() []NodeIface {
	var out []NodeIface
	for _, r := range nodeIfacesFn() {
		if r.Loopback || !r.Ethernet {
			continue
		}
		ni := NodeIface{Name: r.Name}
		for _, a := range r.Addrs {
			n, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(n.IP)
			if !ok {
				continue
			}
			ones, _ := n.Mask.Size()
			ip = ip.Unmap()
			pfx := netip.PrefixFrom(ip, ones).String()
			switch {
			case ip.Is4():
				if !ip.IsLinkLocalUnicast() && !ip.IsLoopback() && !ip.IsUnspecified() {
					ni.V4 = append(ni.V4, pfx)
				}
			case gua6.Contains(ip):
				ni.V6 = append(ni.V6, pfx)
			}
		}
		if len(ni.V4)+len(ni.V6) > 0 {
			sort.Strings(ni.V4)
			sort.Strings(ni.V6)
			out = append(out, ni)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
