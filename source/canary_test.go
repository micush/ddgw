package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The first node to take a new version is the canary: the others hold while it
// has run the version for less than updateSoak or shows a bad gateway, and while
// an update to that version has failed on any member.
func TestCanaryHold(t *testing.T) {
	a, b := newTNode(t, "A"), newTNode(t, "B")
	b.join(a)
	for i := 0; i < 2; i++ {
		a.sync()
		b.sync()
	}
	next := fmt.Sprintf("%d", mustInt(version())+1)
	cl := a.mg.cl
	peer := b.addr
	set := func(m PeerStatusMsg) {
		cl.mu.Lock()
		pi := cl.info[peer]
		pi.Reachable, pi.Msg = true, m
		cl.mu.Unlock()
	}
	old := updateSoak
	updateSoak = 2 * time.Minute
	defer func() { updateSoak = old }()

	set(PeerStatusMsg{Version: version()})
	if w := cl.canaryHold(next); w != "" {
		t.Fatalf("nobody runs %s yet; got hold %q", next, w)
	}
	set(PeerStatusMsg{Version: next, UptimeSec: 30})
	if w := cl.canaryHold(next); !strings.Contains(w, "min left") {
		t.Fatalf("a young canary must hold the rest: %q", w)
	}
	set(PeerStatusMsg{Version: next, UptimeSec: 3600, Gateways: []GwState{{GroupID: 1, Serving: true, Health: "bad"}}})
	if w := cl.canaryHold(next); !strings.Contains(w, "bad") {
		t.Fatalf("a bad gateway must hold the rest: %q", w)
	}
	set(PeerStatusMsg{Version: next, UptimeSec: 3600, Gateways: []GwState{{GroupID: 1, Serving: true, Health: "ok"}}})
	if w := cl.canaryHold(next); w != "" {
		t.Fatalf("a soaked healthy canary must release the rest: %q", w)
	}
	set(PeerStatusMsg{Version: version(), UpdateFailed: next})
	if w := cl.canaryHold(next); !strings.Contains(w, "failed") {
		t.Fatalf("a failed update must hold the rest: %q", w)
	}
	set(PeerStatusMsg{Version: next, UptimeSec: 3600, Updating: true})
	if w := cl.canaryHold(next); w != "" {
		t.Fatalf("an updating member is the rolling rule's business, not the canary's: %q", w)
	}
}

// The canary shows a countdown while it soaks, and only while another member still runs an older version.
func TestSoakLeft(t *testing.T) {
	a, b := newTNode(t, "A"), newTNode(t, "B")
	b.join(a)
	for i := 0; i < 2; i++ {
		a.sync()
		b.sync()
	}
	next := fmt.Sprintf("%d", mustInt(version())+1)
	cl := a.mg.cl
	old := updateSoak
	updateSoak = 2 * time.Minute
	defer func() { updateSoak = old }()
	left := func(m PeerStatusMsg) map[string]int {
		cl.mu.Lock()
		defer cl.mu.Unlock()
		pi := cl.info[b.addr]
		pi.Reachable, pi.Msg = true, m
		return cl.soakLeftLocked(cl.node.Snapshot())
	}
	if l := left(PeerStatusMsg{Version: next, UptimeSec: 20}); l[b.addr] < 99 || l[b.addr] > 101 {
		t.Fatalf("the canary has ~100s left: %v", l)
	}
	if l := left(PeerStatusMsg{Version: next, UptimeSec: 200}); len(l) != 0 {
		t.Fatalf("a soaked canary shows nothing: %v", l)
	}
	if l := left(PeerStatusMsg{Version: version(), UptimeSec: 5}); len(l) != 0 {
		t.Fatalf("with no newer version nothing soaks: %v", l)
	}
}
