package proxy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func closeTestSession(t *testing.T) *appSession {
	t.Helper()
	s, _ := newNotificationSession(t)
	s.ctx = context.Background()
	return s
}

func testLease(t *testing.T) *Lease {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "lease")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	return &Lease{file: file}
}

// closeMessage drives the native close notification for a thread.
func closeMessage(method, thread string) rpcMessage {
	return rpcMessage{"method": rpcValue(method), "params": rpcValue(map[string]any{"threadId": thread})}
}

func TestServerCloseThreadCancelsSlotAdmission(t *testing.T) {
	for _, method := range []string{"thread/closed", "thread/deleted", "thread/archived"} {
		t.Run(method, func(t *testing.T) {
			s := closeTestSession(t)
			work := s.work("closed-thread")
			job := &appAdmission{work: work, method: "turn/start", thread: "closed-thread", initial: true, stage: "slot",
				message: rpcMessage{"id": rpcValue("waiting"), "method": rpcValue("turn/start"), "params": rpcValue(map[string]any{"threadId": "closed-thread"})}}
			s.beginAdmissionJob(t, work, job)
			if err := s.server(rpcValue(closeMessage(method, "closed-thread"))); err != nil {
				t.Fatal(err)
			}
			if work.pending != nil {
				t.Fatal("closed thread still has a pending admission")
			}
			s.admitted(appServerEvent{job: job, lease: testLease(t)})
			if work.lease != nil || work.inflight != 0 || work.awaiting != 0 {
				t.Fatalf("late admission revived closed work: lease=%v inflight=%d awaiting=%d", work.lease != nil, work.inflight, work.awaiting)
			}
			if !s.closedTasks["closed-thread"] {
				t.Fatal("closed thread not marked closed")
			}
			if s.policyReady["closed-thread"] {
				t.Fatal("policyReady survived thread close")
			}
			if state := s.goals["closed-thread"]; state != nil && state.known {
				t.Fatal("goal cache survived thread close")
			}
		})
	}
}

func TestServerCloseThreadCancelsStateAndPolicyWait(t *testing.T) {
	for _, stage := range []string{"state", "policy"} {
		t.Run(stage, func(t *testing.T) {
			s := closeTestSession(t)
			work := s.work("closed-thread")
			work.lease = testLease(t)
			s.policyReady["closed-thread"] = true
			job := &appAdmission{work: work, method: "turn/start", thread: "closed-thread", initial: false, stage: stage,
				message: rpcMessage{"id": rpcValue("waiting"), "method": rpcValue("turn/start"), "params": rpcValue(map[string]any{"threadId": "closed-thread"})}}
			s.beginAdmissionJob(t, work, job)
			if err := s.server(rpcValue(closeMessage("thread/closed", "closed-thread"))); err != nil {
				t.Fatal(err)
			}
			if work.pending != nil || work.lease != nil {
				t.Fatalf("close did not release %s work: pending=%v lease=%v", stage, work.pending != nil, work.lease != nil)
			}
		})
	}
}

// awaitCaptured waits until the native fixture captured at least count
// requests for the given method.
func awaitCaptured(t *testing.T, dir, method string, count int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(filepath.Join(dir, "rpc.jsonl"))
		if strings.Count(string(data), `"method":"`+method+`"`) >= count {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("native never captured %d %s requests", count, method)
}

// beginAdmissionJob installs a pre-built admission without spawning workers.
func (s *appSession) beginAdmissionJob(t *testing.T, work *appWork, job *appAdmission) {
	t.Helper()
	ctx, cancel := context.WithCancel(s.ctx)
	job.work, job.ctx, job.cancel = work, ctx, cancel
	job.lifeGen = s.taskLifeGeneration(job.thread)
	work.pending = job
}

func TestServerCloseChildKeepsSharedParentState(t *testing.T) {
	s := closeTestSession(t)
	parent := s.work("parent")
	parent.lease = testLease(t)
	parent.addInflight("parent", 1)
	parent.addAwaiting("parent", 1)
	parent.running["parent:turn-parent"] = true
	parent.setGoal("parent", true)
	parent.setRealtime("parent", true)
	// A native subagent shares the parent appWork.
	s.threads["child"] = parent
	s.threadCreated("parent", 1)
	s.threadCreated("child", 2)
	childJob := &appAdmission{work: parent, method: "turn/start", thread: "child", initial: false,
		message: rpcMessage{"id": rpcValue("child-waiting"), "method": rpcValue("turn/start"), "params": rpcValue(map[string]any{"threadId": "child"})}}
	s.beginAdmissionJob(t, parent, childJob)
	if err := s.server(rpcValue(closeMessage("thread/closed", "child"))); err != nil {
		t.Fatal(err)
	}
	if parent.pending != nil {
		t.Fatal("child admission survived close")
	}
	if parent.lease == nil {
		t.Fatal("closing child released the shared parent slot")
	}
	if parent.inflight != 1 || parent.awaiting != 1 {
		t.Fatalf("parent counters changed: inflight=%d awaiting=%d", parent.inflight, parent.awaiting)
	}
	if !parent.running["parent:turn-parent"] || len(parent.running) != 1 {
		t.Fatalf("parent turn removed: %v", parent.running)
	}
	if !parent.goalThreads["parent"] || !parent.realtimeThreads["parent"] {
		t.Fatalf("parent goal/realtime flags lost: %v %v", parent.goalThreads, parent.realtimeThreads)
	}
	if parent.goalThreads["child"] || parent.realtimeThreads["child"] {
		t.Fatal("closed child retained goal/realtime flags")
	}
	if s.subs["child"].subscribed || !s.subs["parent"].subscribed {
		t.Fatal("subscription state crossed threads")
	}
}

func TestServerLateTaskAckDoesNotReviveClosedThread(t *testing.T) {
	s := closeTestSession(t)
	work := s.work("closing")
	work.lease = testLease(t)
	work.addInflight("closing", 1)
	work.addAwaiting("closing", 1)
	request := &appRPCRequest{id: rpcValue("task"), method: "turn/start", work: work, thread: "closing", task: true, awaitStart: true,
		initial: true, lifeGen: s.taskLifeGeneration("closing")}
	s.send(rpcMessage{"id": rpcValue("task"), "method": rpcValue("turn/start"), "params": rpcValue(map[string]any{"threadId": "closing"})}, request)
	if err := s.server(rpcValue(closeMessage("thread/closed", "closing"))); err != nil {
		t.Fatal(err)
	}
	if err := s.response(request, rpcMessage{"id": request.id, "result": rpcValue(map[string]any{"turn": map[string]any{"id": "turn-late"}})}); err != nil {
		t.Fatal(err)
	}
	if len(work.running) != 0 || work.inflight < 0 || work.awaiting < 0 {
		t.Fatalf("late ack revived closed work: running=%v inflight=%d awaiting=%d", work.running, work.inflight, work.awaiting)
	}
	if work.lease != nil {
		t.Fatal("late ack retained a slot for closed work")
	}
}

func TestServerCloseIdempotentAndIgnoresUnrelated(t *testing.T) {
	s := closeTestSession(t)
	other := s.work("other")
	other.lease = testLease(t)
	other.addInflight("other", 1)
	work := s.work("closing")
	for i := 0; i < 2; i++ {
		if err := s.server(rpcValue(closeMessage("thread/closed", "closing"))); err != nil {
			t.Fatal(err)
		}
	}
	if other.lease == nil || other.inflight != 1 {
		t.Fatal("closing an unrelated thread released other work")
	}
	if work.inflight != 0 || work.awaiting != 0 || work.goal || work.realtime {
		t.Fatalf("counters went negative or stale: %+v", work)
	}
}

func TestServerReopenOnExplicitThreadCreation(t *testing.T) {
	s := closeTestSession(t)
	if err := s.server(rpcValue(closeMessage("thread/closed", "reopened"))); err != nil {
		t.Fatal(err)
	}
	if !s.closedTasks["reopened"] {
		t.Fatal("thread not closed")
	}
	s.reopenThread("reopened")
	if s.closedTasks["reopened"] {
		t.Fatal("explicit creation did not reopen the thread")
	}
	if s.taskLifeGeneration("reopened") == 0 {
		t.Fatal("generation did not advance on close")
	}
}

func TestUnsubscribeDoesNotCancelRunningWork(t *testing.T) {
	s := closeTestSession(t)
	work := s.work("running")
	work.lease = testLease(t)
	work.addInflight("running", 1)
	work.running["running:turn"] = true
	s.threadCreated("running", 1)
	if err := s.unsubscribeThread(rpcValue("unsub"), "running"); err != nil {
		t.Fatal(err)
	}
	if work.lease == nil || work.inflight != 1 || !work.running["running:turn"] {
		t.Fatal("logical unsubscribe canceled running work")
	}
	if s.closedTasks["running"] || s.taskLifeGeneration("running") != 0 {
		t.Fatal("unsubscribe advanced the task lifecycle generation")
	}
}

func TestPredictionWaitIsolation(t *testing.T) {
	s := closeTestSession(t)
	work := s.work("thread")
	prediction := &appRPCRequest{id: rpcValue("pred"), method: "thread/prediction/request", work: work, thread: "thread", awaitStart: true, prediction: true, sourceTurnID: "turn-a", sequence: 1}
	compact := &appRPCRequest{id: rpcValue("compact"), method: "thread/compact/start", work: work, thread: "thread", awaitStart: true, sequence: 2}
	s.requests["pred"] = prediction
	s.requests["compact"] = compact
	// A prediction update for a different source turn is ignored.
	work.registerWait(prediction)
	work.registerWait(compact)
	if work.completePredictionWait("thread", "turn-b") != nil {
		t.Fatal("unrelated prediction consumed a wait")
	}
	if work.completePredictionWait("thread", "turn-a") != prediction {
		t.Fatal("matching prediction wait was not completed")
	}
	if !prediction.started || compact.started || work.awaiting != 1 {
		t.Fatalf("prediction isolation failed: pred=%v compact=%v awaiting=%d", prediction.started, compact.started, work.awaiting)
	}
	// Duplicate updates are idempotent.
	if work.completePredictionWait("thread", "turn-a") != nil || work.awaiting != 1 {
		t.Fatalf("duplicate prediction update consumed another wait: %d", work.awaiting)
	}
	// turn started completes only the compact wait, not a prediction.
	if work.completeTurnWait("thread") != compact || work.awaiting != 0 {
		t.Fatalf("turn started did not complete compact exactly once: awaiting=%d", work.awaiting)
	}
}

func TestQueueFailurePausesAndKeepsInput(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	client := startApp(t, home)
	thread := client.thread()
	client.call("turn/start", taskParams(thread, "active"))
	awaitSlots(t, home, 1)
	queued := client.call("thread/queue/add", map[string]any{"threadId": thread, "clientUserMessageId": "queued-1", "input": []any{map[string]any{"type": "text", "text": "queued"}}})
	queueID := rpcString(rpcObject(queued, "queuedSubmission"), "id")
	client.call("test/rejectTurns", map[string]any{})
	client.finish(thread)
	// The automatic submission is rejected. The client sees a correlated
	// error, the queue stays paused, and the input remains listed.
	failure := client.await(func(m rpcMessage) bool {
		return rpcString(m, "method") == "error" && rpcString(rpcObject(m, "params"), "queuedSubmissionId") == queueID
	})
	params := rpcObject(failure, "params")
	if rpcString(params, "threadId") != thread {
		t.Fatalf("error not correlated to queued input: %v", params)
	}
	list := client.call("thread/queue/list", map[string]any{"threadId": thread})
	if !strings.Contains(string(list["data"]), queueID) {
		t.Fatalf("failed queued input was dropped: %v", list)
	}
	awaitSlots(t, home, 0)
	if code := client.stop(); code != 0 {
		t.Fatalf("exit=%d", code)
	}
}

func TestExplicitQueueStartErrorUsesClientID(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	client := startApp(t, home)
	thread := client.thread()
	// Reject before enqueue so the automatic submission deterministically
	// fails first; the explicit retry must then report the native rejection
	// under the client's own id rather than a busy error.
	client.call("test/rejectTurns", map[string]any{})
	queued := client.call("thread/queue/add", map[string]any{"threadId": thread, "clientUserMessageId": "explicit", "input": []any{map[string]any{"type": "text", "text": "queued"}}})
	queueID := rpcString(rpcObject(queued, "queuedSubmission"), "id")
	failure := client.await(func(m rpcMessage) bool {
		return rpcString(m, "method") == "error" && rpcString(rpcObject(m, "params"), "queuedSubmissionId") == queueID
	})
	if failure == nil {
		t.Fatal("automatic queue failure notification missing")
	}
	awaitSlots(t, home, 0)
	client.send("explicit-start", "thread/queue/start", map[string]any{"threadId": thread, "queuedSubmissionId": queueID})
	response := client.response("explicit-start")
	if response["error"] == nil {
		t.Fatalf("explicit queue start was not rejected: %v", response)
	}
	if string(rpcObject(response, "error")["code"]) != "-32602" {
		t.Fatalf("explicit queue start did not surface native rejection: %v", response)
	}
	awaitSlots(t, home, 0)
	client.call("test/acceptTurns", map[string]any{})
	client.call("thread/queue/start", map[string]any{"threadId": thread, "queuedSubmissionId": queueID})
	awaitSlots(t, home, 1)
	client.finish(thread)
	awaitSlots(t, home, 0)
	if code := client.stop(); code != 0 {
		t.Fatalf("exit=%d", code)
	}
}

func TestPredictionRequestSchemaAndWaiters(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	client := startApp(t, home)
	thread := client.thread()
	source := client.call("turn/start", taskParams(thread, "source"))
	sourceTurn := rpcString(rpcObject(source, "turn"), "id")
	client.finish(thread)
	awaitSlots(t, home, 0)
	// ACK before notification: the fixture returns only the ACK, then emits
	// the update on demand, so the ordering is real rather than client-side.
	client.call("test/ackBeforePrediction", map[string]any{})
	client.send("ack-first", "thread/prediction/request", map[string]any{"threadId": thread, "sourceTurnId": sourceTurn})
	client.response("ack-first")
	awaitSlots(t, home, 1)
	client.call("test/flushPredictions", map[string]any{})
	client.await(func(m rpcMessage) bool {
		return rpcString(m, "method") == "thread/prediction/updated" && rpcString(rpcObject(rpcObject(m, "params"), "result"), "type") == "completed"
	})
	awaitSlots(t, home, 0)
	// Notification before ACK, and failure result, both release exactly once.
	client.call("test/ackBeforePredictionOff", map[string]any{})
	client.call("test/delayPredictions", map[string]any{})
	client.call("test/failPredictions", map[string]any{})
	client.send("late-ack", "thread/prediction/request", map[string]any{"threadId": thread, "sourceTurnId": sourceTurn})
	awaitCaptured(t, client.dir, "thread/prediction/request", 2)
	client.call("test/flushPredictions", map[string]any{})
	failed := client.await(func(m rpcMessage) bool {
		return rpcString(m, "method") == "thread/prediction/updated" && rpcString(rpcObject(rpcObject(m, "params"), "result"), "type") == "failed"
	})
	if failed == nil {
		t.Fatal("failed prediction notification missing")
	}
	client.response("late-ack")
	awaitSlots(t, home, 0)
	client.call("test/acceptTurns", map[string]any{})
	if code := client.stop(); code != 0 {
		t.Fatalf("exit=%d", code)
	}
}

func TestForkInitializationGoalFailureIsBestEffort(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	client := startApp(t, home)
	source := client.thread()
	// Ephemeral forks reject goal/get; their creation must still succeed and
	// disable goals without a model request.
	fork := client.call("thread/fork", map[string]any{"threadId": source, "ephemeral": true, "excludeTurns": true, "experimentalPredictionMode": true})
	child := rpcString(rpcObject(fork, "thread"), "id")
	awaitSlots(t, home, 0)
	client.call("turn/start", taskParams(child, "child task"))
	awaitSlots(t, home, 1)
	client.finish(child)
	awaitSlots(t, home, 0)
	if code := client.stop(); code != 0 {
		t.Fatalf("exit=%d", code)
	}
	capture, err := os.ReadFile(filepath.Join(client.dir, "rpc.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(capture), `"method":"thread/goal/get"`) < 1 {
		t.Fatal("goal initialization was not attempted")
	}
}
