package proxy

import (
	"bytes"
	"fmt"
	"testing"
)

func TestNonTaskControlsWaitForNativeConfirmation(t *testing.T) {
	for _, control := range []struct {
		method, pending string
		params          map[string]any
	}{
		{"turn/interrupt", "turn/start", map[string]any{"turnId": "active-turn"}},
		{"thread/goal/clear", "thread/goal/set", nil},
		{"thread/goal/set", "thread/goal/set", map[string]any{"status": "paused"}},
		{"thread/realtime/stop", "thread/realtime/start", nil},
	} {
		for _, rejected := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/rejected=%t", control.method, rejected), func(t *testing.T) {
				s, upstream := newNotificationSession(t)
				work := s.work("thread")
				job := &appAdmission{work: work, thread: "thread", method: control.pending, initial: true,
					message: rpcMessage{"id": rpcValue("waiting"), "method": rpcValue(control.pending)}}
				s.beginAdmissionJob(t, work, job)
				params := map[string]any{"threadId": "thread"}
				for key, value := range control.params {
					params[key] = value
				}
				if err := s.client(rpcValue(map[string]any{"id": "control", "method": control.method, "params": params})); err != nil {
					t.Fatal(err)
				}
				if work.pending != job || job.ctx.Err() != nil || s.queue("thread").paused {
					t.Fatal("control changed local work before native accepted it")
				}
				request, err := decodeRPC(upstream.Bytes())
				if err != nil || rpcString(request, "method") != control.method {
					t.Fatalf("control did not reach native: %v: %s", err, upstream.Bytes())
				}
				response := rpcMessage{"id": request["id"], "result": rpcValue(map[string]any{})}
				if rejected {
					delete(response, "result")
					response["error"] = rpcValue(map[string]any{"code": -32600, "message": "native rejected control", "data": "original detail"})
				}
				reviewNative(t, s, response)
				got := controlResponse(t, s, "control")
				response["id"] = rpcValue("control")
				if !bytes.Equal(rpcValue(got), rpcValue(response)) {
					t.Fatalf("control response was replaced: %s", rpcValue(got))
				}
				if rejected {
					if work.pending != job || job.ctx.Err() != nil || s.queue("thread").paused {
						t.Fatal("rejected control canceled work or paused its queue")
					}
				} else if work.pending != nil || job.ctx.Err() == nil || !s.queue("thread").paused {
					t.Fatal("accepted control did not cancel the captured waiting task")
				}
			})
		}
	}
}

func TestInterruptReachesNativeWhenShellAndAdmissionShareThread(t *testing.T) {
	s, upstream := newNotificationSession(t)
	work := s.work("thread")
	work.running["thread:shell-turn"] = false // Native shell commands own no model slot.
	job := &appAdmission{work: work, thread: "thread", method: "turn/start", initial: true,
		message: rpcMessage{"id": rpcValue("waiting"), "method": rpcValue("turn/start")}}
	s.beginAdmissionJob(t, work, job)
	if err := s.client(rpcValue(map[string]any{"id": "interrupt", "method": "turn/interrupt", "params": map[string]any{"threadId": "thread", "turnId": "shell-turn"}})); err != nil {
		t.Fatal(err)
	}
	request, err := decodeRPC(upstream.Bytes())
	if err != nil || rpcString(request, "method") != "turn/interrupt" {
		t.Fatalf("running shell interrupt was swallowed: %v: %s", err, upstream.Bytes())
	}
	if s.stdout.(*bytes.Buffer).Len() != 0 {
		t.Fatal("interrupt returned success before native confirmation")
	}
	reviewNative(t, s, rpcMessage{"id": request["id"], "result": rpcValue(map[string]any{})})
	if work.pending != nil || !work.hasRunning("thread") {
		t.Fatal("interrupt must cancel the pending task and await native shell completion")
	}
	reviewNative(t, s, rpcMessage{"method": rpcValue("turn/completed"), "params": rpcValue(map[string]any{"threadId": "thread", "turn": map[string]any{"id": "shell-turn", "status": "interrupted"}})})
	if s.busy() {
		t.Fatal("interrupted shell retained task state")
	}
}

func TestSharedChildShellRemainsUnrestrictedAfterParentFinishes(t *testing.T) {
	s, work, upstream := sharedParentChildWork(t)
	if err := s.client(rpcValue(map[string]any{"id": "child-shell", "method": "thread/shellCommand", "params": map[string]any{"threadId": "child", "command": "sleep 1"}})); err != nil {
		t.Fatal(err)
	}
	request, err := decodeRPC(upstream.Bytes())
	if err != nil || rpcString(request, "method") != "thread/shellCommand" {
		t.Fatalf("shell request missing: %v: %s", err, upstream.Bytes())
	}
	// Native shell startup is asynchronous. The parent's model turn can end
	// before the child's standalone shell turn/started notification arrives.
	reviewNative(t, s, rpcMessage{"method": rpcValue("turn/completed"), "params": rpcValue(map[string]any{"threadId": "parent", "turn": map[string]any{"id": "turn", "status": "completed"}})})
	if work.lease != nil {
		t.Fatal("standalone shell reserved the completed model task's slot")
	}
	reviewNative(t, s, rpcMessage{"method": rpcValue("turn/started"), "params": rpcValue(map[string]any{"threadId": "child", "turn": map[string]any{"id": "shell-turn", "status": "inProgress"}})})
	if work.running["child:shell-turn"] {
		t.Fatal("standalone child shell was treated as a model task")
	}
	reviewNative(t, s, rpcMessage{"id": request["id"], "result": rpcValue(map[string]any{})})
	reviewNative(t, s, rpcMessage{"method": rpcValue("turn/completed"), "params": rpcValue(map[string]any{"threadId": "child", "turn": map[string]any{"id": "shell-turn", "status": "completed"}})})
	if s.busy() {
		t.Fatal("completed shell retained work state")
	}
}

func TestPolicyOverlayPreservesMalformedNativeParameters(t *testing.T) {
	for _, test := range []struct {
		method string
		params any
	}{
		{"thread/start", "invalid"},
		{"thread/start", nil},
		{"thread/resume", []any{}},
		{"thread/start", map[string]any{"config": "invalid"}},
		{"thread/resume", map[string]any{"threadId": "thread", "config": []any{}}},
		{"thread/fork", map[string]any{"threadId": "thread", "config": 1}},
		{"initialize", map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}, "capabilities": "invalid"}},
		{"initialize", map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}, "capabilities": map[string]any{"experimentalApi": "invalid"}}},
		{"initialize", map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}, "capabilities": map[string]any{"optOutNotificationMethods": "invalid"}}},
	} {
		t.Run(fmt.Sprintf("%s/%s", test.method, rpcValue(test.params)), func(t *testing.T) {
			s, upstream := newNotificationSession(t)
			if err := s.client(rpcValue(map[string]any{"id": 7, "method": test.method, "params": test.params})); err != nil {
				t.Fatal(err)
			}
			request, err := decodeRPC(upstream.Bytes())
			if err != nil || !bytes.Equal(request["params"], rpcValue(test.params)) {
				t.Fatalf("invalid params became a different native request: %v: %s", err, upstream.Bytes())
			}
		})
	}
}

func TestCancelRequestWithoutTargetDoesNotCancelPrivateRPC(t *testing.T) {
	s, upstream := newNotificationSession(t)
	if err := s.internal("thread/settings/update", rpcMessage{"threadId": rpcValue("thread")}, func(rpcMessage) error { return nil }); err != nil {
		t.Fatal(err)
	}
	upstream.Reset()
	message := rpcMessage{"method": rpcValue("$/cancelRequest"), "params": rpcValue(map[string]any{})}
	if err := s.client(rpcValue(message)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(upstream.Bytes()), rpcValue(message)) {
		t.Fatalf("missing cancel target was replaced by a private RPC id: %s", upstream.Bytes())
	}
}

func TestNonTaskRPCPassesUnknownFieldsAndNativeErrors(t *testing.T) {
	for _, method := range []string{"model/list", "thread/read", "thread/archive", "thread/unarchive", "thread/name/set", "config/read", "account/read", "process/kill", "future/control", "thread/queue/future"} {
		t.Run(method, func(t *testing.T) {
			s, upstream := newNotificationSession(t)
			message := rpcMessage{"id": rpcValue(73), "method": rpcValue(method), "params": rpcValue(map[string]any{"threadId": "thread", "futureOption": map[string]any{"enabled": false, "value": nil}}), "futureEnvelope": rpcValue("keep")}
			if err := s.client(rpcValue(message)); err != nil {
				t.Fatal(err)
			}
			request, err := decodeRPC(upstream.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			nativeID := request["id"]
			request["id"] = message["id"]
			if !bytes.Equal(rpcValue(request), rpcValue(message)) {
				t.Fatalf("passthrough changed request: %s", upstream.Bytes())
			}
			response := rpcMessage{"id": nativeID, "error": rpcValue(map[string]any{"code": -32601, "message": "native detail", "data": map[string]any{"future": true}})}
			reviewNative(t, s, response)
			response["id"] = message["id"]
			if !bytes.Equal(bytes.TrimSpace(s.stdout.(*bytes.Buffer).Bytes()), rpcValue(response)) {
				t.Fatal("passthrough replaced native error")
			}
		})
	}
}
