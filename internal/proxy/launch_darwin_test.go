package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDarwinSandboxFilesystemIsolation(t *testing.T) {
	home := t.TempDir()
	configureProxy(t, home, fakeCodex(t), 1, "1s")
	own := filepath.Join(home, "sandbox-user")
	other := filepath.Join(home, "other-user")
	if err := os.MkdirAll(own, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(other, 0700); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{filepath.Join(own, "own-file"), filepath.Join(other, "secret")} {
		if err := os.WriteFile(file, []byte("test-data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(other, "secret"), filepath.Join(own, "sibling-link")); err != nil {
		t.Fatal(err)
	}
	// The native marker/finished files are deliberately denied at the root.
	// Probe mode skips those markers and tests the sandbox through Run itself.
	child := startProxyEnv(t, home, "sandbox-probe", "", "", 0,
		[]string{"FAKE_SANDBOX_ROOT=" + home}, "exec", "--sandbox", "read-only", "--skip-git-repo-check")
	if code := childExit(t, child, 5*time.Second); code != 0 {
		t.Fatalf("sandbox launcher exit=%d stderr=%s", code, child.stderr.String())
	}
	var checks map[string]bool
	if err := json.Unmarshal(child.stdout.Bytes(), &checks); err != nil {
		t.Fatal(err)
	}
	if len(checks) != 8 {
		t.Fatalf("missing sandbox checks: %v", checks)
	}
	for name, passed := range checks {
		if !passed {
			t.Errorf("sandbox check %s failed", name)
		}
	}
}

func TestDarwinExecRejectsInvalidCODEXHOMEBeforeAdmission(t *testing.T) {
	for _, codeHome := range []string{"", "relative/user/.codex", "/user/.codex", "/shared/user/not-codex"} {
		t.Run(codeHome, func(t *testing.T) {
			home := t.TempDir()
			configureProxy(t, home, fakeCodex(t), 1, "1s")
			child := startProxyEnv(t, home, "echo", "", "", 0, []string{"CODEX_HOME=" + codeHome}, "exec")
			if code := childExit(t, child, 5*time.Second); code != 2 {
				t.Fatalf("exit=%d stderr=%s", code, child.stderr.String())
			}
			if _, err := os.Stat(filepath.Join(home, ".codex-proxy", "locks")); !os.IsNotExist(err) {
				t.Fatalf("invalid CODEX_HOME reserved a slot: %v", err)
			}
			if !strings.Contains(child.stderr.String(), "CODEX_HOME") {
				t.Fatalf("missing CODEX_HOME diagnostic: %s", child.stderr.String())
			}
		})
	}
}

func TestDarwinOtherCommandsDoNotRequireCODEXHOME(t *testing.T) {
	home := t.TempDir()
	configureProxy(t, home, fakeCodex(t), 1, "1s")
	child := startProxyEnv(t, home, "echo", "", "", 0, []string{"CODEX_HOME="}, "login", "--help")
	if code := childExit(t, child, 5*time.Second); code != 0 {
		t.Fatalf("passthrough exit=%d stderr=%s", code, child.stderr.String())
	}
	records := readLoggerRecords(t, filepath.Join(home, ".codex-proxy"), loggerDefaultMaxSize)
	if len(records) != 2 || records[0]["event"] != "started" || records[1]["event"] != "finished" {
		t.Fatalf("expected started and finished log events: %v", records)
	}
	for _, record := range records {
		if record["user"] != "" {
			t.Errorf("%s user=%v, want empty string", record["event"], record["user"])
		}
	}
}
