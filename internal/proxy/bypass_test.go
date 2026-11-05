package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
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
			if !reflect.DeepEqual(output.Args, want) || output.Stdin != "unparsed protocol input\n" {
				t.Fatalf("direct app-server changed arguments/input: %+v; want args=%q", output, want)
			}
			if output, err := os.ReadFile(filepath.Join(home, ".codex-proxy", stateFilename)); !os.IsNotExist(err) {
				t.Fatalf("direct app-server touched state: %s: %v", bytes.TrimSpace(output), err)
			}
		})
	}
}

// holdAllSlots reserves every global slot from the test process.
func holdAllSlots(t *testing.T, home string, slots int) *Lease {
	t.Helper()
	locksDir := filepath.Join(home, ".codex-proxy", "locks")
	if err := os.MkdirAll(locksDir, 0700); err != nil {
		t.Fatal(err)
	}
	lease, err := reserveSlot(context.Background(), locksDir, slots)
	if err != nil {
		t.Fatal(err)
	}
	if lease == nil {
		t.Fatalf("could not reserve %d slot(s)", slots)
	}
	t.Cleanup(func() {
		if err := lease.Close(); err != nil {
			t.Errorf("close test slot reservation: %v", err)
		}
	})
	return lease
}
