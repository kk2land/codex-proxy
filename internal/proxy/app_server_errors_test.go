package proxy

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// errorTestLogs returns all JSON log records written by the test session.
func errorTestLogs(t *testing.T, s *appSession) string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(s.dir, "*.log"))
	if err != nil {
		t.Fatal(err)
	}
	var result strings.Builder
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		result.Write(data)
	}
	return result.String()
}

func TestAppServerRPCErrorCauseAndDataLogged(t *testing.T) {
	s, _ := newNotificationSession(t)
	if err := s.client(rpcValue(map[string]any{"id": "diagnostic-rpc", "method": "model/list", "params": map[string]any{}})); err != nil {
		t.Fatal(err)
	}
	cause := map[string]any{"code": -32051, "message": "upstream transport unavailable", "data": map[string]any{"retry_after_ms": 50}}
	reviewNative(t, s, rpcMessage{"id": controlRequestID(t, s, "model/list", "diagnostic-rpc"), "error": rpcValue(cause)})
	if response := controlResponse(t, s, "diagnostic-rpc"); string(response["error"]) != string(rpcValue(cause)) {
		t.Fatalf("native error changed: %s", rpcValue(response))
	}
	log := errorTestLogs(t, s)
	for _, want := range []string{"upstream transport unavailable", "retry_after_ms", `error_data=`, `error_object=`} {
		if !strings.Contains(log, want) {
			t.Errorf("actual RPC error details absent from log: %q", want)
		}
	}
}

func TestAppServerNotificationCausesLogged(t *testing.T) {
	for _, mode := range []string{"error", "thread/realtime/error", "turn/completed"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := newNotificationSession(t)
			cause := "diagnostic cause for " + mode
			params := map[string]any{"threadId": "thread"}
			switch mode {
			case "error":
				params["error"] = map[string]any{"code": -32052, "message": cause, "data": map[string]any{"backend": "failed"}}
			case "thread/realtime/error":
				params["message"] = cause
			default:
				params["turn"] = map[string]any{"id": "turn", "status": "failed", "error": map[string]any{"message": cause, "additionalDetails": "worker crashed"}}
			}
			reviewNative(t, s, rpcMessage{"method": rpcValue(mode), "params": rpcValue(params)})
			log := errorTestLogs(t, s)
			if !strings.Contains(log, cause) {
				t.Fatalf("notification cause absent from log: %s", mode)
			}
		})
	}
}

func TestAppServerAdmissionCauseReturnedAndLogged(t *testing.T) {
	s, _ := newNotificationSession(t)
	ctx, cancel := context.WithCancel(context.Background())
	s.ctx = ctx
	t.Cleanup(cancel)
	work := s.work("thread")
	job := &appAdmission{work: work, thread: "thread", method: "turn/start", initial: true,
		message: rpcMessage{"id": rpcValue("admission-cause"), "method": rpcValue("turn/start"), "params": rpcValue(taskParams("thread", "input"))}}
	s.beginAdmissionJob(t, work, job)
	cause := errors.New("underlying config read failed: permission denied")
	if err := s.admitted(appServerEvent{source: "admitted", job: job, err: cause}); err != nil {
		t.Fatal(err)
	}
	response := controlResponse(t, s, "admission-cause")
	if string(rpcObject(response, "error")["code"]) != "-32000" {
		t.Fatalf("admission error code changed: %s", rpcValue(response))
	}
	if !strings.Contains(string(rpcValue(response)), cause.Error()) {
		t.Errorf("admission cause not returned: %s", rpcValue(response))
	}
	if !strings.Contains(errorTestLogs(t, s), cause.Error()) {
		t.Error("admission cause not logged")
	}
}

func TestAppServerStateReadyCauseReturnedAndLogged(t *testing.T) {
	s, _ := newNotificationSession(t)
	work := s.work("thread")
	job := &appAdmission{work: work, thread: "thread", method: "turn/start", initial: true,
		message: rpcMessage{"id": rpcValue("state-cause"), "method": rpcValue("turn/start"), "params": rpcValue(taskParams("thread", "input"))}}
	s.beginAdmissionJob(t, work, job)
	cause := errors.New("open state lock: permission denied")
	if err := s.stateReady(appServerEvent{source: "state_ready", job: job, err: cause}); err != nil {
		t.Fatal(err)
	}
	response := controlResponse(t, s, "state-cause")
	if string(rpcObject(response, "error")["code"]) != "-32000" {
		t.Fatalf("state error code changed: %s", rpcValue(response))
	}
	if !strings.Contains(string(rpcValue(response)), cause.Error()) {
		t.Errorf("state cause not returned: %s", rpcValue(response))
	}
	if !strings.Contains(errorTestLogs(t, s), cause.Error()) {
		t.Error("state cause not logged")
	}
}

func TestAppServerGuardCauseReturnedAndLogged(t *testing.T) {
	s, _ := newNotificationSession(t)
	s.selfBin = filepath.Join(t.TempDir(), "missing-guard")
	work := s.work("thread")
	job := &appAdmission{work: work, thread: "thread", method: "turn/start", initial: true,
		message: rpcMessage{"id": rpcValue("guard-cause"), "method": rpcValue("turn/start"), "params": rpcValue(taskParams("thread", "input"))}}
	s.beginAdmissionJob(t, work, job)
	if err := s.admitted(appServerEvent{source: "admitted", job: job, lease: testLease(t)}); err != nil {
		t.Fatal(err)
	}
	response := controlResponse(t, s, "guard-cause")
	if string(rpcObject(response, "error")["code"]) != "-32000" {
		t.Fatalf("guard error code changed: %s", rpcValue(response))
	}
	for _, want := range []string{"cannot start task lock guardian", "missing-guard"} {
		if !strings.Contains(string(rpcValue(response)), want) {
			t.Errorf("guard cause not returned: %s", rpcValue(response))
		}
		if !strings.Contains(errorTestLogs(t, s), want) {
			t.Errorf("guard cause not logged: %s", want)
		}
	}
}

func TestAppServerPolicyCauseReturnedAndLogged(t *testing.T) {
	s, _ := newNotificationSession(t)
	if err := s.client(rpcValue(map[string]any{"id": "thread-policy", "method": "thread/start", "params": map[string]any{}})); err != nil {
		t.Fatal(err)
	}
	reviewNative(t, s, rpcMessage{"id": controlRequestID(t, s, "thread/start", "thread-policy"), "result": rpcValue(map[string]any{"thread": map[string]any{"id": "policy-thread"}})})
	cause := map[string]any{"code": -32053, "message": "policy backend unavailable", "data": map[string]any{"reason": "backend restart"}}
	reviewNative(t, s, rpcMessage{"id": controlRequestID(t, s, "thread/settings/update", ""), "error": rpcValue(cause)})
	response := controlResponse(t, s, "thread-policy")
	if string(rpcObject(response, "error")["code"]) != "-32000" {
		t.Fatalf("proxy policy error code changed: %s", rpcValue(response))
	}
	if !strings.Contains(string(rpcValue(response)), "policy backend unavailable") {
		t.Errorf("native policy cause not returned: %s", rpcValue(response))
	}
	if !strings.Contains(errorTestLogs(t, s), "policy backend unavailable") {
		t.Error("native policy cause not logged")
	}
}

func TestAppServerAdmissionPolicyCauseReturnedAndLogged(t *testing.T) {
	s, _ := newNotificationSession(t)
	// A real guard process may start here; a nonexistent child pid keeps its
	// termination attempts harmless.
	s.selfBin, s.childPID = os.Args[0], 999999
	work := s.work("thread")
	t.Cleanup(func() { s.release(work) })
	job := &appAdmission{work: work, thread: "thread", method: "turn/start", initial: true,
		message: rpcMessage{"id": rpcValue("admission-policy"), "method": rpcValue("turn/start"), "params": rpcValue(taskParams("thread", "input"))}}
	s.beginAdmissionJob(t, work, job)
	if err := s.admitted(appServerEvent{source: "admitted", job: job, lease: testLease(t)}); err != nil {
		t.Fatal(err)
	}
	cause := map[string]any{"code": -32055, "message": "admission policy rejected", "data": map[string]any{"policy": "externalSandbox"}}
	reviewNative(t, s, rpcMessage{"id": controlRequestID(t, s, "thread/settings/update", ""), "error": rpcValue(cause)})
	response := controlResponse(t, s, "admission-policy")
	if string(rpcObject(response, "error")["code"]) != "-32000" {
		t.Fatalf("admission policy error code changed: %s", rpcValue(response))
	}
	if !strings.Contains(string(rpcValue(response)), "admission policy rejected") {
		t.Errorf("admission policy cause not returned: %s", rpcValue(response))
	}
	if !strings.Contains(errorTestLogs(t, s), "admission policy rejected") {
		t.Error("admission policy cause not logged")
	}
}

func TestAppServerQueueFailureCauseReturnedAndLogged(t *testing.T) {
	s, _ := newNotificationSession(t)
	work, id := queuedControlWork(t, s)
	cause := map[string]any{"code": -32054, "message": "native queue quota exhausted", "data": map[string]any{"limit": 2}}
	reviewNative(t, s, rpcMessage{"id": id, "error": rpcValue(cause)})
	responseText := s.stdout.(*bytes.Buffer).String()
	if !strings.Contains(responseText, "native queue quota exhausted") {
		t.Errorf("automatic queued error lost native cause: %s", responseText)
	}
	if !strings.Contains(errorTestLogs(t, s), "native queue quota exhausted") {
		t.Error("automatic queued error cause not logged")
	}
	state := s.queueState("thread")
	if state == nil || !state.paused || len(state.items) != 2 || work.lease != nil {
		t.Fatal("diagnostics changed queue failure cleanup")
	}
}
