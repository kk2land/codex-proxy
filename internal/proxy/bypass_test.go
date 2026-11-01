package proxy

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRunBypassReplacesProcessWithoutAdmission(t *testing.T) {
	for _, entry := range []string{"exec", "e", "app-server"} {
		for _, blocked := range []string{"slots", "submission interval"} {
			t.Run(entry+"/"+blocked, func(t *testing.T) {
				home, nativeDir := t.TempDir(), t.TempDir()
				configureProxy(t, home, fakeCodex(t), 1, "80ms", false)
				dir := filepath.Join(home, ".codex-proxy")
				stamp := time.Now().Add(time.Hour).Unix()
				writeTaskState(t, dir, stamp)
				if blocked == "slots" {
					holdAllSlots(t, home, 1)
				}
				args := []string{"-c", `model_reasoning_effort="high"`, entry, "--future-option", "a prompt containing app-server"}
				child := startProxyEnv(t, home, "echo", nativeDir, "raw input\n", 37,
					[]string{"CODEX_PROXY_BYPASS=1", "CODEX_PROXY_TEST_TASK_INTERVAL=15"}, args...)
				if code := childExit(t, child, 5*time.Second); code != 37 {
					t.Fatalf("exit=%d stderr=%s", code, child.stderr.String())
				}
				if pids := startedPIDs(t, nativeDir); !reflect.DeepEqual(pids, []string{strconv.Itoa(child.cmd.Process.Pid)}) {
					t.Fatalf("native did not replace proxy PID %d: %v", child.cmd.Process.Pid, pids)
				}
				var output struct {
					Args                   []string
					Stdin, Home, User, Cwd string
				}
				if err := json.Unmarshal(child.stdout.Bytes(), &output); err != nil {
					t.Fatalf("invalid stdout: %q: %v", child.stdout.String(), err)
				}
				wantArgs := append([]string(nil), args...)
				if entry == "app-server" {
					wantArgs = append(wantArgs, "--listen", "stdio://", "-c", `sandbox_mode="danger-full-access"`, "-c", `approval_policy="never"`, "-c", "thread_unload_delay_secs=0")
				} else if runtime.GOOS == "darwin" {
					wantArgs = append(append(append([]string(nil), args[:3]...), "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox"), args[3:]...)
				}
				cwd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(output.Args, wantArgs) || output.Stdin != "raw input\n" || output.Home != filepath.Join(home, "runtime-home") || output.User != "runtime-user" || output.Cwd != cwd {
					t.Fatalf("passthrough changed: %+v; want args=%q", output, wantArgs)
				}
				if child.stderr.String() != "fake-codex-stderr\n" {
					t.Fatalf("stderr=%q", child.stderr.String())
				}
				if got := readTaskState(t, dir); got != stamp {
					t.Fatalf("bypass changed submission timestamp: %d -> %d", stamp, got)
				}
				if _, err := os.Stat(filepath.Join(dir, stateLockFilename)); !os.IsNotExist(err) {
					t.Fatalf("bypass accessed submission lock: %v", err)
				}
			})
		}
	}
}

func TestRunBypassRequiresProcessEnvironmentValueOne(t *testing.T) {
	for _, value := range []string{"", "0", "true"} {
		for _, entry := range []string{"exec", "app-server"} {
			t.Run(entry+"/value="+value, func(t *testing.T) {
				home, nativeDir := t.TempDir(), t.TempDir()
				configureProxy(t, home, fakeCodex(t), 1, "80ms", false)
				path := filepath.Join(home, ".codex-proxy", "env")
				config, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(config, []byte("CODEX_PROXY_BYPASS=1\n")...), 0600); err != nil {
					t.Fatal(err)
				}
				holdAllSlots(t, home, 1)
				child := startProxyEnv(t, home, "echo", nativeDir, "", 0, []string{"CODEX_PROXY_BYPASS=" + value}, entry)
				wantCode := 124
				if entry == "app-server" {
					wantCode = 1
				}
				if code := childExit(t, child, 5*time.Second); code != wantCode {
					t.Fatalf("exit=%d want %d stderr=%s", code, wantCode, child.stderr.String())
				}
				if pids := startedPIDs(t, nativeDir); len(pids) != 0 {
					t.Fatalf("bypass unexpectedly enabled: native PIDs=%v", pids)
				}
			})
		}
	}
}

func TestRunBypassAppServerLaunchModes(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want []string
	}{
		{"listener", []string{"app-server", "--listen=ws://127.0.0.1:4500"}, []string{"app-server", "--listen=ws://127.0.0.1:4500", "-c", `sandbox_mode="danger-full-access"`, "-c", `approval_policy="never"`, "-c", "thread_unload_delay_secs=0"}},
		{"tooling", []string{"app-server", "generate-json-schema", "--out", "schema"}, []string{"app-server", "generate-json-schema", "--out", "schema"}},
		{"proxy", []string{"app-server", "proxy", "--sock", "socket with spaces"}, []string{"app-server", "proxy", "--sock", "socket with spaces", "-c", `sandbox_mode="danger-full-access"`, "-c", `approval_policy="never"`, "-c", "thread_unload_delay_secs=0"}},
		{"daemon", []string{"app-server", "-c", "feature=value", "daemon", "start"}, []string{"app-server", "-c", "feature=value", "daemon", "start", "-c", `sandbox_mode="danger-full-access"`, "-c", `approval_policy="never"`, "-c", "thread_unload_delay_secs=0"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			configureProxy(t, home, fakeCodex(t), 1, "80ms", false)
			// An empty auth pool must not reject direct execution. Its existing
			// file-auth flag is retained without choosing or rotating accounts.
			path := filepath.Join(home, ".codex-proxy", "env")
			config, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(config, []byte("CODEX_PROXY_AUTH_DIR="+strconv.Quote(t.TempDir())+"\n")...), 0600); err != nil {
				t.Fatal(err)
			}
			child := startProxyEnv(t, home, "echo", "", "unparsed protocol input\n", 0, []string{"CODEX_PROXY_BYPASS=1"}, test.args...)
			if code := childExit(t, child, 5*time.Second); code != 0 {
				t.Fatalf("exit=%d stderr=%s", code, child.stderr.String())
			}
			var output struct {
				Args  []string
				Stdin string
			}
			if err := json.Unmarshal(child.stdout.Bytes(), &output); err != nil {
				t.Fatal(err)
			}
			want := test.want
			if test.name != "tooling" {
				want = append([]string{"-c", `cli_auth_credentials_store="file"`}, want...)
			}
			if !reflect.DeepEqual(output.Args, want) || output.Stdin != "unparsed protocol input\n" {
				t.Fatalf("direct app-server changed arguments/input: %+v; want args=%q", output, want)
			}
			if output, err := os.ReadFile(filepath.Join(home, ".codex-proxy", stateFilename)); !os.IsNotExist(err) {
				t.Fatalf("direct app-server touched state: %s: %v", bytes.TrimSpace(output), err)
			}
		})
	}
}

func TestRunBypassKeepsExistingAuthSandboxGrant(t *testing.T) {
	for _, entry := range []string{"exec", "app-server"} {
		for _, credential := range []string{"pool account", "local login", "outside pool", "missing"} {
			t.Run(entry+"/"+credential, func(t *testing.T) {
				base := t.TempDir()
				home := filepath.Join(base, "system")
				codeHome := filepath.Join(base, "shared", "alice", ".codex")
				authDir := filepath.Join(base, "shared", "accounts")
				for _, dir := range []string{home, codeHome, authDir} {
					if err := os.MkdirAll(dir, 0700); err != nil {
						t.Fatal(err)
					}
				}
				// Canonical paths are what the sandbox and auth pool use.
				codeHome = canonicalSandboxTestPath(t, codeHome)
				authDir = canonicalSandboxTestPath(t, authDir)
				selected := filepath.Join(authDir, "auth0_alice.json")
				other := filepath.Join(authDir, "auth1_bob.json")
				for _, path := range []string{selected, other} {
					if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				link := filepath.Join(codeHome, "auth.json")
				target := selected
				switch credential {
				case "pool account":
					if err := os.Symlink(target, link); err != nil {
						t.Fatal(err)
					}
				case "local login":
					if err := os.WriteFile(link, []byte("{}"), 0600); err != nil {
						t.Fatal(err)
					}
				case "outside pool":
					target = filepath.Join(base, "shared", "auth0_foreign.json")
					if err := os.WriteFile(target, []byte("{}"), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, link); err != nil {
						t.Fatal(err)
					}
				}
				alias := filepath.Join(base, "accounts-alias")
				if err := os.Symlink(authDir, alias); err != nil {
					t.Fatal(err)
				}
				configureAuthProxySlots(t, home, alias, 1)
				holdAllSlots(t, home, 1)
				dir := filepath.Join(home, ".codex-proxy")
				// A broken state must not prevent direct execution or get repaired.
				state := []byte("invalid proxy state")
				if err := os.WriteFile(filepath.Join(dir, stateFilename), state, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, sandboxRulesFilename), []byte("(deny network-outbound)"), 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("CODEX_PROXY_BYPASS", "1")
				t.Setenv("CODEX_HOME", codeHome)
				calls := 0
				code := Run([]string{entry}, Options{
					AccountHome: func() (string, error) { return home, nil },
					launchOverride: func(spec launchSpec) launchSpec {
						// Exercise the unsandboxed-parent branch even when this test
						// runs inside a sandbox, without needing sandbox-exec to work.
						launch, err := sandboxLaunch(codeHome, dir, spec.args)
						if err != nil {
							t.Fatal(err)
						}
						return launch
					},
					exec: func(path string, args, env []string) error {
						calls++
						if path != "/usr/bin/sandbox-exec" || len(args) < 4 || args[1] != "-p" {
							t.Fatalf("sandbox wrapper missing: %s %q", path, args)
						}
						profile := args[2]
						grant := "(literal " + quoteSandboxPath(selected) + ")"
						if got := strings.Contains(profile, grant); got != (credential == "pool account") {
							t.Errorf("selected auth grant=%t for %s: %s", got, credential, profile)
						}
						if strings.Contains(profile, quoteSandboxPath(other)) || strings.Contains(profile, "(subpath "+quoteSandboxPath(authDir)+")") {
							t.Error("sandbox grants access to unselected accounts")
						}
						if credential == "outside pool" && strings.Contains(profile, quoteSandboxPath(target)) {
							t.Error("sandbox grants access outside the configured account pool")
						}
						if !strings.Contains(profile, "(deny network-outbound)") {
							t.Error("administrator rules were lost")
						}
						assertProxyConfigReadOnly(t, profile, canonicalSandboxTestPath(t, dir), dir)
						if environmentValue(env, "CODEX_PROXY_BYPASS") != "1" || environmentValue(env, "CODEX_HOME") != codeHome {
							t.Error("exec environment changed")
						}
						return nil
					},
				})
				if code != 0 || calls != 1 {
					t.Fatalf("exit=%d exec calls=%d", code, calls)
				}
				if got, err := os.ReadFile(filepath.Join(dir, stateFilename)); err != nil || !bytes.Equal(got, state) {
					t.Fatalf("bypass touched proxy state: %s: %v", got, err)
				}
				if _, err := os.Stat(filepath.Join(dir, stateLockFilename)); !os.IsNotExist(err) {
					t.Fatalf("bypass accessed state lock: %v", err)
				}
				if credential == "pool account" || credential == "outside pool" {
					if got, err := os.Readlink(link); err != nil || got != target {
						t.Fatalf("bypass changed auth.json: %q: %v", got, err)
					}
				}
			})
		}
	}
}
