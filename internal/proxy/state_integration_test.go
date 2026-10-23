package proxy

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestStateChecksOnlyAfterSlotReservation(t *testing.T) {
	for _, entry := range []string{"exec", "app-server"} {
		t.Run(entry, func(t *testing.T) {
			home := t.TempDir()
			configureAppProxy(t, home, fakeCodex(t), 1, "120ms")
			dir := filepath.Join(home, ".codex-proxy")
			held, _, err := testLimiter(dir, 1, time.Second).Acquire(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer held.Close()
			if entry == "exec" {
				child := startProxyEnv(t, home, "echo", "", "", 0, []string{"CODEX_PROXY_TEST_TASK_INTERVAL=15"}, "exec")
				if code := childExit(t, child, 5*time.Second); code != 124 {
					t.Fatalf("slot timeout exit=%d: %s", code, child.stderr.String())
				}
			} else {
				client := startAppEnv(t, home, []string{"CODEX_PROXY_TEST_TASK_INTERVAL=15"})
				client.send("waiting", "turn/start", taskParams(client.thread(), "slot waiter"))
				if code := string(rpcObject(client.response("waiting"), "error")["code"]); code != "-32001" {
					t.Fatalf("slot timeout code=%s", code)
				}
				if code := client.stop(); code != 0 {
					t.Fatalf("service exit=%d", code)
				}
			}
			for _, name := range []string{stateLockFilename, stateFilename} {
				if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
					t.Fatalf("slot waiter accessed %s: %v", name, err)
				}
			}
		})
	}
}

func TestStateExecWaitHoldsSlotAndSignalCancels(t *testing.T) {
	home, nativeDir := t.TempDir(), t.TempDir()
	configureProxy(t, home, fakeCodex(t), 1, "120ms")
	dir := filepath.Join(home, ".codex-proxy")
	stamp := time.Now().Unix()
	writeTaskState(t, dir, stamp)
	waiter := startProxyEnv(t, home, "hold", nativeDir, "", 0, []string{"CODEX_PROXY_TEST_TASK_INTERVAL=15"}, "e")
	awaitSlots(t, home, 1)
	awaitStateLock(t, dir)
	blocked := startProxy(t, home, "echo", nativeDir, "", 0, "exec")
	if code := childExit(t, blocked, 5*time.Second); code != 124 {
		t.Fatalf("interval waiter did not occupy slot: exit=%d", code)
	}
	if len(startedPIDs(t, nativeDir)) != 0 || readTaskState(t, dir) != stamp {
		t.Fatal("interval waiter submitted prematurely")
	}
	if err := waiter.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := childExit(t, waiter, 5*time.Second); code != 143 {
		t.Fatalf("interval wait ignored SIGTERM: exit=%d", code)
	}
	awaitSlots(t, home, 0)
	if readTaskState(t, dir) != stamp {
		t.Fatal("cancellation updated submission timestamp")
	}
	child := startProxy(t, home, "echo", nativeDir, "", 0, "exec")
	if code := childExit(t, child, 5*time.Second); code != 0 {
		t.Fatalf("cancellation leaked lock: exit=%d: %s", code, child.stderr.String())
	}
}

func TestStateConcurrentExecProcessesAreSpaced(t *testing.T) {
	home, nativeDir := t.TempDir(), t.TempDir()
	configureProxy(t, home, fakeCodex(t), 3, "5s")
	dir := filepath.Join(home, ".codex-proxy")
	var children []*proxyChild
	for i := 0; i < 3; i++ {
		children = append(children, startProxyEnv(t, home, "hold", nativeDir, "", 0,
			[]string{"CODEX_PROXY_TEST_TASK_INTERVAL=3"}, "exec"))
	}
	awaitSlots(t, home, 3)
	var previous int64
	for i := 1; i <= 3; i++ {
		awaitStarts(t, nativeDir, i)
		stamp := readTaskState(t, dir)
		if previous != 0 && stamp-previous < 3 {
			t.Fatalf("parallel tasks were not spaced: %d -> %d", previous, stamp)
		}
		previous = stamp
	}
	if err := os.WriteFile(filepath.Join(nativeDir, "release-all"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, child := range children {
		if code := childExit(t, child, 5*time.Second); code != 0 {
			t.Fatalf("exec exit=%d: %s", code, child.stderr.String())
		}
	}
	awaitSlots(t, home, 0)
}

func TestStateSharedAcrossExecAndAppServer(t *testing.T) {
	t.Run("defaults", func(t *testing.T) { testStateSharedAcrossExecAndAppServer(t, false) })
	t.Run("custom paths", func(t *testing.T) { testStateSharedAcrossExecAndAppServer(t, true) })
}

func testStateSharedAcrossExecAndAppServer(t *testing.T, customPaths bool) {
	t.Helper()
	home, nativeDir := t.TempDir(), t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 2, "120ms")
	dir := filepath.Join(home, ".codex-proxy")
	if customPaths {
		configPath := filepath.Join(dir, "env")
		data, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		dir = t.TempDir()
		data = append(data, []byte("CODEX_PROXY_STATE_LOCK_PATH="+strconv.Quote(filepath.Join(dir, stateLockFilename))+"\n"+
			"CODEX_PROXY_STATE_PATH="+strconv.Quote(filepath.Join(dir, stateFilename))+"\n")...)
		if err := os.WriteFile(configPath, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	execTask := startProxyEnv(t, home, "hold", nativeDir, "", 0,
		[]string{"CODEX_PROXY_TEST_TASK_INTERVAL=3"}, "exec")
	awaitStarts(t, nativeDir, 1)
	first := readTaskState(t, dir)
	client := startAppEnv(t, home, []string{"CODEX_PROXY_TEST_TASK_INTERVAL=3"})
	thread := client.thread()
	if readTaskState(t, dir) != first {
		t.Fatal("starting idle app-server changed submission timestamp")
	}
	client.send("new-turn", "turn/start", taskParams(thread, "app task"))
	awaitSlots(t, home, 2)
	awaitStateLock(t, dir)
	started := time.Now()
	client.call("model/list", map[string]any{})
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("query blocked by interval wait: %v", elapsed)
	}
	if response := client.response("new-turn"); response["error"] != nil {
		t.Fatalf("app submission failed: %s", response["error"])
	}
	second := readTaskState(t, dir)
	if second-first < 3 {
		t.Fatalf("app-server bypassed exec timestamp: %d -> %d", first, second)
	}
	client.send("cancel-supplement", "turn/start", taskParams(thread, "cancel supplement"))
	awaitStateLock(t, dir)
	client.send(nil, "$/cancelRequest", map[string]any{"id": "cancel-supplement"})
	if code := string(rpcObject(client.response("cancel-supplement"), "error")["code"]); code != "-32800" {
		t.Fatalf("supplement cancellation code=%s", code)
	}
	awaitSlots(t, home, 2)
	if readTaskState(t, dir) != second {
		t.Fatal("canceled supplement changed timestamp")
	}
	client.call("turn/start", taskParams(thread, "next supplement"))
	third := readTaskState(t, dir)
	if third-second < 3 {
		t.Fatalf("existing slot bypassed interval: %d -> %d", second, third)
	}
	// Interrupt must cancel the waiting supplement and the already running turn.
	client.send("interrupt-supplement", "turn/start", taskParams(thread, "pending supplement"))
	awaitStateLock(t, dir)
	client.call("turn/interrupt", map[string]any{"threadId": thread})
	if code := string(rpcObject(client.response("interrupt-supplement"), "error")["code"]); code != "-32800" {
		t.Fatalf("interrupt did not cancel supplement: %s", code)
	}
	awaitSlots(t, home, 1)
	if readTaskState(t, dir) != third {
		t.Fatal("interruption changed timestamp")
	}
	if code := client.stop(); code != 0 {
		t.Fatalf("service exit=%d", code)
	}
	if err := os.WriteFile(filepath.Join(nativeDir, "release-all"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if code := childExit(t, execTask, 5*time.Second); code != 0 {
		t.Fatalf("exec exit=%d", code)
	}
	awaitSlots(t, home, 0)
}

func TestStateAppServerDisconnectCancelsWait(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "120ms")
	dir := filepath.Join(home, ".codex-proxy")
	stamp := time.Now().Unix()
	writeTaskState(t, dir, stamp)
	client := startAppEnv(t, home, []string{"CODEX_PROXY_TEST_TASK_INTERVAL=15"})
	client.send("waiting", "turn/start", taskParams(client.thread(), "pending task"))
	awaitSlots(t, home, 1)
	awaitStateLock(t, dir)
	if code := client.stop(); code != 0 {
		t.Fatalf("disconnect ignored interval wait: exit=%d", code)
	}
	awaitSlots(t, home, 0)
	if readTaskState(t, dir) != stamp {
		t.Fatal("disconnect updated timestamp")
	}
	child := startProxy(t, home, "echo", "", "", 0, "exec")
	if code := childExit(t, child, 5*time.Second); code != 0 {
		t.Fatalf("disconnect leaked state lock: exit=%d", code)
	}
}

func TestStateStateFailureRejectsTaskAndReleasesSlot(t *testing.T) {
	home, nativeDir := t.TempDir(), t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "1s")
	dir := filepath.Join(home, ".codex-proxy")
	if err := os.WriteFile(filepath.Join(dir, stateFilename), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	child := startProxy(t, home, "echo", nativeDir, "", 0, "exec")
	if code := childExit(t, child, 5*time.Second); code != 2 {
		t.Fatalf("invalid state exit=%d", code)
	}
	if len(startedPIDs(t, nativeDir)) != 0 {
		t.Fatal("invalid state started exec")
	}
	awaitSlots(t, home, 0)
	client := startApp(t, home)
	thread := client.thread()
	client.send("bad-state", "turn/start", taskParams(thread, "rejected task"))
	if code := string(rpcObject(client.response("bad-state"), "error")["code"]); code != "-32000" {
		t.Fatalf("invalid state RPC code=%s", code)
	}
	awaitSlots(t, home, 0)
	client.call("model/list", map[string]any{})
	writeTaskState(t, dir, 1)
	client.call("turn/start", taskParams(thread, "repaired task"))
	client.finish(thread)
	awaitSlots(t, home, 0)
	if code := client.stop(); code != 0 {
		t.Fatalf("service exit=%d", code)
	}
}
