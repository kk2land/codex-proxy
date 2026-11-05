package proxy

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Existing deployments may still carry the removed setting. It must neither
// select credentials nor reject a native launch, even with an empty directory.
func legacyAccountConfig(t *testing.T, home string) string {
	t.Helper()
	path := filepath.Join(home, ".codex-proxy", "env")
	config, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	config = append(config, []byte("CODEX_PROXY_AUTH_DIR="+strconv.Quote(t.TempDir())+"\n")...)
	if err := os.WriteFile(path, config, 0600); err != nil {
		t.Fatal(err)
	}
	login := filepath.Join(home, "sandbox-user", ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(login), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(login, []byte(`{"OPENAI_API_KEY":"native-fixture"}`), 0600); err != nil {
		t.Fatal(err)
	}
	return login
}

func assertNativeLoginUntouched(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("native login was replaced: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != `{"OPENAI_API_KEY":"native-fixture"}` {
		t.Fatalf("native login was modified: %s: %v", data, err)
	}
}

func TestRunPreservesNativeQuotaOutputAndExit(t *testing.T) {
	for _, exit := range []int{0, 1, 37} {
		t.Run(fmt.Sprint(exit), func(t *testing.T) {
			home := t.TempDir()
			configureProxy(t, home, fakeCodex(t), 1, "5s")
			login := legacyAccountConfig(t, home)
			stdout := "{\"type\":\"turn.failed\",\"error\":{\"message\":\"You’ve hit your usage limit.\"}}\n"
			stderr := "\x1b[31mERROR:\x1b[0m HTTP status 429 Too Many Requests\n"
			child := startProxyEnv(t, home, "error-output", t.TempDir(), "", exit,
				[]string{"FAKE_CODEX_STDOUT=" + stdout, "FAKE_CODEX_STDERR=" + stderr}, "exec", "test")
			if code := childExit(t, child, 5*time.Second); code != exit {
				t.Fatalf("native exit=%d proxy exit=%d stderr=%s", exit, code, child.stderr.String())
			}
			if child.stdout.String() != stdout || child.stderr.String() != stderr {
				t.Fatalf("native output changed: stdout=%q stderr=%q", child.stdout.String(), child.stderr.String())
			}
			assertNativeLoginUntouched(t, login)
		})
	}
}

func TestAppServerPassesAccountRPCsAndSurvives429(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 2, "5s")
	login := legacyAccountConfig(t, home)
	client := startApp(t, home)
	client.call("account/login/start", map[string]any{"type": "apiKey", "apiKey": "native-fixture"})
	client.call("account/logout", map[string]any{})
	thread := client.thread()
	client.call("test/rejectNext429", map[string]any{})
	client.send("native-429", "turn/start", taskParams(thread, "native rejects this task"))
	response := client.response("native-429")
	if rpcText(rpcObject(response, "error")["code"]) != "429" || rpcText(rpcObject(rpcObject(response, "error"), "data")["retry_after"]) != "120" {
		t.Fatalf("native error changed: %s", rpcValue(response))
	}
	client.call("turn/start", taskParams(thread, "same connection remains usable"))
	client.call("test/notify", map[string]any{"method": "error", "params": map[string]any{
		"threadId": thread, "willRetry": false,
		"error": map[string]any{"message": "usage limit reached", "codexErrorInfo": "usageLimitExceeded"},
	}})
	client.await(func(message rpcMessage) bool { return rpcString(message, "method") == "error" })
	client.finish(thread)
	client.call("turn/start", taskParams(thread, "native notification leaves the connection open"))
	client.finish(thread)
	if code := client.stop(); code != 0 {
		t.Fatalf("proxy exited after a native quota error: %d", code)
	}
	assertNativeLoginUntouched(t, login)
}
