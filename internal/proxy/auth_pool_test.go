package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func authFixture(t *testing.T, names ...string) *authPool {
	t.Helper()
	dir, home, stateDir := t.TempDir(), filepath.Join(t.TempDir(), ".codex"), t.TempDir()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{"tokens":{"access_token":"fixture"}}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	store := newStateStore(stateDir, Config{}, time.Millisecond, func() int64 { return 0 })
	pool, err := newAuthPool(Config{AuthDir: dir}, home, store)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestAuthPoolOrderCooldownAndWrap(t *testing.T) {
	p := authFixture(t, "auth2_bob.json", "auth10_charlie.json", "auth0_张三.json", "auth1.json", "auth3_.json", "authX_wrong.json")
	now := time.Unix(2000, 0)
	p.store.now = func() time.Time { return now }
	for _, want := range []string{"auth0_张三.json", "auth10_charlie.json", "auth2_bob.json"} {
		name, err := p.choose()
		if err != nil || name != want {
			t.Fatalf("choice=%q want=%q err=%v", name, want, err)
		}
		if err := p.activate(name); err != nil {
			t.Fatal(err)
		}
		if again, err := p.choose(); err != nil || again != name {
			t.Fatalf("successful account did not stick: %q %v", again, err)
		}
		p.note(name, authLimit{At: 2000, Until: 2060})
	}
	if _, err := p.choose(); !errors.Is(err, errNoAuthAccount) {
		t.Fatalf("all cooled: %v", err)
	}
	now = time.Unix(2060, 0)
	if name, err := p.choose(); err != nil || name != "auth0_张三.json" {
		t.Fatalf("wrap after expiry: %q %v", name, err)
	}
	// Expiry does not erase or lower any recorded timestamp.
	if p.local["auth0_张三.json"].Until != 2060 {
		t.Fatal("expired record was reset")
	}
}

func TestAuthPoolAtomicLinkPreservesLogin(t *testing.T) {
	p := authFixture(t, "auth0_alice.json", "auth1_bob.json")
	if err := os.MkdirAll(p.home, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(p.home, "auth.json")
	if err := os.WriteFile(path, []byte("original login"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"auth0_alice.json", "auth1_bob.json"} {
		if err := p.activate(name); err != nil {
			t.Fatal(err)
		}
		link, err := os.Readlink(path)
		if err != nil || link != filepath.Join(p.dir, name) {
			t.Fatalf("link=%q err=%v", link, err)
		}
	}
	data, err := os.ReadFile(path + ".codex-proxy-backup")
	if err != nil || string(data) != "original login" {
		t.Fatalf("lost original login: %s %v", data, err)
	}
	entries, _ := os.ReadDir(p.home)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".codex-proxy-auth-") {
			t.Fatal("temporary link leaked")
		}
	}
}

func TestAuthPoolRemovedAndNewFiles(t *testing.T) {
	p := authFixture(t, "auth0_alice.json", "auth2_bob.json")
	if err := p.activate("auth0_alice.json"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(p.dir, "auth0_alice.json")); err != nil {
		t.Fatal(err)
	}
	if name, err := p.choose(); err != nil || name != "auth2_bob.json" {
		t.Fatalf("removed current: %s %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(p.dir, "auth1_new.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if name, err := p.choose(); err != nil || name != "auth1_new.json" {
		t.Fatalf("new file not scanned: %s %v", name, err)
	}
}

func TestAuthRecordHelper(t *testing.T) {
	if os.Getenv("CODEX_PROXY_AUTH_TEST_HELPER") != "1" {
		return
	}
	until, _ := strconv.ParseInt(os.Getenv("CODEX_PROXY_AUTH_TEST_UNTIL"), 10, 64)
	store := newStateStore(os.Getenv("CODEX_PROXY_AUTH_TEST_STATE"), Config{}, time.Millisecond, nil)
	p := &authPool{store: store}
	if err := p.record(context.Background(), "auth0_alice.json", authLimit{At: until - 10, Until: until}); err != nil {
		t.Fatal(err)
	}
}

func TestAuthStateConcurrentMaxAndUnknownFields(t *testing.T) {
	p := authFixture(t, "auth0_alice.json")
	initial := `{"last_codex_task_at":123,"future":9007199254740993,"codex_accounts":{"auth0_alice.json":{"future":{"n":9007199254740995},"cooldown_until":10,"last_429_at":5},"auth1_bob.json":{"cooldown_until":999}}}`
	if err := os.WriteFile(p.store.statePath, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	var children []*exec.Cmd
	for _, until := range []int{5000, 1000, 3000, 2000, 4000, 6000} {
		cmd := exec.Command(self, "-test.run=^TestAuthRecordHelper$")
		cmd.Env = append(os.Environ(), "CODEX_PROXY_AUTH_TEST_HELPER=1", "CODEX_PROXY_AUTH_TEST_STATE="+filepath.Dir(p.store.statePath), fmt.Sprintf("CODEX_PROXY_AUTH_TEST_UNTIL=%d", until))
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		children = append(children, cmd)
	}
	for _, cmd := range children {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	// A late smaller update, including after expiry, must never reduce state.
	if err := p.record(context.Background(), "auth0_alice.json", authLimit{At: 20, Until: 30}); err != nil {
		t.Fatal(err)
	}
	state, err := p.store.readState()
	if err != nil {
		t.Fatal(err)
	}
	accounts, _ := decodeAccountStates(state)
	fields, limit, err := decodeAccountLimit(accounts["auth0_alice.json"])
	if err != nil || limit.At != 5990 || limit.Until != 6000 {
		t.Fatalf("merged limit=%+v err=%v", limit, err)
	}
	if string(state["future"]) != "9007199254740993" || string(fields["future"]) != `{"n":9007199254740995}` || string(state[lastCodexTaskKey]) != "123" || string(accounts["auth1_bob.json"]) != `{"cooldown_until":999}` {
		t.Fatalf("unrelated state lost: %s", rpcValue(state))
	}
	// The normal task timestamp writer must preserve account state too.
	lease, err := p.store.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Record(context.Background()); err != nil {
		t.Fatal(err)
	}
	lease.Close()
	after, _ := p.store.readState()
	if string(after[codexAccountsKey]) != string(state[codexAccountsKey]) {
		t.Fatal("task submission overwrote account updates")
	}
}

func TestAuthMalformedStateFailsClosed(t *testing.T) {
	p := authFixture(t, "auth0_alice.json")
	for _, data := range []string{`{"codex_accounts":null}`, `{"codex_accounts":[]}`, `{"codex_accounts":{"auth0_alice.json":null}}`, `{"codex_accounts":{"auth0_alice.json":{"cooldown_until":-1}}}`, `{"codex_accounts":{"auth0_alice.json":{"cooldown_until":"future"}}}`} {
		if err := os.WriteFile(p.store.statePath, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := p.choose(); err == nil || errors.Is(err, errNoAuthAccount) {
			t.Fatalf("accepted invalid state %s: %v", data, err)
		}
	}
}

func configureAuthProxy(t *testing.T, home, authDir string, enabled bool) {
	t.Helper()
	configureProxy(t, home, fakeCodex(t), 2, "10m", enabled)
	path := filepath.Join(home, ".codex-proxy", "env")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "CODEX_PROXY_AUTH_DIR=%s\n", strconv.Quote(authDir)); err != nil {
		t.Fatal(err)
	}
}

func readAuthState(t *testing.T, home, name string) authLimit {
	t.Helper()
	state, err := newStateStore(filepath.Join(home, ".codex-proxy"), Config{}, 0, nil).readState()
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := decodeAccountStates(state)
	if err != nil {
		t.Fatal(err)
	}
	_, limit, err := decodeAccountLimit(accounts[name])
	if err != nil {
		t.Fatal(err)
	}
	return limit
}

func TestAuthExecRecordsAndNextProcessSkips(t *testing.T) {
	p := authFixture(t, "auth0_alice.json", "auth1_bob.json")
	home := t.TempDir()
	configureAuthProxy(t, home, p.dir, false)
	first := startProxy(t, home, "auth-limit", t.TempDir(), "", 1, "exec", "test")
	if code := childExit(t, first, 5*time.Second); code != authLimitExitCode {
		t.Fatalf("exit=%d stderr=%s", code, first.stderr.String())
	}
	if limit := readAuthState(t, home, "auth0_alice.json"); limit.Until < time.Now().Unix()+100 {
		t.Fatalf("429 not recorded: %+v", limit)
	}
	second := startProxy(t, home, "echo", t.TempDir(), "", 0, "exec", "test")
	if code := childExit(t, second, 5*time.Second); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, second.stderr.String())
	}
	link, _ := os.Readlink(filepath.Join(home, "sandbox-user", ".codex", "auth.json"))
	if filepath.Base(link) != "auth1_bob.json" {
		t.Fatalf("next process used %q", link)
	}
}

func TestAuthExecColored429PreservesOutputAndRotates(t *testing.T) {
	p := authFixture(t, "auth0_alice.json", "auth1_bob.json")
	home := t.TempDir()
	configureAuthProxy(t, home, p.dir, false)
	child := startProxy(t, home, "auth-color-limit", t.TempDir(), "", 1, "exec", "--color", "always", "test")
	if code := childExit(t, child, 5*time.Second); code != authLimitExitCode {
		t.Fatalf("exit=%d stderr=%q", code, child.stderr.String())
	}
	if got := child.stdout.String(); got != "\x1b[36mnative stdout\x1b[0m\n" {
		t.Fatalf("native stdout changed: %q", got)
	}
	wantStderr := "\x1b[31m\x1b[1mERROR:\x1b[0m You've hit your usage limit. Try again later.\n"
	if !strings.HasPrefix(child.stderr.String(), wantStderr) {
		t.Fatalf("native stderr changed: %q", child.stderr.String())
	}
	if limit := readAuthState(t, home, "auth0_alice.json"); limit.Until < time.Now().Unix()+55 {
		t.Fatalf("colored 429 was not recorded: %+v", limit)
	}
	link, err := os.Readlink(filepath.Join(home, "sandbox-user", ".codex", "auth.json"))
	if err != nil || filepath.Base(link) != "auth1_bob.json" {
		t.Fatalf("colored 429 did not rotate: %q %v", link, err)
	}
	awaitSlots(t, home, 0)
}

func TestAuthExecExhaustedRejectsBeforeSleep(t *testing.T) {
	p := authFixture(t, "auth0_alice.json")
	home, nativeDir := t.TempDir(), t.TempDir()
	configureAuthProxy(t, home, p.dir, false)
	p.store = newStateStore(filepath.Join(home, ".codex-proxy"), Config{}, 0, nil)
	if err := p.record(context.Background(), "auth0_alice.json", defaultAuthLimit(time.Now())); err != nil {
		t.Fatal(err)
	}
	// A launch that entered the normal gate would wait 30s.
	writeTaskStateWithAccounts, _ := p.store.readState()
	writeTaskStateWithAccounts[lastCodexTaskKey] = rpcValue(time.Now().Unix())
	if err := os.WriteFile(p.store.statePath, rpcValue(writeTaskStateWithAccounts), 0600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(p.store.statePath)
	child := startProxyEnv(t, home, "echo", nativeDir, "", 0, []string{"CODEX_PROXY_TEST_TASK_INTERVAL=30"}, "exec", "test")
	if code := childExit(t, child, 2*time.Second); code != authLimitExitCode || !strings.Contains(child.stderr.String(), "429") {
		t.Fatalf("exit=%d stderr=%s", code, child.stderr.String())
	}
	if pids := startedPIDs(t, nativeDir); len(pids) != 0 {
		t.Fatal("exhausted pool launched native Codex")
	}
	after, _ := os.ReadFile(p.store.statePath)
	if string(before) != string(after) {
		t.Fatal("local rejection mutated cooldowns or task timestamp")
	}
}

func TestAuthErrorDetectionAndDeadlines(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{`{"code":429,"message":"limited"}`, true},
		{`{"codexErrorInfo":"usageLimitExceeded"}`, true},
		{`{"codexErrorInfo":{"responseTooManyFailedAttempts":{"httpStatusCode":429}}}`, true},
		{`"unexpected status 429 Too Many Requests"`, true},
		{`{"message":"line 429 failed"}`, false},
		{`{"code":401,"message":"unauthorized"}`, false},
		{`{"codexErrorInfo":"serverOverloaded"}`, false},
	} {
		if got := authRateLimited(json.RawMessage(tc.raw)); got != tc.want {
			t.Errorf("%s = %v want %v", tc.raw, got, tc.want)
		}
	}
	now := time.Unix(2000, 0)
	if got := authErrorLimit(rpcValue(map[string]any{"retry_after": 120}), now); got.Until != 2120 {
		t.Fatal(got)
	}
	if got := authErrorLimit(rpcValue(map[string]any{"resetsAt": 9000}), now); got.Until != 9000 {
		t.Fatal(got)
	}
	observer := &authOutputObserver{now: func() time.Time { return now }}
	observer.Write([]byte("{\"type\":\"item.completed\",\"item\":{\"output\":\"HTTP 429 Too Many Requests\"}}\n"))
	if observer.result().At != 0 {
		t.Fatal("tool output triggered account rotation")
	}
	observer.Write([]byte(`{"type":"turn.failed","error":{"codexErrorInfo":"usageLimitExceeded"}}`))
	if observer.result().Until != 2060 {
		t.Fatal("unterminated native error was not observed")
	}
}

func TestAuthHomeReadersAndSingleNonblockingRotation(t *testing.T) {
	p := authFixture(t, "auth0_alice.json", "auth1_bob.json", "auth2_carol.json")
	first, err := p.acquireHome(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	other, err := newAuthPool(Config{AuthDir: p.dir}, p.home, p.store)
	if err != nil {
		t.Fatal(err)
	}
	second, err := other.acquireHome(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	limit := defaultAuthLimit(time.Now())
	p.note("auth0_alice.json", limit)
	if err := p.record(context.Background(), "auth0_alice.json", limit); err != nil {
		t.Fatal(err)
	}
	if err := releaseAuthHome(first); err != nil {
		t.Fatal(err)
	}
	if locked, _, err := p.rotate(); err != nil || locked {
		t.Fatalf("writer bypassed another reader: %v %v", locked, err)
	}
	if linked, _ := p.linkedAccount(); linked != "auth0_alice.json" {
		t.Fatal(linked)
	}
	if err := releaseAuthHome(second); err != nil {
		t.Fatal(err)
	}
	if locked, name, err := p.rotate(); err != nil || !locked || name != "auth1_bob.json" {
		t.Fatalf("rotation=%v %s %v", locked, name, err)
	}
	// A late attempt must inspect the current link and retain an account that
	// another exiting proxy already selected, instead of advancing twice.
	if locked, name, err := other.rotate(); err != nil || !locked || name != "auth1_bob.json" {
		t.Fatalf("late rotation=%v %s %v", locked, name, err)
	}
}

func TestAuthExecNestedDoesNotWaitOnParentReader(t *testing.T) {
	p := authFixture(t, "auth0_alice.json", "auth1_bob.json")
	home := t.TempDir()
	configureAuthProxy(t, home, p.dir, false)
	path := filepath.Join(home, ".codex-proxy", "env")
	config, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(strings.Replace(string(config), "CODEX_PROXY_WAIT_TIMEOUT=10m", "CODEX_PROXY_WAIT_TIMEOUT=300ms", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	// Exercise authentication locking independently of macOS filesystem rules;
	// TestNestedProxySharesAdmissionAndState covers the inherited sandbox too.
	child := startProxyEnv(t, home, "auth-nested", t.TempDir(), "", 0, []string{"FAKE_CODEX_PROXY=" + self, "CODEX_PROXY_TEST_NO_SANDBOX=1"}, "exec", "outer")
	if code := childExit(t, child, 4*time.Second); code != 0 {
		t.Fatalf("nested exec exit=%d stdout=%s stderr=%s", code, child.stdout.String(), child.stderr.String())
	}
	if !strings.Contains(child.stdout.String(), "nested result=<nil>") {
		t.Fatalf("missing nested completion: %s", child.stdout.String())
	}
}

func TestAuthExecPersists429WhileStoppingNative(t *testing.T) {
	p := authFixture(t, "auth0_alice.json", "auth1_bob.json")
	home, dir := t.TempDir(), t.TempDir()
	configureAuthProxy(t, home, p.dir, false)
	child := startProxy(t, home, "auth-limit-hold", dir, "", 0, "exec", "test")
	pid, _ := strconv.Atoi(awaitStarts(t, dir, 1)[0])
	deadline := time.Now().Add(3 * time.Second)
	for readAuthState(t, home, "auth0_alice.json").Until == 0 {
		if time.Now().After(deadline) {
			t.Fatal("429 was not shared before native termination")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("native exited before state was shared: %v", err)
	}
	p.home = filepath.Join(home, "sandbox-user", ".codex")
	writer, err := p.homeLock()
	if err != nil {
		t.Fatal(err)
	}
	locked, err := tryAuthLock(context.Background(), writer, syscall.LOCK_EX)
	writer.Close()
	if err != nil || locked {
		t.Fatalf("reader released before native stopped: %v %v", locked, err)
	}
	if code := childExit(t, child, 4*time.Second); code != authLimitExitCode {
		t.Fatalf("exit=%d stderr=%s", code, child.stderr.String())
	}
	if linked, _ := p.linkedAccount(); linked != "auth1_bob.json" {
		t.Fatalf("exit did not rotate: %s", linked)
	}
	if len(startedPIDs(t, dir)) != 1 {
		t.Fatal("proxy restarted native exec")
	}
}

func TestAuthExecBackgroundPipesDoNotRetainReservations(t *testing.T) {
	for _, tc := range []struct {
		name       string
		nativeExit int
		limited    bool
		wantExit   int
	}{
		{name: "success"},
		{name: "native_failure", nativeExit: 37, wantExit: 37},
		{name: "final_429", nativeExit: 1, limited: true, wantExit: authLimitExitCode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := authFixture(t, "auth0_alice.json", "auth1_bob.json")
			home, dir := t.TempDir(), t.TempDir()
			configureAuthProxy(t, home, p.dir, false)
			child := startProxyEnv(t, home, "auth-background", dir, "", tc.nativeExit,
				[]string{"FAKE_CODEX_BACKGROUND_429=" + strconv.FormatBool(tc.limited)}, "exec", "test")
			deadline := time.Now().Add(5 * time.Second)
			var backgroundPID int
			for backgroundPID == 0 {
				data, err := os.ReadFile(filepath.Join(dir, "background.pid"))
				if err == nil {
					backgroundPID, err = strconv.Atoi(string(data))
					if err != nil || backgroundPID <= 1 {
						t.Fatalf("invalid background pid %q: %v", data, err)
					}
				} else if !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				if time.Now().After(deadline) {
					t.Fatal("native did not start the background fixture")
				}
				time.Sleep(5 * time.Millisecond)
			}
			t.Cleanup(func() { _ = syscall.Kill(backgroundPID, syscall.SIGKILL) })
			if code := childExit(t, child, 3*time.Second); code != tc.wantExit {
				t.Fatalf("exit=%d want=%d stderr=%s", code, tc.wantExit, child.stderr.String())
			}
			if err := syscall.Kill(backgroundPID, 0); err != nil {
				t.Fatalf("background fixture stopped before the proxy exited: %v", err)
			}
			if !strings.HasPrefix(child.stdout.String(), "native stdout\n") || !strings.HasPrefix(child.stderr.String(), "native stderr\n") {
				t.Fatalf("native output lost: stdout=%q stderr=%q", child.stdout.String(), child.stderr.String())
			}
			awaitSlots(t, home, 0)
			p.home = filepath.Join(home, "sandbox-user", ".codex")
			writer, err := p.homeLock()
			if err != nil {
				t.Fatal(err)
			}
			locked, err := tryAuthLock(context.Background(), writer, syscall.LOCK_EX)
			writer.Close()
			if err != nil || !locked {
				t.Fatalf("authentication reader retained after native exit: %v %v", locked, err)
			}
			wantAccount := "auth0_alice.json"
			limit := readAuthState(t, home, wantAccount)
			if tc.limited {
				wantAccount = "auth1_bob.json"
				if limit.Until < time.Now().Unix()+100 {
					t.Fatalf("final 429 lost during output cleanup: %+v", limit)
				}
			} else if limit.Until != 0 {
				t.Fatalf("successful output triggered cooldown: %+v", limit)
			}
			if linked, err := p.linkedAccount(); err != nil || linked != wantAccount {
				t.Fatalf("account=%s want=%s err=%v", linked, wantAccount, err)
			}
		})
	}
}
