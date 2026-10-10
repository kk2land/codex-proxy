package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var fakeOnce sync.Once
var fakeBinary, fakeBuildDir string
var fakeBuildErr error

func TestMain(m *testing.M) {
	code := m.Run()
	if fakeBuildDir != "" {
		_ = os.RemoveAll(fakeBuildDir)
	}
	os.Exit(code)
}

func fakeCodex(t *testing.T) string {
	t.Helper()
	fakeOnce.Do(func() {
		fakeBuildDir, fakeBuildErr = os.MkdirTemp("", "codex-proxy-native-fixture-")
		if fakeBuildErr != nil {
			return
		}
		fakeBinary = filepath.Join(fakeBuildDir, "codex-native")
		cmd := exec.Command("go", "build", "-o", fakeBinary, "testdata/fakecodex/main.go")
		output, err := cmd.CombinedOutput()
		if err != nil {
			fakeBuildErr = fmt.Errorf("build native fixture: %w: %s", err, output)
		}
	})
	if fakeBuildErr != nil {
		t.Fatal(fakeBuildErr)
	}
	return fakeBinary
}

func TestRunHelperProcess(t *testing.T) {
	if os.Getenv("CODEX_PROXY_TEST_PROCESS") != "1" {
		return
	}
	separator := 0
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i + 1
			break
		}
	}
	home := os.Getenv("CODEX_PROXY_TEST_HOME")
	os.Exit(Run(os.Args[separator:], Options{
		AccountHome: func() (string, error) { return home, nil }, PollInterval: 10 * time.Millisecond,
	}))
}

type proxyChild struct {
	cmd    *exec.Cmd
	stdout bytes.Buffer
	stderr bytes.Buffer
	result chan error
}

func startProxy(t *testing.T, home, mode, dir string, input string, exit int, args ...string) *proxyChild {
	return startProxyEnv(t, home, mode, dir, input, exit, nil, args...)
}

func startProxyEnv(t *testing.T, home, mode, dir string, input string, exit int, extraEnv []string, args ...string) *proxyChild {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := &proxyChild{result: make(chan error, 1)}
	child.cmd = exec.Command(self, append([]string{"-test.run=^TestRunHelperProcess$", "--"}, args...)...)
	codexHome := filepath.Join(home, "sandbox-user", ".codex")
	if err := os.MkdirAll(codexHome, 0700); err != nil {
		t.Fatal(err)
	}
	child.cmd.Env = append(os.Environ(), "CODEX_PROXY_TEST_PROCESS=1", "CODEX_PROXY_TEST_HOME="+home,
		"FAKE_CODEX_MODE="+mode, "FAKE_CODEX_DIR="+dir, "FAKE_CODEX_EXIT="+strconv.Itoa(exit),
		"HOME="+filepath.Join(home, "runtime-home"), "USER=runtime-user",
		"CODEX_HOME="+codexHome,
		"CODEX_PROXY_MAX_CONCURRENCY=999", "CODEX_PROXY_WAIT_TIMEOUT=0")
	child.cmd.Env = append(child.cmd.Env, extraEnv...)
	child.cmd.Stdin = strings.NewReader(input)
	// Files avoid Wait waiting for pipe EOF while an orphan native process
	// legitimately retains its inherited stdout and stderr.
	outputDir := t.TempDir()
	outFile, err := os.Create(filepath.Join(outputDir, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	errFile, err := os.Create(filepath.Join(outputDir, "stderr"))
	if err != nil {
		outFile.Close()
		t.Fatal(err)
	}
	child.cmd.Stdout, child.cmd.Stderr = outFile, errFile
	if err := child.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		err := child.cmd.Wait()
		output, _ := os.ReadFile(outFile.Name())
		diagnostic, _ := os.ReadFile(errFile.Name())
		child.stdout.Write(output)
		child.stderr.Write(diagnostic)
		child.result <- err
	}()
	t.Cleanup(func() {
		_ = child.cmd.Process.Kill()
		outFile.Close()
		errFile.Close()
		if dir != "" {
			for _, pid := range startedPIDs(t, dir) {
				id, _ := strconv.Atoi(pid)
				_ = syscall.Kill(id, syscall.SIGKILL)
			}
		}
	})
	return child
}

func childExit(t *testing.T, child *proxyChild, timeout time.Duration) int {
	t.Helper()
	select {
	case err := <-child.result:
		if err == nil {
			return 0
		}
		var exited *exec.ExitError
		if errors.As(err, &exited) {
			return exited.ExitCode()
		}
		t.Fatal(err)
	case <-time.After(timeout):
		t.Fatal("proxy did not exit within deadline")
	}
	return -1
}

func configureProxy(t *testing.T, home, binary string, limit int, timeout string) {
	t.Helper()
	dir := filepath.Join(home, ".codex-proxy")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	text := fmt.Sprintf("CODEX_PROXY_REAL_BIN=%s\nCODEX_PROXY_MAX_CONCURRENCY=%d\nCODEX_PROXY_WAIT_TIMEOUT=%s\n", binary, limit, timeout)
	// Atomic replacement exercises the documented update workflow.
	path := filepath.Join(dir, "env.next")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, filepath.Join(dir, "env")); err != nil {
		t.Fatal(err)
	}
}

func startedPIDs(t *testing.T, dir string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.started"))
	if err != nil {
		t.Fatal(err)
	}
	pids := make([]string, 0, len(paths))
	for _, path := range paths {
		pids = append(pids, strings.TrimSuffix(filepath.Base(path), ".started"))
	}
	return pids
}

func awaitStarts(t *testing.T, dir string, count int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pids := startedPIDs(t, dir)
		if len(pids) == count {
			return pids
		}
		if len(pids) > count {
			t.Fatalf("started %d native processes, expected %d", len(pids), count)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d native starts, got %v", count, startedPIDs(t, dir))
	return nil
}

func releaseNative(t *testing.T, dir, pid string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "release-"+pid), nil, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRunPassthroughAndStdio(t *testing.T) {
	for _, command := range []string{"exec", "e", "login"} {
		t.Run(command, func(t *testing.T) {
			home := t.TempDir()
			configureProxy(t, home, fakeCodex(t), 1, "1s")
			child := startProxy(t, home, "echo", "", "input\n", 37, command, "--json", "a prompt containing app-server")
			if code := childExit(t, child, 5*time.Second); code != 37 {
				t.Fatalf("exit=%d stderr=%s", code, child.stderr.String())
			}
			var output struct {
				Args  []string `json:"args"`
				Input string   `json:"stdin"`
				Home  string   `json:"home"`
				User  string   `json:"user"`
				Cwd   string   `json:"cwd"`
			}
			if err := json.Unmarshal(child.stdout.Bytes(), &output); err != nil {
				t.Fatalf("invalid or contaminated stdout: %q: %v", child.stdout.String(), err)
			}
			cwd, _ := os.Getwd()
			expectedArgs := command + "|--json|a prompt containing app-server"
			if runtime.GOOS == "darwin" && command != "login" {
				expectedArgs = command + "|--skip-git-repo-check|--sandbox|danger-full-access|--json|a prompt containing app-server"
			}
			if output.Input != "input\n" || output.Home != filepath.Join(home, "runtime-home") || output.User != "runtime-user" || output.Cwd != cwd || strings.Join(output.Args, "|") != expectedArgs {
				t.Fatalf("passthrough changed: %+v", output)
			}
			if child.stderr.String() != "fake-codex-stderr\n" {
				t.Fatalf("stderr=%q", child.stderr.String())
			}
			log, err := os.ReadFile(filepath.Join(home, ".codex-proxy", "codex-proxy.log"))
			if err != nil || !bytes.Contains(log, []byte(`"event":"finished"`)) || bytes.Contains(log, []byte("a prompt containing")) || bytes.Contains(log, []byte("fake-codex-stderr")) {
				t.Fatalf("lifecycle/privacy log: %s %v", log, err)
			}
		})
	}
}

func TestRunRejectAppServerWithoutConfiguration(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	child := startProxy(t, home, "hold", dir, "", 0, "-C", "somewhere", "app-server", "--listen", "stdio://")
	if code := childExit(t, child, 5*time.Second); code != 1 {
		t.Fatalf("exit=%d stderr=%s", code, child.stderr.String())
	}
	if len(startedPIDs(t, dir)) != 0 || child.stdout.Len() != 0 || !strings.Contains(child.stderr.String(), "app-server is not supported") {
		t.Fatal("app-server was not rejected cleanly")
	}
}

func TestRunThreeSlotsAndDynamicDrain(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	configureProxy(t, home, fakeCodex(t), 3, "5s")
	children := []*proxyChild{}
	for i := 0; i < 3; i++ {
		children = append(children, startProxy(t, home, "hold", dir, "", 0, "exec"))
	}
	pids := awaitStarts(t, dir, 3)
	configureProxy(t, home, fakeCodex(t), 1, "5s")
	waiter := startProxy(t, home, "hold", dir, "", 0, "e")
	time.Sleep(100 * time.Millisecond)
	if len(startedPIDs(t, dir)) != 3 {
		t.Fatal("new process entered while above lowered cap")
	}
	releaseNative(t, dir, pids[0])
	releaseNative(t, dir, pids[1])
	time.Sleep(120 * time.Millisecond)
	if len(startedPIDs(t, dir)) != 3 {
		t.Fatal("new process entered at lowered cap")
	}
	releaseNative(t, dir, pids[2])
	awaitStarts(t, dir, 4)
	if err := os.WriteFile(filepath.Join(dir, "release-all"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, child := range append(children, waiter) {
		if code := childExit(t, child, 5*time.Second); code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, child.stderr.String())
		}
	}
}

func TestRunConcurrentProcessesShareThreeSlots(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	configureProxy(t, home, fakeCodex(t), 3, "5s")
	var children []*proxyChild
	for i := 0; i < 6; i++ {
		children = append(children, startProxy(t, home, "hold", dir, "", 0, "exec"))
	}
	awaitStarts(t, dir, 3)
	time.Sleep(150 * time.Millisecond)
	if pids := startedPIDs(t, dir); len(pids) != 3 {
		t.Fatalf("cap=3 let %d simultaneous native processes start", len(pids))
	}
	if err := os.WriteFile(filepath.Join(dir, "release-all"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, child := range children {
		if code := childExit(t, child, 5*time.Second); code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, child.stderr.String())
		}
	}
	if len(startedPIDs(t, dir)) != 6 {
		t.Fatal("waiting tasks did not enter after slot release")
	}
}

func TestRunDynamicTimeoutAndPassthroughWithoutSlot(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	configureProxy(t, home, fakeCodex(t), 1, "5s")
	holder := startProxy(t, home, "hold", dir, "", 0, "exec")
	awaitStarts(t, dir, 1)
	waiter := startProxy(t, home, "hold", dir, "", 0, "exec")
	passthrough := startProxy(t, home, "echo", "", "", 0, "--version")
	if code := childExit(t, passthrough, 5*time.Second); code != 0 {
		t.Fatalf("passthrough waited for slot: %d", code)
	}
	time.Sleep(150 * time.Millisecond)
	configureProxy(t, home, fakeCodex(t), 1, "100ms")
	if code := childExit(t, waiter, 2*time.Second); code != 124 {
		t.Fatalf("new timeout ignored: exit=%d stderr=%s", code, waiter.stderr.String())
	}
	if len(startedPIDs(t, dir)) != 1 {
		t.Fatal("timed-out waiter started")
	}
	_ = os.WriteFile(filepath.Join(dir, "release-all"), nil, 0600)
	if code := childExit(t, holder, 5*time.Second); code != 0 {
		t.Fatalf("holder exit=%d", code)
	}
}

func TestRunKilledSupervisorKeepsNativeSlot(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	configureProxy(t, home, fakeCodex(t), 1, "150ms")
	holder := startProxy(t, home, "hold", dir, "", 0, "exec")
	pids := awaitStarts(t, dir, 1)
	nativePID, _ := strconv.Atoi(pids[0])
	t.Cleanup(func() { _ = syscall.Kill(nativePID, syscall.SIGKILL) })
	if err := holder.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-holder.result:
	case <-time.After(5 * time.Second):
		t.Fatal("killed supervisor did not exit")
	}
	if err := syscall.Kill(nativePID, 0); err != nil {
		t.Fatalf("native did not survive supervisor kill: %v", err)
	}
	waiter := startProxy(t, home, "hold", dir, "", 0, "exec")
	if code := childExit(t, waiter, 5*time.Second); code != 124 {
		t.Fatalf("orphan lost slot: exit=%d stderr=%s", code, waiter.stderr.String())
	}
	if err := syscall.Kill(nativePID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	configureProxy(t, home, fakeCodex(t), 1, "2s")
	final := startProxy(t, home, "echo", "", "", 0, "exec")
	if code := childExit(t, final, 5*time.Second); code != 0 {
		t.Fatalf("slot not released on native death: %d stderr=%s", code, final.stderr.String())
	}
}

func TestRunSignalsAndStartupFailureReleaseSlots(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	configureProxy(t, home, fakeCodex(t), 1, "2s")
	holder := startProxy(t, home, "hold", dir, "", 0, "exec")
	awaitStarts(t, dir, 1)
	waiter := startProxy(t, home, "hold", dir, "", 0, "exec")
	time.Sleep(100 * time.Millisecond)
	_ = waiter.cmd.Process.Signal(syscall.SIGTERM)
	if code := childExit(t, waiter, 5*time.Second); code != 143 {
		t.Fatalf("queued SIGTERM exit=%d", code)
	}
	_ = holder.cmd.Process.Signal(syscall.SIGTERM)
	if code := childExit(t, holder, 5*time.Second); code != 143 {
		t.Fatalf("running SIGTERM exit=%d", code)
	}
	// A native header with no valid executable body passes format screening,
	// then fails Start. Its reservation must not poison the next invocation.
	broken := filepath.Join(t.TempDir(), "broken-native")
	if err := os.WriteFile(broken, []byte{0x7f, 'E', 'L', 'F'}, 0700); err != nil {
		t.Fatal(err)
	}
	configureProxy(t, home, broken, 1, "500ms")
	failed := startProxy(t, home, "echo", "", "", 0, "exec")
	code := childExit(t, failed, 5*time.Second)
	if (runtime.GOOS != "darwin" && code != 2) || (runtime.GOOS == "darwin" && (code == 0 || code == 124)) {
		t.Fatalf("startup failure exit=%d", code)
	}
	configureProxy(t, home, fakeCodex(t), 1, "500ms")
	success := startProxy(t, home, "echo", "", "", 0, "exec")
	if code := childExit(t, success, 5*time.Second); code != 0 {
		t.Fatalf("slot leaked: %d stderr=%s", code, success.stderr.String())
	}
}
