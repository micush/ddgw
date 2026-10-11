package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Ping and traceroute of a client, from Statistics ▸ Top clients ▸ right-click ▸ Ping / Traceroute.  The command runs
// on this node (the node picked in the Node menu) with fixed options: the address is the only thing a caller chooses,
// and it is checked to be one host's address before it gets near the command line.  The output is kept while the
// command runs, so the popup can show it as it grows.
const (
	netToolParallel = 4
	netToolTextMax  = 16384
	pingTimeout     = 30 * time.Second
	traceTimeout    = 120 * time.Second
)

// NetJob is a ping or traceroute, running or finished.
type NetJob struct {
	Kind  string `json:"kind"` // ping, trace
	Addr  string `json:"addr"`
	State string `json:"state"` // running, done, error, none
	Text  string `json:"text,omitempty"`
	At    int64  `json:"at,omitempty"`
	End   int64  `json:"end,omitempty"`
}

var netJobs = struct {
	mu   sync.Mutex
	jobs map[string]*NetJob
}{jobs: map[string]*NetJob{}}

// netToolSpec is the program and arguments for kind and addr: ten pings two seconds apart at most, or an mtr report
// of ten probes per hop, both without name lookups.
func netToolSpec(kind string, a netip.Addr) (string, []string, time.Duration, error) {
	switch kind {
	case "ping":
		args := []string{"-n", "-c", "10", "-W", "2"}
		if a.Is6() {
			args = append(args, "-6")
		}
		return "ping", append(args, "--", a.String()), pingTimeout, nil
	case "trace":
		args := []string{"-n", "-r", "-w", "-c", "10"}
		if a.Is6() {
			args = append(args, "-6")
		}
		return "mtr", append(args, "--", a.String()), traceTimeout, nil
	}
	return "", nil, 0, fmt.Errorf("unknown tool %q", kind)
}

// netBuf is the output of a running job: every write goes straight into the job's text.
type netBuf struct {
	j *NetJob
}

func (b netBuf) Write(p []byte) (int, error) {
	netJobs.mu.Lock()
	if room := netToolTextMax - len(b.j.Text); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		b.j.Text += string(p)
	}
	netJobs.mu.Unlock()
	return len(p), nil
}

// netToolStart begins ping or traceroute of addr (or returns the one already running).  A finished one is replaced.
func netToolStart(kind, addr, actor string) (*NetJob, error) {
	a, err := scanAddr(addr)
	if err != nil {
		return nil, err
	}
	prog, args, limit, err := netToolSpec(kind, a)
	if err != nil {
		return nil, err
	}
	path, err := exec.LookPath(prog)
	if err != nil {
		pkg := map[string]string{"ping": "iputils-ping (iputils on the RHEL family)", "mtr": "mtr"}[prog]
		return nil, fmt.Errorf("%s is not installed on this node (install the %s package)", prog, pkg)
	}
	key := kind + "|" + a.String()
	netJobs.mu.Lock()
	if j := netJobs.jobs[key]; j != nil && j.State == "running" {
		c := *j
		netJobs.mu.Unlock()
		return &c, nil
	}
	running := 0
	for _, j := range netJobs.jobs {
		if j.State == "running" {
			running++
		}
	}
	if running >= netToolParallel {
		netJobs.mu.Unlock()
		return nil, errors.New("too many are running already; try again in a moment")
	}
	j := &NetJob{Kind: kind, Addr: a.String(), State: "running", At: time.Now().Unix()}
	netJobs.jobs[key] = j
	for k, o := range netJobs.jobs { // finished ones older than an hour are forgotten
		if o.State != "running" && time.Now().Unix()-o.End > 3600 {
			delete(netJobs.jobs, k)
		}
	}
	c := *j
	netJobs.mu.Unlock()
	infof("nettool: %q started %s of %s", actor, prog, j.Addr)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), limit)
		defer cancel()
		cmd := exec.CommandContext(ctx, path, args...)
		cmd.Stdout, cmd.Stderr = netBuf{j}, netBuf{j}
		err := cmd.Run()
		netJobs.mu.Lock()
		j.End = time.Now().Unix()
		j.State = "done"
		if err != nil {
			// ping exits 1 when nothing came back: its output already says so, and is not an error here
			var ee *exec.ExitError
			if ctx.Err() != nil {
				j.State = "error"
				j.Text = strings.TrimSpace(j.Text + "\n(stopped after " + limit.String() + ")")
			} else if !errors.As(err, &ee) || strings.TrimSpace(j.Text) == "" {
				j.State = "error"
				if strings.TrimSpace(j.Text) == "" {
					j.Text = err.Error()
				}
			}
		}
		if strings.TrimSpace(j.Text) == "" {
			j.Text = prog + " printed nothing"
		}
		netJobs.mu.Unlock()
		infof("nettool: %s of %s %s", prog, j.Addr, j.State)
	}()
	return &c, nil
}

// netToolGet is the state of kind for addr: running, done, error, or none when there never was one.
func netToolGet(kind, addr string) (*NetJob, error) {
	a, err := scanAddr(addr)
	if err != nil {
		return nil, err
	}
	if _, _, _, err := netToolSpec(kind, a); err != nil {
		return nil, err
	}
	netJobs.mu.Lock()
	defer netJobs.mu.Unlock()
	if j := netJobs.jobs[kind+"|"+a.String()]; j != nil {
		c := *j
		return &c, nil
	}
	return &NetJob{Kind: kind, Addr: a.String(), State: "none"}, nil
}
