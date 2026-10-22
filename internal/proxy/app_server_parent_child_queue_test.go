package proxy

import (
	"bytes"
	"context"
	"testing"
)

// parentChildQueue drives one queue request through the production client path
// and returns its local result.
func parentChildQueue(t *testing.T, s *appSession, id, method, thread, text string) rpcMessage {
	t.Helper()
	params := map[string]any{"threadId": thread}
	if method == "thread/queue/add" {
		params["clientUserMessageId"] = id
		params["input"] = taskParams(thread, text)["input"]
	}
	if err := s.client(rpcValue(map[string]any{"id": id, "method": method, "params": params})); err != nil {
		t.Fatal(err)
	}
	response := controlResponse(t, s, id)
	if response["error"] != nil {
		t.Fatalf("%s: %s", method, rpcValue(response))
	}
	return rpcObject(response, "result")
}

// sharedParentChildWork returns a session where a native subagent shares the
// parent's appWork and the parent owns a live slot and turn.
func sharedParentChildWork(t *testing.T) (*appSession, *appWork, *bytes.Buffer) {
	t.Helper()
	s, upstream := newNotificationSession(t)
	ctx, cancel := context.WithCancel(context.Background())
	s.ctx, s.events = ctx, make(chan appServerEvent, 4)
	w := s.work("parent")
	w.lease = testLease(t)
	w.running["parent:turn"] = true
	s.threads["child"] = w
	s.threadCreated("parent", s.nextThreadEpoch())
	s.threadCreated("child", s.nextThreadEpoch())
	t.Cleanup(func() {
		cancel()
		s.workers.Wait()
		s.release(w)
	})
	return s, w, upstream
}

func TestParentChildQueueListsStaySeparate(t *testing.T) {
	s, _ := newNotificationSession(t)
	w := s.work("parent")
	w.lease = testLease(t)
	w.running["parent:turn"] = true
	s.threads["child"] = w
	s.threadCreated("parent", s.nextThreadEpoch())
	s.threadCreated("child", s.nextThreadEpoch())
	t.Cleanup(func() { s.release(w) })
	parentChildQueue(t, s, "parent-add", "thread/queue/add", "parent", "parent-only")
	parentChildQueue(t, s, "child-add", "thread/queue/add", "child", "child-only")
	for _, thread := range []string{"parent", "child"} {
		result := parentChildQueue(t, s, thread+"-list", "thread/queue/list", thread, "")
		want, other := []byte(thread+"-only"), []byte("parent-only")
		if thread == "parent" {
			other = []byte("child-only")
		}
		if !bytes.Contains(result["data"], want) || bytes.Contains(result["data"], other) {
			t.Errorf("%s queue contains another thread's input: %s", thread, result["data"])
		}
	}
}

func TestParentChildQueueMutationsAndDispatchTarget(t *testing.T) {
	s, w, upstream := sharedParentChildWork(t)
	parentChildQueue(t, s, "parent-add", "thread/queue/add", "parent", "parent-only")
	child := parentChildQueue(t, s, "child-add", "thread/queue/add", "child", "child-only")
	childID := rpcString(rpcObject(child, "queuedSubmission"), "id")
	// The parent's head does not move when the child reorders or deletes.
	parent := s.queueState("parent")
	childState := s.queueState("child")
	parentHead := parent.items[0].ID
	if err := s.client(rpcValue(map[string]any{"id": "child-reorder", "method": "thread/queue/reorder", "params": map[string]any{"threadId": "child", "queuedSubmissionIds": []string{childID}}})); err != nil {
		t.Fatal(err)
	}
	if response := controlResponse(t, s, "child-reorder"); response["error"] != nil {
		t.Fatalf("child reorder failed: %s", rpcValue(response))
	}
	if len(parent.items) != 1 || parent.items[0].ID != parentHead || parent.paused {
		t.Fatalf("child reorder changed the parent queue: %v", parent.items)
	}
	if err := s.client(rpcValue(map[string]any{"id": "child-delete", "method": "thread/queue/delete", "params": map[string]any{"threadId": "child", "queuedSubmissionId": childID}})); err != nil {
		t.Fatal(err)
	}
	if response := controlResponse(t, s, "child-delete"); response["error"] != nil {
		t.Fatalf("child delete failed: %s", rpcValue(response))
	}
	if len(parent.items) != 1 || parent.items[0].ID != parentHead {
		t.Fatalf("child delete changed the parent queue: %v", parent.items)
	}
	if len(childState.items) != 0 {
		t.Fatalf("child delete kept its own entry: %v", childState.items)
	}
	// Drain the parent's own entry so only the child's queue can schedule.
	if err := s.client(rpcValue(map[string]any{"id": "parent-delete", "method": "thread/queue/delete", "params": map[string]any{"threadId": "parent", "queuedSubmissionId": parentHead}})); err != nil {
		t.Fatal(err)
	}
	if response := controlResponse(t, s, "parent-delete"); response["error"] != nil {
		t.Fatalf("parent delete failed: %s", rpcValue(response))
	}
	if len(parent.items) != 0 {
		t.Fatalf("parent delete kept its own entry: %v", parent.items)
	}
	// A fresh child entry dispatches to the child even though the work's
	// thread is still the parent.
	parentChildQueue(t, s, "child-add-2", "thread/queue/add", "child", "child-only")
	delete(w.running, "parent:turn")
	if err := s.finish(w); err != nil {
		t.Fatal(err)
	}
	if w.pending == nil || w.pending.thread != "child" {
		t.Fatal("idle shared work did not schedule the child queue")
	}
	if err := s.dispatch(w, w.pending); err != nil {
		t.Fatal(err)
	}
	var dispatched rpcMessage
	for _, line := range bytes.Split(upstream.Bytes(), []byte{'\n'}) {
		message, err := decodeRPC(line)
		if err == nil && rpcString(message, "method") == "turn/start" {
			dispatched = message
		}
	}
	if actual := rpcString(rpcObject(dispatched, "params"), "threadId"); actual != "child" {
		t.Fatalf("child's queued input dispatched to %q", actual)
	}
	if !bytes.Contains(rpcObject(dispatched, "params")["input"], []byte("child-only")) {
		t.Fatalf("child's queued input content was not used: %s", rpcValue(dispatched))
	}
}

func TestParentChildCloseAndInterruptKeepOtherQueue(t *testing.T) {
	for _, control := range []string{"thread/closed", "turn/interrupt"} {
		t.Run(control, func(t *testing.T) {
			s, w, _ := sharedParentChildWork(t)
			parentChildQueue(t, s, "parent-add", "thread/queue/add", "parent", "parent-only")
			parentChildQueue(t, s, "child-add", "thread/queue/add", "child", "child-only")
			if control == "thread/closed" {
				reviewNative(t, s, closeMessage(control, "child"))
			} else if err := s.client(rpcValue(map[string]any{"id": "child-interrupt", "method": control, "params": map[string]any{"threadId": "child", "turnId": "child-turn"}})); err != nil {
				t.Fatal(err)
			}
			if parent := s.queueState("parent"); parent.paused {
				t.Fatal("child control paused the parent queue")
			}
			reviewNative(t, s, rpcMessage{"method": rpcValue("turn/completed"), "params": rpcValue(map[string]any{"threadId": "parent", "turn": map[string]any{"id": "turn", "status": "completed"}})})
			if w.pending == nil || w.pending.thread != "parent" {
				t.Fatal("child close/interrupt prevented the parent's queued input from scheduling")
			}
			if s.queueState("child").paused != true {
				t.Fatal("child's own queue was not paused by its control")
			}
		})
	}
}

// A shared execution chain still runs one admission at a time; the child's
// queued input waits for the parent turn instead of dispatching concurrently.
func TestSharedWorkSchedulesOneQueueAtATime(t *testing.T) {
	s, w, _ := sharedParentChildWork(t)
	parentChildQueue(t, s, "parent-add", "thread/queue/add", "parent", "parent-only")
	parentChildQueue(t, s, "child-add", "thread/queue/add", "child", "child-only")
	if w.pending != nil {
		t.Fatal("busy shared work auto-dispatched a queue")
	}
	if err := s.finish(w); err != nil {
		t.Fatal(err)
	}
	if w.pending != nil {
		t.Fatal("running shared turn released a queued admission")
	}
	delete(w.running, "parent:turn")
	if err := s.finish(w); err != nil {
		t.Fatal(err)
	}
	if w.pending == nil || w.pending.thread != "parent" {
		t.Fatalf("earliest queued thread not scheduled: %+v", w.pending)
	}
	child := s.queueState("child")
	parent := s.queueState("parent")
	if len(child.items) != 1 || len(parent.items) != 1 {
		t.Fatal("scheduling popped a queue before its ACK")
	}
	// Only one admission may exist; the child queue is not dispatched too.
	first := w.pending
	if err := s.finish(w); err != nil {
		t.Fatal(err)
	}
	if w.pending != first {
		t.Fatal("second queue was dispatched while the first admission was pending")
	}
}

func TestClosedChildQueueIsNotScheduledForSharedWork(t *testing.T) {
	s, w, _ := sharedParentChildWork(t)
	parentChildQueue(t, s, "child-add", "thread/queue/add", "child", "child-only")
	reviewNative(t, s, closeMessage("thread/closed", "child"))
	delete(w.running, "parent:turn")
	if err := s.finish(w); err != nil {
		t.Fatal(err)
	}
	if w.pending != nil {
		t.Fatalf("closed child queue was scheduled: %+v", w.pending)
	}
}

// Ending a child admission must not leave other threads' queues stranded: the
// shared work becomes idle and must reschedule an eligible parent queue.
func TestEndingChildAdmissionSchedulesParent(t *testing.T) {
	for _, reason := range []string{"interrupt prepared child", "slot timeout", "state preparation failure"} {
		t.Run(reason, func(t *testing.T) {
			s, _ := newNotificationSession(t)
			ctx, cancel := context.WithCancel(context.Background())
			s.ctx, s.events = ctx, make(chan appServerEvent, 4)
			w := s.work("parent")
			s.threads["child"] = w
			t.Cleanup(func() {
				cancel()
				s.workers.Wait()
				s.release(w)
			})
			job := &appAdmission{work: w, thread: "child", method: "turn/start", initial: true,
				message: rpcMessage{"id": rpcValue("child-admission"), "method": rpcValue("turn/start"), "params": rpcValue(taskParams("child", "child input"))}}
			s.beginAdmissionJob(t, w, job)
			if reason != "slot timeout" {
				w.lease = testLease(t)
			}
			parentChildQueue(t, s, "parent-add", "thread/queue/add", "parent", "parent-only")
			switch reason {
			case "interrupt prepared child":
				if err := s.client(rpcValue(map[string]any{"id": "interrupt", "method": "turn/interrupt", "params": map[string]any{"threadId": "child", "turnId": "not-started"}})); err != nil {
					t.Fatal(err)
				}
			case "state preparation failure":
				if err := s.stateReady(appServerEvent{source: "state_ready", job: job, err: context.Canceled}); err != nil {
					t.Fatal(err)
				}
			default:
				if err := s.admitted(appServerEvent{source: "admitted", job: job, err: ErrWaitTimeout}); err != nil {
					t.Fatal(err)
				}
			}
			if w.pending == nil || w.pending.thread != "parent" {
				t.Fatal("ending child admission left the eligible parent queue idle")
			}
			if !s.queueState("child").paused {
				t.Fatal("ended child admission did not pause its own queue")
			}
		})
	}
}

// A child sharing the parent work must not be able to update, delete, or
// reorder entries that belong to the parent's queue.
func TestChildQueueMutationsCannotTargetParent(t *testing.T) {
	for _, method := range []string{"thread/queue/update", "thread/queue/delete", "thread/queue/reorder"} {
		t.Run(method, func(t *testing.T) {
			s, _ := newNotificationSession(t)
			w := s.work("parent")
			w.lease = testLease(t)
			w.running["parent:turn"] = true
			s.threads["child"] = w
			t.Cleanup(func() { s.release(w) })
			parent := parentChildQueue(t, s, "parent-add", "thread/queue/add", "parent", "parent-only")
			child := parentChildQueue(t, s, "child-add", "thread/queue/add", "child", "child-only")
			parentID := rpcString(rpcObject(parent, "queuedSubmission"), "id")
			childID := rpcString(rpcObject(child, "queuedSubmission"), "id")
			params := map[string]any{"threadId": "child", "queuedSubmissionId": parentID, "input": taskParams("child", "evil-edit")["input"], "queuedSubmissionIds": []string{parentID, childID}}
			if err := s.client(rpcValue(map[string]any{"id": "mutation", "method": method, "params": params})); err != nil {
				t.Fatal(err)
			}
			response := controlResponse(t, s, "mutation")
			if method == "thread/queue/delete" {
				if response["error"] != nil || string(rpcObject(response, "result")["deleted"]) != "false" {
					t.Fatalf("child delete affected parent queue: %s", rpcValue(response))
				}
			} else if response["error"] == nil {
				t.Fatalf("cross-thread mutation accepted: %s", rpcValue(response))
			}
			result := parentChildQueue(t, s, "parent-list", "thread/queue/list", "parent", "")
			if !bytes.Contains(result["data"], []byte("parent-only")) || bytes.Contains(result["data"], []byte("evil-edit")) || bytes.Contains(result["data"], []byte("child-only")) {
				t.Fatalf("parent's input changed through child operation: %s", result["data"])
			}
		})
	}
}
