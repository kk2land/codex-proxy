package proxy

import (
	"testing"
	"time"
)

func processParams(handle, command string) map[string]any {
	return map[string]any{"processHandle": handle, "command": []string{command}}
}

func TestAppServerProcessesPreventIdleUntilAllExit(t *testing.T) {
	home := t.TempDir()
	configureAppIdle(t, home, "400ms")
	client := startApp(t, home)
	client.call("process/spawn", processParams("first", "hold"))
	client.call("process/spawn", processParams("second", "hold"))
	awaitSlots(t, home, 0) // Explicit processes do not consume model task slots.

	client.send("duplicate", "process/spawn", processParams("second", "hold"))
	if client.response("duplicate")["error"] == nil {
		t.Fatal("fixture accepted a duplicate live process handle")
	}
	client.call("test/processExit", map[string]any{"processHandle": "first"})
	time.Sleep(650 * time.Millisecond)
	client.call("model/list", map[string]any{})

	// A kill acknowledgement alone must not release the remaining process.
	client.call("process/kill", map[string]any{"processHandle": "second"})
	time.Sleep(650 * time.Millisecond)
	client.call("model/list", map[string]any{})
	client.call("test/processExit", map[string]any{"processHandle": "second"})
	// Completion starts a new idle window instead of expiring immediately.
	time.Sleep(100 * time.Millisecond)
	client.call("model/list", map[string]any{})
	assertAppIdleExit(t, client)
}

func TestAppServerProcessFailureAndEarlyExitAllowIdle(t *testing.T) {
	for _, command := range []string{"fail", "exit-before-reply"} {
		t.Run(command, func(t *testing.T) {
			home := t.TempDir()
			configureAppIdle(t, home, "400ms")
			client := startApp(t, home)
			client.send("spawn", "process/spawn", processParams("process", command))
			response := client.response("spawn")
			if (response["error"] != nil) != (command == "fail") {
				t.Fatalf("unexpected spawn response: %v", response)
			}
			assertAppIdleExit(t, client)
		})
	}
}

func TestAppServerReusedProcessHandleIgnoresLateSpawnReply(t *testing.T) {
	home := t.TempDir()
	configureAppIdle(t, home, "400ms")
	client := startApp(t, home)
	client.send("old-spawn", "process/spawn", processParams("reused", "exit-before-delayed-reply"))
	client.await(func(message rpcMessage) bool {
		return rpcString(message, "method") == "process/exited" && rpcString(rpcObject(message, "params"), "processHandle") == "reused"
	})
	client.call("process/spawn", processParams("reused", "hold"))
	client.call("test/flushProcessReplies", map[string]any{})
	if response := client.response("old-spawn"); response["error"] != nil {
		t.Fatalf("late spawn failed: %v", response)
	}
	time.Sleep(650 * time.Millisecond)
	client.call("model/list", map[string]any{})
	client.call("test/processExit", map[string]any{"processHandle": "reused"})
	assertAppIdleExit(t, client)
}
