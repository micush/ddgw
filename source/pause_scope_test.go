package main

import "testing"

func pauseTestConfig() *DaemonConfig {
	dc := newDaemonConfig()
	g := defaultGroup()
	g.DNS = nil
	dc.Groups = []GroupConfig{g}
	dc.DNS.Servers = []string{"192.0.2.1:53", "192.0.2.2:53"}
	dc.DNS.Queries = []DNSQuery{{Name: "a.example", Type: "A"}, {Name: "b.example", Type: "A"}}
	if err := dc.Validate(); err != nil {
		panic(err)
	}
	return dc
}

func TestPauseScopeGatewayServerDomain(t *testing.T) {
	dc := pauseTestConfig()
	ed := func(kind, action, scope string) error {
		_, err := applyCanvasEdit(dc, canvasEdit{Action: action, Kind: kind, Group: 1, Server: "192.0.2.1:53", Name: "a.example", Scope: scope})
		return err
	}
	// gateway: default node, all is shared
	if err := ed("gateway", "pause", ""); err != nil || !dc.Groups[0].Paused {
		t.Fatalf("gateway default scope: %v %v", err, dc.Groups[0].Paused)
	}
	if err := ed("gateway", "pause", "all"); err != nil || !dc.Groups[0].PausedAll {
		t.Fatalf("gateway all: %v", err)
	}
	// server: default all, node is local
	if err := ed("server", "pause", ""); err != nil || len(dc.Groups[0].DNS.PausedServers) != 1 {
		t.Fatalf("server default scope: %v %v", err, dc.Groups[0].DNS.PausedServers)
	}
	if err := ed("server", "pause", "node"); err != nil || len(dc.PausedServersHere) != 1 {
		t.Fatalf("server node: %v", err)
	}
	// domain: scope required; the last active domain of a server cannot be paused
	if err := ed("domain", "pause", ""); err == nil {
		t.Fatal("domain pause without a scope accepted")
	}
	if err := ed("domain", "pause", "all"); err != nil || len(dc.Groups[0].DNS.PausedQueries) != 1 {
		t.Fatalf("domain all: %v %v", err, dc.Groups[0].DNS.PausedQueries)
	}
	if err := ed("domain", "pause", "all"); err == nil {
		t.Fatal("pausing twice accepted")
	}
	dc.Groups[0].DNS.PausedQueries = nil
	if err := ed("domain", "pause", "node"); err != nil || len(dc.PausedQueriesHere) != 1 {
		t.Fatalf("domain node: %v", err)
	}
	if _, err := applyCanvasEdit(dc, canvasEdit{Action: "pause", Kind: "domain", Group: 1, Server: "192.0.2.1:53", Name: "b.example", Scope: "all"}); err == nil {
		t.Fatal("paused the last active domain of a server")
	}
	if err := ed("domain", "resume", "node"); err != nil || len(dc.PausedQueriesHere) != 0 {
		t.Fatalf("domain resume: %v", err)
	}
	if err := dc.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestPauseScopeEffectiveLiveShared(t *testing.T) {
	dc := pauseTestConfig()
	k := queryKey("192.0.2.1:53", "a.example", "A")
	dc.PausedQueriesHere = []string{k}
	dc.PausedServersHere = []string{"192.0.2.2:53"}
	if err := dc.Validate(); err != nil {
		t.Fatal(err)
	}
	eff := dc.effective()
	if eff.DNS.queryPausedScope("192.0.2.1:53", DNSQuery{Name: "a.example", Type: "A"}) != "node" || eff.DNS.serverPausedScope("192.0.2.2:53") != "node" {
		t.Fatal("effective config lost the node-local pauses")
	}
	live := eff.DNS.live()
	if len(live.Servers) != 1 || live.Servers[0] != "192.0.2.1:53" {
		t.Fatalf("live servers: %v", live.Servers)
	}
	for _, q := range live.queriesFor("192.0.2.1:53") {
		if q.Name == "a.example" {
			t.Fatal("paused domain still probed")
		}
	}
	// the node-local lists are never replicated; the shared ones are
	if h1 := sharedOf(dc).hash(); h1 != sharedOf(pauseTestConfig()).hash() {
		t.Fatal("node-local pauses changed the shared hash")
	}
	dc.DNS.PausedQueries = []string{queryKey("192.0.2.2:53", "b.example", "A")}
	if sharedOf(dc).hash() == sharedOf(pauseTestConfig()).hash() {
		t.Fatal("shared domain pause not replicated")
	}
	// pausing a domain must restart the pool; a rename of a server label must not
	a, b := pauseTestConfig(), pauseTestConfig()
	b.DNS.PausedQueries = []string{k}
	if reflectEqualLive(a.DNS, b.DNS) {
		t.Fatal("domain pause does not change the live pool")
	}
}

func reflectEqualLive(a, b DNSConfig) bool {
	x, y := a.live(), b.live()
	return len(x.queriesFor("192.0.2.1:53")) == len(y.queriesFor("192.0.2.1:53"))
}
