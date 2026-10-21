package proxy

import "strings"

// Task lifecycle generations are independent from client subscription epochs.
// A logically unsubscribed thread keeps its work; a closed/deleted/archived
// thread advances this generation so late admissions, private settings replies,
// goal queries, and task acknowledgements cannot revive it.
func (s *appSession) taskLifeGeneration(thread string) uint64 {
	return s.taskGen[thread]
}

func (s *appSession) taskGenerationCurrent(thread string, gen uint64) bool {
	if thread == "" {
		return true
	}
	return s.taskGen[thread] == gen && !s.closedTasks[thread]
}

func (s *appSession) nextTaskGeneration(thread string) uint64 {
	s.taskGen[thread]++
	return s.taskGen[thread]
}

func (w *appWork) addInflight(thread string, delta int) {
	if w.inflightByThread == nil {
		w.inflightByThread = make(map[string]int)
	}
	next := w.inflightByThread[thread] + delta
	if next <= 0 {
		delete(w.inflightByThread, thread)
	} else {
		w.inflightByThread[thread] = next
	}
	// Derive the aggregate from the per-thread record; never clamp a
	// mis-accounted count to zero.
	w.inflight = 0
	for _, count := range w.inflightByThread {
		w.inflight += count
	}
}

func (w *appWork) addAwaiting(thread string, delta int) {
	if w.awaitingByThread == nil {
		w.awaitingByThread = make(map[string]int)
	}
	next := w.awaitingByThread[thread] + delta
	if next <= 0 {
		delete(w.awaitingByThread, thread)
	} else {
		w.awaitingByThread[thread] = next
	}
	w.awaiting = 0
	for _, count := range w.awaitingByThread {
		w.awaiting += count
	}
}

func (w *appWork) threadAwaiting(thread string) int {
	return w.awaitingByThread[thread]
}

func (w *appWork) setGoal(thread string, active bool) {
	if w.goalThreads == nil {
		w.goalThreads = make(map[string]bool)
	}
	if active {
		w.goalThreads[thread] = true
	} else {
		delete(w.goalThreads, thread)
	}
	w.goal = len(w.goalThreads) > 0
}

func (w *appWork) setRealtime(thread string, active bool) {
	if w.realtimeThreads == nil {
		w.realtimeThreads = make(map[string]bool)
	}
	if active {
		w.realtimeThreads[thread] = true
	} else {
		delete(w.realtimeThreads, thread)
	}
	w.realtime = len(w.realtimeThreads) > 0
}

// registerWait records an operation wait independently from the RPC pending
// map: an ACK may end inflight while turn/started or a prediction result still
// owns the wait.
func (w *appWork) registerWait(request *appRPCRequest) {
	if w.waits == nil {
		w.waits = make(map[*appRPCRequest]bool)
	}
	if w.waits[request] {
		return
	}
	w.waits[request] = true
	w.addAwaiting(request.thread, 1)
}

// completeWait removes a wait at most once. It is the single place that
// decrements the per-thread awaiting count.
func (w *appWork) completeWait(request *appRPCRequest) bool {
	if !w.waits[request] {
		return false
	}
	delete(w.waits, request)
	request.started = true
	w.addAwaiting(request.thread, -1)
	return true
}

// completeTurnWait completes the oldest non-prediction wait for the thread
// that emitted turn/started. Prediction waits are completed only by their own
// result notification.
func (w *appWork) completeTurnWait(thread string) *appRPCRequest {
	var match *appRPCRequest
	for request := range w.waits {
		if request.prediction || request.thread != thread {
			continue
		}
		switch request.method {
		case "turn/start", "review/start", "thread/compact/start", "thread/approveGuardianDeniedAction", "thread/realtime/start":
		default:
			continue
		}
		if match == nil || request.sequence < match.sequence {
			match = request
		}
	}
	if match != nil && w.completeWait(match) {
		return match
	}
	return nil
}

// completePredictionWait completes the prediction wait matching thread and
// source turn exactly once.
func (w *appWork) completePredictionWait(thread, sourceTurnID string) *appRPCRequest {
	var match *appRPCRequest
	for request := range w.waits {
		if !request.prediction || request.thread != thread {
			continue
		}
		if sourceTurnID == "" || request.sourceTurnID != sourceTurnID {
			continue
		}
		if match == nil || request.sequence < match.sequence {
			match = request
		}
	}
	if match != nil && w.completeWait(match) {
		return match
	}
	return nil
}

func (w *appWork) hasRunning(thread string) bool {
	for key := range w.running {
		if len(key) > len(thread) && key[:len(thread)+1] == thread+":" {
			return true
		}
	}
	return false
}

// closeThread invalidates pending admission, per-thread policy/goal caches, and
// in-flight task requests for one thread id. Shared child work remains intact
// for other thread ids and keeps its slot. Closing advances both the client
// subscription epoch (so a late resume reply cannot resubscribe) and the
// independent task lifecycle generation.
func (s *appSession) closeThread(work *appWork, thread string) error {
	if s.closedTasks[thread] {
		return nil
	}
	s.nextTaskGeneration(thread)
	s.closedTasks[thread] = true
	delete(s.policyReady, thread)
	s.invalidateGoal(thread)
	s.threadClosed(thread)
	if work == nil {
		return nil
	}
	work.queuePaused = true
	for key := range work.running {
		if strings.HasPrefix(key, thread+":") {
			s.logTurnFinished(work, key, "canceled", "thread_closed")
			delete(work.running, key)
		}
	}
	// Child and parent can share one appWork, so drop only this thread's
	// bookkeeping. Other threads keep their reservations and their turns.
	work.addInflight(thread, -work.inflightByThread[thread])
	work.addAwaiting(thread, -work.awaitingByThread[thread])
	work.setGoal(thread, false)
	work.setRealtime(thread, false)
	for _, request := range s.requests {
		if request.work != work || request.thread != thread || request.independent {
			continue
		}
		// Mark the acknowledgement stale; response() forwards it but skips
		// state mutation so a late success cannot revive counters or turns.
		request.lifeGen = ^uint64(0)
	}
	for request := range work.waits {
		if request.thread == thread && !request.independent {
			work.completeWait(request)
		}
	}
	if job := work.pending; job != nil && job.thread == thread {
		if err := s.cancelThreadAdmission(work, job); err != nil {
			return err
		}
	}
	// A detached review has a separate work reservation, but before dispatch
	// it still needs its source for preparation. Cancel those pending jobs;
	// already dispatched independent reviews retain their own lifetime.
	for candidate := range s.works {
		if candidate != work && candidate.pending != nil && candidate.pending.thread == thread {
			if err := s.cancelThreadAdmission(candidate, candidate.pending); err != nil {
				return err
			}
		}
	}
	return nil
}

// invalidateGoal bumps the goal sequence and marks the cached goal unknown.
// Deleting the entry would let an old epoch=0 query match again.
func (s *appSession) invalidateGoal(thread string) {
	state := s.goalState(thread)
	state.known = false
	state.status, state.objective = "", ""
	state.seq = s.nextGoalSeq()
	state.observedSeq = state.seq
}

// cancelThreadAdmission cancels a pending admission that belongs to a closed
// thread and releases any reservation it acquired.
func (s *appSession) cancelThreadAdmission(work *appWork, job *appAdmission) error {
	work.pending = nil
	job.cancel()
	work.queuePaused = true
	fields := job.logFields()
	fields["reason"] = "thread_closed"
	s.logger.Log("canceled", fields)
	if err := s.admissionError(job, -32800, "thread closed before execution"); err != nil {
		return err
	}
	return s.finish(work)
}
