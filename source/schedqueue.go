package main

// One scheduled job at a time per node: a job that falls due while another is running waits its turn (first come, first
// served) and starts when the one before it has finished.  The queue is part of the saved state, so a restart keeps it.

type schedQueued struct {
	ID   string `json:"id"`
	Base int64  `json:"base"`
}

func (s *Scheduler) queuedLocked(id string) bool {
	for _, q := range s.st.Queue {
		if q.ID == id {
			return true
		}
	}
	return false
}

// drainLocked starts the first queued job that can still run, when nothing is running.  s.mu is held.
func (s *Scheduler) drainLocked() {
	node := s.node
	if node == "" {
		node = localNodeID()
	}
	for len(s.running) == 0 && len(s.st.Queue) > 0 {
		q := s.st.Queue[0]
		s.st.Queue = s.st.Queue[1:]
		for _, t := range s.tasks {
			if t.ID == q.ID && !t.Disabled && t.appliesTo(node) {
				s.startLocked(t, q.Base, "schedule")
				break
			}
		}
		s.saveLocked()
	}
}
