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

func TestAuthAppServerRotatesAndRestoresThreads(t *testing.T) {
	p := authFixture(t, "auth0_alice.json", "auth1_bob.json")
	home := t.TempDir()
	configureAuthProxy(t, home, p.dir, true)
	client := startApp(t, home)
	a, b := client.thread(), client.thread()
	client.call("test/rejectNext429", map[string]any{})
	client.send("first-429", "turn/start", taskParams(a, "first"))
	assertRPCCode(t, client.response("first-429"), 429)
	awaitSlots(t, home, 0)
	result := client.call("turn/start", taskParams(b, "next account"))
	if filepath.Base(rpcString(result, "authFile")) != "auth1_bob.json" || !rpcBoolTrue(result, "authReadOK") {
		t.Fatalf("native process retained old credentials: %s", rpcValue(result))
	}
	client.finish(b)
	awaitSlots(t, home, 0)
	if limit := readAuthState(t, home, "auth0_alice.json"); limit.Until < time.Now().Unix()+100 {
		t.Fatalf("missing first account cooldown: %+v", limit)
	}
	// A restored thread can execute too, and the selected account stays sticky.
	result = client.call("turn/start", taskParams(a, "restored"))
	if filepath.Base(rpcString(result, "authFile")) != "auth1_bob.json" {
		t.Fatal("restored thread used wrong account")
	}
	client.finish(a)
	awaitSlots(t, home, 0)
	client.call("test/rejectNext429", map[string]any{})
	client.send("second-429", "turn/start", taskParams(b, "exhaust second"))
	assertRPCCode(t, client.response("second-429"), 429)
	awaitSlots(t, home, 0)
	client.send("all-cooled", "turn/start", taskParams(a, "must reject locally"))
	assertRPCCode(t, client.response("all-cooled"), 429)
	if got := len(startedPIDs(t, client.dir)); got != 2 {
		t.Fatalf("expected one native restart, got %d starts", got)
	}
	// Adding a matching account is picked up without restarting the proxy.
	if err := os.WriteFile(filepath.Join(p.dir, "auth2_carol.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	result = client.call("turn/start", taskParams(a, "new account"))
	if filepath.Base(rpcString(result, "authFile")) != "auth2_carol.json" {
		t.Fatal("new account was not loaded by native")
	}
	client.finish(a)
	awaitSlots(t, home, 0)
	if code := client.stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	data, _ := os.ReadFile(filepath.Join(client.dir, "rpc.jsonl"))
	if !strings.Contains(string(data), `"method":"thread/resume"`) {
		t.Fatal("no thread was restored on native restart")
	}
}

func TestAuthAppServerWaitsForRunningWorkAndKeepsQueriesResponsive(t *testing.T) {
	p := authFixture(t, "auth0_alice.json", "auth1_bob.json")
	home := t.TempDir()
	configureAuthProxy(t, home, p.dir, true)
	client := startApp(t, home)
	a, b := client.thread(), client.thread()
	client.call("turn/start", taskParams(a, "running"))
	client.call("test/authError", map[string]any{"threadId": a, "resetsAt": time.Now().Unix() + 3600})
	client.send("waiting-account", "turn/start", taskParams(b, "wait for safe switch"))
	client.call("test/ping", map[string]any{})
	if got := len(startedPIDs(t, client.dir)); got != 1 {
		t.Fatalf("restarted native with active work: %d", got)
	}
	client.finish(a)
	response := client.response("waiting-account")
	if rpcErrorPresent(response) || filepath.Base(rpcString(rpcObject(response, "result"), "authFile")) != "auth1_bob.json" {
		t.Fatalf("account switch failed: %s", rpcValue(response))
	}
	if limit := readAuthState(t, home, "auth0_alice.json"); limit.Until < time.Now().Unix()+3500 {
		t.Fatalf("reset time lost: %+v", limit)
	}
	client.finish(b)
	if code := client.stop(); code != 0 {
		t.Fatal(code)
	}
}

func TestAuthAppServerExhaustedAndEmptyPoolRejectImmediately(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{true: "empty", false: "cooled"}[empty], func(t *testing.T) {
			p := authFixture(t)
			home := t.TempDir()
			configureAuthProxy(t, home, p.dir, true)
			if !empty {
				if err := os.WriteFile(filepath.Join(p.dir, "auth0_alice.json"), []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
				p.store = newStateStore(filepath.Join(home, ".codex-proxy"), Config{}, 0, nil)
				if err := p.record(context.Background(), "auth0_alice.json", defaultAuthLimit(time.Now())); err != nil {
					t.Fatal(err)
				}
			}
			client := startAppEnv(t, home, []string{"CODEX_PROXY_TEST_TASK_INTERVAL=30"})
			thread := client.thread()
			client.send("rejected", "turn/start", taskParams(thread, "test"))
			assertRPCCode(t, client.response("rejected"), 429)
			awaitSlots(t, home, 0)
			client.call("test/ping", map[string]any{})
			if stamp := readTaskState(t, filepath.Join(home, ".codex-proxy")); stamp != 0 {
				t.Fatalf("local 429 recorded a task timestamp: %d", stamp)
			}
			if code := client.stop(); code != 0 {
				t.Fatal(code)
			}
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

func TestAuthAppServerSharedHomePinsCredentials(t *testing.T) {
	p := authFixture(t, "auth0_alice.json", "auth1_bob.json")
	home := t.TempDir()
	configureAuthProxy(t, home, p.dir, true)
	first := startApp(t, home)
	a := first.thread()
	first.call("turn/start", taskParams(a, "keep old account pinned"))
	first.call("test/authError", map[string]any{"threadId": a, "resetsAt": time.Now().Unix() + 3600})
	second := startApp(t, home)
	b := second.thread()
	second.send("after-home-release", "turn/start", taskParams(b, "must wait"))
	second.call("test/ping", map[string]any{})
	link, _ := os.Readlink(filepath.Join(home, "sandbox-user", ".codex", "auth.json"))
	if filepath.Base(link) != "auth0_alice.json" {
		t.Fatalf("another connection changed an active auth.json: %s", link)
	}
	first.finish(a)
	response := second.response("after-home-release")
	if rpcErrorPresent(response) || filepath.Base(rpcString(rpcObject(response, "result"), "authFile")) != "auth1_bob.json" {
		t.Fatalf("second connection did not load next account: %s", rpcValue(response))
	}
	second.finish(b)
	if code := first.stop(); code != 0 {
		t.Fatal(code)
	}
	if code := second.stop(); code != 0 {
		t.Fatal(code)
	}
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
	args := []string{"-c", `cli_auth_credentials_store="keyring"`, "app-server", `--config=cli_auth_credentials_store="auto"`, "--", `-ccli_auth_credentials_store="ephemeral"`}
	got := authFileArgs(args)
	want := []string{"-c", `cli_auth_credentials_store="file"`, "app-server", "--", `-ccli_auth_credentials_store="ephemeral"`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args=%q want=%q", got, want)
	}
}

func TestAuthAppServerAccountWaitTimeout(t *testing.T) {
	p := authFixture(t, "auth0_alice.json", "auth1_bob.json")
	home := t.TempDir()
	configureAuthProxy(t, home, p.dir, true)
	path := filepath.Join(home, ".codex-proxy", "env")
	config, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(strings.Replace(string(config), "CODEX_PROXY_WAIT_TIMEOUT=10m", "CODEX_PROXY_WAIT_TIMEOUT=300ms", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	client := startApp(t, home)
	a, b := client.thread(), client.thread()
	client.call("turn/start", taskParams(a, "keep active"))
	client.call("test/authError", map[string]any{"threadId": a, "resetsAt": time.Now().Unix() + 3600})
	client.send("switch-timeout", "turn/start", taskParams(b, "cannot switch yet"))
	assertRPCCode(t, client.response("switch-timeout"), -32001)
	client.finish(a)
	if code := client.stop(); code != 0 {
		t.Fatal(code)
	}
}

func TestAuthAppServerClosesEphemeralHistoryOnSwitch(t *testing.T) {
	p := authFixture(t, "auth0_alice.json", "auth1_bob.json")
	home := t.TempDir()
	configureAuthProxy(t, home, p.dir, true)
	client := startApp(t, home)
	result := client.call("thread/start", map[string]any{"ephemeral": true})
	ephemeral := rpcString(rpcObject(result, "thread"), "id")
	persistent := client.thread()
	client.call("test/rejectNext429", map[string]any{})
	client.send("ephemeral-429", "turn/start", taskParams(ephemeral, "test"))
	assertRPCCode(t, client.response("ephemeral-429"), 429)
	client.call("turn/start", taskParams(persistent, "switch"))
	client.await(func(message rpcMessage) bool {
		return rpcString(message, "method") == "thread/closed" && rpcString(rpcObject(message, "params"), "threadId") == ephemeral
	})
	client.finish(persistent)
	if code := client.stop(); code != 0 {
		t.Fatal(code)
	}
}
