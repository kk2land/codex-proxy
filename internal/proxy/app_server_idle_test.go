package proxy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func configureAppIdle(t *testing.T, home, timeout string) {
	t.Helper()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	path := filepath.Join(home, ".codex-proxy", "env")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString("CODEX_PROXY_APP_SERVER_IDLE_TIMEOUT=" + timeout + "\n"); err != nil {
		t.Fatal(err)
	}
}

func assertAppIdleExit(t *testing.T, client *appClient) {
	t.Helper()
	if code := client.waitExit(); code != 0 {
		data, _ := os.ReadFile(client.stderr.Name())
		t.Fatalf("idle exit=%d: %s", code, data)
	}
	for _, value := range startedPIDs(t, client.dir) {
		pid, _ := strconv.Atoi(value)
		if syscall.Kill(pid, 0) == nil {
			t.Fatalf("native process %d survived idle shutdown", pid)
		}
	}
	data, err := os.ReadFile(filepath.Join(client.home, ".codex-proxy", appServerLogFilename))
	if err != nil || !strings.Contains(string(data), `reason=idle_timeout`) {
		t.Fatalf("idle shutdown not logged: %s, %v", data, err)
	}
}

func TestAppServerDirectChildAndIdleExit(t *testing.T) {
	home := t.TempDir()
	configureAppIdle(t, home, "400ms")
	client := startApp(t, home)
	launch := client.call("test/launch", map[string]any{})
	var parent int
	if err := json.Unmarshal(launch["parentPid"], &parent); err != nil || parent != client.cmd.Process.Pid {
		t.Fatalf("native parent=%d want fake codex pid=%d: %v", parent, client.cmd.Process.Pid, err)
	}
	assertAppIdleExit(t, client)
	records := readLoggerRecords(t, filepath.Join(home, ".codex-proxy"), loggerDefaultMaxSize)
	if len(records) != 2 || records[0]["event"] != "started" || records[1]["event"] != "finished" {
		t.Fatalf("CLI lifecycle logs missing: %v", records)
	}
	for _, record := range records {
		if string(rpcValue(record["args"])) != `["app-server"]` || record["user"] != "sandbox-user" {
			t.Fatalf("CLI log fields changed: %v", record)
		}
	}
	// A new invocation can immediately create a fresh connection.
	if code := startApp(t, home).stop(); code != 0 {
		t.Fatalf("reconnected exit=%d", code)
	}
}

func TestAppServerIdleResetsOnClientRequests(t *testing.T) {
	home := t.TempDir()
	configureAppIdle(t, home, "500ms")
	client := startApp(t, home)
	for i := 0; i < 5; i++ {
		time.Sleep(200 * time.Millisecond)
		client.call("model/list", map[string]any{})
	}
	assertAppIdleExit(t, client)
}

func TestAppServerIdleProtectsWork(t *testing.T) {
	for _, mode := range []string{"turn", "slot", "state", "rpc", "server-request", "goal", "realtime", "paused-queue"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			configureAppIdle(t, home, "400ms")
			dir := filepath.Join(home, ".codex-proxy")
			var env []string
			if mode == "state" {
				writeTaskState(t, dir, time.Now().Unix())
				env = []string{"CODEX_PROXY_TEST_TASK_INTERVAL=15"}
			}
			client := startAppEnv(t, home, env)
			thread := client.thread()
			var finish func()
			switch mode {
			case "turn":
				client.call("turn/start", taskParams(thread, "silent task"))
				finish = func() { client.finish(thread) }
			case "slot":
				held, _, err := testLimiter(dir, 1, time.Second).Acquire(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				defer held.Close()
				client.send("waiting", "turn/start", taskParams(thread, "waiting for quota"))
				awaitLog(t, home, "waiting")
				finish = func() {
					client.send(nil, "$/cancelRequest", map[string]any{"id": "waiting"})
					if code := string(rpcObject(client.response("waiting"), "error")["code"]); code != "-32800" {
						t.Fatalf("cancel waiting task: %s", code)
					}
				}
			case "state":
				client.send("waiting", "turn/start", taskParams(thread, "submission interval"))
				awaitStateLock(t, dir)
				finish = func() {
					client.send(nil, "$/cancelRequest", map[string]any{"id": "waiting"})
					client.response("waiting")
				}
			case "rpc":
				client.send("deferred", "test/deferReply", map[string]any{})
				client.call("model/list", map[string]any{}) // Confirms the fixture saw the request.
				finish = func() { client.call("test/flushReply", map[string]any{}); client.response("deferred") }
			case "server-request":
				client.call("test/toolRequest", map[string]any{"threadId": thread})
				finish = func() {
					if err := writeRPC(client.input, rpcMessage{"id": rpcValue("tool-server-id"), "result": rpcValue(map[string]any{})}); err != nil {
						t.Fatal(err)
					}
					client.await(func(m rpcMessage) bool { return rpcString(m, "method") == "test/toolReply" })
				}
			case "goal":
				client.call("thread/goal/set", map[string]any{"threadId": thread, "objective": "active goal"})
				started := awaitThreadNotification(t, client, "turn/started", thread)
				turnID := rpcString(rpcObject(rpcObject(started, "params"), "turn"), "id")
				finish = func() { client.call("turn/interrupt", map[string]any{"threadId": thread, "turnId": turnID}) }
			case "realtime":
				client.call("thread/realtime/start", map[string]any{"threadId": thread, "outputModality": "text", "version": "v2"})
				finish = func() { client.call("thread/realtime/stop", map[string]any{"threadId": thread}) }
			case "paused-queue":
				turn := client.call("turn/start", taskParams(thread, "first task"))
				queued := client.call("thread/queue/add", map[string]any{"threadId": thread, "clientUserMessageId": "queued", "input": []any{map[string]any{"type": "text", "text": "queued task"}}})
				client.call("turn/interrupt", map[string]any{"threadId": thread, "turnId": rpcString(rpcObject(turn, "turn"), "id")})
				finish = func() {
					client.call("thread/queue/delete", map[string]any{"threadId": thread, "queuedSubmissionId": rpcString(rpcObject(queued, "queuedSubmission"), "id")})
				}
			}
			// No client or server traffic while work is outstanding.
			time.Sleep(650 * time.Millisecond)
			finish()
			// The completed work receives a fresh full idle window.
			time.Sleep(100 * time.Millisecond)
			client.call("model/list", map[string]any{})
			assertAppIdleExit(t, client)
		})
	}
}

func TestAppServerIdleCanBeDisabled(t *testing.T) {
	home := t.TempDir()
	configureAppIdle(t, home, "0")
	client := startApp(t, home)
	time.Sleep(650 * time.Millisecond)
	client.call("model/list", map[string]any{})
	if code := client.stop(); code != 0 {
		t.Fatalf("disabled idle timeout exit=%d", code)
	}
}
