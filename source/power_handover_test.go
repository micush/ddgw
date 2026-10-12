package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A scheduled restart or shut down hands the primary role over a minute before it fires, not before.
func TestScheduledPowerHandsOverThePrimary(t *testing.T) {
	a, b := newTNode(t, "A"), newTNode(t, "B")
	b.join(a)
	for i := 0; i < 3; i++ {
		a.sync()
		b.sync()
	}
	old := powerScheduledFile
	defer func() { powerScheduledFile = old }()
	powerScheduledFile = filepath.Join(t.TempDir(), "scheduled")
	write := func(in time.Duration) {
		body := fmt.Sprintf("USEC=%d\nWARN_WALL=1\nMODE=reboot\n", time.Now().Add(in).UnixMicro())
		if err := os.WriteFile(powerScheduledFile, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(time.Hour) // an hour away: too early
	a.mg.powerHandoverTick()
	if a.mg.cl.node.Snapshot().Role != RolePrimary {
		t.Fatal("handed over an hour early")
	}
	os.Remove(powerScheduledFile) // cancelled or none
	a.mg.powerHandoverTick()
	if a.mg.cl.node.Snapshot().Role != RolePrimary {
		t.Fatal("handed over with nothing scheduled")
	}
	write(30 * time.Second) // inside the last minute
	a.mg.powerHandoverTick()
	if a.mg.cl.node.Snapshot().Role != RoleReplica || b.mg.cl.node.Snapshot().Role != RolePrimary {
		t.Fatalf("not handed over: A %s, B %s", a.mg.cl.node.Snapshot().Role, b.mg.cl.node.Snapshot().Role)
	}
}

// Pausing the primary node hands the primary role over; pausing a replica leaves it where it is; resuming never moves it.
func TestPausingThePrimaryHandsItOver(t *testing.T) {
	a, b := newTNode(t, "A"), newTNode(t, "B")
	b.join(a)
	for i := 0; i < 3; i++ {
		a.sync()
		b.sync()
	}
	a.mg.othersFn, b.mg.othersFn = func() int { return 1 }, func() int { return 1 }
	role := func(n *tnode) string { return string(n.mg.cl.node.Snapshot().Role) }
	if _, err := b.mg.NodePause(true, "test"); err != nil {
		t.Fatal(err)
	}
	if role(a) != string(RolePrimary) {
		t.Fatalf("pausing a replica moved the role: A %s, B %s", role(a), role(b))
	}
	if _, err := a.mg.NodePause(true, "test"); err != nil {
		t.Fatal(err)
	}
	if role(a) != string(RoleReplica) || role(b) != string(RolePrimary) {
		t.Fatalf("pausing the primary did not hand over: A %s, B %s", role(a), role(b))
	}
	if _, err := a.mg.NodePause(false, "test"); err != nil {
		t.Fatal(err)
	}
	if role(b) != string(RolePrimary) {
		t.Fatalf("resuming moved the role back: B %s", role(b))
	}
}
