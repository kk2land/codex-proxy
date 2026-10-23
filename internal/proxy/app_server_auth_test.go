package proxy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func assertRPCCode(t *testing.T, response rpcMessage, want int) {
	t.Helper()
	var code int
	if err := json.Unmarshal(rpcObject(response, "error")["code"], &code); err != nil || code != want {
		t.Fatalf("RPC error=%s want code %d", response["error"], want)
	}
}

func TestAuthAppServer429StopsAllThreadsAndCallerRestarts(t *testing.T) {
	p := authFixture(t, "auth0_alice.json", "auth1_bob.json")
	home := t.TempDir()
	configureAuthProxy(t, home, p.dir, true)
	client := startApp(t, home)
	a, b := client.thread(), client.thread()
	client.call("turn/start", taskParams(a, "first running task"))
	client.call("turn/start", taskParams(b, "second running task"))
	awaitSlots(t, home, 2)
	client.send("emit-429", "test/authError", map[string]any{"threadId": a, "resetsAt": time.Now().Unix() + 3600})
	client.await(func(message rpcMessage) bool { return rpcString(message, "method") == "error" })
	if code := client.waitExit(); code != authLimitExitCode {
		t.Fatalf("exit=%d", code)
	}
	awaitSlots(t, home, 0)
	if got := len(startedPIDs(t, client.dir)); got != 1 {
		t.Fatalf("proxy restarted native: %d starts", got)
	}
	if limit := readAuthState(t, home, "auth0_alice.json"); limit.Until < time.Now().Unix()+3500 {
		t.Fatalf("cooldown=%+v", limit)
	}
	link, _ := os.Readlink(filepath.Join(home, "sandbox-user", ".codex", "auth.json"))
	if filepath.Base(link) != "auth1_bob.json" {
		t.Fatalf("rotation at exit failed: %s", link)
	}
	// A new caller-owned process loads the new credentials; no restoration or
	// failed-task replay occurs in the previous proxy.
	next := startApp(t, home)
	thread := next.thread()
	result := next.call("turn/start", taskParams(thread, "caller retry"))
	if filepath.Base(rpcString(result, "authFile")) != "auth1_bob.json" || !rpcBoolTrue(result, "authReadOK") {
		t.Fatalf("credentials=%s", rpcValue(result))
	}
	next.finish(thread)
	if code := next.stop(); code != 0 {
		t.Fatal(code)
	}
}

func TestAuthAppServerRPC429PreservesResponseAndExits(t *testing.T) {
	p := authFixture(t, "auth0_alice.json", "auth1_bob.json")
	home := t.TempDir()
	configureAuthProxy(t, home, p.dir, true)
	client := startApp(t, home)
	thread := client.thread()
	client.call("test/rejectNext429", map[string]any{})
	client.send("original-id", "turn/start", taskParams(thread, "fail"))
	response := client.response("original-id")
	assertRPCCode(t, response, 429)
	if !strings.Contains(string(response["error"]), `"retry_after":120`) {
		t.Fatalf("native details lost: %s", rpcValue(response))
	}
	if code := client.waitExit(); code != authLimitExitCode {
		t.Fatal(code)
	}
	awaitSlots(t, home, 0)
	if len(startedPIDs(t, client.dir)) != 1 {
		t.Fatal("native was restarted")
	}
}

func TestAuthAppServerExhaustedAndEmptyPoolRejectImmediately(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{true: "empty", false: "cooled"}[empty], func(t *testing.T) {
			p := authFixture(t)
			home := t.TempDir()
			configureAuthProxy(t, home, p.dir, true)
			p.store = newStateStore(filepath.Join(home, ".codex-proxy"), Config{}, 0, nil)
			if !empty {
				if err := os.WriteFile(filepath.Join(p.dir, "auth0_alice.json"), []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
				if err := p.record(context.Background(), "auth0_alice.json", defaultAuthLimit(time.Now())); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(p.store.statePath)
			client := startAppInitialize(t, home, []string{"CODEX_PROXY_TEST_TASK_INTERVAL=30"}, nil)
			response := client.await(func(message rpcMessage) bool { return rpcErrorPresent(message) })
			assertRPCCode(t, response, 429)
			if string(response["id"]) != "null" {
				t.Fatalf("unexpected startup id: %s", response["id"])
			}
			if code := client.waitExit(); code != authLimitExitCode {
				t.Fatal(code)
			}
			if len(startedPIDs(t, client.dir)) != 0 {
				t.Fatal("exhausted pool launched native")
			}
			after, _ := os.ReadFile(p.store.statePath)
			if string(before) != string(after) {
				t.Fatal("local rejection changed shared state")
			}
			awaitSlots(t, home, 0)
		})
	}
}

func TestAuthConfigOptionalAndValidated(t *testing.T) {
	dir := t.TempDir()
	real := configNativeFixture(t, dir, "native", 0x7f454c46)
	for _, value := range []string{"", "/xxx/codex-auth"} {
		writeConfigFixture(t, dir, "CODEX_PROXY_REAL_BIN="+real+"\nCODEX_PROXY_AUTH_DIR="+value+"\n")
		cfg, err := LoadConfig(dir, configSelf(t))
		if err != nil || cfg.AuthDir != value {
			t.Fatalf("AuthDir=%q err=%v", cfg.AuthDir, err)
		}
	}
	writeConfigFixture(t, dir, "CODEX_PROXY_REAL_BIN="+real+"\nCODEX_PROXY_AUTH_DIR=relative/path\n")
	if _, err := LoadConfig(dir, configSelf(t)); err == nil {
		t.Fatal("relative auth directory accepted")
	}
	if pool, err := newAuthPool(Config{}, "", nil); pool != nil || err != nil {
		t.Fatal("empty config did not bypass all auth handling")
	}
}

func TestAuthAppServerSharedReadersSkipWriterUntilLastReaderExits(t *testing.T) {
	p := authFixture(t, "auth0_alice.json", "auth1_bob.json")
	home := t.TempDir()
	configureAuthProxy(t, home, p.dir, true)
	first, second := startApp(t, home), startApp(t, home)
	a, b := first.thread(), second.thread()
	// Independent proxies sharing CODEX_HOME can both run under reader locks.
	first.call("turn/start", taskParams(a, "one"))
	second.call("turn/start", taskParams(b, "two"))
	awaitSlots(t, home, 2)
	second.finish(b) // Its idle native process must keep the lifetime reader.
	first.send("limit", "test/authError", map[string]any{"threadId": a, "resetsAt": time.Now().Unix() + 3600})
	first.await(func(message rpcMessage) bool { return rpcString(message, "method") == "error" })
	if code := first.waitExit(); code != authLimitExitCode {
		t.Fatal(code)
	}
	linkPath := filepath.Join(home, "sandbox-user", ".codex", "auth.json")
	link, _ := os.Readlink(linkPath)
	if filepath.Base(link) != "auth0_alice.json" {
		t.Fatalf("writer changed an idle reader's credentials: %s", link)
	}
	before := readAuthState(t, home, "auth0_alice.json")
	second.send("local-429", "turn/start", taskParams(b, "shared cooldown must reject"))
	assertRPCCode(t, second.response("local-429"), 429)
	if code := second.waitExit(); code != authLimitExitCode {
		t.Fatal(code)
	}
	if after := readAuthState(t, home, "auth0_alice.json"); after != before {
		t.Fatalf("local rejection extended cooldown: %+v -> %+v", before, after)
	}
	link, _ = os.Readlink(linkPath)
	if filepath.Base(link) != "auth1_bob.json" {
		t.Fatalf("last reader did not rotate: %s", link)
	}
	awaitSlots(t, home, 0)
}

func TestAuthSandboxGrantsOnlySelectedFile(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "alice", ".codex")
	file := filepath.Join(root, "accounts", "auth0_alice.json")
	profile, err := sandboxProfileWithAuth(home, file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(profile, "(literal "+strconv.Quote(file)+")") || strings.Contains(profile, "(subpath "+strconv.Quote(filepath.Dir(file))+")") {
		t.Fatalf("auth sandbox exception too broad or missing: %s", profile)
	}
}

func TestAuthFileArgsEnforcesFileAndPreservesLiteralPrompt(t *testing.T) {
	args := []string{"-c", `cli_auth_credentials_store="keyring"`, "app-server", `--config=cli_auth_credentials_store="auto"`, `-c=cli_auth_credentials_store="keyring"`, `-ccli_auth_credentials_store="auto"`, "--", `-ccli_auth_credentials_store="ephemeral"`}
	got := authFileArgs(args)
	want := []string{"-c", `cli_auth_credentials_store="file"`, "app-server", "--", `-ccli_auth_credentials_store="ephemeral"`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args=%q want=%q", got, want)
	}
}

func TestAuthAppServerIgnoresExternalMCP429(t *testing.T) {
	p := authFixture(t, "auth0_alice.json", "auth1_bob.json")
	home := t.TempDir()
	configureAuthProxy(t, home, p.dir, true)
	client := startApp(t, home)
	thread := client.thread()
	for _, method := range []string{"mcpServer/oauthLogin/completed", "error"} {
		client.call("test/notify", map[string]any{"method": method, "params": map[string]any{"threadId": thread, "error": "MCP OAuth token endpoint returned HTTP status 429 Too Many Requests"}})
	}
	result := client.call("turn/start", taskParams(thread, "healthy GPT account"))
	if filepath.Base(rpcString(result, "authFile")) != "auth0_alice.json" {
		t.Fatalf("MCP 429 rotated account: %s", rpcValue(result))
	}
	if limit := readAuthState(t, home, "auth0_alice.json"); limit.Until != 0 {
		t.Fatalf("MCP 429 polluted state: %+v", limit)
	}
	client.finish(thread)
	if code := client.stop(); code != 0 {
		t.Fatal(code)
	}
}

func TestAuthAppServerExhaustedWindowHintSurvivesExit(t *testing.T) {
	p := authFixture(t, "auth0_alice.json", "auth1_bob.json")
	home := t.TempDir()
	configureAuthProxy(t, home, p.dir, true)
	client := startApp(t, home)
	thread := client.thread()
	reset := time.Now().Unix() + 7200
	client.call("test/notify", map[string]any{"method": "account/rateLimits/updated", "params": map[string]any{"rateLimits": map[string]any{
		"primary":   map[string]any{"usedPercent": 100, "resetsAt": reset},
		"secondary": map[string]any{"usedPercent": 50, "resetsAt": reset + 9000},
	}}})
	client.call("test/rejectNext429", map[string]any{})
	client.send("limit", "turn/start", taskParams(thread, "test"))
	assertRPCCode(t, client.response("limit"), 429)
	if code := client.waitExit(); code != authLimitExitCode {
		t.Fatal(code)
	}
	if limit := readAuthState(t, home, "auth0_alice.json"); limit.Until != reset {
		t.Fatalf("limit=%+v want reset=%d", limit, reset)
	}
}

func TestAuthAppServer429PersistsAfterClientDisconnect(t *testing.T) {
	p := authFixture(t, "auth0_alice.json", "auth1_bob.json")
	home := t.TempDir()
	configureAuthProxy(t, home, p.dir, true)
	client := startApp(t, home)
	thread := client.thread()
	store := newStateStore(filepath.Join(home, ".codex-proxy"), Config{}, time.Millisecond, nil)
	lease, err := store.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	client.send("limit", "test/authError", map[string]any{"threadId": thread, "resetsAt": time.Now().Unix() + 3600})
	client.await(func(message rpcMessage) bool { return rpcString(message, "method") == "error" })
	client.input.Close()
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if code := client.waitExit(); code != authLimitExitCode {
		t.Fatal(code)
	}
	if limit := readAuthState(t, home, "auth0_alice.json"); limit.Until < time.Now().Unix()+3500 {
		t.Fatalf("disconnect lost 429: %+v", limit)
	}
}

func TestAuthAppServerModelFailureNotificationsExit(t *testing.T) {
	for _, method := range []string{"turn/completed", "thread/realtime/error"} {
		t.Run(method, func(t *testing.T) {
			p := authFixture(t, "auth0_alice.json", "auth1_bob.json")
			home := t.TempDir()
			configureAuthProxy(t, home, p.dir, true)
			client := startApp(t, home)
			thread := client.thread()
			params := map[string]any{"threadId": thread}
			if method == "turn/completed" {
				params["turn"] = map[string]any{"id": "failed-turn", "status": "failed", "error": map[string]any{"codexErrorInfo": "usageLimitExceeded"}}
			} else {
				params["message"] = "HTTP status 429 Too Many Requests"
			}
			client.send("notification", "test/notify", map[string]any{"method": method, "params": params})
			client.await(func(message rpcMessage) bool { return rpcString(message, "method") == method })
			if code := client.waitExit(); code != authLimitExitCode {
				t.Fatal(code)
			}
			if limit := readAuthState(t, home, "auth0_alice.json"); limit.Until == 0 {
				t.Fatal("model failure was not recorded")
			}
		})
	}
}
