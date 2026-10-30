package proxy

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// On macOS these are end-to-end tests through sandbox-exec, a native outer
// agent, and an inner proxy inheriting that sandbox. Linux exercises the same
// shared admission, state, and logging paths without Seatbelt.
func TestNestedProxySharesAdmissionAndState(t *testing.T) {
	if !platformSandboxAvailable() {
		t.Skip("sandbox-exec unavailable: sandbox_apply rejected by this environment")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"exec", "app-server"} {
		for _, scenario := range []string{"success", "slot timeout", "submission interval", "custom state paths"} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				home := t.TempDir()
				limit, interval := 2, 0
				if scenario == "slot timeout" {
					limit = 1
				}
				if scenario == "submission interval" {
					interval = 2
				}
				configureAppProxy(t, home, fakeCodex(t), limit, "150ms")
				if scenario == "custom state paths" {
					dir := filepath.Join(home, ".codex-proxy", "runtime")
					if err := os.Mkdir(dir, 0700); err != nil {
						t.Fatal(err)
					}
					config, err := os.OpenFile(filepath.Join(home, ".codex-proxy", "env"), os.O_WRONLY|os.O_APPEND, 0)
					if err != nil {
						t.Fatal(err)
					}
					_, err = config.WriteString("CODEX_PROXY_STATE_LOCK_PATH=" + strconv.Quote(filepath.Join(dir, "custom.lock")) + "\n" +
						"CODEX_PROXY_STATE_PATH=" + strconv.Quote(filepath.Join(dir, "custom.json")) + "\n")
					closeErr := config.Close()
					if err != nil || closeErr != nil {
						t.Fatalf("write custom state configuration: %v %v", err, closeErr)
					}
				}
				child := startProxyEnv(t, home, "nested-proxy", t.TempDir(), "", 0, []string{
					"FAKE_CODEX_PROXY=" + self, "CODEX_PROXY_TEST_NESTED_MODE=" + mode,
					"CODEX_PROXY_TEST_NESTED_LIMIT=" + strconv.Itoa(limit),
					"CODEX_PROXY_TEST_TASK_INTERVAL=" + strconv.Itoa(interval),
				}, "exec", "outer")
				if code := childExit(t, child, 15*time.Second); code != 0 {
					t.Fatalf("nested %s exit=%d stdout=%s stderr=%s", mode, code, child.stdout.String(), child.stderr.String())
				}
				if !strings.Contains(child.stdout.String(), "nested result=<nil>") {
					t.Fatalf("missing nested completion: %s", child.stdout.String())
				}
				awaitSlots(t, home, 0)
			})
		}
	}
}

func TestNestedProxyHelperProcess(t *testing.T) {
	mode := os.Getenv("CODEX_PROXY_TEST_NESTED_MODE")
	if mode == "" {
		return
	}
	home := os.Getenv("CODEX_PROXY_TEST_HOME")
	dir := filepath.Join(home, ".codex-proxy")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(dir, self)
	if err != nil {
		t.Fatal(err)
	}
	store := newStateStore(dir, cfg, 0, nil)
	stamp := func() int64 {
		t.Helper()
		state, err := store.readState()
		if err != nil {
			t.Fatal(err)
		}
		value, err := state.lastCodexTaskAt()
		if err != nil || value == 0 {
			t.Fatalf("missing parent task timestamp: %d %v", value, err)
		}
		return value
	}
	before := stamp()
	limited := os.Getenv("CODEX_PROXY_TEST_NESTED_LIMIT") == "1"
	if mode == "exec" {
		nativeDir := t.TempDir()
		child := startProxy(t, home, "hold", nativeDir, "", 0, "exec", "inner")
		if limited {
			if code := childExit(t, child, 5*time.Second); code != 124 || !strings.Contains(child.stderr.String(), "timed out waiting") {
				t.Fatalf("inner exec bypassed parent's slot: exit=%d stderr=%s", code, child.stderr.String())
			}
		} else {
			pid := awaitStarts(t, nativeDir, 1)[0]
			awaitSlots(t, home, 2)
			releaseNative(t, nativeDir, pid)
			if code := childExit(t, child, 5*time.Second); code != 0 {
				t.Fatalf("inner exec exit=%d stderr=%s", code, child.stderr.String())
			}
		}
	} else {
		client := startApp(t, home)
		thread := client.thread()
		if got := stamp(); got != before {
			t.Fatalf("idle inner app-server changed state: %d -> %d", before, got)
		}
		client.send("nested-turn", "turn/start", taskParams(thread, "inner task"))
		response := client.response("nested-turn")
		if limited {
			if code := string(rpcObject(response, "error")["code"]); code != "-32001" {
				t.Fatalf("inner app-server bypassed parent's slot: %s", response)
			}
		} else {
			if response["error"] != nil {
				t.Fatalf("inner turn failed: %s", response)
			}
			awaitSlots(t, home, 2)
			client.finish(thread)
		}
		if code := client.stop(); code != 0 {
			t.Fatalf("inner app-server exit=%d", code)
		}
	}
	awaitSlots(t, home, 1)
	after := stamp()
	interval, _ := strconv.ParseInt(os.Getenv("CODEX_PROXY_TEST_TASK_INTERVAL"), 10, 64)
	if (limited && after != before) || (!limited && after-before < interval) {
		t.Fatalf("inner task did not use parent's state: %d -> %d, limited=%v interval=%d", before, after, limited, interval)
	}
	// Exercise directory listing, log file chmod, archive creation, rename,
	// and pruning while the inherited sandbox is still active.
	var diagnostic bytes.Buffer
	logger := NewLogger(dir, "nested-rotation", &diagnostic, func() (Config, error) {
		return Config{LogMaxSize: 256, LogMaxBackups: 2}, nil
	})
	for i := 0; i < 5; i++ {
		logger.Log("nested", map[string]any{"message": strings.Repeat("x", 200)})
	}
	if diagnostic.Len() != 0 {
		t.Fatal(diagnostic.String())
	}
	if _, err := os.Stat(filepath.Join(dir, loggerFilename+".1")); err != nil {
		t.Fatalf("nested log rotation failed: %v", err)
	}
}
