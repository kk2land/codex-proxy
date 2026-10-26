package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

// authBurstMessages builds a single-write native burst for the real running
// threads a and b: a 429 for a, unrelated noise, a 429 for b behind enough
// noise that the longer reset itself sits past the proxy's 32-entry event
// queue, and a farther external MCP 429 that must never count.
func authBurstMessages(a, b string, firstReset, secondReset, mcpReset int64) []map[string]any {
	limit := func(thread string, reset int64) map[string]any {
		return map[string]any{
			"method": "error",
			"params": map[string]any{"threadId": thread, "willRetry": false, "error": map[string]any{"message": "rate limited", "codexErrorInfo": "usageLimitExceeded", "resetsAt": reset}},
		}
	}
	noise := func(n int) []map[string]any {
		messages := make([]map[string]any, 0, n)
		for i := 0; i < n; i++ {
			messages = append(messages, map[string]any{
				"method": "item/agentMessage/delta",
				"params": map[string]any{"threadId": b, "itemId": "msg-" + fmt.Sprint(i), "delta": "chunk"},
			})
		}
		return messages
	}
	messages := []map[string]any{limit(a, firstReset)}
	messages = append(messages, noise(40)...)
	// Index 41: the longer reset is itself outside the 32-entry queue, so the
	// drain has to keep reading the pipe rather than only the tail noise.
	messages = append(messages, limit(b, secondReset))
	// An external MCP/OAuth 429 may carry a much later reset; it describes a
	// tool endpoint, not the GPT account, so it must never extend the cooldown.
	messages = append(messages, map[string]any{
		"method": "error",
		"params": map[string]any{"threadId": a, "message": "MCP OAuth token endpoint returned HTTP status 429 Too Many Requests", "resetsAt": mcpReset},
	})
	messages = append(messages, noise(20)...)
	return messages
}

// burstPayloadSize returns the exact newline-framed byte count the fixture
// writes in one syscall, asserted below against the pipe capacity.
func burstPayloadSize(t *testing.T, messages []map[string]any) int {
	t.Helper()
	total := 0
	for _, message := range messages {
		line, err := json.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		total += len(line) + 1
	}
	return total
}

// assertAuthReaderReleasable proves the writer path is free: an exclusive
// non-blocking lock on the shared auth lock succeeds only after the proxy
// released (and closed) its reader descriptor.
func assertAuthReaderReleasable(t *testing.T, home string) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(home, "sandbox-user", ".codex", ".codex-proxy-auth.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	locked, err := tryAuthLock(context.Background(), file, syscall.LOCK_EX)
	if err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Fatal("auth reader was not released before rotation")
	}
}

func TestAuthDrainKeepsLongestBurst429AndRotatesOnce(t *testing.T) {
	for _, name := range []string{"ascending", "descending"} {
		t.Run(name, func(t *testing.T) {
			p := authFixture(t, "auth0_alice.json", "auth1_bob.json", "auth2_carol.json")
			home := t.TempDir()
			configureAuthProxy(t, home, p.dir, true)
			client := startApp(t, home)
			a, b := client.thread(), client.thread()
			client.call("turn/start", taskParams(a, "first running task"))
			client.call("turn/start", taskParams(b, "second running task"))
			awaitSlots(t, home, 2)
			now := time.Now().Unix()
			short, long := now+60, now+3600
			firstReset, secondReset := short, long
			if name == "descending" {
				// Reverse arrival order: the maximum must win, not the 429 the
				// protocol loop happened to handle first.
				firstReset, secondReset = long, short
			}
			messages := authBurstMessages(a, b, firstReset, secondReset, now+100000)
			if size := burstPayloadSize(t, messages); size >= 16<<10 {
				t.Fatalf("burst payload %d bytes exceeds the 16KiB bound", size)
			}
			// Two tasks are still running when the burst is written, so the
			// proxy cannot answer this request before it exits.
			client.send("burst", "test/authErrorBurst", map[string]any{"messages": messages})
			if code := client.waitExit(); code != authLimitExitCode {
				t.Fatalf("exit=%d", code)
			}
			awaitSlots(t, home, 0)
			if got := len(startedPIDs(t, client.dir)); got != 1 {
				t.Fatalf("proxy restarted native: %d starts", got)
			}
			if limit := readAuthState(t, home, "auth0_alice.json"); limit.Until != long {
				t.Fatalf("cooldown=%+v want %d (short 429 was %d)", limit, long, short)
			}
			assertAuthReaderReleasable(t, home)
			// The drain releases the reader before a single rotation: a second
			// rotation would skip past auth1 to auth2.
			if base := filepath.Base(mustReadlink(t, filepath.Join(home, "sandbox-user", ".codex", "auth.json"))); base != "auth1_bob.json" {
				t.Fatalf("shutdown rotation used %s", base)
			}
		})
	}
}

// TestAuthDrainLateHintMergesWithoutInventing429 covers a hint that arrives
// after the 429: it may extend the recorded cooldown, but a hint alone may not
// create a 429 or move last_429_at.
func TestAuthDrainLateHintMergesWithoutInventing429(t *testing.T) {
	p := authFixture(t, "auth0_alice.json", "auth1_bob.json", "auth2_carol.json")
	home := t.TempDir()
	configureAuthProxy(t, home, p.dir, true)
	client := startApp(t, home)
	thread := client.thread()
	client.call("turn/start", taskParams(thread, "running task"))
	awaitSlots(t, home, 1)
	now := time.Now().Unix()
	hint := now + 7200
	client.send("burst", "test/authErrorBurst", map[string]any{"messages": []map[string]any{
		{"method": "error", "params": map[string]any{"threadId": thread, "error": map[string]any{"message": "rate limited", "codexErrorInfo": "usageLimitExceeded", "resetsAt": now + 60}}},
		{"method": "account/rateLimits/updated", "params": map[string]any{"rateLimits": map[string]any{"primary": map[string]any{"usedPercent": 100, "resetsAt": hint}}}},
	}})
	if code := client.waitExit(); code != authLimitExitCode {
		t.Fatalf("exit=%d", code)
	}
	limit := readAuthState(t, home, "auth0_alice.json")
	if limit.Until != hint {
		t.Fatalf("cooldown=%+v want merged hint %d", limit, hint)
	}
	if limit.At < now || limit.At > hint {
		t.Fatalf("hint moved last_429_at: %+v", limit)
	}
	if base := filepath.Base(mustReadlink(t, filepath.Join(home, "sandbox-user", ".codex", "auth.json"))); base != "auth1_bob.json" {
		t.Fatalf("shutdown rotation used %s", base)
	}
}

func TestAuthDrainHintAloneDoesNotRecord429(t *testing.T) {
	p := authFixture(t, "auth0_alice.json", "auth1_bob.json")
	home := t.TempDir()
	configureAuthProxy(t, home, p.dir, true)
	client := startApp(t, home)
	thread := client.thread()
	client.call("turn/start", taskParams(thread, "healthy task"))
	awaitSlots(t, home, 1)
	client.call("test/authErrorBurst", map[string]any{"messages": []map[string]any{
		{"method": "account/rateLimits/updated", "params": map[string]any{"rateLimits": map[string]any{"primary": map[string]any{"usedPercent": 100, "resetsAt": time.Now().Unix() + 7200}}}},
	}})
	if limit := readAuthState(t, home, "auth0_alice.json"); limit != (authLimit{}) {
		t.Fatalf("hint invented a 429: %+v", limit)
	}
	client.finish(thread)
	if code := client.stop(); code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if base := filepath.Base(mustReadlink(t, filepath.Join(home, "sandbox-user", ".codex", "auth.json"))); base != "auth0_alice.json" {
		t.Fatalf("hint rotated account: %s", base)
	}
}

// TestAuthDrainBoundedWithInheritedPipe keeps native's stdout write end open
// from an independently grouped background process. The proxy still has to
// finish the drain (deadline/cancel path), persist the longer 429, exit 75, and
// release reader and slot while that holder is demonstrably still alive.
func TestAuthDrainBoundedWithInheritedPipe(t *testing.T) {
	p := authFixture(t, "auth0_alice.json", "auth1_bob.json", "auth2_carol.json")
	home := t.TempDir()
	configureAuthProxy(t, home, p.dir, true)
	client := startApp(t, home)
	thread := client.thread()
	now := time.Now().Unix()
	short, long := now+60, now+3600
	client.call("turn/start", taskParams(thread, "running task"))
	awaitSlots(t, home, 1)
	// The fixture spawns the holder and then emits the burst; its reply only
	// proves the background process and its pid file exist.
	client.call("test/authErrorBurst", map[string]any{"holdOutputPipe": true, "messages": []map[string]any{
		{"method": "error", "params": map[string]any{"threadId": thread, "error": map[string]any{"message": "rate limited", "codexErrorInfo": "usageLimitExceeded", "resetsAt": short}}},
		{"method": "error", "params": map[string]any{"threadId": thread, "error": map[string]any{"message": "rate limited", "codexErrorInfo": "usageLimitExceeded", "resetsAt": long}}},
	}})
	pid := backgroundPID(t, client.dir)
	t.Cleanup(func() {
		if process, err := os.FindProcess(pid); err == nil {
			_ = process.Signal(syscall.SIGKILL)
		}
	})
	if code := client.waitExit(); code != authLimitExitCode {
		t.Fatalf("exit=%d", code)
	}
	if !backgroundAlive(pid) {
		t.Fatal("background holder exited; the bounded path was not exercised")
	}
	if limit := readAuthState(t, home, "auth0_alice.json"); limit.Until != long {
		t.Fatalf("cooldown=%+v want %d", limit, long)
	}
	awaitSlots(t, home, 0)
	assertAuthReaderReleasable(t, home)
	if base := filepath.Base(mustReadlink(t, filepath.Join(home, "sandbox-user", ".codex", "auth.json"))); base != "auth1_bob.json" {
		t.Fatalf("shutdown rotation used %s", base)
	}
}

func backgroundPID(t *testing.T, dir string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "background.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func backgroundAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

// TestAuthDrainHintPreservesLast429At uses a deterministic clock: a real 429 at
// 2000 followed by an exhausted hint at 9000 may extend only cooldown_until.
func TestAuthDrainHintPreservesLast429At(t *testing.T) {
	p := authFixture(t, "auth0_alice.json", "auth1_bob.json")
	if err := p.activate("auth0_alice.json"); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	now := time.Unix(2000, 0)
	p.store.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	s, _ := newNotificationSession(t)
	s.stateStore, s.authPool = p.store, p
	limited := rpcValue(map[string]any{"method": "error", "params": map[string]any{
		"threadId": "thread-1", "error": map[string]any{"message": "rate limited", "codexErrorInfo": "usageLimitExceeded", "resetsAt": 2060},
	}})
	message, err := decodeRPC(limited)
	if err != nil {
		t.Fatal(err)
	}
	s.observeAuthMessage(message)
	if !s.authLimited {
		t.Fatal("429 was not observed")
	}
	s.authRecords.Wait()
	mu.Lock()
	now = time.Unix(2100, 0)
	mu.Unlock()
	hint, err := decodeRPC(rpcValue(map[string]any{"method": "account/rateLimits/updated", "params": map[string]any{
		"rateLimits": map[string]any{"primary": map[string]any{"usedPercent": 100, "resetsAt": 9000}},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	s.observeAuthMessage(hint)
	s.authRecords.Wait()
	state, err := p.store.readState()
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := decodeAccountStates(state)
	if err != nil {
		t.Fatal(err)
	}
	_, limit, err := decodeAccountLimit(accounts["auth0_alice.json"])
	if err != nil {
		t.Fatal(err)
	}
	if limit != (authLimit{At: 2000, Until: 9000}) {
		t.Fatalf("limit=%+v want At=2000 Until=9000", limit)
	}
}

func mustReadlink(t *testing.T, path string) string {
	t.Helper()
	link, err := os.Readlink(path)
	if err != nil {
		t.Fatal(err)
	}
	return link
}
