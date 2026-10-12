package proxy

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func daemonTestHome(t *testing.T) string {
	t.Helper()
	// Darwin's Unix socket path limit is shorter than many t.TempDir paths.
	home, err := os.MkdirTemp("", "cp-daemon-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	return home
}

func startAppDaemon(t *testing.T, home string) *proxyChild {
	t.Helper()
	child := &proxyChild{result: make(chan error, 1)}
	child.cmd = exec.Command(testAppService(t), "--daemon")
	child.cmd.Env = append(os.Environ(), "CODEX_PROXY_TEST_SERVICE=1", "CODEX_PROXY_TEST_HOME="+home,
		"HOME=/changed/runtime/home", "USER=runtime-user")
	if !platformSandboxAvailable() {
		child.cmd.Env = append(child.cmd.Env, "CODEX_PROXY_TEST_NO_SANDBOX=1")
	}
	child.cmd.Stdout, child.cmd.Stderr = &child.stdout, &child.stderr
	if err := child.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		child.result <- child.cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = child.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = child.cmd.Process.Kill()
			<-done
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(filepath.Join(home, ".codex-proxy", appServerPIDFilename))
		info, err := os.Stat(filepath.Join(home, ".codex-proxy", appServerSocketFilename))
		if strings.TrimSpace(string(data)) == strconv.Itoa(child.cmd.Process.Pid) && err == nil && info.Mode()&os.ModeSocket != 0 {
			return child
		}
		select {
		case <-done:
			t.Fatalf("daemon exited during startup: %s", child.stderr.String())
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("daemon did not become ready")
	return nil
}

func TestAppServerDaemonIgnoresEnablementAndRejectsDuplicate(t *testing.T) {
	home := daemonTestHome(t)
	configureProxy(t, home, fakeCodex(t), 1, "5s", false)
	daemon := startAppDaemon(t, home)
	duplicate := exec.Command(testAppService(t), "--daemon")
	duplicate.Env = daemon.cmd.Env
	if data, err := duplicate.CombinedOutput(); err == nil || !strings.Contains(string(data), "daemon is already running") {
		t.Fatalf("duplicate start: %v, %s", err, data)
	}
	if err := daemon.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := childExit(t, daemon, 5*time.Second); code != 0 {
		t.Fatalf("daemon exit=%d stderr=%s", code, daemon.stderr.String())
	}
	for _, name := range []string{appServerPIDFilename, appServerSocketFilename} {
		if _, err := os.Stat(filepath.Join(home, ".codex-proxy", name)); !os.IsNotExist(err) {
			t.Fatalf("daemon left %s: %v", name, err)
		}
	}
}

func TestAppServerDaemonStopsAllConnectionsAndPreservesQuota(t *testing.T) {
	home := daemonTestHome(t)
	configureAppProxy(t, home, fakeCodex(t), 2, "5s")
	daemon := startAppDaemon(t, home)
	first, second := startApp(t, home), startApp(t, home)
	for _, client := range []*appClient{first, second} {
		client.call("turn/start", taskParams(client.thread(), "daemon task"))
	}
	awaitSlots(t, home, 2)
	execDir := t.TempDir()
	execTask := startProxy(t, home, "hold", execDir, "", 0, "exec")
	time.Sleep(100 * time.Millisecond)
	if len(startedPIDs(t, execDir)) != 0 {
		t.Fatal("exec bypassed daemon task reservations")
	}
	if err := daemon.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := childExit(t, daemon, 5*time.Second); code != 0 {
		t.Fatalf("daemon exit=%d", code)
	}
	for _, client := range []*appClient{first, second} {
		if code := client.waitExit(); code != 143 {
			t.Fatalf("client exit=%d", code)
		}
		for _, value := range startedPIDs(t, client.dir) {
			pid, _ := strconv.Atoi(value)
			if syscall.Kill(pid, 0) == nil {
				t.Fatalf("native app-server %d survived daemon stop", pid)
			}
		}
	}
	pid := awaitStarts(t, execDir, 1)[0]
	releaseNative(t, execDir, pid)
	if code := childExit(t, execTask, 5*time.Second); code != 0 {
		t.Fatalf("exec exit=%d", code)
	}
	awaitSlots(t, home, 0)
}

func TestAppServerDaemonPreservesNativeExitCode(t *testing.T) {
	home := daemonTestHome(t)
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	startAppDaemon(t, home)
	client := startApp(t, home)
	client.send("exit", "test/exit", map[string]any{})
	if code := client.waitExit(); code != 39 {
		t.Fatalf("client exit=%d", code)
	}
}

func TestAppServerDaemonClientShutdown(t *testing.T) {
	for _, mode := range []string{"eof", "term", "kill-client"} {
		t.Run(mode, func(t *testing.T) {
			home := daemonTestHome(t)
			configureAppProxy(t, home, fakeCodex(t), 1, "5s")
			startAppDaemon(t, home)
			client := startApp(t, home)
			client.call("turn/start", taskParams(client.thread(), "client shutdown task"))
			awaitSlots(t, home, 1)
			if mode == "eof" {
				if code := client.stop(); code != 0 {
					t.Fatalf("EOF exit=%d", code)
				}
			} else {
				if mode == "term" {
					_ = client.cmd.Process.Signal(syscall.SIGTERM)
				} else {
					_ = client.cmd.Process.Kill()
				}
				code := client.waitExit()
				if mode == "term" && code != 143 {
					t.Fatalf("TERM exit=%d", code)
				}
			}
			awaitSlots(t, home, 0)
			for _, value := range startedPIDs(t, client.dir) {
				pid, _ := strconv.Atoi(value)
				if syscall.Kill(pid, 0) == nil {
					t.Fatalf("native app-server %d survived %s", pid, mode)
				}
			}
			if code := startApp(t, home).stop(); code != 0 {
				t.Fatalf("daemon stopped accepting connections: exit=%d", code)
			}
		})
	}
}

func TestAppServerDaemonShutdownDuringHandshake(t *testing.T) {
	home := daemonTestHome(t)
	configureProxy(t, home, fakeCodex(t), 1, "5s")
	daemon := startAppDaemon(t, home)
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: filepath.Join(home, ".codex-proxy", appServerSocketFilename), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = daemon.cmd.Process.Signal(syscall.SIGTERM)
	if code := childExit(t, daemon, 3*time.Second); code != 0 {
		t.Fatalf("incomplete handshake blocked shutdown: exit=%d", code)
	}
}

func TestAppServerControlScript(t *testing.T) {
	home := daemonTestHome(t)
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	root := filepath.Join(t.TempDir(), "project with spaces")
	binDir, stubDir := filepath.Join(root, "bin"), filepath.Join(root, "system-tools")
	for _, dir := range []string{binDir, stubDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	scriptData, err := os.ReadFile("../../app-server-proxy.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "app-server-proxy.sh")
	if err := os.WriteFile(script, scriptData, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(testAppService(t), filepath.Join(binDir, "codex-app-server-proxy")); err != nil {
		t.Fatal(err)
	}
	stubs := map[string]string{
		"dscacheutil": "#!/bin/sh\nprintf 'dir: %s\\n' \"$CODEX_PROXY_TEST_HOME\"\n",
		"getent":      "#!/bin/sh\nprintf 'fixture:x:1:1::%s:/bin/sh\\n' \"$CODEX_PROXY_TEST_HOME\"\n",
		// Process queries are limited to this fixture's daemon. This avoids
		// depending on unrestricted ps access or inspecting unrelated sessions.
		"ps": `#!/bin/sh
target=""
format=""
while [ "$#" -gt 0 ]; do
    case "$1" in
        -p) shift; target=$1 ;;
        -o|-axo) shift; format=$1 ;;
    esac
    shift
done
if [ "$format" = 'pid=' ]; then printf '%s\n' "$target"; exit 0; fi
pid_file="$CODEX_PROXY_TEST_HOME/.codex-proxy/app-server.pid"
[ -f "$pid_file" ] || exit 1
pid=$(cat "$pid_file")
case "$format" in
    args=)
        [ "$target" = "$pid" ] || exit 1
        printf '%s --daemon\n' "$CODEX_PROXY_TEST_SCRIPT_BINARY"
        ;;
    pid=,uid=,args=)
        printf '%s %s %s --daemon\n' "$pid" "$(id -ru)" "$CODEX_PROXY_TEST_SCRIPT_BINARY"
        ;;
esac
`,
	}
	for name, data := range stubs {
		if err := os.WriteFile(filepath.Join(stubDir, name), []byte(data), 0700); err != nil {
			t.Fatal(err)
		}
	}
	physicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "CODEX_PROXY_TEST_SERVICE=1", "CODEX_PROXY_TEST_HOME="+home,
		"CODEX_PROXY_TEST_SCRIPT_BINARY="+filepath.Join(physicalRoot, "bin", "codex-app-server-proxy"),
		"PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"), "HOME=/changed/runtime/home")
	if !platformSandboxAvailable() {
		env = append(env, "CODEX_PROXY_TEST_NO_SANDBOX=1")
	}
	command := func(action string) *exec.Cmd {
		cmd := exec.Command(script, action)
		cmd.Env, cmd.Dir = env, os.TempDir()
		return cmd
	}
	t.Cleanup(func() { _ = command("stop").Run() })
	run := func(action string, code int) string {
		t.Helper()
		data, err := command(action).CombinedOutput()
		got := 0
		if err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				got = exit.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		if got != code {
			stderr, _ := os.ReadFile(filepath.Join(home, ".codex-proxy", "app-server-daemon.stderr"))
			t.Fatalf("%s exit=%d want=%d: %s daemon stderr=%s", action, got, code, data, stderr)
		}
		return string(data)
	}
	run("status", 3)
	run("start", 0)
	run("status", 0)
	pidBefore, _ := os.ReadFile(filepath.Join(home, ".codex-proxy", appServerPIDFilename))
	if output := run("start", 0); !strings.Contains(output, "already running") {
		t.Fatalf("repeated start: %s", output)
	}
	client := startApp(t, home)
	client.call("turn/start", taskParams(client.thread(), "script restart task"))
	awaitSlots(t, home, 1)
	run("restart", 0)
	if code := client.waitExit(); code != 143 {
		t.Fatalf("restarted client exit=%d", code)
	}
	awaitSlots(t, home, 0)
	pidAfter, _ := os.ReadFile(filepath.Join(home, ".codex-proxy", appServerPIDFilename))
	if string(pidBefore) == string(pidAfter) {
		t.Fatal("restart kept the old daemon pid")
	}
	newClient := startApp(t, home)
	newClient.call("turn/start", taskParams(newClient.thread(), "script stop task"))
	awaitSlots(t, home, 1)
	run("stop", 0)
	if code := newClient.waitExit(); code != 143 {
		t.Fatalf("stopped client exit=%d", code)
	}
	awaitSlots(t, home, 0)
	for _, c := range []*appClient{client, newClient} {
		for _, value := range startedPIDs(t, c.dir) {
			pid, _ := strconv.Atoi(value)
			if syscall.Kill(pid, 0) == nil {
				t.Fatalf("native app-server %d survived script shutdown", pid)
			}
		}
	}
	run("stop", 0)
	run("status", 3)
}
