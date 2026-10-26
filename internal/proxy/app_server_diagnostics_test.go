package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestAppServerPolicyRejectsEveryErrorPayload(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload json.RawMessage
		want    string
	}{
		{"empty object", rpcValue(map[string]any{}), `error_payload={}`},
		{"string", rpcValue("worker unavailable"), "worker unavailable"},
		{"array", rpcValue([]any{"unexpected worker failure"}), "unexpected worker failure"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, _ := newNotificationSession(t)
			if err := s.client(rpcValue(map[string]any{"id": "policy-error-shape", "method": "thread/start", "params": map[string]any{}})); err != nil {
				t.Fatal(err)
			}
			reviewNative(t, s, rpcMessage{"id": controlRequestID(t, s, "thread/start", "policy-error-shape"), "result": rpcValue(map[string]any{"thread": map[string]any{"id": "policy-thread"}})})
			reviewNative(t, s, rpcMessage{"id": controlRequestID(t, s, "thread/settings/update", ""), "error": test.payload})
			if s.policyReady["policy-thread"] {
				t.Fatal("native policy error was treated as successful policy application")
			}
			response := controlResponse(t, s, "policy-error-shape")
			if string(rpcObject(response, "error")["code"]) != "-32000" {
				t.Fatalf("native policy error not rejected: %s", rpcValue(response))
			}
			log := errorTestLogs(t, s)
			if !strings.Contains(log, test.want) {
				t.Fatalf("original error payload lost from log: %s", log)
			}
			if !strings.Contains(log, `status=failed`) {
				t.Fatalf("non-standard error payload logged as success: %s", log)
			}
		})
	}
}

func TestAppServerBootstrapCauseOnStderr(t *testing.T) {
	stderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stderr.Close() })
	cause := errors.New("account lookup failed: directory service unavailable")
	code := runAppServer(Options{Stderr: stderr, AccountHome: func() (string, error) { return "", cause }}, appServerStartup{Args: []string{"app-server"}, Cwd: t.TempDir()}, "bootstrap-probe", nil)
	if code != 2 {
		t.Fatalf("bootstrap exit changed: %d", code)
	}
	data, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), cause.Error()) {
		t.Fatalf("bootstrap cause missing from stderr: %s", data)
	}
}

func TestAppServerStartupConfigCauseInLog(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".codex-proxy")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "env"), []byte("CODEX_PROXY_MAX_CONCURRENCY=invalid-integer\n"), 0600); err != nil {
		t.Fatal(err)
	}
	stderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stderr.Close() })
	code := runAppServer(Options{Stderr: stderr, AccountHome: func() (string, error) { return home, nil }}, appServerStartup{Args: []string{"app-server"}, Cwd: t.TempDir()}, "config-cause-probe", nil)
	if code != 2 {
		t.Fatalf("config failure exit changed: %d", code)
	}
	data, err := os.ReadFile(filepath.Join(dir, appServerLogFilename))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "CODEX_PROXY_MAX_CONCURRENCY") || !strings.Contains(string(data), "invalid syntax") {
		t.Fatalf("configuration cause absent from JSON log: %s", data)
	}
	if stderrData, _ := os.ReadFile(stderr.Name()); !strings.Contains(string(stderrData), "invalid syntax") {
		t.Fatalf("configuration cause absent from stderr: %s", stderrData)
	}
}

func TestAppServerStartupPathFailuresCarryCause(t *testing.T) {
	t.Run("non-absolute home", func(t *testing.T) {
		stderr, err := os.CreateTemp(t.TempDir(), "stderr")
		if err != nil {
			t.Fatal(err)
		}
		defer stderr.Close()
		code := runAppServer(Options{Stderr: stderr, AccountHome: func() (string, error) { return "relative/home", nil }}, appServerStartup{Args: []string{"app-server"}, Cwd: t.TempDir()}, "bootstrap", nil)
		if code != 2 {
			t.Fatalf("exit=%d", code)
		}
		data, _ := os.ReadFile(stderr.Name())
		if !strings.Contains(string(data), "not absolute") {
			t.Fatalf("missing path context: %s", data)
		}
	})
	t.Run("state directory", func(t *testing.T) {
		home := t.TempDir()
		if err := os.WriteFile(filepath.Join(home, ".codex-proxy"), []byte("occupied"), 0600); err != nil {
			t.Fatal(err)
		}
		stderr, err := os.CreateTemp(t.TempDir(), "stderr")
		if err != nil {
			t.Fatal(err)
		}
		defer stderr.Close()
		code := runAppServer(Options{Stderr: stderr, AccountHome: func() (string, error) { return home, nil }}, appServerStartup{Args: []string{"app-server"}, Cwd: t.TempDir()}, "bootstrap", nil)
		if code != 2 {
			t.Fatalf("exit=%d", code)
		}
		data, _ := os.ReadFile(stderr.Name())
		if !strings.Contains(string(data), "cannot open state directory") || !strings.Contains(string(data), "not a directory") {
			t.Fatalf("missing state directory cause: %s", data)
		}
	})
	t.Run("invalid working directory", func(t *testing.T) {
		home := t.TempDir()
		if err := os.MkdirAll(filepath.Join(home, ".codex-proxy"), 0700); err != nil {
			t.Fatal(err)
		}
		stderr, err := os.CreateTemp(t.TempDir(), "stderr")
		if err != nil {
			t.Fatal(err)
		}
		defer stderr.Close()
		code := runAppServer(Options{Stderr: stderr, AccountHome: func() (string, error) { return home, nil }}, appServerStartup{Args: []string{"app-server"}, Cwd: "relative"}, "bootstrap", nil)
		if code != 2 {
			t.Fatalf("exit=%d", code)
		}
		data, _ := os.ReadFile(filepath.Join(home, ".codex-proxy", appServerLogFilename))
		if !strings.Contains(string(data), `stage="invalid app-server working directory"`) || !strings.Contains(string(data), "relative") {
			t.Fatalf("missing working directory cause: %s", data)
		}
	})
}

func TestAppServerConfigAndStateCausesRemainWrapped(t *testing.T) {
	t.Run("boolean", func(t *testing.T) {
		_, err := appServerEnabledValue(map[string]string{"CODEX_PROXY_APP_SERVER_ENABLED": "invalid-bool"})
		if !errors.Is(err, strconv.ErrSyntax) {
			t.Fatalf("boolean parser cause lost: %v", err)
		}
	})
	t.Run("integer", func(t *testing.T) {
		_, err := configFromValues(map[string]string{"CODEX_PROXY_MAX_CONCURRENCY": "invalid-int"}, "")
		var num *strconv.NumError
		if !errors.As(err, &num) {
			t.Fatalf("integer parser cause lost: %v", err)
		}
	})
	t.Run("duration", func(t *testing.T) {
		_, err := configFromValues(map[string]string{"CODEX_PROXY_REAL_BIN": "/missing", "CODEX_PROXY_WAIT_TIMEOUT": "10"}, "")
		if err == nil || !strings.Contains(err.Error(), "missing unit in duration") || !strings.Contains(err.Error(), "CODEX_PROXY_WAIT_TIMEOUT") {
			t.Fatalf("duration parser cause lost: %v", err)
		}
	})
	t.Run("state JSON", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, stateFilename), []byte("{broken"), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := newStateStore(dir, Config{}, 0, nil).readState()
		var syntax *json.SyntaxError
		if !errors.As(err, &syntax) {
			t.Fatalf("state JSON cause lost: %v", err)
		}
	})
	t.Run("timestamp type", func(t *testing.T) {
		_, err := (proxyState{lastCodexTaskKey: rpcValue("bad timestamp")}).lastCodexTaskAt()
		var mismatch *json.UnmarshalTypeError
		if !errors.As(err, &mismatch) {
			t.Fatalf("timestamp parser cause lost: %v", err)
		}
	})
	t.Run("native header short read", func(t *testing.T) {
		dir := t.TempDir()
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "short")
		if err := os.WriteFile(path, []byte("ELF"), 0700); err != nil {
			t.Fatal(err)
		}
		err = validateNativeBinary(path, self)
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("short header cause lost: %v", err)
		}
	})
	t.Run("quoted escape", func(t *testing.T) {
		_, err := parseEnvValue(`"\q"`)
		if !errors.Is(err, strconv.ErrSyntax) {
			t.Fatalf("escape cause lost: %v", err)
		}
	})
}

func TestWriteRPCErrorsCarryOperationAndCause(t *testing.T) {
	err := writeRPC(shortWriter{}, rpcMessage{"id": rpcValue("1")})
	if !errors.Is(err, io.ErrShortWrite) || !strings.Contains(err.Error(), "write JSON-RPC message") {
		t.Fatalf("short write cause lost: %v", err)
	}
	err = writeRPC(io.Discard, rpcMessage{"bad": json.RawMessage("{not json")})
	if err == nil || !strings.Contains(err.Error(), "encode JSON-RPC message") {
		t.Fatalf("encode cause lost: %v", err)
	}
	_, err = decodeRPC([]byte("{broken"))
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) || !strings.Contains(err.Error(), "invalid JSON-RPC message") {
		t.Fatalf("decode cause lost: %v", err)
	}
	if _, err := decodeRPC([]byte("null")); err == nil || !strings.Contains(err.Error(), "null object") {
		t.Fatalf("null decode not rejected: %v", err)
	}
}

type shortWriter struct{}

func (shortWriter) Write(data []byte) (int, error) { return len(data) - 1, nil }

func TestAppServerGuardNormalReleaseIsQuiet(t *testing.T) {
	s, _ := newNotificationSession(t)
	events := make(chan appServerEvent, 4)
	s.events = events
	s.selfBin, s.childPID = os.Args[0], 999999
	guard, err := s.startGuard(testLease(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := guard.release(); err != nil {
		t.Fatalf("normal guardian release reported failure: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("normal guardian release emitted an error event: %v", <-events)
	}
}

func TestAppServerGuardReleaseReportsControlFailure(t *testing.T) {
	s, _ := newNotificationSession(t)
	s.events = make(chan appServerEvent, 4)
	s.selfBin, s.childPID = os.Args[0], 999999
	guard, err := s.startGuard(testLease(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := guard.control.Close(); err != nil {
		t.Fatal(err)
	}
	if err := guard.release(); err == nil || !strings.Contains(err.Error(), "write guardian release") {
		t.Fatalf("guardian control failure not reported: %v", err)
	}
}

func TestAppServerReleaseLogsLeaseCloseFailure(t *testing.T) {
	s, _ := newNotificationSession(t)
	work := s.work("thread")
	lease := testLease(t)
	if err := lease.file.Close(); err != nil {
		t.Fatal(err)
	}
	work.lease = lease
	s.release(work)
	log := errorTestLogs(t, s)
	if !strings.Contains(log, "close task slot") || !strings.Contains(log, "file already closed") {
		t.Fatalf("lease close failure not diagnosed: %s", log)
	}
}

func TestAppServerNormalShutdownDoesNotReportWaitFailure(t *testing.T) {
	for _, mode := range []string{"eof", "native-exit"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			configureAppProxy(t, home, fakeCodex(t), 1, "5s")
			client := startApp(t, home)
			thread := client.thread()
			client.call("turn/start", taskParams(thread, "shutdown task"))
			awaitSlots(t, home, 1)
			if mode == "native-exit" {
				client.send("exit", "test/exit", map[string]any{})
				if code := client.waitExit(); code != 39 {
					t.Fatalf("native exit=%d", code)
				}
			} else if code := client.stop(); code != 0 {
				t.Fatalf("exit=%d", code)
			}
			awaitSlots(t, home, 0)
			data, err := os.ReadFile(filepath.Join(home, ".codex-proxy", appServerLogFilename))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "wait for real app-server") {
				t.Fatalf("expected shutdown reported as wait failure: %s", data)
			}
		})
	}
}

// TestAppServerUnexpectedChildWaitFailure covers a child reaped outside
// cmd.Wait, which yields a system ECHILD instead of *exec.ExitError. It must be
// diagnosed regardless of how shutdown started, including on a single CPU
// where pipe EOF is observed before the Wait goroutine runs.
func TestAppServerUnexpectedChildWaitFailure(t *testing.T) {
	if _, err := os.Stat("/usr/bin/true"); err != nil {
		t.Skip("native true utility unavailable")
	}
	home := t.TempDir()
	dir := filepath.Join(home, ".codex-proxy")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "env"), []byte("CODEX_PROXY_REAL_BIN=/usr/bin/true\nCODEX_PROXY_APP_SERVER_ENABLED=true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	input, held, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { input.Close(); held.Close() })
	stderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stderr.Close() })
	output, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { output.Close() })
	opts := Options{Stdin: input, Stdout: output, Stderr: stderr, AccountHome: func() (string, error) { return home, nil }, launchOverride: func(spec launchSpec) launchSpec { spec.profile = ""; return spec }}
	startup := appServerStartup{Args: []string{"app-server"}, Cwd: home, Env: []string{"CODEX_HOME=" + filepath.Join(t.TempDir(), "runtime-user", ".codex")}}
	code := runAppServer(opts, startup, "wait-cause-probe", func(pid int, _ bool) {
		var status syscall.WaitStatus
		if _, err := syscall.Wait4(pid, &status, 0, nil); err != nil {
			t.Fatalf("reap exact test child: %v", err)
		}
	})
	if code != 2 {
		t.Errorf("unexpected child Wait failure reported successful/native exit: %d", code)
	}
	data, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(string(data)), "wait") {
		t.Errorf("actual child Wait failure absent from stderr: %s", data)
	}
}
