package proxy

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func configureAppProxy(t *testing.T, home, binary string, limit int, timeout string) {
	t.Helper()
	configureProxy(t, home, binary, limit, timeout, true)
}

type appClient struct {
	t         *testing.T
	cmd       *exec.Cmd
	input     io.WriteCloser
	messages  chan rpcMessage
	stash     []rpcMessage
	result    chan error
	stderr    *os.File
	home, dir string
	sequence  int
}

func startApp(t *testing.T, home string, args ...string) *appClient {
	return startAppEnv(t, home, nil, args...)
}

func startAppEnv(t *testing.T, home string, extraEnv []string, args ...string) *appClient {
	t.Helper()
	return startAppInitialize(t, home, extraEnv, map[string]any{"clientInfo": map[string]any{"name": "proxy-test", "version": "1"}, "capabilities": map[string]bool{"experimentalApi": false}}, args...)
}

func startAppInitialize(t *testing.T, home string, extraEnv []string, initParams any, args ...string) *appClient {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if len(args) == 0 {
		args = []string{"app-server"}
	}
	cmd := exec.Command(self, append([]string{"-test.run=^TestRunHelperProcess$", "--"}, args...)...)
	codexHome := filepath.Join(home, "sandbox-user", ".codex")
	if err := os.MkdirAll(codexHome, 0700); err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), "CODEX_PROXY_TEST_PROCESS=1", "CODEX_PROXY_TEST_HOME="+home,
		"FAKE_CODEX_MODE=app-server", "FAKE_CODEX_DIR="+dir,
		"HOME=/changed/runtime/home", "USER=runtime-user", "CODEX_HOME="+codexHome,
		"CODEX_PROXY_MAX_CONCURRENCY=999", "CODEX_PROXY_WAIT_TIMEOUT=0")
	cmd.Env = append(cmd.Env, extraEnv...)
	if !platformSandboxAvailable() {
		cmd.Env = append(cmd.Env, "CODEX_PROXY_TEST_NO_SANDBOX=1")
	}
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.Create(filepath.Join(dir, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	client := &appClient{t: t, cmd: cmd, input: input, messages: make(chan rpcMessage, 512), result: make(chan error, 1), stderr: stderr, home: home, dir: dir}
	go func() {
		defer close(client.messages)
		scanner := appScanner(output)
		for scanner.Scan() {
			message, err := decodeRPC(scanner.Bytes())
			if err == nil {
				client.messages <- message
			}
		}
	}()
	go func() { client.result <- cmd.Wait() }()
	t.Cleanup(func() {
		input.Close()
		_ = cmd.Process.Kill()
		stderr.Close()
		for _, pid := range startedPIDs(t, dir) {
			n, _ := strconv.Atoi(pid)
			_ = syscall.Kill(-n, syscall.SIGKILL)
		}
	})
	client.call("initialize", initParams)
	return client
}

func (c *appClient) send(id any, method string, params any) {
	c.t.Helper()
	if err := writeRPC(c.input, rpcMessage{"id": rpcValue(id), "method": rpcValue(method), "params": rpcValue(params)}); err != nil {
		c.t.Fatal(err)
	}
}

func (c *appClient) await(match func(rpcMessage) bool) rpcMessage {
	c.t.Helper()
	for i, message := range c.stash {
		if match(message) {
			c.stash = append(c.stash[:i], c.stash[i+1:]...)
			return message
		}
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case message, ok := <-c.messages:
			if !ok {
				data, _ := os.ReadFile(c.stderr.Name())
				c.t.Fatalf("app-server stdout closed: %s", data)
			}
			if match(message) {
				return message
			}
			c.stash = append(c.stash, message)
		case <-timer.C:
			data, _ := os.ReadFile(c.stderr.Name())
			c.t.Fatalf("app-server response timed out: %s", data)
		}
	}
}

func (c *appClient) response(id any) rpcMessage {
	c.t.Helper()
	raw := string(rpcValue(id))
	return c.await(func(message rpcMessage) bool {
		return string(message["id"]) == raw && rpcString(message, "method") == ""
	})
}

func (c *appClient) call(method string, params any) rpcMessage {
	c.t.Helper()
	c.sequence++
	id := fmt.Sprintf("client-%d", c.sequence)
	c.send(id, method, params)
	message := c.response(id)
	if message["error"] != nil {
		c.t.Fatalf("%s failed: %s", method, message["error"])
	}
	return rpcObject(message, "result")
}

func (c *appClient) thread() string {
	c.t.Helper()
	result := c.call("thread/start", map[string]any{"sandbox": "read-only", "approvalPolicy": "on-request"})
	if rpcString(rpcObject(result, "sandbox"), "type") != "externalSandbox" {
		c.t.Fatalf("sandbox not applied: %v", result)
	}
	return rpcString(rpcObject(result, "thread"), "id")
}

func taskParams(thread, prompt string) map[string]any {
	return map[string]any{"threadId": thread, "input": []any{map[string]any{"type": "text", "text": prompt}}, "sandboxPolicy": map[string]any{"type": "readOnly"}}
}

func (c *appClient) finish(thread string) {
	c.t.Helper()
	c.call("test/finish", map[string]any{"threadId": thread})
}

func (c *appClient) stop() int {
	c.t.Helper()
	c.input.Close()
	return c.waitExit()
}

func (c *appClient) waitExit() int {
	c.t.Helper()
	select {
	case err := <-c.result:
		if err == nil {
			return 0
		}
		if exited, ok := err.(*exec.ExitError); ok {
			return exited.ExitCode()
		}
		c.t.Fatal(err)
	case <-time.After(5 * time.Second):
		c.t.Fatal("app-server did not stop")
	}
	return -1
}

func awaitSlots(t *testing.T, home string, count int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		paths, err := filepath.Glob(filepath.Join(home, ".codex-proxy", "locks", "slot-*.lock"))
		if err != nil {
			t.Fatal(err)
		}
		occupied := 0
		for _, path := range paths {
			file, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			locked, err := tryLock(context.Background(), file)
			file.Close()
			if err != nil {
				t.Fatal(err)
			}
			if !locked {
				occupied++
			}
		}
		if occupied == count {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("slot occupancy did not reach %d", count)
}

func awaitLog(t *testing.T, home, event string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(filepath.Join(home, ".codex-proxy", appServerLogFilename))
		if strings.Contains(string(data), `"event":"`+event+`"`) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("missing app-server %s event", event)
}

func TestAppServerEnablementReloadAffectsNewConnections(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	client := startApp(t, home)
	thread := client.thread()
	client.call("turn/start", taskParams(thread, "active task"))
	awaitSlots(t, home, 1)

	configureProxy(t, home, fakeCodex(t), 1, "5s", false)
	dir := t.TempDir()
	refused := startProxy(t, home, "app-server", dir, "", 0, "app-server")
	if code := childExit(t, refused, 5*time.Second); code != 1 {
		t.Fatalf("new connection exit=%d stderr=%s", code, refused.stderr.String())
	}
	if len(startedPIDs(t, dir)) != 0 || refused.stdout.Len() != 0 {
		t.Fatal("disabled switch started a new connection")
	}
	awaitSlots(t, home, 1)
	client.finish(thread)
	awaitSlots(t, home, 0)
	// Existing connections continue and still acquire quota for each new task.
	client.call("turn/start", taskParams(thread, "next task on existing connection"))
	awaitSlots(t, home, 1)
	client.finish(thread)
	awaitSlots(t, home, 0)

	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	newClient := startApp(t, home)
	if code := newClient.stop(); code != 0 {
		t.Fatalf("reenabled connection exit=%d", code)
	}
	if code := client.stop(); code != 0 {
		t.Fatalf("existing connection exit=%d", code)
	}
}

func TestAppServerSharedQuotaAndHotReload(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	execTask := startProxy(t, home, "hold", dir, "", 0, "exec")
	pid := awaitStarts(t, dir, 1)[0]
	client := startApp(t, home)
	thread := client.thread()
	awaitSlots(t, home, 1)
	client.send(42, "turn/start", taskParams(thread, "private task prompt"))
	awaitLog(t, home, "waiting")
	client.call("model/list", map[string]any{})
	client.call("thread/shellCommand", map[string]any{"threadId": thread, "command": "true"})
	awaitSlots(t, home, 1)
	configureAppProxy(t, home, fakeCodex(t), 2, "5s")
	response := client.response(42)
	if response["error"] != nil {
		t.Fatalf("admission failed: %s", response["error"])
	}
	awaitSlots(t, home, 2)
	// Supplementing the active turn retains its slot even after lowering quota.
	configureAppProxy(t, home, fakeCodex(t), 1, "150ms")
	supplement := client.call("turn/start", taskParams(thread, "private supplement"))
	if rpcString(rpcObject(supplement, "turn"), "id") != rpcString(rpcObject(rpcObject(response, "result"), "turn"), "id") {
		t.Fatal("supplement created a new task")
	}
	other := client.thread()
	client.send("timeout", "turn/start", taskParams(other, "timeout prompt"))
	if rpcObject(client.response("timeout"), "error")["code"] == nil {
		t.Fatal("waiting task did not time out")
	}
	client.finish(thread)
	awaitSlots(t, home, 1)
	releaseNative(t, dir, pid)
	if code := childExit(t, execTask, 5*time.Second); code != 0 {
		t.Fatalf("exec exit=%d", code)
	}
	awaitSlots(t, home, 0)
	client.call("turn/start", taskParams(other, "fresh task"))
	awaitSlots(t, home, 1)
	client.finish(other)
	awaitSlots(t, home, 0)
	if code := client.stop(); code != 0 {
		t.Fatalf("EOF exit=%d", code)
	}
}

func TestAppServerCancellationQueueAndFreshTimeout(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	client := startApp(t, home)
	first, second := client.thread(), client.thread()
	firstResult := client.call("turn/start", taskParams(first, "first task"))
	firstTurn := rpcString(rpcObject(firstResult, "turn"), "id")
	client.send("cancel-me", "turn/start", taskParams(second, "cancel task"))
	awaitLog(t, home, "waiting")
	client.call("turn/interrupt", map[string]any{"threadId": second, "turnId": "pending"})
	if rpcObject(client.response("cancel-me"), "error")["code"] == nil {
		t.Fatal("pending task not canceled")
	}
	client.send("cancel-by-id", "turn/start", taskParams(second, "cancel by request id"))
	client.call("model/list", map[string]any{})
	client.send(nil, "$/cancelRequest", map[string]any{"id": "cancel-by-id"})
	if string(rpcObject(client.response("cancel-by-id"), "error")["code"]) != "-32800" {
		t.Fatal("request cancellation did not cancel slot admission")
	}
	// Queuing, updating, reordering and listing do not reserve another slot.
	q1 := client.call("thread/queue/add", map[string]any{"threadId": first, "clientUserMessageId": "message-1", "input": taskParams(first, "queued secret")["input"]})
	q2 := client.call("thread/queue/add", map[string]any{"threadId": first, "clientUserMessageId": "message-2", "input": taskParams(first, "other queued secret")["input"]})
	id1, id2 := rpcString(rpcObject(q1, "queuedSubmission"), "id"), rpcString(rpcObject(q2, "queuedSubmission"), "id")
	client.call("thread/queue/update", map[string]any{"threadId": first, "queuedSubmissionId": id2, "input": taskParams(first, "updated secret")["input"]})
	client.call("thread/queue/reorder", map[string]any{"threadId": first, "queuedSubmissionIds": []string{id2, id1}})
	list := client.call("thread/queue/list", map[string]any{"threadId": first, "limit": 1})
	if rpcString(list, "nextCursor") != "1" {
		t.Fatalf("bad queue page: %v", list)
	}
	awaitSlots(t, home, 1)
	// A new timeout value is used at the next queued execution.
	configureAppProxy(t, home, fakeCodex(t), 2, "5s")
	dir := filepath.Join(home, ".codex-proxy")
	self, _ := os.Executable()
	limiter := Limiter{Dir: dir, Load: func() (Config, error) { return LoadConfig(dir, self) }}
	held, _, err := limiter.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	configureAppProxy(t, home, fakeCodex(t), 1, "120ms")
	client.finish(first)
	client.await(func(m rpcMessage) bool { return rpcString(m, "method") == "error" })
	awaitSlots(t, home, 1)
	held.Close()
	awaitSlots(t, home, 0)
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	queueResult := client.call("thread/queue/start", map[string]any{"threadId": first, "queuedSubmissionId": id2})
	awaitSlots(t, home, 1)
	queueTurn := rpcString(rpcObject(queueResult, "turn"), "id")
	client.finish(first)
	client.await(func(m rpcMessage) bool {
		turnID := rpcString(rpcObject(rpcObject(m, "params"), "turn"), "id")
		return rpcString(m, "method") == "turn/started" && rpcString(rpcObject(m, "params"), "threadId") == first && turnID != firstTurn && turnID != queueTurn
	})
	client.call("thread/queue/list", map[string]any{"threadId": first})
	client.finish(first)
	awaitSlots(t, home, 0)
	client.stop()
}

func TestAppServerTaskKindsGoalAndDetachedReview(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 2, "5s")
	client := startApp(t, home)
	thread := client.thread()
	turnResult := client.call("turn/start", taskParams(thread, "prediction source"))
	sourceTurn := rpcString(rpcObject(turnResult, "turn"), "id")
	client.finish(thread)
	awaitSlots(t, home, 0)
	for _, method := range []string{"review/start", "thread/compact/start", "thread/approveGuardianDeniedAction", "thread/realtime/start"} {
		client.call(method, map[string]any{"threadId": thread, "target": map[string]any{"type": "uncommittedChanges"}, "event": map[string]any{}, "outputModality": "text", "version": "v2"})
		awaitSlots(t, home, 1)
		if method == "thread/realtime/start" {
			client.call("thread/realtime/stop", map[string]any{"threadId": thread})
		} else {
			client.finish(thread)
		}
		awaitSlots(t, home, 0)
	}
	// Prediction uses the real request/notification schema and releases.
	client.call("thread/prediction/request", map[string]any{"threadId": thread, "sourceTurnId": sourceTurn})
	updated := client.await(func(m rpcMessage) bool { return rpcString(m, "method") == "thread/prediction/updated" })
	params := rpcObject(updated, "params")
	if rpcString(params, "sourceTurnId") != sourceTurn || rpcString(rpcObject(params, "result"), "type") != "completed" {
		t.Fatalf("prediction notification schema: %v", params)
	}
	awaitSlots(t, home, 0)
	client.call("thread/goal/set", map[string]any{"threadId": thread, "objective": "private goal"})
	awaitSlots(t, home, 1)
	client.finish(thread)
	awaitSlots(t, home, 1)
	client.call("thread/goal/set", map[string]any{"threadId": thread, "status": "paused"})
	awaitSlots(t, home, 1)
	client.finish(thread)
	awaitSlots(t, home, 0)
	client.call("turn/start", taskParams(thread, "parent prompt"))
	review := client.call("review/start", map[string]any{"threadId": thread, "delivery": "detached", "target": map[string]any{"type": "uncommittedChanges"}})
	awaitSlots(t, home, 2)
	child := rpcString(review, "reviewThreadId")
	client.finish(child)
	awaitSlots(t, home, 1)
	client.finish(thread)
	awaitSlots(t, home, 0)
	client.stop()
}

func TestAppServerPersistedGoalAndPendingGoalPause(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	client := startApp(t, home)
	source, pendingThread := client.thread(), client.thread()
	client.call("test/persistGoal", map[string]any{"threadId": source})
	fork := client.call("thread/fork", map[string]any{"threadId": source})
	thread := rpcString(rpcObject(fork, "thread"), "id")
	awaitSlots(t, home, 0)
	client.call("turn/start", taskParams(thread, "fork turn"))
	client.finish(thread)
	awaitSlots(t, home, 1)
	client.send("waiting-goal", "thread/goal/set", map[string]any{"threadId": pendingThread, "objective": "pending goal"})
	awaitLog(t, home, "waiting")
	client.call("thread/goal/set", map[string]any{"threadId": pendingThread, "status": "paused"})
	if rpcObject(client.response("waiting-goal"), "error")["code"] == nil {
		t.Fatal("pausing a pending goal did not cancel admission")
	}
	client.call("thread/goal/clear", map[string]any{"threadId": thread})
	client.finish(thread)
	awaitSlots(t, home, 0)
	client.stop()
}

func TestAppServerConcurrentDetachedReviewsAndFork(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 2, "5s")
	client := startApp(t, home)
	source := client.thread()
	client.call("test/delayReviews", map[string]any{})
	params := map[string]any{"threadId": source, "delivery": "detached", "target": map[string]any{"type": "uncommittedChanges"}}
	client.send("first-review", "review/start", params)
	client.await(func(m rpcMessage) bool {
		return rpcString(m, "method") == "thread/started" && rpcString(rpcObject(rpcObject(m, "params"), "thread"), "forkedFromId") == source
	})
	// An ordinary fork must not be mistaken for the review's destination.
	fork := client.call("thread/fork", map[string]any{"threadId": source})
	forkID := rpcString(rpcObject(fork, "thread"), "id")
	client.send("second-review", "review/start", params)
	first := rpcString(rpcObject(client.response("first-review"), "result"), "reviewThreadId")
	second := rpcString(rpcObject(client.response("second-review"), "result"), "reviewThreadId")
	if first == second || first == forkID || second == forkID {
		t.Fatal("detached review association was lost")
	}
	awaitSlots(t, home, 2)
	client.finish(first)
	awaitSlots(t, home, 1)
	client.finish(second)
	awaitSlots(t, home, 0)
	client.call("turn/start", taskParams(forkID, "fork task"))
	client.finish(forkID)
	awaitSlots(t, home, 0)
	client.stop()
}

func TestAppServerProtocolPolicyAndLogPrivacy(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	client := startApp(t, home, "-c", "developer_instructions=private-startup-prompt", "app-server", "--listen", "stdio://")
	thread := client.thread()
	launch := client.call("test/launch", map[string]any{})
	if rpcString(launch, "home") != "/changed/runtime/home" || rpcString(launch, "user") != "runtime-user" {
		t.Fatalf("environment lost: %v", launch)
	}
	cwd, _ := os.Getwd()
	if rpcString(launch, "cwd") != cwd {
		t.Fatal("working directory lost")
	}
	client.call("turn/start", taskParams(thread, "private-task-prompt"))
	client.send("native-error", "test/error", map[string]any{})
	client.response("native-error")
	client.call("test/stderr", map[string]any{})
	client.call("test/toolRequest", map[string]any{"threadId": thread})
	tool := client.await(func(m rpcMessage) bool { return rpcString(m, "method") == "item/tool/call" })
	if err := writeRPC(client.input, rpcMessage{"id": tool["id"], "result": rpcValue(map[string]any{"content": "private-tool-reply"})}); err != nil {
		t.Fatal(err)
	}
	client.await(func(m rpcMessage) bool { return rpcString(m, "method") == "test/toolReply" })
	client.finish(thread)
	awaitSlots(t, home, 0)
	client.stop()
	capture, _ := os.ReadFile(filepath.Join(client.dir, "rpc.jsonl"))
	if !strings.Contains(string(capture), `"type":"externalSandbox"`) || !strings.Contains(string(capture), `"networkAccess":"enabled"`) {
		t.Fatal("native did not receive external sandbox policy")
	}
	for _, line := range strings.Split(strings.TrimSpace(string(capture)), "\n") {
		m, _ := decodeRPC([]byte(line))
		if rpcString(m, "method") == "turn/start" && rpcString(rpcObject(rpcObject(m, "params"), "sandboxPolicy"), "type") != "externalSandbox" {
			t.Fatal("turn sandbox override bypassed policy")
		}
	}
	paths, _ := filepath.Glob(filepath.Join(home, ".codex-proxy", "*.log*"))
	errorCauseLogged := false
	for _, path := range paths {
		data, _ := os.ReadFile(path)
		if strings.Contains(string(data), "private prompt in error") {
			errorCauseLogged = true
		}
		for _, secret := range []string{"private-startup-prompt", "private-task-prompt", "private prompt in native stderr", "private-tool-reply", "private tool data"} {
			if strings.Contains(string(data), secret) {
				t.Fatalf("%s leaked %s", path, secret)
			}
		}
	}
	// Native error text is recorded for diagnosis; other protocol bodies are
	// still deliberately excluded.
	if !errorCauseLogged {
		t.Fatal("native error cause was not logged")
	}
}

func TestAppServerEOFSignalsAndKilledProxy(t *testing.T) {
	for _, mode := range []string{"eof", "term", "kill-fake", "kill-native", "native-exit"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			configureAppProxy(t, home, fakeCodex(t), 1, "5s")
			client := startApp(t, home)
			thread := client.thread()
			client.call("turn/start", taskParams(thread, "shutdown secret"))
			awaitSlots(t, home, 1)
			pid, _ := strconv.Atoi(awaitStarts(t, client.dir, 1)[0])
			switch mode {
			case "term":
				_ = client.cmd.Process.Signal(syscall.SIGTERM)
			case "kill-fake":
				_ = client.cmd.Process.Kill()
			case "kill-native":
				_ = syscall.Kill(pid, syscall.SIGKILL)
			case "native-exit":
				client.send("exit", "test/exit", map[string]any{})
			}
			var code int
			if mode == "eof" {
				code = client.stop()
			} else {
				code = client.waitExit()
			}
			if mode == "eof" && code != 0 {
				t.Fatalf("EOF exit=%d", code)
			}
			if mode == "native-exit" && code != 39 {
				t.Fatalf("native exit=%d", code)
			}
			awaitSlots(t, home, 0)
			if syscall.Kill(pid, 0) == nil {
				t.Fatalf("native process %d survived %s", pid, mode)
			}
		})
	}
}

func TestAppServerRejectsAlternateListenersAndPassesTooling(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	for _, args := range [][]string{{"app-server", "--listen", "ws://127.0.0.1:9876"}, {"app-server", "daemon"}, {"app-server", "proxy"}} {
		child := startProxy(t, home, "echo", "", "", 0, args...)
		if code := childExit(t, child, 5*time.Second); code != 2 {
			t.Fatalf("accepted bypass: %v", args)
		}
	}
	child := startProxy(t, home, "echo", "", "", 0, "app-server", "generate-json-schema", "--out", "/tmp/schema")
	if code := childExit(t, child, 5*time.Second); code != 0 {
		t.Fatalf("tooling exit=%d", code)
	}
}
