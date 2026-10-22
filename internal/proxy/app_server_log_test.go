package proxy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func appLogRecords(t *testing.T, home string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".codex-proxy", appServerLogFilename))
	if err != nil {
		t.Fatal(err)
	}
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

func findAppLog(t *testing.T, records []map[string]any, event, key string, value any) map[string]any {
	t.Helper()
	for _, record := range records {
		if record["event"] == event && (key == "" || record[key] == value) {
			return record
		}
	}
	t.Fatalf("missing %s with %s=%v", event, key, value)
	return nil
}

func assertLogMS(t *testing.T, record map[string]any, field string, minimum float64) float64 {
	t.Helper()
	value, ok := record[field].(float64)
	if !ok || value < minimum {
		t.Fatalf("%s should be >= %v: %v", field, minimum, record)
	}
	return value
}

func TestAppServerLogsUserCorrelationAndTaskDurations(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "2s")
	held, _, err := testLimiter(filepath.Join(home, ".codex-proxy"), 1, time.Second).Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	client := startApp(t, home)
	thread := client.thread()
	client.send("logged-turn", "turn/start", taskParams(thread, "private logged task prompt"))
	awaitLog(t, home, "waiting")
	time.Sleep(70 * time.Millisecond)
	held.Close()
	response := client.response("logged-turn")
	turn := rpcString(rpcObject(rpcObject(response, "result"), "turn"), "id")
	if turn == "" {
		t.Fatalf("missing turn: %v", response)
	}
	queued := client.call("thread/queue/add", map[string]any{"threadId": thread, "clientUserMessageId": "queued-client", "input": []any{map[string]any{"type": "text", "text": "private queued prompt"}}})
	queueID := rpcString(rpcObject(queued, "queuedSubmission"), "id")
	time.Sleep(70 * time.Millisecond)
	client.finish(thread)
	client.await(func(m rpcMessage) bool {
		return rpcString(m, "method") == "turn/started" && rpcString(rpcObject(rpcObject(m, "params"), "turn"), "id") != turn
	})
	client.finish(thread)
	awaitSlots(t, home, 0)
	client.call("process/spawn", processParams("logged-process", "hold"))
	time.Sleep(40 * time.Millisecond)
	client.call("test/processExit", map[string]any{"processHandle": "logged-process"})
	client.send("native-failure", "test/error", map[string]any{})
	client.response("native-failure")
	client.call("private prompt in method", map[string]any{})
	if code := client.stop(); code != 0 {
		t.Fatalf("exit=%d", code)
	}

	records := appLogRecords(t, home)
	started := findAppLog(t, records, "started", "", nil)
	for _, record := range records {
		if record["user"] != "sandbox-user" || record["pid"] == nil || record["child_pid"] != started["child_pid"] || record["request_id"] != started["request_id"] || record["session_cwd"] == nil || record["timestamp"] == nil {
			t.Fatalf("incomplete common metadata: %v", record)
		}
		switch record["event"] {
		case "notification", "rpc_received", "rpc_started":
			t.Fatalf("unexpected noisy event: %v", record)
		case "rpc_finished":
			if record["internal"] == true && record["status"] == "completed" {
				t.Fatalf("unexpected successful internal RPC log: %v", record)
			}
		}
	}
	cli := readLoggerRecords(t, filepath.Join(home, ".codex-proxy"), loggerDefaultMaxSize)
	if cli[0]["request_id"] != started["request_id"] {
		t.Fatal("CLI and app-server logs cannot be correlated")
	}
	received := findAppLog(t, records, "task_received", "rpc_id", "logged-turn")
	taskID := received["task_id"]
	if taskID == nil || taskID == "" || received["submission_id"] == nil {
		t.Fatalf("missing early task identity: %v", received)
	}
	for _, event := range []string{"waiting", "admitted", "submitted", "task_started", "task_finished", "released"} {
		record := findAppLog(t, records, event, "task_id", taskID)
		if record["thread_id"] != thread || record["method"] != "turn/start" {
			t.Fatalf("missing task context: %v", record)
		}
	}
	submitted := findAppLog(t, records, "submitted", "task_id", taskID)
	waitMS := assertLogMS(t, submitted, "wait_ms", 60)
	if assertLogMS(t, submitted, "slot_wait_ms", 60) > waitMS {
		t.Fatal("slot wait exceeds total wait")
	}
	finished := findAppLog(t, records, "task_finished", "task_id", taskID)
	assertLogMS(t, finished, "run_ms", 60)
	if finished["turn_id"] != turn || finished["status"] != "completed" {
		t.Fatalf("missing turn outcome: %v", finished)
	}
	rpc := findAppLog(t, records, "rpc_finished", "rpc_id", "logged-turn")
	assertLogMS(t, rpc, "run_ms", 0)
	if rpc["status"] != "completed" || rpc["method"] != "turn/start" {
		t.Fatalf("missing client RPC outcome: %v", rpc)
	}
	assertLogMS(t, findAppLog(t, records, "submitted", "queue_id", queueID), "queue_wait_ms", 60)
	process := findAppLog(t, records, "process_finished", "process_handle", "logged-process")
	assertLogMS(t, process, "run_ms", 30)
	if process["exit_code"] != float64(0) {
		t.Fatalf("missing process exit: %v", process)
	}
	failure := findAppLog(t, records, "rpc_finished", "rpc_id", "native-failure")
	if failure["status"] != "failed" || failure["error_code"] != float64(-32000) {
		t.Fatalf("missing RPC failure: %v", failure)
	}
	// Native error text is retained for internal diagnosis; prompts and tool
	// bodies outside error responses remain unlogged.
	if failure["error"] != "private prompt in error" {
		t.Fatalf("missing native RPC error text: %v", failure)
	}
	data, _ := json.Marshal(records)
	for _, secret := range []string{"private logged task prompt", "private queued prompt", "private prompt in method"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("logs leaked %q", secret)
		}
	}
}

func TestAppServerLogsNotificationErrorsAndForwardsUpdates(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "2s")
	client := startApp(t, home)
	thread := client.thread()
	for _, notification := range []struct {
		method string
		params map[string]any
	}{
		{"item/agentMessage/delta", map[string]any{"threadId": thread, "delta": "private streaming delta"}},
		{"test/updated", map[string]any{"error": nil}},
		{"error", map[string]any{"threadId": thread, "turnId": "failed-turn", "error": map[string]any{"code": -32000, "message": "private notification text"}}},
		{"thread/realtime/error", map[string]any{"threadId": thread, "message": "private realtime failure"}},
		{"test/failed", map[string]any{"error": map[string]any{"code": -32002, "message": "private notification failure"}}},
	} {
		client.call("test/notify", map[string]any{"method": notification.method, "params": notification.params})
		message := client.await(func(m rpcMessage) bool { return rpcString(m, "method") == notification.method })
		if got, want := string(message["params"]), string(rpcValue(notification.params)); got != want {
			t.Fatalf("notification changed during forwarding: got %s, want %s", got, want)
		}
	}
	client.stop()
	records := appLogRecords(t, home)
	errors := 0
	for _, record := range records {
		if record["event"] == "notification" {
			t.Fatalf("unexpected notification log: %v", record)
		}
		if record["stage"] == "notification" {
			errors++
			if record["event"] != "error" || record["status"] != "failed" {
				t.Fatalf("missing notification error outcome: %v", record)
			}
		}
	}
	if errors != 3 {
		t.Fatalf("got %d notification errors, want 3", errors)
	}
	failure := findAppLog(t, records, "error", "method", "error")
	if failure["thread_id"] != thread || failure["turn_id"] != "failed-turn" || failure["error_code"] != float64(-32000) {
		t.Fatalf("missing notification error metadata: %v", failure)
	}
	if failure["error"] != "private notification text" {
		t.Fatalf("missing notification error text: %v", failure)
	}
	if findAppLog(t, records, "error", "method", "test/failed")["error_code"] != float64(-32002) {
		t.Fatal("missing error code on notification without thread")
	}
	// params.message on realtime/error is the actual cause and must be visible.
	if findAppLog(t, records, "error", "method", "thread/realtime/error")["error"] != "private realtime failure" {
		t.Fatal("missing realtime notification cause")
	}
	data, _ := json.Marshal(records)
	if strings.Contains(string(data), "private streaming delta") {
		t.Fatal("streaming delta was logged")
	}
	if !strings.Contains(string(data), "private notification failure") {
		t.Fatal("notification failure cause was not logged")
	}
}

func TestAppServerLogsTimeoutCancellationAndDisconnect(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "150ms")
	held, _, err := testLimiter(filepath.Join(home, ".codex-proxy"), 1, time.Second).Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	client := startApp(t, home)
	thread := client.thread()
	client.send("timeout-log", "turn/start", taskParams(thread, "private timeout"))
	client.response("timeout-log")
	client.send("cancel-log", "turn/start", taskParams(thread, "private canceled"))
	client.call("model/list", map[string]any{}) // Ensure the task has reached admission.
	time.Sleep(40 * time.Millisecond)
	client.send(nil, "$/cancelRequest", map[string]any{"id": "cancel-log"})
	client.response("cancel-log")
	client.send("disconnect-log", "turn/start", taskParams(thread, "private disconnected"))
	client.call("model/list", map[string]any{})
	client.stop()
	records := appLogRecords(t, home)
	timeout := findAppLog(t, records, "timeout", "rpc_id", "timeout-log")
	assertLogMS(t, timeout, "wait_ms", 140)
	assertLogMS(t, timeout, "slot_wait_ms", 140)
	if timeout["error_code"] != float64(-32001) || timeout["task_id"] == nil {
		t.Fatalf("missing timeout details: %v", timeout)
	}
	canceled := findAppLog(t, records, "canceled", "rpc_id", "cancel-log")
	assertLogMS(t, canceled, "wait_ms", 30)
	assertLogMS(t, canceled, "slot_wait_ms", 30)
	disconnected := findAppLog(t, records, "canceled", "rpc_id", "disconnect-log")
	if disconnected["reason"] != "session_closed" {
		t.Fatalf("missing disconnect reason: %v", disconnected)
	}
}

func TestAppServerLogsSubmissionIntervalSeparately(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "2s")
	client := startAppEnv(t, home, []string{"CODEX_PROXY_TEST_TASK_INTERVAL=2"})
	thread := client.thread()
	writeTaskState(t, filepath.Join(home, ".codex-proxy"), time.Now().Unix())
	client.send("interval-log", "turn/start", taskParams(thread, "private interval prompt"))
	client.response("interval-log")
	client.finish(thread)
	awaitSlots(t, home, 0)
	client.stop()
	records := appLogRecords(t, home)
	submitted := findAppLog(t, records, "submitted", "rpc_id", "interval-log")
	interval := assertLogMS(t, submitted, "interval_wait_ms", 900)
	assertLogMS(t, submitted, "wait_ms", interval)
	released := findAppLog(t, records, "released", "task_id", submitted["task_id"])
	run := assertLogMS(t, released, "run_ms", 0)
	assertLogMS(t, released, "held_ms", run+interval-10)
}
