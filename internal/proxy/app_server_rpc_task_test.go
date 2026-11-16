package proxy

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func newRPCTaskSession(t *testing.T) (*appSession, *bytes.Buffer, chan appServerEvent) {
	t.Helper()
	s, upstream := newNotificationSession(t)
	events := make(chan appServerEvent, 8)
	s.events = events
	s.load = func() (Config, error) {
		return Config{MaxConcurrency: 1, WaitTimeout: 5 * time.Second}, nil
	}
	s.stateStore = newStateStore(s.dir, Config{}, time.Millisecond, func() int64 { return 0 })
	return s, upstream, events
}

func holdRPCTaskSlot(t *testing.T, s *appSession) {
	t.Helper()
	lease, _, err := testLimiter(s.dir, 1, 5*time.Second).Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lease.Close() })
}

func rpcTaskWork(t *testing.T, s *appSession) *appWork {
	t.Helper()
	for work := range s.works {
		if work.rpcTask {
			return work
		}
	}
	return nil
}

func rpcTaskRequest(t *testing.T, s *appSession, method string) (rpcRaw, string) {
	t.Helper()
	for nativeID, request := range s.requests {
		if request.method == method {
			return rpcInput([]byte(nativeID)), nativeID
		}
	}
	t.Fatalf("missing native %s request", method)
	return nil, ""
}

func lastForwarded(t *testing.T, upstream *bytes.Buffer) rpcMessage {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(upstream.Bytes()), []byte{'\n'})
	if len(lines) == 0 || len(lines[len(lines)-1]) == 0 {
		t.Fatalf("no forwarded message: %q", upstream.String())
	}
	message, err := decodeRPC(lines[len(lines)-1])
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func rpcTaskResponse(t *testing.T, s *appSession, id any) rpcMessage {
	t.Helper()
	want := rpcText(rpcValue(id))
	for _, line := range bytes.Split(s.stdout.(*bytes.Buffer).Bytes(), []byte{'\n'}) {
		message, err := decodeRPC(line)
		if err == nil && rpcText(message["id"]) == want {
			return message
		}
	}
	t.Fatalf("missing client response for %v", id)
	return nil
}

func hasClientResponse(s *appSession, id string) bool {
	for _, line := range bytes.Split(s.stdout.(*bytes.Buffer).Bytes(), []byte{'\n'}) {
		message, err := decodeRPC(line)
		if err == nil && rpcText(message["id"]) == rpcText(rpcValue(id)) {
			return true
		}
	}
	return false
}

// A sample waiting for its own slot is canceled locally: the original request
// fails with -32800, the cancel request is acknowledged, native sees neither,
// and the source thread's queue is not paused.
func TestRPCTaskCancelWhileWaitingKeepsSourceQueueRunning(t *testing.T) {
	s, upstream, events := newRPCTaskSession(t)
	source := s.work("thread")
	source.lease = testLease(t)
	t.Cleanup(func() { s.release(source) })
	source.running["thread:turn"] = true
	state := s.queue("thread")
	holdRPCTaskSlot(t, s)
	if err := s.client(rpcValue(rpcMessage{"id": rpcValue(7), "method": rpcValue("thread/sample"), "params": rpcValue(map[string]any{
		"threadId": "thread", "expectedTurnId": "turn", "allowInProgressTurn": true, "prompt": "sample",
	})})); err != nil {
		t.Fatal(err)
	}
	work := rpcTaskWork(t, s)
	if work == nil || work.pending == nil || work.lease != nil {
		t.Fatal("sample did not wait for its own slot")
	}
	if err := s.client(rpcValue(rpcMessage{"id": rpcValue("cancel"), "method": rpcValue("thread/sample/cancel"), "params": rpcValue(map[string]any{"requestId": 7})})); err != nil {
		t.Fatal(err)
	}
	if response := rpcTaskResponse(t, s, 7); rpcText(rpcObject(response, "error")["code"]) != "-32800" {
		t.Fatalf("local cancel did not fail the sample: %s", rpcValue(response))
	}
	if response := rpcTaskResponse(t, s, "cancel"); response["error"] != nil {
		t.Fatalf("cancel was not acknowledged: %s", rpcValue(response))
	}
	if state.paused {
		t.Fatal("sample cancel paused the source queue")
	}
	if _, exists := s.works[work]; exists {
		t.Fatal("locally canceled sample work leaked")
	}
	if upstream.Len() != 0 {
		t.Fatalf("local cancel reached native: %s", upstream.String())
	}
	if source.lease == nil || !source.running["thread:turn"] {
		t.Fatal("sample cancel changed the running source")
	}
	// The canceled slot worker still reports its acquisition result; it must
	// close the undelivered lease without reviving the sample.
	select {
	case event := <-events:
		if err := s.handle(event); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled admission did not report its result")
	}
}

// Equivalent JSON string encodings must address the same queued sample.
func TestRPCTaskCancelMatchesEscapedIDWhileWaiting(t *testing.T) {
	for _, test := range []struct {
		name, sampleID, cancelID string
	}{
		{"escaped sample", `"req\u003c1\u003e"`, `"req<1>"`},
		{"escaped cancel", `"req<1>"`, `"req\u003c1\u003e"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, upstream, _ := newRPCTaskSession(t)
			holdRPCTaskSlot(t, s)
			t.Cleanup(func() {
				s.close()
				s.workers.Wait()
			})
			if err := s.client([]byte(`{"id":` + test.sampleID + `,"method":"thread/sample","params":{"threadId":"thread","expectedTurnId":"turn","prompt":"sample"}}`)); err != nil {
				t.Fatal(err)
			}
			work := rpcTaskWork(t, s)
			if work == nil || work.pending == nil {
				t.Fatal("sample did not wait for a slot")
			}
			if err := s.client([]byte(`{"id":"cancel","method":"thread/sample/cancel","params":{"requestId":` + test.cancelID + `}}`)); err != nil {
				t.Fatal(err)
			}
			if work.pending != nil || s.works[work] {
				t.Fatal("equivalent request id did not cancel the waiting sample")
			}
			response := rpcTaskResponse(t, s, "req<1>")
			if code := rpcText(rpcObject(response, "error")["code"]); code != "-32800" {
				t.Fatalf("sample cancel code=%s, want -32800", code)
			}
			if result := rpcTaskResponse(t, s, "cancel"); rpcText(result["result"]) != "{}" {
				t.Fatalf("cancel was not acknowledged: %s", rpcValue(result))
			}
			if upstream.Len() != 0 {
				t.Fatalf("local cancellation reached native: %s", upstream.String())
			}
		})
	}
}

// Invalid cancel parameters (unknown fields or a non string/i64 requestId)
// must be forwarded for native validation without touching a waiting sample.
func TestRPCTaskInvalidCancelDoesNotCancelWaitingSample(t *testing.T) {
	s, upstream, _ := newRPCTaskSession(t)
	source := s.work("thread")
	t.Cleanup(func() { s.release(source) })
	holdRPCTaskSlot(t, s)
	if err := s.client(rpcValue(rpcMessage{"id": rpcValue("waiting"), "method": rpcValue("thread/sample"), "params": rpcValue(map[string]any{
		"threadId": "thread", "expectedTurnId": "turn", "prompt": "sample",
	})})); err != nil {
		t.Fatal(err)
	}
	work := rpcTaskWork(t, s)
	if work == nil || work.pending == nil {
		t.Fatal("sample did not wait for a slot")
	}
	if err := s.client(rpcValue(rpcMessage{"id": rpcValue("invalid"), "method": rpcValue("thread/sample/cancel"), "params": rpcValue(map[string]any{
		"requestId": 7, "extra": true,
	})})); err != nil {
		t.Fatal(err)
	}
	if work.pending == nil {
		t.Fatal("invalid cancel stopped the waiting sample")
	}
	if hasClientResponse(s, "waiting") {
		t.Fatal("invalid cancel answered the sample")
	}
	if !bytes.Contains(upstream.Bytes(), []byte(`"thread/sample/cancel"`)) {
		t.Fatal("invalid cancel was not forwarded for native validation")
	}
	correlated := false
	for _, request := range s.requests {
		if request.method == "thread/sample/cancel" && rpcText(request.id) == `"invalid"` {
			correlated = true
		}
	}
	if !correlated {
		t.Fatal("invalid params lost the valid request id correlation")
	}
	if err := s.client(rpcValue(rpcMessage{"id": rpcValue("valid"), "method": rpcValue("thread/sample/cancel"), "params": rpcValue(map[string]any{"requestId": "waiting"})})); err != nil {
		t.Fatal(err)
	}
	if code := rpcText(rpcObject(rpcTaskResponse(t, s, "waiting"), "error")["code"]); code != "-32800" {
		t.Fatalf("valid cancel did not stop the sample: %s", code)
	}
}

// Invalid cancel request envelopes (missing/null/non string-or-i64 id) must be
// passed through for native validation and leave a real queued sample running.
func TestRPCTaskInvalidCancelRequestIDsDoNotCancelWaitingSample(t *testing.T) {
	s, upstream, _ := newRPCTaskSession(t)
	source := s.work("thread")
	t.Cleanup(func() { s.release(source) })
	holdRPCTaskSlot(t, s)
	if err := s.client(rpcValue(rpcMessage{"id": rpcValue("waiting"), "method": rpcValue("thread/sample"), "params": rpcValue(map[string]any{
		"threadId": "thread", "expectedTurnId": "turn", "prompt": "sample",
	})})); err != nil {
		t.Fatal(err)
	}
	work := rpcTaskWork(t, s)
	if work == nil || work.pending == nil {
		t.Fatal("sample did not wait for a slot")
	}
	before := upstream.Len()
	requests := len(s.requests)
	for _, test := range []struct {
		name      string
		id        any
		includeID bool
	}{
		{"missing id", nil, false},
		{"null id", nil, true},
		{"boolean id", true, true},
		{"object id", map[string]any{"id": 1}, true},
		{"fractional id", 1.5, true},
	} {
		message := map[string]any{"method": "thread/sample/cancel", "params": map[string]any{"requestId": "waiting"}}
		if test.includeID {
			message["id"] = test.id
		}
		if err := s.client(rpcValue(message)); err != nil {
			t.Fatal(err)
		}
		forwarded := lastForwarded(t, upstream)
		if test.includeID {
			if rpcText(forwarded["id"]) != rpcText(rpcValue(test.id)) {
				t.Fatalf("%s was rewritten: id=%s", test.name, rpcText(forwarded["id"]))
			}
		} else if rpcLen(forwarded["id"]) != 0 {
			t.Fatalf("%s gained an id: %s", test.name, rpcText(forwarded["id"]))
		}
		if len(s.requests) != requests {
			t.Fatalf("%s allocated request tracking", test.name)
		}
		if work.pending == nil {
			t.Fatalf("%s canceled the queued sample", test.name)
		}
		if hasClientResponse(s, "waiting") {
			t.Fatalf("%s answered the queued sample", test.name)
		}
	}
	if upstream.Len() == before {
		t.Fatal("invalid cancel envelopes were not forwarded for native validation")
	}
	if err := s.client(rpcValue(rpcMessage{"id": rpcValue("valid"), "method": rpcValue("thread/sample/cancel"), "params": rpcValue(map[string]any{"requestId": "waiting"})})); err != nil {
		t.Fatal(err)
	}
	if code := rpcText(rpcObject(rpcTaskResponse(t, s, "waiting"), "error")["code"]); code != "-32800" {
		t.Fatalf("valid cancel did not stop the sample: %s", code)
	}
}

// The in-flight cancel maps the client's typed request id to the forwarded id
// and its acknowledgement never releases the sample's slot.
func TestRPCTaskCancelMidflightMapsIDWithoutReleasingSlot(t *testing.T) {
	s, upstream := newNotificationSession(t)
	source := s.work("thread")
	source.lease = testLease(t)
	t.Cleanup(func() { s.release(source) })
	source.running["thread:turn"] = true
	state := s.threadState("thread")
	state.loaded = true
	epoch := state.epoch
	work := &appWork{thread: "thread", running: make(map[string]bool), completed: make(map[string]bool), rpcTask: true, lease: testLease(t)}
	s.works[work] = true
	t.Cleanup(func() { s.release(work) })
	job := &appAdmission{work: work, thread: "thread", method: "thread/sample", initial: true, independent: true, lifeGen: s.taskLifeGeneration("thread"),
		message: rpcMessage{"id": rpcValue(7), "method": rpcValue("thread/sample"), "params": rpcValue(map[string]any{
			"threadId": "thread", "expectedTurnId": "turn", "prompt": "sample",
		})}}
	if err := s.dispatch(work, job); err != nil {
		t.Fatal(err)
	}
	_, sampleID := rpcTaskRequest(t, s, "thread/sample")

	// Unknown cancel fields are forwarded for native validation and cannot
	// affect the in-flight sample.
	if err := s.client(rpcValue(rpcMessage{"id": rpcValue("invalid"), "method": rpcValue("thread/sample/cancel"), "params": rpcValue(map[string]any{
		"requestId": 7, "extra": true,
	})})); err != nil {
		t.Fatal(err)
	}
	if work.lease == nil {
		t.Fatal("invalid cancel released the sample slot")
	}
	if _, exists := s.requests[sampleID]; !exists {
		t.Fatal("invalid cancel disturbed the in-flight sample")
	}

	// The string "7" must not match the numeric request id 7.
	if err := s.client(rpcValue(rpcMessage{"id": rpcValue("string"), "method": rpcValue("thread/sample/cancel"), "params": rpcValue(map[string]any{"requestId": "7"})})); err != nil {
		t.Fatal(err)
	}
	if got := rpcText(rpcObject(lastForwarded(t, upstream), "params")["requestId"]); got != `"7"` {
		t.Fatalf("string request id was rewritten: %s", got)
	}
	if _, exists := s.requests[sampleID]; !exists {
		t.Fatal("mismatched cancel disturbed the sample request")
	}

	// The numeric 7 addresses the forwarded request id.
	if err := s.client(rpcValue(rpcMessage{"id": rpcValue("number"), "method": rpcValue("thread/sample/cancel"), "params": rpcValue(map[string]any{"requestId": 7})})); err != nil {
		t.Fatal(err)
	}
	cancel := lastForwarded(t, upstream)
	if got := rpcText(rpcObject(cancel, "params")["requestId"]); got != sampleID {
		t.Fatalf("cancel request id=%s, want forwarded id %s", got, sampleID)
	}
	// ACK first: the sample slot and the source turn must both survive.
	if err := s.server(rpcValue(rpcMessage{"id": cancel["id"], "result": rpcValue(map[string]any{})})); err != nil {
		t.Fatal(err)
	}
	if work.lease == nil {
		t.Fatal("cancel ACK released the sample slot early")
	}
	if source.lease == nil || !source.running["thread:turn"] {
		t.Fatal("cancel ACK changed the running source")
	}
	if response := rpcTaskResponse(t, s, "number"); response["error"] != nil {
		t.Fatalf("cancel ACK was not forwarded: %s", rpcValue(response))
	}
	if err := s.server(rpcValue(rpcMessage{"id": rpcInput([]byte(sampleID)), "result": rpcValue(map[string]any{
		"sourceTurnId": "turn", "responseId": "response", "text": "answer",
	})})); err != nil {
		t.Fatal(err)
	}
	if work.lease != nil || s.works[work] {
		t.Fatal("sample reply did not release and clean its work")
	}
	if response := rpcTaskResponse(t, s, 7); response["error"] != nil {
		t.Fatalf("sample reply was not forwarded with its original id: %s", rpcValue(response))
	}
	if state.subscribed || state.epoch != epoch {
		t.Fatal("sample changed the source subscription")
	}
}

func TestRPCTaskCancelMatchesEscapedIDInFlight(t *testing.T) {
	for _, test := range []struct {
		name, sampleID, cancelID string
	}{
		{"escaped sample", `"req\u003c1\u003e"`, `"req<1>"`},
		{"escaped cancel", `"req<1>"`, `"req\u003c1\u003e"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, upstream := newNotificationSession(t)
			t.Cleanup(s.close)
			work := &appWork{thread: "thread", rpcTask: true, lease: testLease(t)}
			s.works[work] = true
			message, err := decodeRPC([]byte(`{"id":` + test.sampleID + `,"method":"thread/sample","params":{"threadId":"thread","expectedTurnId":"turn","prompt":"sample"}}`))
			if err != nil {
				t.Fatal(err)
			}
			job := &appAdmission{work: work, thread: "thread", method: "thread/sample", independent: true, message: message}
			if err := s.dispatch(work, job); err != nil {
				t.Fatal(err)
			}
			sampleID, _ := rpcTaskRequest(t, s, "thread/sample")
			if err := s.client([]byte(`{"id":"cancel","method":"thread/sample/cancel","params":{"requestId":` + test.cancelID + `}}`)); err != nil {
				t.Fatal(err)
			}
			cancel := lastForwarded(t, upstream)
			if got := rpcText(rpcObject(cancel, "params")["requestId"]); got != rpcText(sampleID) {
				t.Fatalf("cancel request id=%s, want native id %s", got, rpcText(sampleID))
			}
			if err := s.server(rpcValue(rpcMessage{"id": cancel["id"], "result": rpcValue(map[string]any{})})); err != nil {
				t.Fatal(err)
			}
			if work.lease == nil {
				t.Fatal("cancel acknowledgement released the sample slot")
			}
			if err := s.server(rpcValue(rpcMessage{"id": sampleID, "error": rpcValue(map[string]any{"code": -32800, "message": "sample canceled"})})); err != nil {
				t.Fatal(err)
			}
			if work.lease != nil || s.works[work] {
				t.Fatal("sample completion did not release its slot and work")
			}
			response := rpcTaskResponse(t, s, "req<1>")
			if code := rpcText(rpcObject(response, "error")["code"]); code != "-32800" {
				t.Fatalf("sample response lost its original id or error: %s", rpcValue(response))
			}
		})
	}
}

func TestGuardianReviewReplyKeepsNativeSubscription(t *testing.T) {
	s, upstream := newNotificationSession(t)
	source := s.work("thread")
	source.running["thread:turn"] = true
	state := s.threadState("thread")
	state.loaded, state.subscribed, state.unsubscribePending = true, false, true
	state.epoch = s.nextThreadEpoch()
	work := &appWork{thread: "thread", running: make(map[string]bool), completed: make(map[string]bool), rpcTask: true, lease: testLease(t)}
	s.works[work] = true
	t.Cleanup(func() { s.release(work) })
	job := &appAdmission{work: work, thread: "thread", method: "thread/guardianReview", initial: true, independent: true, threadEpoch: s.nextThreadEpoch(), lifeGen: s.taskLifeGeneration("thread"),
		message: rpcMessage{"id": rpcValue("guardian"), "method": rpcValue("thread/guardianReview"), "params": rpcValue(map[string]any{"threadId": "thread"})}}
	if err := s.dispatch(work, job); err != nil {
		t.Fatal(err)
	}
	_, nativeID := rpcTaskRequest(t, s, "thread/guardianReview")
	// Native attaches the listener before reviewing, so a review error still
	// leaves the connection subscribed and cancels a deferred unsubscribe.
	if err := s.server(rpcValue(rpcMessage{"id": rpcInput([]byte(nativeID)), "error": rpcValue(map[string]any{"code": -32000, "message": "review failed"})})); err != nil {
		t.Fatal(err)
	}
	if !state.subscribed || !state.loaded || state.unsubscribePending {
		t.Fatalf("review error lost the subscription: %+v", *state)
	}
	if work.lease != nil || s.works[work] {
		t.Fatal("guardian review did not release its own work")
	}
	if source.running["thread:turn"] != true {
		t.Fatal("guardian review changed the source turn")
	}
	if bytes.Contains(upstream.Bytes(), []byte(`"thread/unsubscribe"`)) {
		t.Fatal("deferred unsubscribe reversed the review subscription")
	}
	if response := rpcTaskResponse(t, s, "guardian"); response["error"] == nil {
		t.Fatalf("review error was not forwarded: %s", rpcValue(response))
	}
	// A later client unsubscribe still works once the review has finished.
	delete(source.running, "thread:turn")
	if err := s.unsubscribeThread(rpcValue("unsubscribe"), "thread"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(upstream.Bytes(), []byte(`"thread/unsubscribe"`)) {
		t.Fatal("later unsubscribe was not forwarded after the review")
	}
}

// Repeated samples that end in the admission timeout release every private work
// record and leave the source queue runnable.
func TestRPCTaskTimeoutReleasesRepeatedWork(t *testing.T) {
	s, _, events := newRPCTaskSession(t)
	s.load = func() (Config, error) {
		return Config{MaxConcurrency: 1, WaitTimeout: 50 * time.Millisecond}, nil
	}
	source := s.work("thread")
	t.Cleanup(func() { s.release(source) })
	state := s.queue("thread")
	holdRPCTaskSlot(t, s)
	for _, id := range []string{"first", "second", "third"} {
		if err := s.client(rpcValue(rpcMessage{"id": rpcValue(id), "method": rpcValue("thread/sample"), "params": rpcValue(map[string]any{
			"threadId": "thread", "expectedTurnId": "turn", "prompt": "sample",
		})})); err != nil {
			t.Fatal(err)
		}
		select {
		case event := <-events:
			if err := s.handle(event); err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("sample admission did not time out")
		}
		if response := rpcTaskResponse(t, s, id); rpcText(rpcObject(response, "error")["code"]) != "-32001" {
			t.Fatalf("sample %s did not time out: %s", id, rpcValue(response))
		}
		if rpcTaskWork(t, s) != nil {
			t.Fatalf("timed-out sample %s leaked its work", id)
		}
		if state.paused {
			t.Fatalf("timed-out sample %s paused the source queue", id)
		}
	}
}

// Closing the source thread while the sample RPC is already in flight must not
// release the sample's own slot or stale its pending reply.
func TestRPCTaskInFlightSurvivesSourceClose(t *testing.T) {
	s, _ := newNotificationSession(t)
	source := s.work("source")
	source.lease = testLease(t)
	t.Cleanup(func() { s.release(source) })
	source.running["source:turn"] = true
	work := &appWork{thread: "source", running: make(map[string]bool), completed: make(map[string]bool), rpcTask: true, lease: testLease(t)}
	s.works[work] = true
	t.Cleanup(func() { s.release(work) })
	job := &appAdmission{work: work, thread: "source", method: "thread/sample", initial: true, independent: true, lifeGen: s.taskLifeGeneration("source"),
		message: rpcMessage{"id": rpcValue("sample"), "method": rpcValue("thread/sample"), "params": rpcValue(map[string]any{
			"threadId": "source", "expectedTurnId": "turn", "allowInProgressTurn": true, "prompt": "sample",
		})}}
	if err := s.dispatch(work, job); err != nil {
		t.Fatal(err)
	}
	_, nativeID := rpcTaskRequest(t, s, "thread/sample")
	if err := s.server(rpcValue(closeMessage("thread/closed", "source"))); err != nil {
		t.Fatal(err)
	}
	if work.lease == nil || !s.works[work] {
		t.Fatal("source close released the running sample")
	}
	if _, exists := s.requests[nativeID]; !exists {
		t.Fatal("source close staled the running sample reply")
	}
	if err := s.server(rpcValue(rpcMessage{"id": rpcInput([]byte(nativeID)), "result": rpcValue(map[string]any{
		"sourceTurnId": "turn", "responseId": "response", "text": "answer",
	})})); err != nil {
		t.Fatal(err)
	}
	if work.lease != nil || s.works[work] {
		t.Fatal("sample reply after source close did not release its work")
	}
}

// A sample still waiting for a slot has not reached native, so closing the
// source cancels it locally with -32800.
func TestRPCTaskSourceCloseCancelsWaitingSample(t *testing.T) {
	s, upstream, _ := newRPCTaskSession(t)
	source := s.work("source")
	t.Cleanup(func() { s.release(source) })
	holdRPCTaskSlot(t, s)
	if err := s.client(rpcValue(rpcMessage{"id": rpcValue("waiting"), "method": rpcValue("thread/sample"), "params": rpcValue(map[string]any{
		"threadId": "source", "expectedTurnId": "turn", "prompt": "sample",
	})})); err != nil {
		t.Fatal(err)
	}
	if rpcTaskWork(t, s) == nil {
		t.Fatal("sample did not start waiting")
	}
	if err := s.server(rpcValue(closeMessage("thread/closed", "source"))); err != nil {
		t.Fatal(err)
	}
	if response := rpcTaskResponse(t, s, "waiting"); rpcText(rpcObject(response, "error")["code"]) != "-32800" {
		t.Fatalf("source close did not cancel the waiting sample: %s", rpcValue(response))
	}
	if rpcTaskWork(t, s) != nil {
		t.Fatal("closed-source sample leaked its work")
	}
	if upstream.Len() != 0 {
		t.Fatalf("waiting sample reached native: %s", upstream.String())
	}
}

// A client unsubscribe issued after the guardian request wins, even while the
// review waits for its slot or is awaiting its reply. The existing deferred
// unsubscribe path then syncs native after the RPC ends.
func TestGuardianReviewKeepsNewerUnsubscribe(t *testing.T) {
	t.Run("waiting for slot", func(t *testing.T) {
		s, upstream, _ := newRPCTaskSession(t)
		source := s.work("thread")
		t.Cleanup(func() { s.release(source) })
		state := s.threadState("thread")
		state.loaded, state.subscribed = true, true
		holdRPCTaskSlot(t, s)
		if err := s.client(rpcValue(rpcMessage{"id": rpcValue("guardian"), "method": rpcValue("thread/guardianReview"), "params": rpcValue(map[string]any{"threadId": "thread"})})); err != nil {
			t.Fatal(err)
		}
		work := rpcTaskWork(t, s)
		if work == nil || work.pending == nil {
			t.Fatal("guardian did not wait for its slot")
		}
		intent := work.pending.threadEpoch
		if intent == 0 {
			t.Fatal("guardian intent epoch was not captured on receipt")
		}
		if err := s.unsubscribeThread(rpcValue("deferred"), "thread"); err != nil {
			t.Fatal(err)
		}
		if state.epoch <= intent || !state.unsubscribePending || state.subscribed {
			t.Fatalf("later unsubscribe did not win while waiting: %+v", *state)
		}
		// Admit without consuming the slot event, then feed the native reply.
		job := work.pending
		work.pending = nil
		job.cancel()
		work.lease = testLease(t)
		if err := s.dispatch(work, job); err != nil {
			t.Fatal(err)
		}
		_, nativeID := rpcTaskRequest(t, s, "thread/guardianReview")
		reviewNative(t, s, rpcMessage{"id": rpcInput([]byte(nativeID)), "error": rpcValue(map[string]any{"code": -32000, "message": "review failed"})})
		if state.subscribed {
			t.Fatalf("review overwrote a newer unsubscribe: %+v", *state)
		}
		if !bytes.Contains(upstream.Bytes(), []byte(`"thread/unsubscribe"`)) {
			t.Fatal("deferred unsubscribe was not synced after the review")
		}
	})
	t.Run("reply pending", func(t *testing.T) {
		s, upstream := newNotificationSession(t)
		s.work("thread")
		state := s.threadState("thread")
		state.loaded, state.subscribed = true, true
		work := &appWork{thread: "thread", running: make(map[string]bool), completed: make(map[string]bool), rpcTask: true, lease: testLease(t)}
		s.works[work] = true
		t.Cleanup(func() { s.release(work) })
		job := &appAdmission{work: work, thread: "thread", method: "thread/guardianReview", initial: true, independent: true,
			threadEpoch: s.nextThreadEpoch(), lifeGen: s.taskLifeGeneration("thread"),
			message: rpcMessage{"id": rpcValue("guardian"), "method": rpcValue("thread/guardianReview"), "params": rpcValue(map[string]any{"threadId": "thread"})}}
		if err := s.dispatch(work, job); err != nil {
			t.Fatal(err)
		}
		if err := s.unsubscribeThread(rpcValue("deferred"), "thread"); err != nil {
			t.Fatal(err)
		}
		if state.subscribed || !state.unsubscribePending {
			t.Fatalf("unsubscribe was not deferred: %+v", *state)
		}
		_, nativeID := rpcTaskRequest(t, s, "thread/guardianReview")
		reviewNative(t, s, rpcMessage{"id": rpcInput([]byte(nativeID)), "result": rpcValue(map[string]any{"callId": "call-1", "assessment": nil})})
		if state.subscribed {
			t.Fatalf("review overwrote a newer unsubscribe: %+v", *state)
		}
		if !bytes.Contains(upstream.Bytes(), []byte(`"thread/unsubscribe"`)) {
			t.Fatal("deferred unsubscribe was not synced after the review")
		}
	})
}

// A native unsubscribe reply that was already in flight when the review
// started must not close the thread after the review restored the
// subscription; the guard epoch advances past that older intent.
func TestGuardianReviewBlocksLateUnsubscribeReply(t *testing.T) {
	s, _ := newNotificationSession(t)
	state := s.threadState("thread")
	state.loaded, state.subscribed = true, true
	if err := s.unsubscribeThread(rpcValue("old-unsubscribe"), "thread"); err != nil {
		t.Fatal(err)
	}
	oldID, _ := rpcTaskRequest(t, s, "thread/unsubscribe")
	if state.subscribed {
		t.Fatal("unsubscribe was not applied locally")
	}
	work := &appWork{thread: "thread", running: make(map[string]bool), completed: make(map[string]bool), rpcTask: true, lease: testLease(t)}
	s.works[work] = true
	t.Cleanup(func() { s.release(work) })
	job := &appAdmission{work: work, thread: "thread", method: "thread/guardianReview", initial: true, independent: true,
		threadEpoch: s.nextThreadEpoch(), lifeGen: s.taskLifeGeneration("thread"),
		message: rpcMessage{"id": rpcValue("guardian"), "method": rpcValue("thread/guardianReview"), "params": rpcValue(map[string]any{"threadId": "thread"})}}
	if err := s.dispatch(work, job); err != nil {
		t.Fatal(err)
	}
	_, nativeID := rpcTaskRequest(t, s, "thread/guardianReview")
	reviewNative(t, s, rpcMessage{"id": rpcInput([]byte(nativeID)), "result": rpcValue(map[string]any{"callId": "call-1", "assessment": nil})})
	if !state.subscribed || state.closed {
		t.Fatalf("review did not restore the subscription: %+v", *state)
	}
	reviewNative(t, s, rpcMessage{"id": oldID, "result": rpcValue(map[string]any{"status": "notLoaded"})})
	if state.closed || !state.subscribed {
		t.Fatalf("late unsubscribe reply reversed the review subscription: %+v", *state)
	}
}
