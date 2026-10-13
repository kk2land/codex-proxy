package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var fakeClaudeOnce sync.Once
var fakeClaudeDir, fakeClaudeBinary string
var fakeClaudeErr error

func fakeClaude(t *testing.T) string {
	t.Helper()
	fakeClaudeOnce.Do(func() {
		fakeClaudeDir, fakeClaudeErr = os.MkdirTemp("", "claude-proxy-native-fixture-")
		if fakeClaudeErr != nil {
			return
		}
		fakeClaudeBinary = filepath.Join(fakeClaudeDir, "claude-native")
		output, err := exec.Command("go", "build", "-o", fakeClaudeBinary, "./testdata/fakeclaude").CombinedOutput()
		if err != nil {
			fakeClaudeErr = fmt.Errorf("build Claude fixture: %w: %s", err, output)
		}
	})
	if fakeClaudeErr != nil {
		t.Fatal(fakeClaudeErr)
	}
	return fakeClaudeBinary
}

func configureClaude(t *testing.T, home string, limit int, timeout string) string {
	t.Helper()
	dir := filepath.Join(home, ".claude-proxy")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf("CLAUDE_PROXY_REAL_BIN=%s\nCLAUDE_PROXY_MAX_CONCURRENCY=%d\nCLAUDE_PROXY_WAIT_TIMEOUT=%s\n", fakeClaude(t), limit, timeout)
	if err := os.WriteFile(filepath.Join(dir, "env.next"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "env.next"), filepath.Join(dir, "env")); err != nil {
		t.Fatal(err)
	}
	return dir
}

func startClaude(t *testing.T, home, mode, dir, input string, extra []string, args ...string) *proxyChild {
	t.Helper()
	env := []string{"CLAUDE_PROXY_TEST_PROCESS=1", "FAKE_CLAUDE_MODE=" + mode, "FAKE_CLAUDE_DIR=" + dir, "CLAUDE_CONFIG_DIR=" + filepath.Join(home, "sandbox-user", ".claude")}
	return startProxyEnv(t, home, "", dir, input, 0, append(env, extra...), args...)
}

var claudeStreamArgs = []string{"--output-format", "stream-json", "--verbose", "--input-format", "stream-json"}

// An open host stream models the SDK's held stdin. Output files permit inspecting
// responses while the query process (and its background work) is still alive.
func startClaudePipe(t *testing.T, home, mode, dir string, extra []string) (*proxyChild, *os.File, string) {
	t.Helper()
	self, _ := os.Executable()
	child := &proxyChild{result: make(chan error, 1)}
	child.cmd = exec.Command(self, append([]string{"-test.run=^TestRunHelperProcess$", "--"}, claudeStreamArgs...)...)
	child.cmd.Env = append(os.Environ(), "CLAUDE_PROXY_TEST_PROCESS=1", "CODEX_PROXY_TEST_HOME="+home, "CODEX_PROXY_TEST_TASK_INTERVAL=0", "CODEX_PROXY_TEST_NO_SANDBOX=1", "FAKE_CLAUDE_MODE="+mode, "FAKE_CLAUDE_DIR="+dir, "CLAUDE_CONFIG_DIR="+filepath.Join(home, "sandbox-user", ".claude"))
	child.cmd.Env = append(child.cmd.Env, extra...)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	diagnostic, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	child.cmd.Stdin, child.cmd.Stdout, child.cmd.Stderr = r, out, diagnostic
	if err := child.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r.Close()
	go func() {
		err := child.cmd.Wait()
		data, _ := os.ReadFile(out.Name())
		stderr, _ := os.ReadFile(diagnostic.Name())
		child.stdout.Write(data)
		child.stderr.Write(stderr)
		child.result <- err
	}()
	t.Cleanup(func() {
		w.Close()
		child.cmd.Process.Kill()
		out.Close()
		diagnostic.Close()
		for _, pid := range startedPIDs(t, dir) {
			var value int
			fmt.Sscan(pid, &value)
			_ = syscall.Kill(-value, syscall.SIGKILL)
		}
	})
	return child, w, out.Name()
}

func awaitClaudeFiles(t *testing.T, dir, pattern string, count int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		paths, _ := filepath.Glob(filepath.Join(dir, pattern))
		if len(paths) == count {
			return paths
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d Claude files matching %s", count, pattern)
	return nil
}

func TestClaudeCommandClassification(t *testing.T) {
	for _, tc := range []struct {
		args []string
		kind claudeCommand
		bad  bool
	}{
		{nil, claudeTask, false},
		{[]string{"-p", "auth"}, claudeTask, false},
		{[]string{"--model", "auth"}, claudeTask, false},
		{[]string{"--model=auth", "-r", "session", "-p", "prompt"}, claudeTask, false},
		{[]string{"--future-option", "auth"}, claudeTask, false},
		{[]string{"auth", "status"}, claudePassThrough, false},
		{[]string{"mcp", "list"}, claudePassThrough, false},
		{[]string{"--version"}, claudePassThrough, false},
		{[]string{"--settings", `{"prompt":"--version"}`}, claudeTask, false},
		{claudeStreamArgs, claudeStream, false},
		{append(append([]string{}, claudeStreamArgs...), "--permission-mode", "plan", "--thinking", "adaptive"), claudeStream, false},
		{append(append([]string{}, claudeStreamArgs...), "-p", "argv prompt"), claudeTask, true},
		{[]string{"-p", "--input-format=stream-json", "--output-format=json"}, claudeTask, true},
		{[]string{"--bg", "prompt"}, claudeTask, true},
		{[]string{"remote-control"}, claudeTask, true},
		{[]string{"daemon", "run"}, claudeTask, true},
		{[]string{"--environment=ccpool_test", "prompt"}, claudeTask, true},
		{[]string{"-p", "--", "--bg"}, claudeTask, false},
		{[]string{"--system-prompt", "remote-control", "-p", "task"}, claudeTask, false},
		{[]string{"agents", "--json"}, claudePassThrough, false},
		{[]string{"agents"}, claudeTask, true},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			got, err := classifyClaudeArgs(tc.args)
			if got != tc.kind || (err != nil) != tc.bad {
				t.Fatalf("kind=%v err=%v", got, err)
			}
		})
	}
}

func TestClaudeConfigAndProviderState(t *testing.T) {
	home := t.TempDir()
	dir := configureClaude(t, home, 2, "1s")
	t.Setenv("CLAUDE_PROXY_MAX_CONCURRENCY", "999")
	self, _ := os.Executable()
	cfg, err := LoadClaudeConfig(dir, self)
	if err != nil || cfg.MaxConcurrency != 2 {
		t.Fatalf("config=%+v err=%v", cfg, err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFilename), []byte(`{"last_codex_task_at":1,"custom":{"a":42}}`), 0600); err != nil {
		t.Fatal(err)
	}
	gate := newClaudeStateStore(dir, time.Millisecond, func() int64 { return 0 })
	permit, err := gate.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := permit.Record(context.Background()); err != nil {
		t.Fatal(err)
	}
	permit.Close()
	state, err := gate.readState()
	stamp, stampErr := state.lastTaskAt(lastClaudeTaskKey)
	if err != nil || stampErr != nil || stamp <= 0 || string(state[lastCodexTaskKey]) != "1" || string(state["custom"]) != `{"a":42}` {
		t.Fatalf("state=%s err=%v %v", state, err, stampErr)
	}
	if err := os.WriteFile(filepath.Join(dir, "env"), []byte("CODEX_PROXY_REAL_BIN="+fakeClaude(t)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadClaudeConfig(dir, self); err == nil {
		t.Fatal("Claude accepted Codex configuration")
	}
}

func TestClaudeOrdinaryTasksAndManagement(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	configureClaude(t, home, 1, "150ms")
	holder := startClaude(t, home, "hold", dir, "", nil, "-p", "private prompt")
	pid := awaitStarts(t, dir, 1)[0]
	waiter := startClaude(t, home, "echo", "", "", nil, "-c", "-p", "other prompt")
	if code := childExit(t, waiter, 5*time.Second); code != 124 {
		t.Fatalf("quota bypassed: code=%d stderr=%s", code, waiter.stderr.String())
	}
	version := startClaude(t, home, "", "", "", nil, "--version")
	if code := childExit(t, version, 5*time.Second); code != 0 || !strings.Contains(version.stdout.String(), "Claude Code") {
		t.Fatalf("management blocked: code=%d stdout=%s stderr=%s", code, version.stdout.String(), version.stderr.String())
	}
	releaseNative(t, dir, pid)
	if code := childExit(t, holder, 5*time.Second); code != 0 {
		t.Fatalf("holder=%d stderr=%s", code, holder.stderr.String())
	}
	args := []string{"-r", "session-1", "-p", "private prompt", "--permission-mode", "plan"}
	echo := startClaude(t, home, "echo", "", "piped context\n", nil, args...)
	if code := childExit(t, echo, 5*time.Second); code != 0 {
		t.Fatalf("echo=%d stderr=%s", code, echo.stderr.String())
	}
	var data struct {
		Args  []string `json:"args"`
		Input string   `json:"stdin"`
	}
	if err := json.Unmarshal(echo.stdout.Bytes(), &data); err != nil || !reflect.DeepEqual(data.Args, args) || data.Input != "piped context\n" {
		t.Fatalf("changed stdio/options: %s err=%v", echo.stdout.String(), err)
	}
	log, _ := os.ReadFile(filepath.Join(home, ".claude-proxy", "claude-proxy.log"))
	if bytes.Contains(log, []byte("private prompt")) || bytes.Contains(log, []byte("piped context")) {
		t.Fatal("prompt content entered lifecycle logs")
	}
}

func TestClaudeStreamControlsBypassWaitingAndEOFDrains(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	configureClaude(t, home, 1, "5s")
	holder := startClaude(t, home, "hold", dir, "", nil, "-p", "hold")
	pid := awaitStarts(t, dir, 1)[0]
	stream, input, output := startClaudePipe(t, home, "stream", dir, nil)
	fmt.Fprintln(input, `{"type":"control_request","request_id":"init","request":{"subtype":"initialize"}}`)
	fmt.Fprintln(input, `{"type":"user","message":{"role":"user","content":"private SDK prompt"},"parent_tool_use_id":null}`)
	fmt.Fprintln(input, `{"type":"control_response","response":{"subtype":"success","request_id":"hook"}}`)
	awaitClaudeFiles(t, dir, "*-initialize.json", 1)
	awaitClaudeFiles(t, dir, "*-control_response.json", 1)
	paths, _ := filepath.Glob(filepath.Join(dir, "*-user-*.json"))
	if len(paths) != 0 {
		t.Fatal("model input bypassed occupied slot")
	}
	awaitClaudeResponse(t, output, "init")
	input.Close() // Must half-close only after the queued user message is sent.
	releaseNative(t, dir, pid)
	if code := childExit(t, holder, 5*time.Second); code != 0 {
		t.Fatalf("holder=%d", code)
	}
	if code := childExit(t, stream, 5*time.Second); code != 0 || !bytes.Contains(stream.stdout.Bytes(), []byte(`"type":"result"`)) {
		t.Fatalf("EOF lost queued task: code=%d stdout=%s stderr=%s", code, stream.stdout.String(), stream.stderr.String())
	}
	awaitClaudeFiles(t, dir, "*-user-1.json", 1)
	for _, line := range bytes.Split(bytes.TrimSpace(stream.stdout.Bytes()), []byte{'\n'}) {
		if !json.Valid(line) {
			t.Fatalf("stdout contaminated: %s", line)
		}
	}
	log, _ := os.ReadFile(filepath.Join(home, ".claude-proxy", "claude-proxy.log"))
	if bytes.Contains(log, []byte("private SDK prompt")) {
		t.Fatal("stream input entered lifecycle logs")
	}
}

func TestClaudeResultRetainsProcessSlotAndPermissionResponses(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	configureClaude(t, home, 1, "150ms")
	stream, input, output := startClaudePipe(t, home, "stream", dir, []string{"FAKE_CLAUDE_PERMISSION=1"})
	fmt.Fprintln(input, `{"type":"user","message":{"role":"user","content":"permission task"}}`)
	awaitClaudeFiles(t, dir, "*-user-1.json", 1)
	fmt.Fprintln(input, `{"type":"control_response","response":{"subtype":"success","request_id":"tool-permission","response":{"behavior":"allow","updatedInput":{}}}}`)
	awaitClaudeFiles(t, dir, "*-control_response.json", 1)
	awaitClaudeOutput(t, output, func(message rpcMessage) bool { return rpcString(message, "type") == "result" })
	waiter := startClaude(t, home, "echo", "", "", nil, "-p", "another query")
	if code := childExit(t, waiter, 5*time.Second); code != 124 {
		t.Fatalf("result prematurely released slot: code=%d stderr=%s", code, waiter.stderr.String())
	}
	input.Close()
	if code := childExit(t, stream, 5*time.Second); code != 0 {
		t.Fatalf("stream=%d stderr=%s", code, stream.stderr.String())
	}
	final := startClaude(t, home, "echo", "", "", nil, "-p", "after exit")
	if code := childExit(t, final, 5*time.Second); code != 0 {
		t.Fatalf("slot leaked after CLI exit: %d %s", code, final.stderr.String())
	}
}

func TestClaudeWaitingInterruptDropsUnsubmittedInput(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	configureClaude(t, home, 1, "5s")
	holder := startClaude(t, home, "hold", dir, "", nil, "-p", "hold")
	pid := awaitStarts(t, dir, 1)[0]
	stream, input, _ := startClaudePipe(t, home, "stream", dir, nil)
	fmt.Fprintln(input, `{"type":"user","message":{"role":"user","content":"never submit"}}`)
	fmt.Fprintln(input, `{"type":"control_request","request_id":"cancel","request":{"subtype":"interrupt"}}`)
	awaitClaudeFiles(t, dir, "*-interrupt.json", 1)
	input.Close()
	if code := childExit(t, stream, 5*time.Second); code != 0 {
		t.Fatalf("interrupt stuck waiting: %d %s", code, stream.stderr.String())
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "*-user-*.json"))
	if len(paths) != 0 {
		t.Fatal("canceled input reached native CLI")
	}
	releaseNative(t, dir, pid)
	childExit(t, holder, 5*time.Second)
}

func TestClaudeStreamRepeatedInputsUseOneSlotAndInterval(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	configureClaude(t, home, 1, "1s")
	stream, input, _ := startClaudePipe(t, home, "stream", dir, []string{"CODEX_PROXY_TEST_TASK_INTERVAL=2"})
	fmt.Fprintln(input, `{"type":"user","message":{"role":"user","content":"one"}}`)
	first := awaitClaudeFiles(t, dir, "*-user-1.json", 1)[0]
	fmt.Fprintln(input, `{"type":"user","message":{"role":"user","content":"two"}}`)
	fmt.Fprintln(input, `{"type":"control_request","request_id":"during-sleep","request":{"subtype":"set_permission_mode","mode":"plan"}}`)
	awaitClaudeFiles(t, dir, "*-set_permission_mode.json", 1)
	second := awaitClaudeFiles(t, dir, "*-user-2.json", 1)[0]
	a, _ := os.Stat(first)
	b, _ := os.Stat(second)
	if b.ModTime().Sub(a.ModTime()) < time.Second {
		t.Fatal("repeated input skipped shared interval")
	}
	input.Close()
	if code := childExit(t, stream, 5*time.Second); code != 0 {
		t.Fatalf("same-process input requested a second slot: %d %s", code, stream.stderr.String())
	}
}

func TestClaudeProviderQuotasAreIndependent(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	configureProxy(t, home, fakeCodex(t), 1, "2s")
	configureClaude(t, home, 1, "150ms")
	codex := startProxy(t, home, "hold", dir, "", 0, "exec", "hold")
	pid := awaitStarts(t, dir, 1)[0]
	claude := startClaude(t, home, "echo", "", "", nil, "-p", "independent")
	if code := childExit(t, claude, 5*time.Second); code != 0 {
		t.Fatalf("Claude consumed Codex quota: %d %s", code, claude.stderr.String())
	}
	releaseNative(t, dir, pid)
	childExit(t, codex, 5*time.Second)
}

func TestClaudeUnknownControlsRequireAdmission(t *testing.T) {
	for _, subtype := range []string{"initialize", "interrupt", "mcp_message", "set_permission_mode", "stop_task"} {
		if claudeInputLimited(rpcMessage{"type": rpcValue("control_request"), "request": rpcValue(map[string]any{"subtype": subtype})}) {
			t.Fatalf("coordination blocked: %s", subtype)
		}
	}
	if !claudeInputLimited(rpcMessage{"type": rpcValue("control_request"), "request": rpcValue(map[string]any{"subtype": "future_model_request"})}) {
		t.Fatal("future model operation bypassed admission")
	}
}

func TestClaudeStreamTimeoutAndSignalCancel(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	configureClaude(t, home, 1, "150ms")
	holder := startClaude(t, home, "hold", dir, "", nil, "-p", "hold")
	pid := awaitStarts(t, dir, 1)[0]
	stream := startClaude(t, home, "stream", dir, `{"type":"user","message":{"role":"user","content":"timeout"}}`+"\n", nil, claudeStreamArgs...)
	if code := childExit(t, stream, 5*time.Second); code != 124 {
		t.Fatalf("stream timeout=%d stderr=%s", code, stream.stderr.String())
	}
	if err := holder.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := childExit(t, holder, 5*time.Second); code != 143 {
		t.Fatalf("signal exit=%d stderr=%s", code, holder.stderr.String())
	}
	// The native fixture closed its inherited slot FD; normal reaping must
	// still free the guard's reservation without requiring file deletion.
	releaseNative(t, dir, pid)
	final := startClaude(t, home, "echo", "", "", nil, "-p", "after signal")
	if code := childExit(t, final, 5*time.Second); code != 0 {
		t.Fatalf("signal leaked slot: %d %s", code, final.stderr.String())
	}
}

func TestClaudeCorruptStateRejectsInput(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	proxyDir := configureClaude(t, home, 1, "1s")
	os.WriteFile(filepath.Join(proxyDir, stateFilename), []byte(`{"last_claude_task_at":"invalid"}`), 0600)
	child := startClaude(t, home, "stream", dir, `{"type":"user","message":{"role":"user","content":"do not run"}}`+"\n", nil, claudeStreamArgs...)
	if code := childExit(t, child, 5*time.Second); code != 2 {
		t.Fatalf("corrupt state=%d stderr=%s", code, child.stderr.String())
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "*-user-*.json"))
	if len(paths) != 0 {
		t.Fatal("input passed corrupt timestamp state")
	}
	self, _ := os.Executable()
	limiter := Limiter{Dir: proxyDir, Load: func() (Config, error) { return LoadClaudeConfig(proxyDir, self) }}
	lease, _, err := limiter.Acquire(context.Background())
	if err != nil {
		t.Fatal("failed admission leaked its slot:", err)
	}
	lease.Close()
}

func TestClaudeTaskSlotGuardDoesNotDependOnInheritedFD(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	proxyDir := configureClaude(t, home, 1, "150ms")
	holder := startClaude(t, home, "hold", dir, "", []string{"FAKE_CLAUDE_DROP_SLOT_FD=1"}, "-p", "hold")
	awaitStarts(t, dir, 1)
	// Reap the proxy after SIGKILL: it can no longer keep its descriptor alive.
	// Native Claude has already dropped its inherited FD. Only the guard can
	// retain the reservation and stop the native process on pipe closure.
	var guardPID int
	deadline := time.Now().Add(5 * time.Second)
	for guardPID == 0 && time.Now().Before(deadline) {
		data, _ := os.ReadFile(filepath.Join(proxyDir, "claude-proxy.log"))
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			var event struct {
				GuardPID int `json:"guard_pid"`
			}
			if json.Unmarshal(line, &event) == nil && event.GuardPID > 0 {
				guardPID = event.GuardPID
			}
		}
		time.Sleep(time.Millisecond)
	}
	if guardPID == 0 {
		t.Fatal("missing task guard")
	}
	t.Cleanup(func() { _ = syscall.Kill(guardPID, syscall.SIGKILL) })
	if err := holder.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	childExit(t, holder, 5*time.Second)
	self, _ := os.Executable()
	limiter := Limiter{Dir: proxyDir, Load: func() (Config, error) { return LoadClaudeConfig(proxyDir, self) }, PollInterval: time.Millisecond}
	lease, _, err := limiter.Acquire(context.Background())
	if lease != nil {
		lease.Close()
	}
	if !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("guard lost slot after native FD close: %v", err)
	}
	awaitClaudeFiles(t, dir, "*.terminated", 1)
}

// Optional real SDK transport test. It never calls Anthropic: native Claude is
// replaced by the Go fixture, while the actual SDK drives the proxy unchanged.
func TestClaudeAgentSDKCompatibility(t *testing.T) {
	module := os.Getenv("CLAUDE_PROXY_TEST_SDK_MODULE")
	if module == "" {
		t.Skip("set CLAUDE_PROXY_TEST_SDK_MODULE to an installed SDK's sdk.mjs")
	}
	home, dir := t.TempDir(), t.TempDir()
	configureClaude(t, home, 1, "5s")
	self, _ := os.Executable()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "./testdata/sdk_compat.mjs")
	cmd.Env = append(os.Environ(), "CLAUDE_PROXY_SDK_TEST_PROCESS=1", "CODEX_PROXY_TEST_HOME="+home, "CODEX_PROXY_TEST_TASK_INTERVAL=0", "CODEX_PROXY_TEST_NO_SANDBOX=1", "CLAUDE_PROXY_TEST_BINARY="+self, "CLAUDE_CONFIG_DIR="+filepath.Join(home, "sandbox-user", ".claude"), "FAKE_CLAUDE_MODE=stream", "FAKE_CLAUDE_DIR="+dir, "FAKE_CLAUDE_PERMISSION=1")
	output, err := cmd.CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte(`"results":2`)) {
		t.Fatalf("SDK compatibility: %v\n%s", err, output)
	}
	awaitClaudeFiles(t, dir, "*-initialize.json", 1)
	awaitClaudeFiles(t, dir, "*-set_permission_mode.json", 1)
	awaitClaudeFiles(t, dir, "*-set_model.json", 1)
	awaitClaudeFiles(t, dir, "*-interrupt.json", 1)
}
