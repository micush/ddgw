package main

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNetToolSpec(t *testing.T) {
	prog, args, _, err := netToolSpec("ping", netip.MustParseAddr("10.1.2.3"))
	if err != nil || prog != "ping" || args[len(args)-2] != "--" || args[len(args)-1] != "10.1.2.3" {
		t.Errorf("ping: %s %v %v", prog, args, err)
	}
	for _, x := range args {
		if x == "-6" {
			t.Error("-6 for an IPv4 address")
		}
	}
	prog, args, _, err = netToolSpec("trace", netip.MustParseAddr("2001:db8::1"))
	if err != nil || prog != "mtr" || args[len(args)-1] != "2001:db8::1" {
		t.Errorf("trace: %s %v %v", prog, args, err)
	}
	has6 := false
	for _, x := range args {
		has6 = has6 || x == "-6"
	}
	if !has6 {
		t.Error("no -6 for an IPv6 address")
	}
	if _, _, _, err := netToolSpec("rm", netip.MustParseAddr("10.1.2.3")); err == nil {
		t.Error("an unknown tool was accepted")
	}
}

func TestNetToolRefusesBadInput(t *testing.T) {
	for _, bad := range []string{"", "-c 1000000", "10.0.0.1; id", "host.example.com", "224.0.0.1", "10.0.0.0/24"} {
		if _, err := netToolStart("ping", bad, "t"); err == nil {
			t.Errorf("%q accepted", bad)
		}
		if _, err := netToolGet("ping", bad); err == nil {
			t.Errorf("get %q accepted", bad)
		}
	}
	if _, err := netToolStart("nmap", "10.0.0.1", "t"); err == nil {
		t.Error("an unknown kind was accepted")
	}
}

// fakeTools puts stand-in ping and mtr scripts first in PATH.
func fakeTools(t *testing.T, ping, mtr string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"ping": ping, "mtr": mtr} {
		if body == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if ping == "" && mtr == "" {
		t.Setenv("PATH", dir) // neither program exists
		return
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func waitNet(t *testing.T, kind, addr string) *NetJob {
	t.Helper()
	for i := 0; i < 100; i++ {
		j, err := netToolGet(kind, addr)
		if err != nil {
			t.Fatal(err)
		}
		if j.State != "running" {
			return j
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("still running")
	return nil
}

func TestNetToolRuns(t *testing.T) {
	fakeTools(t, `echo "64 bytes from $*"; exit 1`, `echo "HOST: x  Loss%  Snt"; echo " 1.|-- 10.9.9.1  0.0%  10"`)
	if j, err := netToolStart("ping", "10.9.9.9", "t"); err != nil || j.State != "running" {
		t.Fatalf("start: %v %v", j, err)
	}
	j := waitNet(t, "ping", "10.9.9.9")
	// ping exits 1 when nothing came back: that is output to show, not a failure to run
	if j.State != "done" || !strings.Contains(j.Text, "64 bytes from") || !strings.HasSuffix(strings.TrimSpace(j.Text), "10.9.9.9") {
		t.Errorf("ping: %s %q", j.State, j.Text)
	}
	if _, err := netToolStart("trace", "10.9.9.8", "t"); err != nil {
		t.Fatal(err)
	}
	j = waitNet(t, "trace", "10.9.9.8")
	if j.State != "done" || !strings.Contains(j.Text, "10.9.9.1") {
		t.Errorf("trace: %s %q", j.State, j.Text)
	}
	if j, _ := netToolGet("ping", "10.9.9.77"); j.State != "none" {
		t.Errorf("never run: %s", j.State)
	}
}

func TestNetToolOutputGrowsWhileRunning(t *testing.T) {
	fakeTools(t, `echo first; sleep 1; echo second`, "")
	if _, err := netToolStart("ping", "10.9.9.5", "t"); err != nil {
		t.Fatal(err)
	}
	seen := false
	for i := 0; i < 20 && !seen; i++ {
		j, _ := netToolGet("ping", "10.9.9.5")
		seen = j.State == "running" && strings.Contains(j.Text, "first") && !strings.Contains(j.Text, "second")
		time.Sleep(20 * time.Millisecond)
	}
	if !seen {
		t.Error("the first line was not readable while the command still ran")
	}
	waitNet(t, "ping", "10.9.9.5")
}

func TestNetToolMissingProgram(t *testing.T) {
	fakeTools(t, "", "")
	_, err := netToolStart("trace", "10.9.9.4", "t")
	if err == nil || !strings.Contains(err.Error(), "mtr is not installed") {
		t.Errorf("got %v", err)
	}
}

func TestNetToolRelayable(t *testing.T) {
	if err := proxyAllowed("POST", "/api/nettool"); err != nil {
		t.Errorf("not relayable: %v", err)
	}
}
