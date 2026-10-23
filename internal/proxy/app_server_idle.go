package proxy

import "time"

// busy includes work that produces no traffic while waiting: admission, the
// shared submission interval, running turns/goals/processes, and outstanding RPCs. An
// open stdio pipe or a loaded thread alone does not keep a process alive.
func (s *appSession) busy() bool {
	if len(s.requests) != 0 || len(s.serverRequests) != 0 || len(s.processes) != 0 || s.reviewEventSize != 0 {
		return true
	}
	for work := range s.works {
		if work.pending != nil || work.lease != nil || len(work.running) != 0 ||
			work.inflight != 0 || work.awaiting != 0 || work.goal || work.realtime ||
			work.shell != 0 {
			return true
		}
	}
	// Queued input is keyed by thread, not by work. A paused queue still keeps
	// the connection alive because the client may resume it.
	for _, state := range s.queues {
		if len(state.items) != 0 {
			return true
		}
	}
	return false
}

type appServerIdleTimer struct {
	timeout time.Duration
	timer   *time.Timer
	busy    bool
}

func newAppServerIdleTimer(timeout time.Duration) *appServerIdleTimer {
	idle := &appServerIdleTimer{timeout: timeout}
	if timeout > 0 {
		idle.timer = time.NewTimer(timeout)
	}
	return idle
}

func (idle *appServerIdleTimer) channel() <-chan time.Time {
	if idle.timer == nil || idle.busy {
		return nil
	}
	return idle.timer.C
}

func (idle *appServerIdleTimer) stop() {
	if idle.timer != nil && !idle.timer.Stop() {
		select {
		case <-idle.timer.C:
		default:
		}
	}
}

func (idle *appServerIdleTimer) update(clientActivity, busy bool) {
	wasBusy := idle.busy
	idle.busy = busy
	if idle.timer == nil {
		return
	}
	if busy {
		idle.stop()
	} else if wasBusy || clientActivity {
		// Give a completed task a full idle interval, even if it has been
		// running silently for longer than the configured timeout.
		idle.stop()
		idle.timer.Reset(idle.timeout)
	}
}
