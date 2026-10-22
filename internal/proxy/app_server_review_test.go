package proxy

import (
	"bytes"
	"encoding/json"
	"testing"
)

// Dispatch through the production path, then drive the native wire separately
// so ACK/notification order is deterministic without scheduler delays.
func reviewDispatch(t *testing.T, s *appSession, work *appWork, thread, method string, params map[string]any) json.RawMessage {
	t.Helper()
	params["threadId"] = thread
	job := &appAdmission{work: work, thread: thread, method: method, lifeGen: s.taskLifeGeneration(thread), independent: params["delivery"] == "detached",
		message: rpcMessage{"id": rpcValue(method + ":" + thread), "method": rpcValue(method), "params": rpcValue(params)}}
	if err := s.dispatch(work, job); err != nil {
		t.Fatal(err)
	}
	for id, request := range s.requests {
		if request.work == work && request.method == method && request.thread == thread {
			return json.RawMessage(id)
		}
	}
	t.Fatal("dispatched request not recorded")
	return nil
}

func reviewNative(t *testing.T, s *appSession, message rpcMessage) {
	t.Helper()
	if err := s.server(rpcValue(message)); err != nil {
		t.Fatal(err)
	}
}

func TestReviewPredictionACKBeforeCompletion(t *testing.T) {
	s, _ := newNotificationSession(t)
	work := s.work("thread")
	work.lease = testLease(t)
	t.Cleanup(func() { s.release(work) })
	id := reviewDispatch(t, s, work, "thread", "thread/prediction/request", map[string]any{"sourceTurnId": "source"})
	reviewNative(t, s, rpcMessage{"id": id, "result": rpcValue(map[string]any{})})
	if work.awaiting != 1 || work.lease == nil {
		t.Fatal("prediction ACK ended unfinished prediction")
	}
	reviewNative(t, s, rpcMessage{"method": rpcValue("thread/prediction/updated"), "params": rpcValue(map[string]any{"threadId": "thread", "sourceTurnId": "source", "result": map[string]any{"type": "completed", "text": "done"}})})
	if work.awaiting != 0 || work.inflight != 0 || work.lease != nil {
		t.Fatalf("completed prediction leaked its reservation: awaiting=%d inflight=%d lease=%v", work.awaiting, work.inflight, work.lease)
	}
}

func TestReviewTurnNotificationDoesNotConsumePrediction(t *testing.T) {
	s, _ := newNotificationSession(t)
	work := s.work("thread")
	work.lease = testLease(t)
	t.Cleanup(func() { s.release(work) })
	work.running["thread:existing"] = true
	id := reviewDispatch(t, s, work, "thread", "thread/prediction/request", map[string]any{"sourceTurnId": "source"})
	reviewNative(t, s, rpcMessage{"id": id, "result": rpcValue(map[string]any{})})
	reviewNative(t, s, rpcMessage{"method": rpcValue("turn/started"), "params": rpcValue(map[string]any{"threadId": "thread", "turn": map[string]any{"id": "existing", "status": "inProgress"}})})
	if work.awaiting != 1 {
		t.Fatalf("turn notification consumed prediction wait: %d", work.awaiting)
	}
}

func TestReviewChildACKUsesChildThread(t *testing.T) {
	s, _ := newNotificationSession(t)
	work := s.work("parent")
	s.threads["child"] = work
	work.lease = testLease(t)
	t.Cleanup(func() { s.release(work) })
	work.running["parent:parent-turn"] = true
	id := reviewDispatch(t, s, work, "child", "turn/start", taskParams("child", "child input"))
	reviewNative(t, s, rpcMessage{"id": id, "result": rpcValue(map[string]any{"turn": map[string]any{"id": "child-turn", "status": "inProgress"}})})
	if !work.running["child:child-turn"] || work.running["parent:child-turn"] {
		t.Fatalf("child ACK recorded under parent: %v", work.running)
	}
}

func TestReviewLateResumeDoesNotReopenClosedThread(t *testing.T) {
	s, upstream := newNotificationSession(t)
	s.threadCreated("thread", s.nextThreadEpoch())
	if err := s.client(rpcValue(map[string]any{"id": "resume", "method": "thread/resume", "params": map[string]any{"threadId": "thread"}})); err != nil {
		t.Fatal(err)
	}
	var id json.RawMessage
	for key, request := range s.requests {
		if request.method == "thread/resume" {
			id = json.RawMessage(key)
		}
	}
	reviewNative(t, s, closeMessage("thread/closed", "thread"))
	upstream.Reset()
	reviewNative(t, s, rpcMessage{"id": id, "result": rpcValue(map[string]any{"thread": map[string]any{"id": "thread"}})})
	if !s.closedTasks["thread"] || !s.subs["thread"].closed || upstream.Len() != 0 {
		t.Fatalf("late resume revived closed thread: closed=%v subscribed=%+v upstream=%s", s.closedTasks["thread"], s.subs["thread"], upstream.Bytes())
	}
}

func TestReviewDetachedACKSurvivesSourceClose(t *testing.T) {
	s, _ := newNotificationSession(t)
	s.work("source")
	work := &appWork{thread: "source", running: make(map[string]bool), completed: make(map[string]bool), lease: testLease(t)}
	s.works[work] = true
	t.Cleanup(func() { s.release(work) })
	id := reviewDispatch(t, s, work, "source", "review/start", map[string]any{"delivery": "detached", "target": map[string]any{"type": "uncommittedChanges"}})
	reviewNative(t, s, closeMessage("thread/closed", "source"))
	reviewNative(t, s, rpcMessage{"id": id, "result": rpcValue(map[string]any{"reviewThreadId": "review-child", "turn": map[string]any{"id": "review-turn", "status": "inProgress"}})})
	if work.thread != "review-child" || !work.running["review-child:review-turn"] || work.inflight != 0 || work.lease == nil {
		t.Fatalf("source close stranded independent review: thread=%s running=%v inflight=%d", work.thread, work.running, work.inflight)
	}
	reviewNative(t, s, rpcMessage{"method": rpcValue("turn/completed"), "params": rpcValue(map[string]any{"threadId": "review-child", "turn": map[string]any{"id": "review-turn", "status": "completed"}})})
	if work.awaiting != 0 || work.lease != nil {
		t.Fatalf("review destination completion leaked source wait: awaiting=%d lease=%v", work.awaiting, work.lease)
	}
}

func TestReviewSourceCloseCancelsUnsubmittedDetachedReview(t *testing.T) {
	s, _ := newNotificationSession(t)
	s.work("source")
	work := &appWork{thread: "source", running: make(map[string]bool), completed: make(map[string]bool), lease: testLease(t)}
	s.works[work] = true
	t.Cleanup(func() { s.release(work) })
	canceled := false
	work.pending = &appAdmission{work: work, thread: "source", method: "review/start", independent: true, cancel: func() { canceled = true },
		message: rpcMessage{"id": rpcValue("unsubmitted-review"), "params": rpcValue(map[string]any{"threadId": "source"})}}
	reviewNative(t, s, closeMessage("thread/closed", "source"))
	if !canceled || work.pending != nil || work.lease != nil {
		t.Fatal("source close left an unsubmitted review waiting forever")
	}
}

func TestReviewClosedThreadRejectsNewTasks(t *testing.T) {
	s, upstream := newNotificationSession(t)
	reviewNative(t, s, closeMessage("thread/closed", "closed"))
	if err := s.client(rpcValue(map[string]any{"id": "after-close", "method": "turn/start", "params": taskParams("closed", "input")})); err != nil {
		t.Fatal(err)
	}
	if s.work("closed").pending != nil || upstream.Len() != 0 || !bytes.Contains(s.stdout.(*bytes.Buffer).Bytes(), []byte(`"code":-32800`)) {
		t.Fatal("closed thread accepted a task that could never be admitted")
	}
}

func TestReviewClosedCreationGetsReply(t *testing.T) {
	for _, stage := range []string{"settings", "goal"} {
		t.Run(stage, func(t *testing.T) {
			s, _ := newNotificationSession(t)
			if err := s.client(rpcValue(map[string]any{"id": "creation", "method": "thread/start", "params": map[string]any{}})); err != nil {
				t.Fatal(err)
			}
			findRequest := func(method string) json.RawMessage {
				for id, request := range s.requests {
					if request.method == method {
						return json.RawMessage(id)
					}
				}
				t.Fatalf("missing internal %s", method)
				return nil
			}
			reviewNative(t, s, rpcMessage{"id": findRequest("thread/start"), "result": rpcValue(map[string]any{"thread": map[string]any{"id": "created"}})})
			method := "thread/settings/update"
			if stage == "goal" {
				reviewNative(t, s, rpcMessage{"id": findRequest(method), "result": rpcValue(map[string]any{})})
				method = "thread/goal/get"
			}
			reviewNative(t, s, closeMessage("thread/closed", "created"))
			reviewNative(t, s, rpcMessage{"id": findRequest(method), "result": rpcValue(map[string]any{})})
			for _, line := range bytes.Split(s.stdout.(*bytes.Buffer).Bytes(), []byte{'\n'}) {
				message, err := decodeRPC(line)
				if err == nil && rpcString(message, "id") == "creation" && string(rpcObject(message, "error")["code"]) == "-32800" {
					return
				}
			}
			t.Fatal("thread closed during private initialization left client request unanswered")
		})
	}
}

func TestReviewNotLoadedEndsTaskLifecycle(t *testing.T) {
	s, _ := newNotificationSession(t)
	work := s.work("thread")
	work.lease = testLease(t)
	t.Cleanup(func() { s.release(work) })
	work.running["thread:running"] = true
	reviewDispatch(t, s, work, "thread", "thread/prediction/request", map[string]any{"sourceTurnId": "source"})
	reviewNative(t, s, rpcMessage{"method": rpcValue("thread/status/changed"), "params": rpcValue(map[string]any{"threadId": "thread", "status": map[string]any{"type": "notLoaded"}})})
	if !s.closedTasks["thread"] || work.awaiting != 0 || work.inflight != 0 || work.lease != nil || len(work.running) != 0 || !s.queueState("thread").paused {
		t.Fatalf("notLoaded kept task alive: closed=%v awaiting=%d inflight=%d lease=%v", s.closedTasks["thread"], work.awaiting, work.inflight, work.lease)
	}
}

func TestReviewDuplicateTurnDoesNotConsumeCompaction(t *testing.T) {
	s, _ := newNotificationSession(t)
	work := s.work("thread")
	work.lease = testLease(t)
	t.Cleanup(func() { s.release(work) })
	work.running["thread:existing"] = true
	id := reviewDispatch(t, s, work, "thread", "thread/compact/start", map[string]any{})
	reviewNative(t, s, rpcMessage{"id": id, "result": rpcValue(map[string]any{})})
	reviewNative(t, s, rpcMessage{"method": rpcValue("turn/started"), "params": rpcValue(map[string]any{"threadId": "thread", "turn": map[string]any{"id": "existing", "status": "inProgress"}})})
	if work.awaiting != 1 {
		t.Fatal("duplicate turn notification consumed compaction wait")
	}
	reviewNative(t, s, rpcMessage{"method": rpcValue("turn/started"), "params": rpcValue(map[string]any{"threadId": "thread", "turn": map[string]any{"id": "compaction", "status": "inProgress"}})})
	if work.awaiting != 0 {
		t.Fatal("compaction's own turn did not resolve its start wait")
	}
}

func TestReviewPredictionRequiresMatchingTerminalUpdate(t *testing.T) {
	s, _ := newNotificationSession(t)
	work := s.work("thread")
	work.lease = testLease(t)
	t.Cleanup(func() { s.release(work) })
	id := reviewDispatch(t, s, work, "thread", "thread/prediction/request", map[string]any{"sourceTurnId": "source"})
	reviewNative(t, s, rpcMessage{"id": id, "result": rpcValue(map[string]any{})})
	for _, params := range []map[string]any{
		{"threadId": "thread", "result": map[string]any{"type": "completed"}},
		{"threadId": "thread", "sourceTurnId": "other", "result": map[string]any{"type": "failed"}},
		{"threadId": "thread", "sourceTurnId": "source", "result": map[string]any{"type": "unknown"}},
	} {
		reviewNative(t, s, rpcMessage{"method": rpcValue("thread/prediction/updated"), "params": rpcValue(params)})
		if work.awaiting != 1 || work.lease == nil {
			t.Fatal("unrelated or malformed prediction update ended the task")
		}
	}
	reviewNative(t, s, rpcMessage{"method": rpcValue("thread/prediction/updated"), "params": rpcValue(map[string]any{"threadId": "thread", "sourceTurnId": "source", "result": map[string]any{"type": "failed"}})})
	if work.awaiting != 0 || work.lease != nil {
		t.Fatal("matching terminal prediction failed to release the task")
	}
}

func TestReviewChildPauseDoesNotCancelParentPending(t *testing.T) {
	s, _ := newNotificationSession(t)
	work := s.work("parent")
	s.threads["child"] = work
	work.lease = testLease(t)
	t.Cleanup(func() { s.release(work) })
	work.running["parent:turn"] = true
	canceled := false
	job := &appAdmission{work: work, thread: "parent", method: "thread/goal/set", cancel: func() { canceled = true },
		message: rpcMessage{"id": rpcValue("parent-goal"), "params": rpcValue(map[string]any{"threadId": "parent"})}}
	work.pending = job
	if err := s.client(rpcValue(map[string]any{"id": "child-pause", "method": "thread/goal/set", "params": map[string]any{"threadId": "child", "status": "paused"}})); err != nil {
		t.Fatal(err)
	}
	if canceled || work.pending != job {
		t.Fatal("child pause canceled the parent's waiting activation")
	}
}

func TestReviewCreationAfterPriorLifecycle(t *testing.T) {
	for _, method := range []string{"thread/resume", "thread/fork"} {
		t.Run(method, func(t *testing.T) {
			s, _ := newNotificationSession(t)
			s.work("source")
			s.threadCreated("source", s.nextThreadEpoch())
			target := "source"
			if method == "thread/resume" {
				reviewNative(t, s, closeMessage("thread/closed", "source"))
			} else {
				// The parent has already been closed and explicitly resumed.
				// Its new child has an independent generation starting at zero.
				s.nextTaskGeneration("source")
				target = "new-child"
			}
			if err := s.client(rpcValue(map[string]any{"id": "new-creation", "method": method, "params": map[string]any{"threadId": "source"}})); err != nil {
				t.Fatal(err)
			}
			findRequest := func(method string) json.RawMessage {
				for id, request := range s.requests {
					if request.method == method {
						return json.RawMessage(id)
					}
				}
				t.Fatalf("valid creation did not reach %s: %s", method, s.stdout.(*bytes.Buffer).Bytes())
				return nil
			}
			reviewNative(t, s, rpcMessage{"id": findRequest(method), "result": rpcValue(map[string]any{"thread": map[string]any{"id": target}})})
			reviewNative(t, s, rpcMessage{"id": findRequest("thread/settings/update"), "result": rpcValue(map[string]any{})})
			reviewNative(t, s, rpcMessage{"id": findRequest("thread/goal/get"), "result": rpcValue(map[string]any{"goal": nil})})
			if s.closedTasks[target] || !s.policyReady[target] || !s.subs[target].subscribed {
				t.Fatalf("valid creation not ready: target=%s state=%+v", target, s.subs[target])
			}
		})
	}
}

func TestAppServerRejectedPersistedGoalTurnReleasesSlot(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	client := startApp(t, home)
	source := client.thread()
	client.call("test/persistGoal", map[string]any{"threadId": source})
	fork := client.call("thread/fork", map[string]any{"threadId": source})
	thread := rpcString(rpcObject(fork, "thread"), "id")
	client.call("test/rejectTurns", map[string]any{})
	client.send("rejected-goal-turn", "turn/start", taskParams(thread, "rejected turn"))
	if response := client.response("rejected-goal-turn"); response["error"] == nil {
		t.Fatalf("native rejection missing: %v", response)
	}
	awaitSlots(t, home, 0)
	if code := client.stop(); code != 0 {
		t.Fatalf("exit=%d", code)
	}
}

func TestAppServerPersistedGoalTurnReusesPredictionLease(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	client := startApp(t, home)
	source := client.thread()
	seed := client.call("turn/start", taskParams(source, "seed history"))
	sourceTurn := rpcString(rpcObject(seed, "turn"), "id")
	client.finish(source)
	awaitSlots(t, home, 0)
	client.call("test/persistGoal", map[string]any{"threadId": source})
	fork := client.call("thread/fork", map[string]any{"threadId": source})
	thread := rpcString(rpcObject(fork, "thread"), "id")
	client.call("test/ackBeforePrediction", map[string]any{})
	client.call("thread/prediction/request", map[string]any{"threadId": thread, "sourceTurnId": sourceTurn})
	awaitSlots(t, home, 1)
	// Prediction owns the slot, but has not started the persisted goal. A
	// subsequent explicit turn must take ownership of its continuation chain.
	client.call("turn/start", taskParams(thread, "continue persisted goal"))
	client.call("test/flushPredictions", map[string]any{})
	client.finish(thread)
	awaitSlots(t, home, 1)
	client.call("thread/goal/set", map[string]any{"threadId": thread, "status": "paused"})
	client.finish(thread)
	awaitSlots(t, home, 0)
	if code := client.stop(); code != 0 {
		t.Fatalf("exit=%d", code)
	}
}

func TestAppServerDetachedSourceClosesBeforeACK(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 2, "5s")
	client := startApp(t, home)
	firstSource, secondSource := client.thread(), client.thread()
	client.call("test/delayReviews", map[string]any{})
	params := func(thread string) map[string]any {
		return map[string]any{"threadId": thread, "delivery": "detached", "target": map[string]any{"type": "uncommittedChanges"}}
	}
	client.send("first-review", "review/start", params(firstSource))
	client.await(func(message rpcMessage) bool {
		return rpcString(message, "method") == "thread/started" && rpcString(rpcObject(rpcObject(message, "params"), "thread"), "forkedFromId") == firstSource
	})
	client.call("test/notify", map[string]any{"method": "thread/closed", "params": map[string]any{"threadId": firstSource}})
	// The second request causes the fixture to flush both ACKs in reverse
	// order, after the first source has closed.
	client.send("second-review", "review/start", params(secondSource))
	first := rpcString(rpcObject(client.response("first-review"), "result"), "reviewThreadId")
	second := rpcString(rpcObject(client.response("second-review"), "result"), "reviewThreadId")
	awaitSlots(t, home, 2)
	client.finish(first)
	awaitSlots(t, home, 1)
	client.finish(second)
	awaitSlots(t, home, 0)
	if code := client.stop(); code != 0 {
		t.Fatalf("exit=%d", code)
	}
}
