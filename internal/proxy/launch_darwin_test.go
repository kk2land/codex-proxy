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
	if !platformSandboxAvailable() {
		t.Skip("sandbox-exec unavailable: sandbox_apply rejected by this environment")
	}
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

func TestDarwinSandboxAdministratorRules(t *testing.T) {
	if !platformSandboxAvailable() {
		t.Skip("sandbox-exec unavailable: sandbox_apply rejected by this environment")
	}
	for _, placement := range []string{"outside shared root", "inside user directory"} {
		for _, mode := range []string{"exec", "app-server", "dsh", "pi"} {
			t.Run(placement+"/"+mode, func(t *testing.T) {
				base := t.TempDir()
				shared := filepath.Join(base, "shared")
				own := filepath.Join(shared, "alice")
				other := filepath.Join(shared, "other-user")
				proxyDir := filepath.Join(base, ".codex-proxy")
				if placement == "inside user directory" {
					proxyDir = filepath.Join(own, ".codex-proxy")
				}
				home := filepath.Join(own, ".codex")
				for _, dir := range []string{home, other, proxyDir, filepath.Join(own, "private")} {
					if err := os.MkdirAll(dir, 0700); err != nil {
						t.Fatal(err)
					}
				}
				for _, file := range []string{filepath.Join(own, "own-file"), filepath.Join(other, "secret"), filepath.Join(proxyDir, "env"), filepath.Join(proxyDir, "secret"), filepath.Join(own, "private", "secret")} {
					if err := os.WriteFile(file, []byte("test-data"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				for link, target := range map[string]string{"sibling-link": filepath.Join(other, "secret"), "proxy-link": filepath.Join(proxyDir, "env")} {
					if err := os.Symlink(target, filepath.Join(own, link)); err != nil {
						t.Fatal(err)
					}
				}
				// Explicit grants before the final deny must not reopen the proxy
				// directory. The custom deny below targets an otherwise allowed path.
				rules := "(allow file-read* file-write* file-read-metadata file-write-data file-write-create file-write-unlink (subpath " + quoteSandboxPath(canonicalSandboxTestPath(t, proxyDir)) + "))\n" +
					"(deny file-read* file-write* (subpath " + quoteSandboxPath(canonicalSandboxTestPath(t, filepath.Join(own, "private"))) + ")) ; no final newline"
				if err := os.WriteFile(filepath.Join(proxyDir, sandboxRulesFilename), []byte(rules), 0600); err != nil {
					t.Fatal(err)
				}
				env := append(os.Environ(), "CODEX_HOME="+home, "HOME="+filepath.Join(base, "runtime-home"),
					"FAKE_CODEX_MODE=sandbox-probe", "FAKE_CODEX_DIR=", "FAKE_CODEX_EXIT=0",
					"FAKE_SANDBOX_ROOT="+shared, "FAKE_SANDBOX_PROXY_DIR="+proxyDir)
				var spec launchSpec
				var err error
				if mode == "exec" {
					t.Setenv("CODEX_HOME", home)
					spec, err = prepareLaunch(CommandExec, []string{"exec"}, proxyDir)
				} else if mode == "app-server" {
					spec, err = prepareAppServerLaunch(appServerStartup{Args: []string{"app-server"}, Env: env}, proxyDir)
				} else if mode == "dsh" {
					spec.profile, err = dshSandboxProfile(filepath.Join(own, ".dsh"), proxyDir)
					spec.args = []string{"--unmodified-dsh-argument"}
				} else {
					spec.profile, err = piSandboxProfile(filepath.Join(own, ".pi", "agent"), proxyDir)
					spec.args = []string{"--unmodified-pi-argument"}
				}
				if err != nil {
					t.Fatal(err)
				}
				cmd := spec.command(fakeCodex(t))
				cmd.Env = env
				output, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("sandbox probe: %v: %s", err, output)
				}
				var checks map[string]bool
				if err := json.Unmarshal(output, &checks); err != nil {
					t.Fatalf("decode sandbox probe: %v: %s", err, output)
				}
				if len(checks) != 18 {
					t.Fatalf("missing sandbox checks: %v", checks)
				}
				for name, passed := range checks {
					if !passed {
						t.Errorf("sandbox check %s failed", name)
					}
				}
			})
		}
	}
}

func TestDarwinSandboxNestedProxyCannotAccessConfiguration(t *testing.T) {
	if !platformSandboxAvailable() {
		t.Skip("sandbox-exec unavailable: sandbox_apply rejected by this environment")
	}
	home := t.TempDir()
	configureProxy(t, home, fakeCodex(t), 2, "1s")
	codeHome := filepath.Join(home, "workspaces", "alice", ".codex")
	if err := os.MkdirAll(codeHome, 0700); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Put proxy state outside the shared root so only the new deny blocks it.
	child := startProxyEnv(t, home, "auth-nested", t.TempDir(), "", 0,
		[]string{"FAKE_CODEX_PROXY=" + self, "CODEX_HOME=" + codeHome}, "exec", "outer")
	if code := childExit(t, child, 5*time.Second); code != 1 {
		t.Fatalf("nested proxy exit=%d stdout=%s stderr=%s", code, child.stdout.String(), child.stderr.String())
	}
	if output := child.stdout.String(); !strings.Contains(output, "nested result=exit status 2") || !strings.Contains(output, ".codex-proxy") {
		t.Fatalf("nested proxy did not reject protected configuration access: %s", output)
	}
}

func TestDarwinExecRejectsInvalidCODEXHOMEBeforeAdmission(t *testing.T) {
	sandboxed, err := processSandboxed()
	if err != nil {
		t.Fatal(err)
	}
	if sandboxed {
		t.Skip("already inside a sandbox: no profile is generated, so CODEX_HOME is not validated")
	}
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
