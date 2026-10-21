package proxy

import (
	"os"
	"testing"
)

func TestGoalSetModeClassification(t *testing.T) {
	s, _ := newNotificationSession(t)
	params := func(values map[string]any) rpcMessage {
		return rpcMessage(rpcObject(rpcMessage{"params": rpcValue(values)}, "params"))
	}
	work := &appWork{thread: "thread", running: make(map[string]bool), completed: make(map[string]bool)}
	state := s.goalState("thread")
	state.known, state.status, state.objective = true, "paused", "existing goal"
	if got := s.goalSetMode("thread", params(map[string]any{"tokenBudget": 100}), work); got != goalSetPatch {
		t.Fatalf("paused budget update = %v, want patch", got)
	}
	if got := s.goalSetMode("thread", params(map[string]any{"objective": "existing goal"}), work); got != goalSetPatch {
		t.Fatalf("same-objective patch = %v, want patch", got)
	}
	if got := s.goalSetMode("thread", params(map[string]any{"status": nil, "objective": nil}), work); got != goalSetPatch {
		t.Fatalf("null patch = %v, want patch", got)
	}
	state.status = "blocked"
	if got := s.goalSetMode("thread", params(map[string]any{"tokenBudget": 50}), work); got != goalSetPatch {
		t.Fatalf("blocked budget = %v, want patch", got)
	}
	if got := s.goalSetMode("thread", params(map[string]any{"objective": "new goal"}), work); got != goalSetRun {
		t.Fatalf("new objective = %v, want run", got)
	}
	state.status = "complete"
	if got := s.goalSetMode("thread", params(map[string]any{"objective": "existing goal"}), work); got != goalSetPatch {
		t.Fatalf("complete same-objective patch = %v, want patch", got)
	}
	state.status = "paused"
	if got := s.goalSetMode("thread", params(map[string]any{"status": "active"}), work); got != goalSetRun {
		t.Fatalf("explicit active = %v, want run", got)
	}
	// Known active without a slot must run; with a slot it is a control patch.
	state.status = "active"
	if got := s.goalSetMode("thread", params(map[string]any{"tokenBudget": 10}), work); got != goalSetRun {
		t.Fatalf("active idle budget = %v, want run", got)
	}
	work.lease = &Lease{}
	if got := s.goalSetMode("thread", params(map[string]any{"tokenBudget": 10}), work); got != goalSetPatch {
		t.Fatalf("active held budget = %v, want patch", got)
	}
	// A pending initial admission owns no slot; a bare patch must not be
	// forwarded as a safe control.
	work.lease = nil
	work.pending = &appAdmission{work: work}
	state.known, state.status = false, ""
	if got := s.goalSetMode("thread", params(map[string]any{"tokenBudget": 10}), work); got != goalSetRun {
		t.Fatalf("unknown pending budget = %v, want run", got)
	}
	state.known, state.status = true, "active"
	if got := s.goalSetMode("thread", params(map[string]any{"tokenBudget": 10}), work); got != goalSetRun {
		t.Fatalf("active pending budget = %v, want run", got)
	}
	state.status = "paused"
	if got := s.goalSetMode("thread", params(map[string]any{"tokenBudget": 10}), work); got != goalSetPatch {
		t.Fatalf("paused pending budget = %v, want patch", got)
	}
}

func TestGoalStopsWorkOnlyExplicitDeactivation(t *testing.T) {
	for _, test := range []struct {
		params map[string]any
		want   bool
	}{
		{map[string]any{"tokenBudget": 100}, false},
		{map[string]any{"objective": "same"}, false},
		{map[string]any{"status": nil}, false},
		{map[string]any{"status": "active"}, false},
		{map[string]any{"status": "paused"}, true},
		{map[string]any{"status": "blocked"}, true},
		{map[string]any{"status": "usageLimited"}, true},
		{map[string]any{"status": "budgetLimited"}, true},
		{map[string]any{"status": "complete"}, true},
	} {
		if got := goalStopsWork(rpcMessage(rpcObject(rpcMessage{"params": rpcValue(test.params)}, "params"))); got != test.want {
			t.Fatalf("goalStopsWork(%v) = %v, want %v", test.params, got, test.want)
		}
	}
}

func TestGoalEpochRejectsStaleReplies(t *testing.T) {
	s, _ := newNotificationSession(t)
	first := s.reserveGoalControl("thread")
	s.applyGoalSetResult("thread", first, rpcMessage{"result": rpcValue(map[string]any{"goal": map[string]any{"status": "active", "objective": "goal"}})})
	if state := s.goalState("thread"); state.status != "active" {
		t.Fatalf("accepted set not cached: %+v", state)
	}
	pause := s.reserveGoalControl("thread")
	s.applyGoalSetResult("thread", pause, rpcMessage{"result": rpcValue(map[string]any{"goal": map[string]any{"status": "paused", "objective": "goal"}})})
	if s.applyGoalSetResult("thread", first, rpcMessage{"result": rpcValue(map[string]any{"goal": map[string]any{"status": "active", "objective": "old"}})}) {
		t.Fatal("stale set result accepted")
	}
	// A still older get must not restore active.
	if s.applyGoalGet("thread", first, rpcMessage{"result": rpcValue(map[string]any{"goal": map[string]any{"status": "active"}})}) {
		t.Fatal("stale get accepted")
	}
	if state := s.goalState("thread"); state.status != "paused" {
		t.Fatalf("pause overwritten: %+v", state)
	}
	epoch := s.goalStateSeq("thread")
	s.applyGoalCleared("thread")
	if s.applyGoalSetResult("thread", epoch, rpcMessage{"result": rpcValue(map[string]any{"goal": map[string]any{"status": "active"}})}) {
		t.Fatal("pre-clear set result accepted")
	}
}

func TestAppServerGoalPartialUpdatesDoNotStartWork(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	client := startApp(t, home)
	thread := client.thread()
	paused := client.call("thread/goal/set", map[string]any{"threadId": thread, "objective": "paused goal", "status": "paused"})
	if rpcString(rpcObject(paused, "goal"), "status") != "paused" {
		t.Fatalf("paused goal not created: %v", paused)
	}
	awaitSlots(t, home, 0)
	for name, params := range map[string]map[string]any{
		"budget":         {"threadId": thread, "tokenBudget": 9000},
		"same objective": {"threadId": thread, "objective": "paused goal"},
		"null status":    {"threadId": thread, "status": nil, "tokenBudget": 12000},
	} {
		goal := client.call("thread/goal/set", params)
		if rpcString(rpcObject(goal, "goal"), "status") != "paused" {
			t.Fatalf("%s changed status: %v", name, goal)
		}
		awaitSlots(t, home, 0)
	}
	// Explicit activation can start the continuation chain.
	active := client.call("thread/goal/set", map[string]any{"threadId": thread, "status": "active"})
	if rpcString(rpcObject(active, "goal"), "status") != "active" {
		t.Fatalf("activation failed: %v", active)
	}
	awaitSlots(t, home, 1)
	client.finish(thread)
	awaitSlots(t, home, 1)
	// A blocked goal keeps that state on a budget patch and holds its chain.
	client.call("thread/goal/set", map[string]any{"threadId": thread, "status": "blocked"})
	client.finish(thread)
	awaitSlots(t, home, 0)
	blocked := client.call("thread/goal/set", map[string]any{"threadId": thread, "tokenBudget": 4000})
	if rpcString(rpcObject(blocked, "goal"), "status") != "blocked" {
		t.Fatalf("blocked budget changed state: %v", blocked)
	}
	awaitSlots(t, home, 0)
	client.call("thread/goal/set", map[string]any{"threadId": thread, "status": "complete"})
	completed := client.call("thread/goal/set", map[string]any{"threadId": thread, "objective": "paused goal", "tokenBudget": 5000})
	if rpcString(rpcObject(completed, "goal"), "status") != "complete" {
		t.Fatalf("same-objective patch rebuilt complete goal: %v", completed)
	}
	awaitSlots(t, home, 0)
	client.call("thread/goal/clear", map[string]any{"threadId": thread})
	if code := client.stop(); code != 0 {
		t.Fatalf("exit=%d", code)
	}
}

func TestAppServerGoalOriginPassthrough(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	client := startApp(t, home)
	thread := client.thread()
	for _, origin := range []string{"user", "automatic"} {
		goal := client.call("thread/goal/set", map[string]any{"threadId": thread, "objective": "origin goal", "status": "paused", "origin": origin})
		if rpcString(rpcObject(goal, "goal"), "origin") != origin {
			t.Fatalf("origin changed: %v", goal)
		}
	}
	if code := client.stop(); code != 0 {
		t.Fatalf("exit=%d", code)
	}
}

func TestAppServerGoalBudgetDoesNotCancelWaitingActivation(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	client := startApp(t, home)
	first, second := client.thread(), client.thread()
	client.call("turn/start", taskParams(first, "holder"))
	awaitSlots(t, home, 1)
	client.send("waiting", "thread/goal/set", map[string]any{"threadId": second, "objective": "waiting goal"})
	awaitLog(t, home, "waiting")
	for name, params := range map[string]map[string]any{
		"budget":         {"threadId": second, "tokenBudget": 100},
		"same objective": {"threadId": second, "objective": "waiting goal"},
	} {
		client.send(name, "thread/goal/set", params)
		// The activation is still queued, so native may reject a goal patch
		// for a goal it has not seen yet. Either way it must not cancel the
		// waiting activation.
		client.response(name)
	}
	// Release the holder: the waiting activation keeps its reservation and
	// proceeds instead of having been canceled by the patches.
	client.finish(first)
	if response := client.response("waiting"); response["error"] != nil {
		t.Fatalf("waiting activation was canceled: %v", response)
	}
	awaitSlots(t, home, 1)
	client.finish(second)
	awaitSlots(t, home, 1)
	client.call("thread/goal/clear", map[string]any{"threadId": second})
	if code := client.stop(); code != 0 {
		t.Fatalf("exit=%d", code)
	}
}

func TestAppServerGoalPauseAndClearCancelWaitingActivation(t *testing.T) {
	for _, mode := range []string{"pause", "clear"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			configureAppProxy(t, home, fakeCodex(t), 1, "5s")
			client := startApp(t, home)
			first, second := client.thread(), client.thread()
			client.call("turn/start", taskParams(first, "holder"))
			awaitSlots(t, home, 1)
			client.send("waiting", "thread/goal/set", map[string]any{"threadId": second, "objective": "waiting goal"})
			awaitLog(t, home, "waiting")
			if mode == "pause" {
				client.call("thread/goal/set", map[string]any{"threadId": second, "status": "paused"})
			} else {
				client.call("thread/goal/clear", map[string]any{"threadId": second})
			}
			if response := client.response("waiting"); string(rpcObject(response, "error")["code"]) != "-32800" {
				t.Fatalf("waiting activation not canceled: %v", response)
			}
			awaitSlots(t, home, 1)
			client.finish(first)
			awaitSlots(t, home, 0)
			if code := client.stop(); code != 0 {
				t.Fatalf("exit=%d", code)
			}
		})
	}
}

func TestAppServerNewObjectiveOnIdleActivationRequiresAdmission(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	client := startApp(t, home)
	thread := client.thread()
	client.call("thread/goal/set", map[string]any{"threadId": thread, "objective": "first", "status": "paused"})
	awaitSlots(t, home, 0)
	client.send("new-objective", "thread/goal/set", map[string]any{"threadId": thread, "objective": "second"})
	awaitSlots(t, home, 1)
	client.response("new-objective")
	client.finish(thread)
	awaitSlots(t, home, 1)
	client.call("thread/goal/clear", map[string]any{"threadId": thread})
	client.finish(thread)
	awaitSlots(t, home, 0)
	if code := client.stop(); code != 0 {
		t.Fatalf("exit=%d", code)
	}
}

func TestAppServerGoalStaleResponseDoesNotRestoreWorkGoal(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "get"} {
		t.Run(outcome, func(t *testing.T) {
			s, _ := newNotificationSession(t)
			work := s.work("thread")
			file, err := os.CreateTemp(t.TempDir(), "lease")
			if err != nil {
				t.Fatal(err)
			}
			work.lease = &Lease{file: file}
			t.Cleanup(func() { s.release(work) })
			// A running turn keeps the lease while pause ends the automatic chain.
			work.running["thread:turn"] = true
			oldEpoch := s.reserveGoalControl("thread")
			s.applyGoalNotification("thread", rpcMessage{"status": rpcValue("paused"), "objective": rpcValue("goal")})
			request := &appRPCRequest{method: "thread/goal/set", work: work, thread: "thread", goalEpoch: oldEpoch, task: true}
			response := rpcMessage{"result": rpcValue(map[string]any{"goal": map[string]any{"status": "active", "objective": "old goal"}})}
			if outcome == "get" {
				request.method, request.task = "thread/goal/get", false
			} else {
				work.inflight = 1
			}
			if outcome == "failure" {
				response = rpcMessage{"error": rpcValue(map[string]any{"code": -32000})}
			}
			if err := s.response(request, response); err != nil {
				t.Fatal(err)
			}
			if work.goal || s.goalState("thread").status != "paused" {
				t.Fatalf("stale %s restored automatic execution: goal=%v cache=%+v", outcome, work.goal, s.goalState("thread"))
			}
			if work.inflight != 0 || work.lease == nil {
				t.Fatalf("stale %s damaged the running turn: inflight=%d lease=%v", outcome, work.inflight, work.lease)
			}
		})
	}
}

func TestAppServerGoalUnknownBudgetDoesNotBypassAdmission(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	client := startApp(t, home)
	holder := client.thread()
	// The private goal/get fails for an ephemeral fork, leaving its goal state
	// unknown. A bare patch must still pass through admission.
	fork := client.call("thread/fork", map[string]any{"threadId": holder, "ephemeral": true})
	unknownThread := rpcString(rpcObject(fork, "thread"), "id")
	client.call("turn/start", taskParams(holder, "holder"))
	awaitSlots(t, home, 1)
	client.send("budget", "thread/goal/set", map[string]any{"threadId": unknownThread, "tokenBudget": 100})
	client.call("model/list", map[string]any{})
	findAppLog(t, appLogRecords(t, home), "task_received", "rpc_id", "budget")
	client.finish(holder)
	// The patch was admitted (native then rejects a budget-only set without a
	// goal); it did not run outside the shared quota.
	if response := client.response("budget"); response["error"] == nil {
		t.Fatalf("fixture accepted a budget-only goal: %v", response)
	}
	awaitSlots(t, home, 0)
	if code := client.stop(); code != 0 {
		t.Fatalf("exit=%d", code)
	}
}
