package proxy

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func startRPCCompletionApp(t *testing.T, slots int) (*appClient, string) {
	t.Helper()
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), slots, "5s")
	client := startApp(t, home)
	return client, client.thread()
}

func capturedRPC(t *testing.T, client *appClient) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(client.dir, "rpc.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Sample and guardian completion RPCs admit their own slots while the source
// turn keeps running, and release without any source turn notification.
func TestAppServerRPCCompletionTasksOwnTheirSlots(t *testing.T) {
	client, thread := startRPCCompletionApp(t, 2)
	result := client.call("turn/start", taskParams(thread, "source turn keeps running"))
	turnID := rpcString(rpcObject(result, "turn"), "id")
	awaitSlots(t, client.home, 1)

	sample := client.call("thread/sample", map[string]any{
		"threadId": thread, "expectedTurnId": turnID, "allowInProgressTurn": true, "prompt": "sample while running",
	})
	if rpcString(sample, "responseId") != "sample-response" || rpcString(sample, "sourceTurnId") != turnID {
		t.Fatalf("sample reply changed: %s", rpcValue(sample))
	}
	// The sample released its own slot; the source turn still holds one.
	awaitSlots(t, client.home, 1)

	guardian := client.call("thread/guardianReview", map[string]any{"threadId": thread})
	if rpcString(guardian, "callId") != "call-1" || !rpcNull(guardian["assessment"]) {
		t.Fatalf("guardian reply changed: %s", rpcValue(guardian))
	}
	awaitSlots(t, client.home, 1)

	client.finish(thread)
	awaitSlots(t, client.home, 0)
	if code := client.stop(); code != 0 {
		t.Fatalf("exit=%d", code)
	}
}

// A sample canceled before it reaches native fails locally and acknowledges the
// cancel locally, without pausing the source thread's queued work.
func TestAppServerSampleCancelWaitingKeepsQueuedWork(t *testing.T) {
	client, thread := startRPCCompletionApp(t, 1)
	result := client.call("turn/start", taskParams(thread, "source turn keeps running"))
	turnID := rpcString(rpcObject(result, "turn"), "id")
	awaitSlots(t, client.home, 1)
	client.call("thread/queue/add", map[string]any{
		"threadId": thread, "clientUserMessageId": "queued-after-sample", "input": taskParams(thread, "queued turn")["input"],
	})
	client.send("sample-waiting", "thread/sample", map[string]any{
		"threadId": thread, "expectedTurnId": turnID, "prompt": "waiting sample",
	})
	awaitLog(t, client.home, "waiting")
	client.send("cancel-sample", "thread/sample/cancel", map[string]any{"requestId": "sample-waiting"})
	if code := rpcText(rpcObject(client.response("sample-waiting"), "error")["code"]); code != "-32800" {
		t.Fatalf("local sample cancel code=%s", code)
	}
	if response := client.response("cancel-sample"); response["error"] != nil {
		t.Fatalf("local cancel was not acknowledged: %s", rpcValue(response))
	}
	if captured := capturedRPC(t, client); bytes.Contains(captured, []byte(`"thread/sample"`)) || bytes.Contains(captured, []byte(`"thread/sample/cancel"`)) {
		t.Fatalf("local cancel reached native: %s", captured)
	}
	client.finish(thread)
	// Sample cancellation must not pause the source queue: the queued input now
	// starts on the freed slot.
	client.await(func(message rpcMessage) bool {
		params := rpcObject(message, "params")
		return rpcString(message, "method") == "turn/started" && rpcString(params, "threadId") == thread &&
			rpcString(rpcObject(params, "turn"), "id") != turnID
	})
	awaitSlots(t, client.home, 1)
	client.finish(thread)
	awaitSlots(t, client.home, 0)
	if captured := capturedRPC(t, client); !bytes.Contains(captured, []byte(`"queued-after-sample"`)) {
		t.Fatalf("queued submission did not run after the sample cancel: %s", captured)
	}
	if code := client.stop(); code != 0 {
		t.Fatalf("exit=%d", code)
	}
}

// An in-flight cancel maps the typed sample request id and its acknowledgement
// only signals cancellation: the sample keeps its own slot until it replies.
func TestAppServerSampleCancelMidflightMapsTypedID(t *testing.T) {
	client, thread := startRPCCompletionApp(t, 2)
	result := client.call("turn/start", taskParams(thread, "source turn"))
	turnID := rpcString(rpcObject(result, "turn"), "id")
	awaitSlots(t, client.home, 1)
	client.call("test/delaySamples", map[string]any{})
	client.send(7, "thread/sample", map[string]any{
		"threadId": thread, "expectedTurnId": turnID, "allowInProgressTurn": true, "prompt": "delayed sample",
	})
	awaitSlots(t, client.home, 2)

	// The string "7" is not the numeric request id 7.
	client.send("cancel-string", "thread/sample/cancel", map[string]any{"requestId": "7"})
	if response := client.response("cancel-string"); response["error"] != nil {
		t.Fatalf("mismatched cancel refused: %s", rpcValue(response))
	}
	client.send("cancel-number", "thread/sample/cancel", map[string]any{"requestId": 7})
	if response := client.response("cancel-number"); response["error"] != nil {
		t.Fatalf("cancel refused: %s", rpcValue(response))
	}
	// The cancel acknowledgement must not release the sample's slot.
	awaitSlots(t, client.home, 2)

	captured := capturedRPC(t, client)
	lines := bytes.Split(bytes.TrimSpace(captured), []byte{'\n'})
	var cancelIDs [][]byte
	var sampleID []byte
	for _, line := range lines {
		message, err := decodeRPC(line)
		if err != nil {
			continue
		}
		switch rpcString(message, "method") {
		case "thread/sample":
			sampleID = rpcBytes(message["id"])
		case "thread/sample/cancel":
			cancelIDs = append(cancelIDs, rpcBytes(rpcObject(message, "params")["requestId"]))
		}
	}
	if len(cancelIDs) != 2 || string(cancelIDs[0]) != `"7"` || !bytes.Equal(cancelIDs[1], sampleID) {
		t.Fatalf("cancel request ids were not mapped by type: %q sample=%q", cancelIDs, sampleID)
	}

	client.call("test/flushSamples", map[string]any{})
	response := client.response(7)
	if response["error"] != nil || rpcString(rpcObject(response, "result"), "responseId") != "sample-response" {
		t.Fatalf("sample reply changed: %s", rpcValue(response))
	}
	awaitSlots(t, client.home, 1)
	client.finish(thread)
	awaitSlots(t, client.home, 0)
	if code := client.stop(); code != 0 {
		t.Fatalf("exit=%d", code)
	}
}
