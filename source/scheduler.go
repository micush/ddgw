package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The scheduler (Operate ▸ Scheduler): tasks that run on every node of the cluster, or on chosen nodes, at times given
// as hourly / daily / weekly / monthly / yearly rules, including "the 3rd Saturday of every month".  A task also has an
// offset window: it is due at its scheduled time, and each node runs it at a time of its own somewhere in that time plus
// the window, so the nodes do not all act at once.  The task list is part of the shared settings (DNSConfig.Schedule,
// replicated like the rest); every node works out for itself what is due, so a node that is down only misses its own runs.
// The history of what ran is kept per node.

// SchedTask is one task.
type SchedTask struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Disabled bool   `json:"disabled,omitempty"`
	// Action is one of schedActions, or "shell" with Command.
	Action     string `json:"action"`
	Command    string `json:"command,omitempty"`
	TimeoutSec int    `json:"timeout_sec,omitempty"` // shell: stopped after this long (default 300)
	// Nodes are the node IDs that run it; empty means every node.
	Nodes []string `json:"nodes,omitempty"`
	// Freq is hourly, daily, weekly, monthly or yearly; Every N of them (0 or 1 = each).
	Freq  string `json:"freq"`
	Every int    `json:"every,omitempty"`
	// At is the time of day, "HH:MM[:SS]"; for an hourly task only the minute and second count.
	At string `json:"at,omitempty"`
	// Days (weekly): 0 = Sunday … 6 = Saturday.
	Days []int `json:"days,omitempty"`
	// Day (monthly, yearly): the day of the month, 1-31, or -1 for the last.  With Nth set it is not used: the day is the
	// Nth (1-4, or -1 for the last) Weekday (0 = Sunday) of the month.
	Day int `json:"day,omitempty"`
	// Nths and Weekdays (monthly, yearly): when Nths is set the days are the given occurrences (1-4, or -1 for the last)
	// of the given weekdays (0 = Sunday) of the month, e.g. the 2nd and 3rd Saturday and Sunday.
	Nths     []int `json:"nths,omitempty"`
	Weekdays []int `json:"weekdays,omitempty"`
	// Nth and Weekday are the older single form; validate folds them into Nths and Weekdays.
	Nth     int `json:"nth,omitempty"`
	Weekday int `json:"weekday,omitempty"`
	// Month (yearly): 1-12.
	Month int `json:"month,omitempty"`
	// TZ is no longer used: every time is UTC.  The field stays so older settings still load; validate clears it.
	TZ string `json:"tz,omitempty"`
	// WindowSec is the offset window: each node runs the task at the scheduled time plus a time of its own between 0 and
	// this many seconds.
	WindowSec int `json:"window_sec,omitempty"`
	// Until ("HH:MM", optional) states the window as an end time instead: the task runs between At and Until (the next
	// day when Until is not later than At).  validate turns it into WindowSec.
	Until string `json:"until,omitempty"`
}

const (
	maxSchedTasks   = 200
	maxSchedWindow  = 7 * 24 * 3600
	maxSchedCommand = 4096
	schedGrace      = 15 * time.Second // a run is still on time this long after the end of its window (the tick is 5 s)
	schedTick       = 5 * time.Second
	schedHistoryMax = 300
	schedOutputMax  = 4096
)

// schedActions are the built-in actions.  The first is the label shown in the page.
var schedActions = map[string]string{
	"dnsdir_restart": "Restart dnsdir (waits until it is safe)",
	"cache_clear":    "Empty the answer cache",
	"node_pause":     "Pause this node",
	"node_resume":    "Resume this node",
	"stats_clear":    "Clear the statistics",
	"update_online":  "Update from online (newest release, when newer)",
	"node_reboot":    "Reboot this node (waits until it is safe)",
}

var schedEveryMax = map[string]int{"hourly": 720, "daily": 366, "weekly": 52, "monthly": 24, "yearly": 10}

// loc is UTC: the scheduler works in UTC on every node.
func (t *SchedTask) loc() *time.Location { return time.UTC }

func parseSchedAt(s string) (h, m, sec int, err error) {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, 0, 0, fmt.Errorf("the time %q must be HH:MM or HH:MM:SS", s)
	}
	n := make([]int, 3)
	for i, p := range parts {
		v, e := strconv.Atoi(p)
		if e != nil || v < 0 || len(p) > 2 {
			return 0, 0, 0, fmt.Errorf("the time %q must be HH:MM or HH:MM:SS", s)
		}
		n[i] = v
	}
	if n[0] > 23 || n[1] > 59 || n[2] > 59 {
		return 0, 0, 0, fmt.Errorf("the time %q is not a time of day", s)
	}
	return n[0], n[1], n[2], nil
}

// validate checks the task and fills in what was left out.
func (t *SchedTask) validate() error {
	t.Name = strings.TrimSpace(t.Name)
	if t.ID == "" {
		t.ID = randHex(6)
	}
	for _, c := range t.ID {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') || len(t.ID) > 40 {
			return fmt.Errorf("scheduler: bad task id %q", t.ID)
		}
	}
	if t.Name == "" || len(t.Name) > 80 {
		return errors.New("scheduler: a task needs a name of up to 80 characters")
	}
	switch {
	case t.Action == "shell":
		t.Command = strings.TrimSpace(t.Command)
		if t.Command == "" {
			return fmt.Errorf("scheduler: %q: the shell command is empty", t.Name)
		}
		if len(t.Command) > maxSchedCommand {
			return fmt.Errorf("scheduler: %q: the command is longer than %d characters", t.Name, maxSchedCommand)
		}
		if t.TimeoutSec == 0 {
			t.TimeoutSec = 300
		}
		if t.TimeoutSec < 1 || t.TimeoutSec > 86400 {
			return fmt.Errorf("scheduler: %q: the time limit must be 1 to 86400 seconds", t.Name)
		}
	case schedActions[t.Action] != "":
		t.Command, t.TimeoutSec = "", 0
	default:
		return fmt.Errorf("scheduler: %q: unknown action %q", t.Name, t.Action)
	}
	if t.WindowSec < 0 || t.WindowSec > maxSchedWindow {
		return fmt.Errorf("scheduler: %q: the offset window must be 0 to %d seconds", t.Name, maxSchedWindow)
	}
	max, ok := schedEveryMax[t.Freq]
	if !ok {
		return fmt.Errorf("scheduler: %q: the frequency must be hourly, daily, weekly, monthly or yearly", t.Name)
	}
	if t.Every < 0 || t.Every > max {
		return fmt.Errorf("scheduler: %q: every N %s: N must be 1 to %d", t.Name, strings.TrimSuffix(t.Freq, "ly"), max)
	}
	if t.Every == 1 {
		t.Every = 0
	}
	if t.At == "" {
		t.At = "00:00:00"
	}
	h, m, s, err := parseSchedAt(t.At)
	if err != nil {
		return fmt.Errorf("scheduler: %q: %v", t.Name, err)
	}
	t.At = fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	if t.Until != "" {
		uh, um, _, err := parseSchedAt(t.Until)
		if err != nil {
			return fmt.Errorf("scheduler: %q: until: %v", t.Name, err)
		}
		t.Until = fmt.Sprintf("%02d:%02d", uh, um)
		w := (uh*3600 + um*60) - (h*3600 + m*60 + s)
		if w <= 0 {
			w += 24 * 3600
		}
		if t.Freq == "hourly" {
			return fmt.Errorf("scheduler: %q: an hourly task has no until time; use the offset window", t.Name)
		}
		if w > maxSchedWindow {
			return fmt.Errorf("scheduler: %q: the window is too long", t.Name)
		}
		t.WindowSec = w
	}
	t.TZ = ""
	// the fields of the other frequencies are dropped, so a copy of a task compares equal to the original
	days := t.Days
	t.Days = nil
	switch t.Freq {
	case "weekly":
		if len(days) == 0 {
			return fmt.Errorf("scheduler: %q: pick at least one day of the week", t.Name)
		}
		seen := map[int]bool{}
		for _, d := range days {
			if d < 0 || d > 6 {
				return fmt.Errorf("scheduler: %q: a day of the week is 0 (Sunday) to 6 (Saturday)", t.Name)
			}
			if !seen[d] {
				seen[d] = true
				t.Days = append(t.Days, d)
			}
		}
		sort.Ints(t.Days)
	}
	if t.Nth != 0 { // the older single form
		t.Nths, t.Weekdays = []int{t.Nth}, []int{t.Weekday}
	}
	t.Nth, t.Weekday = 0, 0
	nths, wds := t.Nths, t.Weekdays
	t.Nths, t.Weekdays = nil, nil
	if t.Freq != "monthly" && t.Freq != "yearly" {
		t.Day = 0
	} else if len(nths) > 0 {
		seen := map[int]bool{}
		for _, n := range nths {
			if !(n >= 1 && n <= 4 || n == -1) {
				return fmt.Errorf("scheduler: %q: an occurrence is the 1st to 4th or the last", t.Name)
			}
			if !seen[n] {
				seen[n] = true
				t.Nths = append(t.Nths, n)
			}
		}
		sort.Slice(t.Nths, func(i, j int) bool { return (t.Nths[i]+9)%10 < (t.Nths[j]+9)%10 }) // 1,2,3,4,last
		if len(wds) == 0 {
			return fmt.Errorf("scheduler: %q: pick at least one weekday", t.Name)
		}
		seenW := map[int]bool{}
		for _, d := range wds {
			if d < 0 || d > 6 {
				return fmt.Errorf("scheduler: %q: a weekday is 0 (Sunday) to 6 (Saturday)", t.Name)
			}
			if !seenW[d] {
				seenW[d] = true
				t.Weekdays = append(t.Weekdays, d)
			}
		}
		sort.Ints(t.Weekdays)
		t.Day = 0
	} else {
		if t.Day == 0 {
			t.Day = 1
		}
		if !(t.Day >= 1 && t.Day <= 31 || t.Day == -1) {
			return fmt.Errorf("scheduler: %q: the day of the month is 1 to 31, or -1 for the last", t.Name)
		}
	}
	if t.Freq == "yearly" {
		if t.Month < 1 || t.Month > 12 {
			return fmt.Errorf("scheduler: %q: a yearly task needs a month, 1 to 12", t.Name)
		}
	} else {
		t.Month = 0
	}
	ids, err := cleanNodeIDs(t.Nodes)
	if err != nil {
		return fmt.Errorf("scheduler: %q: %v", t.Name, err)
	}
	t.Nodes = ids
	return nil
}

// validateSchedule checks the whole list (names and ids must be unique).
func validateSchedule(l []SchedTask) error {
	if len(l) > maxSchedTasks {
		return fmt.Errorf("scheduler: at most %d tasks", maxSchedTasks)
	}
	ids, names := map[string]bool{}, map[string]bool{}
	for i := range l {
		if err := l[i].validate(); err != nil {
			return err
		}
		if ids[l[i].ID] {
			l[i].ID = randHex(6)
		}
		ids[l[i].ID] = true
		if names[strings.ToLower(l[i].Name)] {
			return fmt.Errorf("scheduler: two tasks are called %q", l[i].Name)
		}
		names[strings.ToLower(l[i].Name)] = true
	}
	return nil
}

func daysIn(y int, m time.Month) int { return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day() }

// dayIndex counts the calendar days since 1970-01-01 of the date in its own zone.
func dayIndex(t time.Time) int {
	y, m, d := t.Date()
	return int(time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400)
}

// dayMatches says whether the date is one of the days the task is on (not looking at Every or the time of day).
func (t *SchedTask) dayMatches(d time.Time) bool {
	y, m, dd := d.Date()
	dim := daysIn(y, m)
	byDay := func() bool {
		if len(t.Nths) > 0 {
			ok := false
			for _, w := range t.Weekdays {
				if int(d.Weekday()) == w {
					ok = true
				}
			}
			if !ok {
				return false
			}
			for _, n := range t.Nths {
				if n == -1 && dd+7 > dim || n > 0 && (dd-1)/7+1 == n {
					return true
				}
			}
			return false
		}
		if t.Day == -1 {
			return dd == dim
		}
		return dd == min(t.Day, dim) // the 31st on a 30-day month is the 30th
	}
	every := max(t.Every, 1)
	switch t.Freq {
	case "daily":
		return dayIndex(d)%every == 0
	case "weekly":
		if (dayIndex(d)+4)/7%every != 0 { // weeks run Sunday to Saturday
			return false
		}
		for _, w := range t.Days {
			if int(d.Weekday()) == w {
				return true
			}
		}
		return false
	case "monthly":
		return ((y-1970)*12+int(m)-1)%every == 0 && byDay()
	case "yearly":
		return (y-1970)%every == 0 && int(m) == t.Month && byDay()
	}
	return false
}

// bases lists the scheduled times (before any offset) in [from, to], oldest first.
func (t *SchedTask) bases(from, to time.Time) []time.Time {
	var out []time.Time
	if to.Before(from) {
		return nil
	}
	_, atm, ats, _ := parseSchedAt(t.At)
	if t.Freq == "hourly" {
		every := int64(max(t.Every, 1))
		for h := from.Unix()/3600 - 1; h <= to.Unix()/3600; h++ {
			if h%every != 0 {
				continue
			}
			c := time.Unix(h*3600+int64(atm*60+ats), 0)
			if !c.Before(from) && !c.After(to) {
				out = append(out, c)
			}
		}
		return out
	}
	hh, mm, ss, _ := parseSchedAt(t.At)
	loc := t.loc()
	d := from.In(loc).AddDate(0, 0, -1)
	end := to.In(loc).AddDate(0, 0, 1)
	for d.Before(end) {
		if t.dayMatches(d) {
			y, m, dd := d.Date()
			c := time.Date(y, m, dd, hh, mm, ss, 0, loc)
			if !c.Before(from) && !c.After(to) {
				out = append(out, c)
			}
		}
		d = d.AddDate(0, 0, 1)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

// next is the first scheduled time after t0 (zero when there is none in the next twelve years).
func (t *SchedTask) next(t0 time.Time) time.Time {
	for i := 0; i < 80; i++ {
		from := t0.Add(time.Duration(i) * 60 * 24 * time.Hour)
		for _, b := range t.bases(from, from.Add(60*24*time.Hour)) {
			if b.After(t0) {
				return b
			}
		}
	}
	return time.Time{}
}

// schedOffset is where in the window this node runs the occurrence: the same every time it is asked, and different
// from node to node and occurrence to occurrence.
func schedOffset(nodeID, taskID string, base time.Time, windowSec int) time.Duration {
	if windowSec <= 0 {
		return 0
	}
	h := sha256.Sum256([]byte(nodeID + "|" + taskID + "|" + strconv.FormatInt(base.Unix(), 10)))
	return time.Duration(binary.BigEndian.Uint64(h[:8])%uint64(windowSec+1)) * time.Second
}

// appliesTo says whether the node runs the task.
func (t *SchedTask) appliesTo(nodeID string) bool {
	return len(t.Nodes) == 0 || containsStr(t.Nodes, nodeID)
}

// describe is the schedule in words, for the list.
func (t *SchedTask) describe() string {
	every := func(unit string) string {
		if t.Every > 1 {
			return fmt.Sprintf("every %d %ss", t.Every, unit)
		}
		return "every " + unit
	}
	ord := map[int]string{1: "1st", 2: "2nd", 3: "3rd", 4: "4th", -1: "last"}
	wd := []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	mon := []string{"", "January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"}
	day := func() string {
		if len(t.Nths) > 0 {
			var o, w []string
			for _, n := range t.Nths {
				o = append(o, ord[n])
			}
			for _, d := range t.Weekdays {
				w = append(w, wd[d])
			}
			return "the " + joinOr(o) + " " + joinOr(w)
		}
		if t.Day == -1 {
			return "the last day"
		}
		return "day " + strconv.Itoa(t.Day)
	}
	_, m, s, _ := parseSchedAt(t.At)
	at := t.At
	if strings.HasSuffix(at, ":00") {
		at = at[:len(at)-3]
	}
	var out string
	switch t.Freq {
	case "hourly":
		out = fmt.Sprintf("%s at %d min %d s past the hour", every("hour"), m, s)
		if s == 0 {
			out = fmt.Sprintf("%s at minute %d", every("hour"), m)
		}
	case "daily":
		out = every("day") + " at " + at
	case "weekly":
		names := make([]string, len(t.Days))
		for i, d := range t.Days {
			names[i] = wd[d][:3]
		}
		out = every("week") + " on " + strings.Join(names, ", ") + " at " + at
	case "monthly":
		out = every("month") + ", " + day() + " at " + at
	case "yearly":
		out = every("year") + ", " + day() + " of " + mon[t.Month] + " at " + at
	}
	if t.Until != "" {
		out += " to " + t.Until
		out = strings.Replace(out, " at "+at+" to ", " between "+at+" and ", 1) + " UTC"
	} else {
		if t.Freq != "hourly" {
			out = strings.Replace(out, " at "+at, " at "+at+" UTC", 1)
		}
		if t.WindowSec > 0 {
			out += ", within " + fmtSchedWindow(t.WindowSec)
		}
	}
	return out
}

// joinOr lists "a", "a or b", "a, b or c".
func joinOr(l []string) string {
	if len(l) < 2 {
		return strings.Join(l, "")
	}
	return strings.Join(l[:len(l)-1], ", ") + " or " + l[len(l)-1]
}

func fmtSchedWindow(sec int) string {
	var p []string
	for _, u := range []struct {
		n int
		s string
	}{{86400, "d"}, {3600, "h"}, {60, "m"}, {1, "s"}} {
		if sec >= u.n {
			p = append(p, strconv.Itoa(sec/u.n)+u.s)
			sec %= u.n
		}
	}
	return strings.Join(p, " ")
}

// ── what ran ─────────────────────────────────────────────────────────────────

// SchedRun is one entry of this node's history.
type SchedRun struct {
	Task   string `json:"task"`
	Name   string `json:"name"`
	Action string `json:"action"`
	Base   int64  `json:"base"`  // the scheduled time it was for (unix)
	Start  int64  `json:"start"` // when it began (unix)
	End    int64  `json:"end,omitempty"`
	Status string `json:"status"` // running, ok, failed, skipped
	Output string `json:"output,omitempty"`
}

type schedState struct {
	Last    map[string]int64 `json:"last"` // task id -> the scheduled time (unix) last dealt with
	History []SchedRun       `json:"history"`
}

// Scheduler runs this node's part of the tasks.
type Scheduler struct {
	m       *Mgmt
	path    string
	mu      sync.Mutex
	tasks   []SchedTask
	st      schedState
	running map[string]bool
	now     func() time.Time // replaceable in tests
	// act runs a built-in action (replaceable in tests)
	act func(action, actor string) (string, error)
}

func newScheduler(m *Mgmt, stateDir string) *Scheduler {
	s := &Scheduler{m: m, path: filepath.Join(stateDir, "scheduler.json"), running: map[string]bool{}, now: time.Now}
	s.st.Last = map[string]int64{}
	if b, err := os.ReadFile(s.path); err == nil {
		var st schedState
		if json.Unmarshal(b, &st) == nil {
			s.st = st
			if s.st.Last == nil {
				s.st.Last = map[string]int64{}
			}
		}
	}
	for i := range s.st.History { // a run that was going when the daemon stopped did not finish
		if s.st.History[i].Status == "running" {
			s.st.History[i].Status, s.st.History[i].Output = "failed", "interrupted: the daemon stopped or restarted while it ran"
		}
	}
	s.act = s.builtin
	return s
}

// SetTasks is called with the task list every time a configuration has been loaded.
func (s *Scheduler) SetTasks(l []SchedTask) {
	s.mu.Lock()
	s.tasks = append([]SchedTask(nil), l...)
	s.mu.Unlock()
}

func (s *Scheduler) saveLocked() {
	b, _ := json.Marshal(s.st)
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		if err := os.Rename(tmp, s.path); err != nil {
			warnf("scheduler: saving the history: %v", err)
		}
	}
}

func (s *Scheduler) addLocked(r SchedRun) {
	s.st.History = append(s.st.History, r)
	if n := len(s.st.History); n > schedHistoryMax {
		s.st.History = append([]SchedRun(nil), s.st.History[n-schedHistoryMax:]...)
	}
}

// Run is the loop.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(schedTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Tick(localNodeID())
		}
	}
}

// Tick starts what is due for the node now.
func (s *Scheduler) Tick(nodeID string) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	known := map[string]bool{}
	dirty := false
	for _, t := range s.tasks {
		known[t.ID] = true
		if t.Disabled || !t.appliesTo(nodeID) {
			delete(s.st.Last, t.ID) // a task switched on again starts from then, not from what it missed
			continue
		}
		last, seen := s.st.Last[t.ID]
		if !seen { // new here: nothing before this moment is owed
			s.st.Last[t.ID] = now.Unix()
			dirty = true
			continue
		}
		from := time.Unix(last, 0).Add(time.Second)
		if lim := now.Add(-31 * 24 * time.Hour); from.Before(lim) {
			from = lim
		}
		window := time.Duration(t.WindowSec) * time.Second
		var due *time.Time
		skipped := 0
		var skipBase time.Time
		for _, b := range t.bases(from, now) {
			b := b
			runAt := b.Add(schedOffset(nodeID, t.ID, b, t.WindowSec))
			if now.Before(runAt) {
				continue // its time in the window has not come yet; Last stays behind it
			}
			if now.After(b.Add(window + schedGrace)) {
				skipped++
				skipBase = b
				s.st.Last[t.ID] = b.Unix()
				dirty = true
				continue
			}
			due = &b
		}
		if skipped > 0 {
			s.addLocked(SchedRun{Task: t.ID, Name: t.Name, Action: t.Action, Base: skipBase.Unix(), Start: now.Unix(), End: now.Unix(), Status: "skipped",
				Output: fmt.Sprintf("%d scheduled run(s) were missed (this node was down or busy) and were past their window when it came back", skipped)})
		}
		if due == nil {
			continue
		}
		s.st.Last[t.ID] = due.Unix()
		dirty = true
		if s.running[t.ID] {
			s.addLocked(SchedRun{Task: t.ID, Name: t.Name, Action: t.Action, Base: due.Unix(), Start: now.Unix(), End: now.Unix(), Status: "skipped", Output: "the previous run was still going"})
			continue
		}
		s.startLocked(t, due.Unix(), "schedule")
	}
	for id := range s.st.Last { // tasks that were deleted
		if !known[id] {
			delete(s.st.Last, id)
			dirty = true
		}
	}
	if dirty {
		s.saveLocked()
	}
}

// startLocked begins a run in the background (the caller holds s.mu).
func (s *Scheduler) startLocked(t SchedTask, base int64, actor string) {
	s.running[t.ID] = true
	r := SchedRun{Task: t.ID, Name: t.Name, Action: t.Action, Base: base, Start: s.now().Unix(), Status: "running"}
	s.addLocked(r)
	start := r.Start
	s.saveLocked()
	go func() {
		out, err := s.execute(t, actor)
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.running, t.ID)
		status := "ok"
		if err != nil {
			status = "failed"
			out = strings.TrimSpace(out + "\n" + err.Error())
		}
		// the entry may have moved if the history was trimmed meanwhile: find it again
		for i := len(s.st.History) - 1; i >= 0; i-- {
			if h := &s.st.History[i]; h.Task == t.ID && h.Start == start && h.Status == "running" {
				h.End, h.Status, h.Output = s.now().Unix(), status, tail(out, schedOutputMax)
				break
			}
		}
		s.saveLocked()
		if err != nil {
			warnf("scheduler: task %q failed: %v", t.Name, err)
		} else {
			infof("scheduler: task %q finished", t.Name)
		}
	}()
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// RunNow runs a task on this node at once, whatever its schedule says.
func (s *Scheduler) RunNow(id, actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tasks {
		if t.ID != id {
			continue
		}
		if s.running[id] {
			return errors.New("that task is running now")
		}
		warnf("scheduler: %q ran task %q now", actor, t.Name)
		s.startLocked(t, 0, actor)
		return nil
	}
	return errors.New("no such task")
}

// execute does the task's work.
func (s *Scheduler) execute(t SchedTask, actor string) (string, error) {
	infof("scheduler: running task %q on this node", t.Name)
	if t.Action != "shell" {
		return s.act(t.Action, "scheduler: "+t.Name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(t.TimeoutSec)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", t.Command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) } // the whole group, not only the shell
	cmd.WaitDelay = 5 * time.Second
	var buf bytes.Buffer
	lw := &limitWriter{w: &buf, n: 1 << 20}
	cmd.Stdout, cmd.Stderr = lw, lw // one writer for both: they share one pipe, and the output stays in order
	cmd.Env = append(os.Environ(), "DNSDIR_TASK="+t.Name)
	err := cmd.Run()
	lw.mu.Lock()
	out := buf.String()
	lw.mu.Unlock()
	if ctx.Err() != nil {
		return out, fmt.Errorf("stopped after %d s", t.TimeoutSec)
	}
	return out, err
}

type limitWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
	n  int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if room := l.n - l.w.Len(); room > 0 {
		if len(p) > room {
			l.w.Write(p[:room])
		} else {
			l.w.Write(p)
		}
	}
	return len(p), nil
}

// builtin runs one of the built-in actions on this node.
func (s *Scheduler) builtin(action, actor string) (string, error) {
	switch action {
	case "dnsdir_restart":
		if s.m.restartFn == nil {
			return "", errors.New("this process cannot restart itself")
		}
		go func() {
			time.Sleep(1500 * time.Millisecond) // let the history entry be written first
			waitUntilSafe(s.m.updateSafeToApply, false, restartRecheck, func(string) {})
			s.m.handOver("scheduler", "restarting for a scheduled task")
			s.m.restartFn()
		}()
		return "restarting once it is safe to go down", nil
	case "cache_clear":
		if s.m.clearFn == nil {
			return "", errors.New("the cache is not available on this node")
		}
		return fmt.Sprintf("%d answers dropped", s.m.clearFn()), nil
	case "node_pause", "node_resume":
		return s.m.NodePause(action == "node_pause", actor)
	case "update_online":
		return s.updateOnline(actor)
	case "node_reboot":
		return s.rebootHost(actor)
	case "stats_clear":
		qstats.Clear()
		if err := savePersisted(s.m.stateDir); err != nil {
			return "", err
		}
		return "statistics cleared", nil
	}
	return "", fmt.Errorf("unknown action %q", action)
}

// ── what the page and the CLI see ────────────────────────────────────────────

// SchedTaskView is a task as the page shows it for this node.
type SchedTaskView struct {
	Name     string    `json:"name"`
	Action   string    `json:"action"`
	Disabled bool      `json:"disabled"`
	ID       string    `json:"id"`
	Describe string    `json:"describe"`
	Applies  bool      `json:"applies"`
	Running  bool      `json:"running"`
	Next     int64     `json:"next,omitempty"`     // the next scheduled time (unix)
	NextRun  int64     `json:"next_run,omitempty"` // when this node will run it (the scheduled time plus this node's offset)
	Last     *SchedRun `json:"last,omitempty"`
	Task     SchedTask `json:"task"`
}

// SchedStatus is the answer of schedule.status: this node's view of the task list and its history (newest first).
type SchedStatus struct {
	NodeID  string            `json:"node_id"`
	Now     int64             `json:"now"`
	Tasks   []SchedTaskView   `json:"tasks"`
	History []SchedRun        `json:"history"`
	Actions map[string]string `json:"actions"`
}

func (s *Scheduler) Status() SchedStatus {
	node := localNodeID()
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := SchedStatus{NodeID: node, Now: now.Unix(), Tasks: []SchedTaskView{}, History: []SchedRun{}, Actions: schedActions}
	for _, t := range s.tasks {
		t := t
		v := SchedTaskView{ID: t.ID, Name: t.Name, Action: t.Action, Disabled: t.Disabled, Task: t, Describe: t.describe(), Applies: t.appliesTo(node), Running: s.running[t.ID]}
		if !t.Disabled {
			// the next run of this node: an occurrence whose own time in the window has not yet passed counts too
			from := now.Add(-time.Duration(t.WindowSec) * time.Second)
			for _, b := range t.bases(from, now) {
				if runAt := b.Add(schedOffset(node, t.ID, b, t.WindowSec)); runAt.After(now) && b.Unix() > s.st.Last[t.ID] {
					v.Next, v.NextRun = b.Unix(), runAt.Unix()
					break
				}
			}
			if v.Next == 0 {
				if b := t.next(now); !b.IsZero() {
					v.Next, v.NextRun = b.Unix(), b.Add(schedOffset(node, t.ID, b, t.WindowSec)).Unix()
				}
			}
		}
		for i := len(s.st.History) - 1; i >= 0; i-- {
			if s.st.History[i].Task == t.ID {
				r := s.st.History[i]
				v.Last = &r
				break
			}
		}
		out.Tasks = append(out.Tasks, v)
	}
	for i := len(s.st.History) - 1; i >= 0; i-- {
		out.History = append(out.History, s.st.History[i])
	}
	return out
}
