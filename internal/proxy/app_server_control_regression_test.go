package proxy

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func controlRequestID(t *testing.T, s *appSession, method, clientID string) json.RawMessage {
	t.Helper()
	for id, request := range s.requests {
		if request.method == method && (clientID == "" || string(request.id) == string(rpcValue(clientID))) {
			return json.RawMessage(id)
		}
	}
	t.Fatalf("missing native request for %s (%s)", method, clientID)
	return nil
}

func controlResponse(t *testing.T, s *appSession, clientID string) rpcMessage {
	t.Helper()
	for _, line := range bytes.Split(s.stdout.(*bytes.Buffer).Bytes(), []byte{'\n'}) {
		message, err := decodeRPC(line)
		if err == nil && string(message["id"]) == string(rpcValue(clientID)) {
			return message
		}
	}
	t.Fatalf("missing client response for %s", clientID)
	return nil
}

func TestAppServerRejectedActivationAfterSuccessfulBudgetPatch(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	client := startAppEnv(t, home, []string{"CODEX_PROXY_TEST_TASK_INTERVAL=2"})
	thread := client.thread()
	client.call("thread/goal/set", map[string]any{"threadId": thread, "objective": "goal", "status": "paused", "tokenBudget": 100})
	state := rpcValue(map[string]any{lastCodexTaskKey: time.Now().Unix()})
	if err := os.WriteFile(filepath.Join(home, ".codex-proxy", stateFilename), state, 0600); err != nil {
		t.Fatal(err)
	}
	client.send("bad-activation", "thread/goal/set", map[string]any{"threadId": thread, "status": "active", "tokenBudget": -1})
	awaitSlots(t, home, 1)
	client.call("thread/goal/set", map[string]any{"threadId": thread, "tokenBudget": 200})
	if response := client.response("bad-activation"); response["error"] == nil {
		t.Fatal("invalid activation was accepted")
	}
	awaitSlots(t, home, 0)
	if code := client.stop(); code != 0 {
		t.Fatalf("exit=%d", code)
	}
}

func TestRejectedGoalControlsReleaseReservation(t *testing.T) {
	for _, order := range []string{"activation first", "patch first"} {
		t.Run(order, func(t *testing.T) {
			s, _ := newNotificationSession(t)
			work := s.work("thread")
			work.lease = testLease(t)
			t.Cleanup(func() { s.release(work) })
			// Leave the native goal unknown, as on an ephemeral thread.
			epoch := s.reserveGoalControl("thread")
			job := &appAdmission{work: work, thread: "thread", method: "thread/goal/set", initial: true, goalEpoch: epoch,
				message: rpcMessage{"id": rpcValue("activation"), "method": rpcValue("thread/goal/set"), "params": rpcValue(map[string]any{"threadId": "thread", "objective": "goal", "status": "active", "tokenBudget": -1})}}
			if err := s.dispatch(work, job); err != nil {
				t.Fatal(err)
			}
			if err := s.client(rpcValue(map[string]any{"id": "patch", "method": "thread/goal/set", "params": map[string]any{"threadId": "thread", "tokenBudget": 0}})); err != nil {
				t.Fatal(err)
			}
			ids := []json.RawMessage{controlRequestID(t, s, "thread/goal/set", "activation"), controlRequestID(t, s, "thread/goal/set", "patch")}
			if order == "patch first" {
				ids[0], ids[1] = ids[1], ids[0]
			}
			for _, id := range ids {
				reviewNative(t, s, rpcMessage{"id": id, "error": rpcValue(map[string]any{"code": -32602})})
			}
			if work.lease != nil || work.goal || work.inflight != 0 || work.awaiting != 0 {
				t.Fatalf("rejected controls retained work: lease=%v goal=%v inflight=%d awaiting=%d", work.lease != nil, work.goal, work.inflight, work.awaiting)
			}
		})
	}
}

func TestGoalActivationAckBeforePendingPause(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(map[bool]string{false: "pause accepted", true: "pause rejected"}[rejected], func(t *testing.T) {
			s, _ := newNotificationSession(t)
			work := s.work("thread")
			work.lease = testLease(t)
			t.Cleanup(func() { s.release(work) })
			s.storeGoal("thread", rpcMessage{"status": rpcValue("paused"), "objective": rpcValue("goal")})
			epoch := s.reserveGoalControl("thread")
			job := &appAdmission{work: work, thread: "thread", method: "thread/goal/set", initial: true, goalEpoch: epoch,
				message: rpcMessage{"id": rpcValue("activation"), "method": rpcValue("thread/goal/set"), "params": rpcValue(map[string]any{"threadId": "thread", "status": "active"})}}
			if err := s.dispatch(work, job); err != nil {
				t.Fatal(err)
			}
			if err := s.client(rpcValue(map[string]any{"id": "pause", "method": "thread/goal/set", "params": map[string]any{"threadId": "thread", "status": "paused"}})); err != nil {
				t.Fatal(err)
			}
			// ACKs can precede notifications. A pending pause has not yet ended
			// the confirmed active chain and must not release its reservation.
			reviewNative(t, s, rpcMessage{"id": controlRequestID(t, s, "thread/goal/set", "activation"), "result": rpcValue(map[string]any{"goal": map[string]any{"status": "active", "objective": "goal"}})})
			if work.lease == nil || !work.goal {
				t.Fatal("pending pause released the confirmed active chain")
			}
			response := rpcMessage{"id": controlRequestID(t, s, "thread/goal/set", "pause")}
			if rejected {
				response["error"] = rpcValue(map[string]any{"code": -32602})
			} else {
				response["result"] = rpcValue(map[string]any{"goal": map[string]any{"status": "paused", "objective": "goal"}})
			}
			reviewNative(t, s, response)
			if rejected {
				if work.lease == nil || !work.goal {
					t.Fatal("rejected pause ended the confirmed active chain")
				}
				reviewNative(t, s, closeMessage("thread/goal/cleared", "thread"))
			}
			if work.lease != nil || work.goal {
				t.Fatal("ended goal retained its reservation")
			}
		})
	}
}

func TestGoalActivationAckAfterPreparationPatch(t *testing.T) {
	s, _ := newNotificationSession(t)
	work := s.work("thread")
	work.lease = testLease(t)
	t.Cleanup(func() { s.release(work) })
	s.storeGoal("thread", rpcMessage{"status": rpcValue("paused"), "objective": rpcValue("goal")})
	epoch := s.reserveGoalControl("thread")
	job := &appAdmission{work: work, thread: "thread", method: "thread/goal/set", initial: true, goalEpoch: epoch,
		message: rpcMessage{"id": rpcValue("activation"), "method": rpcValue("thread/goal/set"), "params": rpcValue(map[string]any{"threadId": "thread", "status": "active"})}}
	work.pending = job
	if err := s.client(rpcValue(map[string]any{"id": "patch", "method": "thread/goal/set", "params": map[string]any{"threadId": "thread", "tokenBudget": 200}})); err != nil {
		t.Fatal(err)
	}
	reviewNative(t, s, rpcMessage{"id": controlRequestID(t, s, "thread/goal/set", "patch"), "result": rpcValue(map[string]any{"goal": map[string]any{"status": "paused", "objective": "goal"}})})
	work.pending = nil
	if err := s.dispatch(work, job); err != nil {
		t.Fatal(err)
	}
	reviewNative(t, s, rpcMessage{"id": controlRequestID(t, s, "thread/goal/set", "activation"), "result": rpcValue(map[string]any{"goal": map[string]any{"status": "active", "objective": "goal"}})})
	if work.lease == nil || !work.goal {
		t.Fatal("an earlier budget patch hid the admitted activation")
	}
	reviewNative(t, s, closeMessage("thread/goal/cleared", "thread"))
	if work.lease != nil {
		t.Fatal("cleared goal retained its reservation")
	}
}

func TestGoalReadDoesNotMaskPendingPause(t *testing.T) {
	s, _ := newNotificationSession(t)
	work := s.work("thread")
	work.lease = testLease(t)
	t.Cleanup(func() { s.release(work) })
	reviewNative(t, s, rpcMessage{"method": rpcValue("thread/goal/updated"), "params": rpcValue(map[string]any{"threadId": "thread", "goal": map[string]any{"status": "active", "objective": "goal"}})})
	for _, request := range []map[string]any{
		{"id": "pause", "method": "thread/goal/set", "params": map[string]any{"threadId": "thread", "status": "paused"}},
		{"id": "read", "method": "thread/goal/get", "params": map[string]any{"threadId": "thread"}},
	} {
		if err := s.client(rpcValue(request)); err != nil {
			t.Fatal(err)
		}
	}
	// The read still sees active while native has not applied the pause.
	reviewNative(t, s, rpcMessage{"id": controlRequestID(t, s, "thread/goal/get", "read"), "result": rpcValue(map[string]any{"goal": map[string]any{"status": "active", "objective": "goal"}})})
	reviewNative(t, s, rpcMessage{"id": controlRequestID(t, s, "thread/goal/set", "pause"), "result": rpcValue(map[string]any{"goal": map[string]any{"status": "paused", "objective": "goal"}})})
	if work.lease != nil || work.goal || s.goalState("thread").status != "paused" {
		t.Fatal("a concurrent read hid the confirmed pause")
	}
}

func queuedControlWork(t *testing.T, s *appSession) (*appWork, json.RawMessage) {
	t.Helper()
	work := s.work("thread")
	work.lease = testLease(t)
	t.Cleanup(func() { s.release(work) })
	state := s.queue("thread")
	for _, id := range []string{"head", "tail"} {
		entry := &appQueuedInput{ID: id, ClientUserMessageID: id, Input: rpcValue(taskParams("thread", id)["input"])}
		state.items = append(state.items, entry)
		s.queuedSize += entry.size()
	}
	head := state.items[0]
	job := &appAdmission{work: work, thread: "thread", method: "turn/start", queueID: head.ID, initial: true,
		message: rpcMessage{"method": rpcValue("turn/start"), "params": rpcValue(map[string]any{"threadId": "thread", "input": head.Input, "clientUserMessageId": head.ClientUserMessageID})}}
	if err := s.dispatch(work, job); err != nil {
		t.Fatal(err)
	}
	return work, controlRequestID(t, s, "turn/start", "")
}

func TestQueuePendingAckRejectsMutations(t *testing.T) {
	for _, method := range []string{"thread/queue/update", "thread/queue/delete", "thread/queue/reorder"} {
		t.Run(method, func(t *testing.T) {
			s, _ := newNotificationSession(t)
			_, nativeID := queuedControlWork(t, s)
			state := s.queueState("thread")
			original, size := string(state.items[0].Input), s.queuedSize
			params := map[string]any{"threadId": "thread", "queuedSubmissionId": "head", "input": taskParams("thread", "edited")["input"], "queuedSubmissionIds": []string{"tail", "head"}}
			if err := s.client(rpcValue(map[string]any{"id": "mutation", "method": method, "params": params})); err != nil {
				t.Fatal(err)
			}
			if response := controlResponse(t, s, "mutation"); string(rpcObject(response, "error")["code"]) != "-32000" {
				t.Fatalf("in-flight mutation was not rejected: %s", rpcValue(response))
			}
			if len(state.items) != 2 || state.items[0].ID != "head" || string(state.items[0].Input) != original || s.queuedSize != size {
				t.Fatal("rejected mutation changed the submitted input or queue accounting")
			}
			reviewNative(t, s, rpcMessage{"id": nativeID, "result": rpcValue(map[string]any{"turn": map[string]any{"id": "turn", "status": "inProgress"}})})
			if len(state.items) != 1 || state.items[0].ID != "tail" || s.queuedSize != state.items[0].size() {
				t.Fatal("ACK failed to remove exactly the submitted input")
			}
		})
	}
}

func TestAppServerQueuePendingAckRejectsEdit(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	client := startApp(t, home)
	thread := client.thread()
	client.call("test/delayTurnReplies", map[string]any{})
	queued := client.call("thread/queue/add", map[string]any{"threadId": thread, "clientUserMessageId": "queued", "input": taskParams(thread, "original input")["input"]})
	queueID := rpcString(rpcObject(queued, "queuedSubmission"), "id")
	client.await(func(message rpcMessage) bool {
		return rpcString(message, "method") == "turn/started" && rpcString(rpcObject(message, "params"), "threadId") == thread
	})
	client.send("edit", "thread/queue/update", map[string]any{"threadId": thread, "queuedSubmissionId": queueID, "input": taskParams(thread, "edited input")["input"]})
	if response := client.response("edit"); string(rpcObject(response, "error")["code"]) != "-32000" {
		t.Fatalf("submitted input remained editable: %s", rpcValue(response))
	}
	client.call("test/flushTurnReplies", map[string]any{})
	list := client.call("thread/queue/list", map[string]any{"threadId": thread})
	if string(list["data"]) != "[]" {
		t.Fatalf("accepted input remained queued: %s", rpcValue(list))
	}
	captured, err := os.ReadFile(filepath.Join(client.dir, "rpc.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(captured, []byte("original input")) || bytes.Contains(captured, []byte("edited input")) {
		t.Fatal("native received the rejected edit")
	}
	client.finish(thread)
	awaitSlots(t, home, 0)
	if code := client.stop(); code != 0 {
		t.Fatalf("exit=%d", code)
	}
}

func TestQueueRejectedInputCanBeEdited(t *testing.T) {
	s, upstream := newNotificationSession(t)
	work, nativeID := queuedControlWork(t, s)
	// An unrelated queued input remains editable while the head awaits ACK.
	for _, id := range []string{"tail", "head"} {
		if id == "head" {
			reviewNative(t, s, rpcMessage{"id": nativeID, "error": rpcValue(map[string]any{"code": -32602})})
		}
		if err := s.client(rpcValue(map[string]any{"id": "edit-" + id, "method": "thread/queue/update", "params": map[string]any{"threadId": "thread", "queuedSubmissionId": id, "input": taskParams("thread", "edited "+id)["input"]}})); err != nil {
			t.Fatal(err)
		}
		if response := controlResponse(t, s, "edit-"+id); response["error"] != nil {
			t.Fatalf("editable input rejected: %s", rpcValue(response))
		}
	}
	if state := s.queueState("thread"); !state.paused || work.lease != nil || len(state.items) != 2 || !bytes.Contains(state.items[0].Input, []byte("edited head")) || !bytes.Contains(state.items[1].Input, []byte("edited tail")) {
		t.Fatal("rejected submission did not preserve editable queued inputs")
	}
	if bytes.Contains(upstream.Bytes(), []byte("edited")) {
		t.Fatal("editing a paused queue resubmitted it")
	}
}

func TestQueueClosedRequestDoesNotLockOrRemoveNewEdit(t *testing.T) {
	s, _ := newNotificationSession(t)
	_, nativeID := queuedControlWork(t, s)
	reviewNative(t, s, closeMessage("thread/closed", "thread"))
	s.reopenThread("thread")
	if err := s.client(rpcValue(map[string]any{"id": "edit", "method": "thread/queue/update", "params": map[string]any{"threadId": "thread", "queuedSubmissionId": "head", "input": taskParams("thread", "new input")["input"]}})); err != nil {
		t.Fatal(err)
	}
	if response := controlResponse(t, s, "edit"); response["error"] != nil {
		t.Fatalf("closed request locked queued input: %s", rpcValue(response))
	}
	reviewNative(t, s, rpcMessage{"id": nativeID, "result": rpcValue(map[string]any{"turn": map[string]any{"id": "old-turn"}})})
	if state := s.queueState("thread"); len(state.items) != 2 || !bytes.Contains(state.items[0].Input, []byte("new input")) {
		t.Fatal("late ACK removed an input edited after close")
	}
}

func TestResumePathTracksActualDestinationClose(t *testing.T) {
	for _, scenario := range []string{"previous close", "ignored id closed", "target closes", "ignored id closes", "target closes and reopens"} {
		t.Run(scenario, func(t *testing.T) {
			s, _ := newNotificationSession(t)
			s.work("destination")
			reviewNative(t, s, closeMessage("thread/closed", "destination"))
			if scenario == "ignored id closed" {
				reviewNative(t, s, closeMessage("thread/closed", "ignored"))
				s.reopenThread("ignored")
				reviewNative(t, s, closeMessage("thread/closed", "ignored"))
			}
			if err := s.client(rpcValue(map[string]any{"id": "resume", "method": "thread/resume", "params": map[string]any{"threadId": "ignored", "path": "/tmp/rollout.jsonl"}})); err != nil {
				t.Fatal(err)
			}
			nativeID := controlRequestID(t, s, "thread/resume", "resume")
			stale := scenario == "target closes" || scenario == "target closes and reopens"
			if stale {
				s.reopenThread("destination")
				reviewNative(t, s, closeMessage("thread/closed", "destination"))
				if scenario == "target closes and reopens" {
					s.reopenThread("destination")
				}
			} else if scenario == "ignored id closes" {
				reviewNative(t, s, closeMessage("thread/closed", "ignored"))
			}
			reviewNative(t, s, rpcMessage{"id": nativeID, "result": rpcValue(map[string]any{"thread": map[string]any{"id": "destination"}})})
			if stale {
				if response := controlResponse(t, s, "resume"); string(rpcObject(response, "error")["code"]) != "-32800" {
					t.Fatalf("stale path resume was accepted: %s", rpcValue(response))
				}
				return
			}
			reviewNative(t, s, rpcMessage{"id": controlRequestID(t, s, "thread/settings/update", ""), "result": rpcValue(map[string]any{})})
			reviewNative(t, s, rpcMessage{"id": controlRequestID(t, s, "thread/goal/get", ""), "result": rpcValue(map[string]any{"goal": nil})})
			if response := controlResponse(t, s, "resume"); response["error"] != nil || s.closedTasks["destination"] || !s.policyReady["destination"] || !s.subs["destination"].subscribed {
				t.Fatalf("valid path resume was rejected: %s", rpcValue(response))
			}
		})
	}
}
