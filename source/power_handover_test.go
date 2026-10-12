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
