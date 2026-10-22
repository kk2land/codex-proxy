package proxy

import (
	"bytes"
	"testing"
)

// A closed thread can still emit late lifecycle notifications after its slot
// was released. They are forwarded like any other notification, but must not
// recreate running turns, waits, or goal state.
func TestLateTurnStartedAfterCloseIsIgnoredWithoutRevival(t *testing.T) {
	s, _ := newNotificationSession(t)
	work := s.work("closed")
	reviewNative(t, s, closeMessage("thread/closed", "closed"))
	if err := s.server(rpcValue(map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "closed", "turn": map[string]any{"id": "late-turn", "status": "inProgress"}}})); err != nil {
		t.Fatalf("late turn/started terminated the connection: %v", err)
	}
	if len(work.running) != 0 || work.inflight != 0 || work.awaiting != 0 || work.lease != nil {
		t.Fatalf("late turn/started revived closed work: running=%v inflight=%d awaiting=%d lease=%v", work.running, work.inflight, work.awaiting, work.lease != nil)
	}
	// The closed thread is unsubscribed, so existing notification filtering
	// still hides its turn events from the client.
	if bytes.Contains(s.stdout.(*bytes.Buffer).Bytes(), []byte("late-turn")) {
		t.Fatal("late turn event bypassed the closed thread's notification filter")
	}
}

// A closed child shares the parent's appWork; its late started/completed
// notifications must not add turns to or remove turns from the parent chain.
func TestLateChildNotificationsDoNotReviveSharedWork(t *testing.T) {
	s, _ := newNotificationSession(t)
	parent := s.work("parent")
	parent.lease = testLease(t)
	parent.running["parent:turn"] = true
	s.threads["child"] = parent
	t.Cleanup(func() { s.release(parent) })
	reviewNative(t, s, closeMessage("thread/closed", "child"))
	reviewNative(t, s, rpcMessage{"method": rpcValue("turn/started"), "params": rpcValue(map[string]any{"threadId": "child", "turn": map[string]any{"id": "late-turn", "status": "inProgress"}})})
	reviewNative(t, s, rpcMessage{"method": rpcValue("turn/completed"), "params": rpcValue(map[string]any{"threadId": "child", "turn": map[string]any{"id": "late-turn", "status": "completed"}})})
	if _, exists := parent.running["child:late-turn"]; exists {
		t.Fatalf("late child turn started on shared work: %v", parent.running)
	}
	if parent.completed["child:late-turn"] {
		t.Fatalf("late child turn completed on shared work: %v", parent.completed)
	}
	if !parent.running["parent:turn"] || parent.lease == nil {
		t.Fatal("parent's own turn or slot changed")
	}
}

// Closing a shared child must not leave the parent's slot pinned by a late
// child goal notification, and that notification must not repopulate the
// closed thread's goal cache.
func TestLateClosedChildGoalDoesNotRetainParentSlot(t *testing.T) {
	s, _ := newNotificationSession(t)
	parent := s.work("parent")
	parent.lease = testLease(t)
	parent.running["parent:turn"] = true
	s.threads["child"] = parent
	t.Cleanup(func() { s.release(parent) })
	reviewNative(t, s, closeMessage("thread/closed", "child"))
	reviewNative(t, s, rpcMessage{"method": rpcValue("thread/goal/updated"), "params": rpcValue(map[string]any{"threadId": "child", "goal": map[string]any{"status": "active", "objective": "stale"}})})
	reviewNative(t, s, rpcMessage{"method": rpcValue("thread/goal/cleared"), "params": rpcValue(map[string]any{"threadId": "child"})})
	if parent.goalThreads["child"] || parent.goal {
		t.Fatalf("late goal reactivated closed child on shared work: %v", parent.goalThreads)
	}
	if state := s.goals["child"]; state != nil && state.known {
		t.Fatalf("late goal repopulated closed cache: %+v", state)
	}
	// Broadcast lifecycle notifications are still forwarded under the existing
	// filter; only the state mutation is skipped.
	if !bytes.Contains(s.stdout.(*bytes.Buffer).Bytes(), []byte("thread/goal/updated")) {
		t.Fatal("late goal notification was not forwarded")
	}
	reviewNative(t, s, rpcMessage{"method": rpcValue("turn/completed"), "params": rpcValue(map[string]any{"threadId": "parent", "turn": map[string]any{"id": "turn", "status": "completed"}})})
	if parent.lease != nil {
		t.Fatal("parent's completed task retained its slot because of stale child goal")
	}
}

// A late thread/started for an already closed id must not bind it to a live
// parent's shared appWork.
func TestLateThreadStartedDoesNotRebindClosedWork(t *testing.T) {
	s, _ := newNotificationSession(t)
	parent := s.work("parent")
	parent.lease = testLease(t)
	t.Cleanup(func() { s.release(parent) })
	reviewNative(t, s, closeMessage("thread/closed", "child"))
	own := s.threads["child"]
	reviewNative(t, s, rpcMessage{"method": rpcValue("thread/started"), "params": rpcValue(map[string]any{"thread": map[string]any{"id": "child", "parentThreadId": "parent"}})})
	if s.threads["child"] == parent {
		t.Fatal("late thread/started rebound closed child to parent work")
	}
	if own == nil || s.threads["child"] != own {
		t.Fatal("late thread/started replaced the closed thread's own work")
	}
}

// An explicit reopen clears the close marker, so the same lifecycle events
// must mutate state normally again.
func TestLifecycleNotificationsResumeAfterReopen(t *testing.T) {
	s, _ := newNotificationSession(t)
	work := s.work("thread")
	reviewNative(t, s, closeMessage("thread/closed", "thread"))
	reviewNative(t, s, rpcMessage{"method": rpcValue("thread/goal/updated"), "params": rpcValue(map[string]any{"threadId": "thread", "goal": map[string]any{"status": "active", "objective": "ignored"}})})
	if state := s.goals["thread"]; state != nil && state.known {
		t.Fatal("goal cache updated while closed")
	}
	s.reopenThread("thread")
	reviewNative(t, s, rpcMessage{"method": rpcValue("thread/goal/updated"), "params": rpcValue(map[string]any{"threadId": "thread", "goal": map[string]any{"status": "active", "objective": "reopened"}})})
	if state := s.goals["thread"]; state == nil || !state.known || state.objective != "reopened" {
		t.Fatalf("reopened thread ignored goal notification: %+v", state)
	}
	work.lease = testLease(t)
	t.Cleanup(func() { s.release(work) })
	reviewNative(t, s, rpcMessage{"method": rpcValue("turn/started"), "params": rpcValue(map[string]any{"threadId": "thread", "turn": map[string]any{"id": "reopened-turn", "status": "inProgress"}})})
	if !work.running["thread:reopened-turn"] {
		t.Fatalf("reopened thread ignored turn/started: %v", work.running)
	}
	reviewNative(t, s, rpcMessage{"method": rpcValue("turn/completed"), "params": rpcValue(map[string]any{"threadId": "thread", "turn": map[string]any{"id": "reopened-turn", "status": "completed"}})})
	if work.running["thread:reopened-turn"] || work.lease != nil {
		t.Fatalf("reopened thread did not apply turn/completed: running=%v lease=%v", work.running, work.lease != nil)
	}
}

// The guard applies only to closed threads: a live thread's unadmitted
// autonomous turn must still terminate the connection.
func TestUnadmittedTurnOnLiveThreadStillRejected(t *testing.T) {
	s, _ := newNotificationSession(t)
	err := s.server(rpcValue(map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "live", "turn": map[string]any{"id": "unadmitted", "status": "inProgress"}}}))
	if err == nil {
		t.Fatal("live thread's unadmitted autonomous turn was accepted")
	}
}
