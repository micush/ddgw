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
	// A (this node) is on the old version here, so make the roles clear: a soaked peer on the newest version ends the soak for all
	if l := left(PeerStatusMsg{Version: next, UptimeSec: 3600}); len(l) != 0 {
		t.Fatalf("after the canary soaked no node shows a soak: %v", l)
	}
	if l := left(PeerStatusMsg{Version: version(), UptimeSec: 5}); len(l) != 0 {
		t.Fatalf("with no newer version nothing soaks: %v", l)
	}
}

// A member that vanishes while updating holds the others for a while: it may be crash-looping.
func TestCanaryHoldLostContact(t *testing.T) {
	a, b := newTNode(t, "A"), newTNode(t, "B")
	b.join(a)
	for i := 0; i < 2; i++ {
		a.sync()
		b.sync()
	}
	next := fmt.Sprintf("%d", mustInt(version())+1)
	cl := a.mg.cl
	set := func(reach bool, m PeerStatusMsg, seen time.Time) {
		cl.mu.Lock()
		pi := cl.info[b.addr]
		pi.Reachable, pi.Msg, pi.LastSeen = reach, m, seen
		cl.mu.Unlock()
	}
	set(false, PeerStatusMsg{Version: version(), Updating: true}, time.Now().Add(-30*time.Second))
	if w := cl.canaryHold(next); !strings.Contains(w, "lost contact") {
		t.Fatalf("a member lost while updating must hold the rest: %q", w)
	}
	set(false, PeerStatusMsg{Version: version(), Updating: true}, time.Now().Add(-time.Hour))
	if w := cl.canaryHold(next); w != "" {
		t.Fatalf("a member gone for an hour must not hold updates forever: %q", w)
	}
	set(false, PeerStatusMsg{Version: version()}, time.Now())
	if w := cl.canaryHold(next); w != "" {
		t.Fatalf("a member that was down and not updating is not a canary: %q", w)
	}
}

// After the boot guard rolls a version back, the node reports it failed (it survives the restart) until a newer one is staged.
func TestRolledBackVersionStaysFailed(t *testing.T) {
	u, err := NewUpdater(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	next := fmt.Sprintf("%d", mustInt(version())+1)
	if u.FailedFor() != "" {
		t.Fatal("nothing failed yet")
	}
	u.writeGuard(bootGuard{State: "rolled-back", From: version(), To: next})
	if got := u.FailedFor(); got != next {
		t.Fatalf("FailedFor = %q, want %q", got, next)
	}
	u.writeGuard(bootGuard{State: "applying", From: version(), To: next})
	if got := u.FailedFor(); got != "" {
		t.Fatalf("a new attempt clears the failure: %q", got)
	}
}

// Every node serves its own gateway and is excluded from the others' (as in a cluster of single-node gateways in four
// subnets).  The update order must still apply: no member may look "held back by the safety rule" just because the others
// do not report its gateway.  Before v341 every member did, so all of them updated at once and the canary never held.
func TestOrderAppliesWhenEveryNodeServesItsOwnGateway(t *testing.T) {
	names := []string{"A", "B", "C", "D"}
	var members []peerGateways
	var groups []GroupConfig
	for i, n := range names {
		gid := i + 1
		pg := selfGateways(n, true, []GwState{{GroupID: gid, Serving: true}})
		pg.NodeID = "id" + n
		members = append(members, pg)
		var ex []string
		for _, o := range names {
			if o != n {
				ex = append(ex, "id"+o)
			}
		}
		groups = append(groups, GroupConfig{GroupID: gid, ExcludedNodes: ex})
	}
	if !heldBySafety("A", members[0].gwList(), members) {
		t.Fatal("test set-up: without the exclusions A must look held (that is the bug)")
	}
	markGatewayExcluded(members, groups)
	for _, m := range members {
		if heldBySafety(m.Addr, m.gwList(), members) {
			t.Fatalf("%s must not look held back once the exclusions are known", m.Addr)
		}
	}
}

// gwList turns the map form back into the list heldBySafety takes.
func (p peerGateways) gwList() []GwState {
	var out []GwState
	for id, s := range p.Serving {
		out = append(out, GwState{GroupID: id, Serving: s})
	}
	return out
}
