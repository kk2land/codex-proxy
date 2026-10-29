package proxy

import (
	"bytes"
	"strings"
	"testing"
)

func TestNativeAppServerPreservesParameterErrors(t *testing.T) {
	binary := nativeUnsubscribeBinary(t)
	a, requests := localResponses(t, "A", nil, false)
	client := startNativeUnsubscribeApp(t, binary, a.URL, a.URL)
	if code := client.stop(); code != 0 {
		t.Fatalf("proxy exit=%d", code)
	}
	// Start with the same isolated configuration but without initializing,
	// so malformed initialize calls cannot be hidden by "Already initialized".
	client = startAppInitialize(t, client.home, nil, nil, "app-server")
	for i, capabilities := range []any{
		"invalid",
		map[string]any{"experimentalApi": "invalid"},
		map[string]any{"optOutNotificationMethods": "invalid"},
		map[string]any{"optOutNotificationMethods": []any{nil}},
	} {
		client.send(i, "initialize", map[string]any{"clientInfo": map[string]any{"name": "control-test", "version": "1"}, "capabilities": capabilities})
		response := client.response(i)
		if string(rpcObject(response, "error")["code"]) != "-32600" || !strings.Contains(rpcString(rpcObject(response, "error"), "message"), "Invalid request") {
			t.Fatalf("malformed initialize was not rejected by native: %s", rpcValue(response))
		}
	}
	client.call("initialize", map[string]any{"clientInfo": map[string]any{"name": "control-test", "version": "1"}})
	for i, params := range []any{"invalid", nil, []any{}, map[string]any{"config": "invalid"}} {
		client.send(i, "thread/start", params)
		response := client.response(i)
		if string(rpcObject(response, "error")["code"]) != "-32600" {
			t.Fatalf("malformed thread/start was accepted: %s", rpcValue(response))
		}
	}
	loaded := client.call("thread/loaded/list", map[string]any{})
	if !bytes.Equal(loaded["data"], []byte("[]")) || len(requests) != 0 {
		t.Fatalf("invalid request created native work: %s", rpcValue(loaded))
	}
	if code := client.stop(); code != 0 {
		t.Fatalf("proxy exit=%d", code)
	}
}

func TestNativeAppServerInterruptShellWithWaitingModelTask(t *testing.T) {
	binary := nativeUnsubscribeBinary(t)
	gate := make(chan struct{})
	a, requests := localResponses(t, "A", gate, false)
	client := startNativeUnsubscribeApp(t, binary, a.URL, a.URL)
	configureAppProxy(t, client.home, binary, 1, "30s")
	holder, thread := nativeTestThread(t, client), nativeTestThread(t, client)
	client.call("turn/start", taskParams(holder, "hold quota while testing controls"))
	awaitResponsesRequest(t, requests)
	client.send("waiting", "turn/start", taskParams(thread, "waiting model task"))
	awaitLog(t, client.home, "waiting")
	for i, params := range []map[string]any{
		{"threadId": thread},
		{"threadId": thread, "turnId": "not-started"},
	} {
		client.send(i, "turn/interrupt", params)
		if response := client.response(i); string(rpcObject(response, "error")["code"]) != "-32600" {
			t.Fatalf("interrupt returned success without a native turn: %s", rpcValue(response))
		}
	}
	client.call("thread/shellCommand", map[string]any{"threadId": thread, "command": "sleep 30"})
	started := awaitThreadNotification(t, client, "turn/started", thread)
	turnID := rpcString(rpcObject(rpcObject(started, "params"), "turn"), "id")
	client.send("wrong-turn", "turn/interrupt", map[string]any{"threadId": thread, "turnId": "wrong-turn"})
	if response := client.response("wrong-turn"); !strings.Contains(rpcString(rpcObject(response, "error"), "message"), "expected active turn id") {
		t.Fatalf("wrong-turn interrupt lost native error: %s", rpcValue(response))
	}
	for _, message := range client.stash {
		if string(message["id"]) == string(rpcValue("waiting")) {
			t.Fatalf("rejected interrupt canceled the waiting task: %s", rpcValue(message))
		}
	}
	client.call("turn/interrupt", map[string]any{"threadId": thread, "turnId": turnID})
	if response := client.response("waiting"); string(rpcObject(response, "error")["code"]) != "-32800" {
		t.Fatalf("accepted interrupt did not cancel its pending model input: %s", rpcValue(response))
	}
	completed := awaitThreadNotification(t, client, "turn/completed", thread)
	if rpcString(rpcObject(rpcObject(completed, "params"), "turn"), "status") != "interrupted" {
		t.Fatalf("native shell was not interrupted: %s", rpcValue(completed))
	}
	awaitSlots(t, client.home, 1)
	close(gate)
	awaitThreadNotification(t, client, "turn/completed", holder)
	awaitSlots(t, client.home, 0)
	if len(requests) != 0 {
		t.Fatal("a canceled model task reached the Responses service")
	}
	if code := client.stop(); code != 0 {
		t.Fatalf("proxy exit=%d", code)
	}
}

func TestNativeAppServerRejectedGoalPauseKeepsWaitingActivation(t *testing.T) {
	binary := nativeUnsubscribeBinary(t)
	gate := make(chan struct{})
	a, requests := localResponses(t, "A", gate, false)
	client := startNativeUnsubscribeApp(t, binary, a.URL, a.URL)
	configureAppProxy(t, client.home, binary, 1, "30s")
	holder, thread := nativeTestThread(t, client), nativeTestThread(t, client)
	client.call("turn/start", taskParams(holder, "hold quota while testing goal controls"))
	awaitResponsesRequest(t, requests)
	client.call("thread/goal/set", map[string]any{"threadId": thread, "objective": "local goal", "status": "paused"})
	client.send("activation", "thread/goal/set", map[string]any{"threadId": thread, "status": "active"})
	awaitLog(t, client.home, "waiting")
	client.send("invalid-pause", "thread/goal/set", map[string]any{"threadId": thread, "status": "paused", "tokenBudget": -1})
	if response := client.response("invalid-pause"); response["error"] == nil {
		t.Fatal("native accepted an invalid goal budget")
	}
	for _, message := range client.stash {
		if string(message["id"]) == string(rpcValue("activation")) {
			t.Fatalf("rejected pause canceled the waiting activation: %s", rpcValue(message))
		}
	}
	client.call("thread/goal/clear", map[string]any{"threadId": thread})
	if response := client.response("activation"); string(rpcObject(response, "error")["code"]) != "-32800" {
		t.Fatalf("accepted clear did not cancel the pending activation: %s", rpcValue(response))
	}
	close(gate)
	awaitThreadNotification(t, client, "turn/completed", holder)
	awaitSlots(t, client.home, 0)
	if code := client.stop(); code != 0 {
		t.Fatalf("proxy exit=%d", code)
	}
}
