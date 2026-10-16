package main

import (
	"errors"
	"fmt"
	"sync"
)

var schedEditMu sync.Mutex

// SchedSet adds a task, or replaces the one with the same id (Operate ▸ Scheduler, --schedule-set).  The whole list goes
// through PutConfig, so a replica sends it to the primary like any other shared change.
func (m *Mgmt) SchedSet(t SchedTask, actor string) (SchedTask, error) {
	schedEditMu.Lock()
	defer schedEditMu.Unlock()
	dc, _, err := m.LiveConfig()
	if err != nil {
		return t, err
	}
	if err := t.validate(); err != nil {
		return t, err
	}
	replaced := false
	for i := range dc.DNS.Schedule {
		if dc.DNS.Schedule[i].ID == t.ID {
			dc.DNS.Schedule[i], replaced = t, true
		}
	}
	if !replaced {
		dc.DNS.Schedule = append(dc.DNS.Schedule, t)
	}
	verb := "added"
	if replaced {
		verb = "changed"
	}
	if err := m.PutConfig(dc, actor, fmt.Sprintf("Scheduled task %q %s", t.Name, verb)); err != nil {
		return t, err
	}
	return t, nil
}

// SchedDel removes a task.
func (m *Mgmt) SchedDel(id, actor string) error {
	schedEditMu.Lock()
	defer schedEditMu.Unlock()
	dc, _, err := m.LiveConfig()
	if err != nil {
		return err
	}
	var keep []SchedTask
	name := ""
	for _, t := range dc.DNS.Schedule {
		if t.ID == id {
			name = t.Name
			continue
		}
		keep = append(keep, t)
	}
	if name == "" {
		return errors.New("no such task")
	}
	dc.DNS.Schedule = keep
	return m.PutConfig(dc, actor, fmt.Sprintf("Scheduled task %q deleted", name))
}
