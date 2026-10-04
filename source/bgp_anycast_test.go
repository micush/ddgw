package main

import (
	"context"
	"testing"
	"time"
)

func TestAnycastBGPStatus(t *testing.T) {
	p := func(af, state string) BGPPeer { return BGPPeer{Peer: "x", AF: af, State: state} }
	cases := []struct {
		name  string
		addr  string
		peers []BGPPeer
		known bool
		want  string
	}{
		{"established", "10.9.9.9", []BGPPeer{p("ipv4", "Established")}, true, "ok"},
		{"one of two established", "10.9.9.9", []BGPPeer{p("ipv4", "Connect"), p("ipv4", "Established")}, true, "ok"},
		{"connect", "10.9.9.9", []BGPPeer{p("ipv4", "Connect")}, true, "warn"},
		{"active", "10.9.9.9", []BGPPeer{p("ipv4", "Active")}, true, "warn"},
		{"opensent", "10.9.9.9", []BGPPeer{p("ipv4", "OpenSent")}, true, "warn"},
		{"idle", "10.9.9.9", []BGPPeer{p("ipv4", "Idle")}, true, "bad"},
		{"none", "10.9.9.9", nil, true, "bad"},
		{"v6 peer does not serve v4", "10.9.9.9", []BGPPeer{p("ipv6", "Established")}, true, "bad"},
		{"v4 peer does not serve v6", "2001:db8::1", []BGPPeer{p("ipv4", "Established")}, true, "bad"},
		{"v6 established", "2001:db8::1", []BGPPeer{p("ipv6", "Established")}, true, "ok"},
		{"bgpd silent", "10.9.9.9", nil, false, "bad"},
	}
	for _, c := range cases {
		got, why := anycastBGPStatus(c.addr, c.peers, c.known, 0)
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
		if (got == "ok") != (why == "") {
			t.Errorf("%s: detail %q for %q", c.name, why, got)
		}
	}
}

// The canvas colours a held address by its BGP sessions, and leaves it alone
// when this node does not manage BGP.
func TestMarkAnycastBGPColour(t *testing.T) {
	hookLo(t)
	up := newFakeDNS(t)
	g := defaultGroup()
	g.Interface = "ddgwnone0"
	d := testDNSCfg(up.addr)
	d.ListenPort = freeUDPPort(t)
	g.DNSProxy, g.DNS, g.ExtraVIPs = true, &d, []string{"127.0.0.9"}
	dc := newDaemonConfig()
	dc.Groups = []GroupConfig{g}
	s := NewSupervisor(context.Background(), dc)
	t.Cleanup(s.StopAll)
	s.refreshPool(dc, true)
	s.mu.Lock()
	s.startGroupLocked(dc.Groups[0])
	s.mu.Unlock()

	oldDir, oldBGP := frrDir, vtyshBGP
	frrDir = t.TempDir()
	t.Cleanup(func() { frrDir, vtyshBGP = oldDir, oldBGP; bgpLive.at = time.Time{} })
	state := ""
	vtyshBGP = func() ([]byte, error) {
		return []byte(`{"ipv4Unicast":{"peers":{"10.0.0.1":{"remoteAs":65001,"state":"` + state + `"}}}}`), nil
	}
	ss := &StatusServer{sup: s}
	look := func(st string) string {
		state = st
		bgpLive.at = time.Time{}
		gs := []CanvasGateway{{GroupID: 1}}
		ss.markAnycast(gs)
		if len(gs[0].Anycast) != 1 || !gs[0].Anycast[0].Up {
			t.Fatalf("%+v", gs[0].Anycast)
		}
		return gs[0].Anycast[0].Status
	}
	if got := look("Established"); got != "" {
		t.Fatalf("BGP not managed here: status %q", got)
	}
	s.mu.Lock()
	s.dc.BGP = &BGPConfig{ASN: 65000}
	s.mu.Unlock()
	for st, want := range map[string]string{"Established": "ok", "Connect": "warn", "Idle": "bad"} {
		if got := look(st); got != want {
			t.Errorf("%s: %q, want %q", st, got, want)
		}
	}
}

// A session that never establishes turns from amber to red.
func TestAnycastBGPGrace(t *testing.T) {
	peers := []BGPPeer{{AF: "ipv4", State: "Connect"}}
	est := []BGPPeer{{AF: "ipv4", State: "Established"}}
	now := time.Now()
	at := func(d time.Duration, p []BGPPeer) string {
		st, _ := anycastBGP("10.7.7.7", p, true, now.Add(d))
		return st
	}
	for d := time.Duration(0); d < bgpGrace; d += 3 * time.Second {
		if st := at(d, peers); st != "warn" {
			t.Fatalf("at %v: %q", d, st)
		}
	}
	if st := at(bgpGrace, peers); st != "bad" {
		t.Fatalf("after the grace: %q", st)
	}
	if st := at(bgpGrace+3*time.Second, est); st != "ok" {
		t.Fatalf("established: %q", st)
	}
	// established resets the clock; a fresh failure starts amber again
	if st := at(bgpGrace+6*time.Second, peers); st != "warn" {
		t.Fatalf("after re-failing: %q", st)
	}
	// not looked at for a while (withdrawn): starts over
	anycastBGP("10.7.7.8", peers, true, now)
	if st, _ := anycastBGP("10.7.7.8", peers, true, now.Add(5*time.Minute)); st != "warn" {
		t.Fatalf("after a gap: %q", st)
	}
}
