package main

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
)

func TestServerIP(t *testing.T) {
	for in, want := range map[string]string{"8.8.8.8": "8.8.8.8", "8.8.8.8:5353": "8.8.8.8", "[2001:db8::1]:53": "2001:db8::1", "2001:db8::1": "2001:db8::1"} {
		if a, ok := serverIP(in); !ok || a.String() != want {
			t.Errorf("%q: %v %v", in, a, ok)
		}
	}
	for _, in := range []string{"dns.example.com", "tls://dns.example.com", "https://1.1.1.1/dns-query", ""} {
		if a, ok := serverIP(in); ok {
			t.Errorf("%q has no address of its own, got %v", in, a)
		}
	}
}

func TestPlanServerRegistration(t *testing.T) {
	dc := &DaemonConfig{}
	if _, err := applyCanvasEdit(dc, canvasEdit{Action: "add", Kind: "gateway", VIP: "10.0.0.1/24", Interface: "eth0"}); err != nil {
		t.Fatal(err)
	}
	add := canvasEdit{Action: "add", Kind: "server", Group: 1, Server: "8.8.8.8", Name: "google.com", Label: "dns-a.corp.example"}
	if _, err := applyCanvasEdit(dc, add); err != nil {
		t.Fatal(err)
	}
	set := canvasEdit{Action: "set", Kind: "server", Group: 1, Server: "8.8.8.8", Label: "DNS-B.Corp.Example"}
	r := planServerRegistration(dc, set)
	if r == nil || r.fqdn != "dns-b.corp.example" || r.old != "dns-a.corp.example" || r.addr != netip.MustParseAddr("8.8.8.8") {
		t.Fatalf("a full name is registered and the old full name removed: %+v", r)
	}
	// the same name again: nothing old to remove
	set.Label = "dns-a.corp.example"
	if r := planServerRegistration(dc, set); r == nil || r.old != "" {
		t.Fatalf("%+v", r)
	}
	// a short name is only a label; an old short name has no record to remove
	if r := planServerRegistration(dc, canvasEdit{Action: "set", Kind: "server", Group: 1, Server: "8.8.8.8", Label: "dns-a"}); r != nil {
		t.Fatalf("a short name registers nothing: %+v", r)
	}
	if _, err := applyCanvasEdit(dc, canvasEdit{Action: "set", Kind: "server", Group: 1, Server: "8.8.8.8", Label: "dns-a"}); err != nil {
		t.Fatal(err)
	}
	if r := planServerRegistration(dc, canvasEdit{Action: "set", Kind: "server", Group: 1, Server: "8.8.8.8", Label: "dns-c.corp.example"}); r == nil || r.old != "" {
		t.Fatalf("%+v", r)
	}
	for _, e := range []canvasEdit{
		{Action: "set", Kind: "server", Group: 1, Server: "8.8.8.8", Label: "-"},
		{Action: "set", Kind: "server", Group: 1, Server: "8.8.8.8", Label: "Google DNS v2.0"}, // not a host name
		{Action: "set", Kind: "server", Group: 1, Server: "8.8.8.8", Label: "a..b"},
		{Action: "set", Kind: "server", Group: 1, Server: "dns.example.com", Label: "x.corp.example"}, // no address of its own
		{Action: "del", Kind: "server", Group: 1, Server: "8.8.8.8", Label: "x.corp.example"},
		{Action: "set", Kind: "gateway", Group: 1, Label: "x.corp.example"},
	} {
		if r := planServerRegistration(dc, e); r != nil {
			t.Errorf("%+v registers %+v", e, r)
		}
	}
}

func TestRegisterServerName(t *testing.T) {
	var got regCall
	m := &Mgmt{regNameFn: func(_ context.Context, name, old string, a netip.Addr) ([]string, error) {
		got = regCall{name, old, a.String()}
		return []string{"A ok"}, nil
	}}
	msg, bad := m.registerServerName(&serverRegistration{fqdn: "dns-b.corp.example", old: "dns-a.corp.example", addr: netip.MustParseAddr("8.8.8.8")}, "t")
	if bad != "" || !strings.Contains(msg, "registered dns-b.corp.example") || got != (regCall{"dns-b.corp.example", "dns-a.corp.example", "8.8.8.8"}) {
		t.Fatalf("%q %q %+v", msg, bad, got)
	}
	m.regNameFn = func(context.Context, string, string, netip.Addr) ([]string, error) {
		return []string{"A of x: no zone found"}, errors.New("no zone found")
	}
	if msg, bad := m.registerServerName(&serverRegistration{fqdn: "x.corp.example", addr: netip.MustParseAddr("8.8.8.8")}, "t"); bad == "" || !strings.Contains(msg, "DNS was not updated") {
		t.Fatalf("%q %q", msg, bad)
	}
}
