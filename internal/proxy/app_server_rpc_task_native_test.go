package proxy

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// Real native 0.169: an in-progress sample owns its own slot while the source
// turn runs, and the cancel reply maps the typed request id to cancel it. The
// ACK-before-terminal slot timing is covered deterministically by the fake
// fixture.
func TestNativeAppServerSampleWhileSourceTurnRuns(t *testing.T) {
	binary := nativeUnsubscribeBinary(t)
	gate := make(chan struct{})
	a, requests := localResponses(t, "A", gate, false)
	client := startNativeUnsubscribeApp(t, binary, a.URL, a.URL)
	thread := nativeTestThread(t, client)
	result := client.call("turn/start", taskParams(thread, "native-sample-source"))
	turnID := rpcString(rpcObject(result, "turn"), "id")
	awaitResponsesRequest(t, requests)
	awaitSlots(t, client.home, 1)

	client.send(7, "thread/sample", map[string]any{
		"threadId": thread, "expectedTurnId": turnID, "allowInProgressTurn": true, "prompt": "native sample",
	})
	awaitResponsesRequest(t, requests)
	awaitSlots(t, client.home, 2)
	client.send("cancel-sample", "thread/sample/cancel", map[string]any{"requestId": 7})
	if response := client.response("cancel-sample"); response["error"] != nil {
		t.Fatalf("native sample cancel failed: %s", rpcValue(response))
	}
	// Fake coverage asserts the ACK-before-terminal slot invariant; native only
	// guarantees that the cancellation is delivered and the reply still arrives.
	close(gate)
	sample := client.response(7)
	if sample["error"] == nil || !strings.Contains(rpcString(rpcObject(sample, "error"), "message"), "cancel") {
		t.Fatalf("canceled native sample completed: %s", rpcValue(sample))
	}
	awaitSlots(t, client.home, 0)
	if code := client.stop(); code != 0 {
		t.Fatalf("proxy exit=%d", code)
	}
}

// Real native 0.169: thread/guardianReview attaches the connection
// subscription before reviewing, so even a review error leaves it subscribed.
// The model cannot trigger a successful guardian review here; only the native
// error path is verified.
func TestNativeAppServerGuardianReviewErrorKeepsSubscription(t *testing.T) {
	binary := nativeUnsubscribeBinary(t)
	a, requests := localResponses(t, "A", nil, false)
	client := startNativeUnsubscribeApp(t, binary, a.URL, a.URL)
	thread := nativeTestThread(t, client)
	client.call("turn/start", taskParams(thread, "guardian-review-source"))
	awaitResponsesRequest(t, requests)
	awaitThreadNotification(t, client, "turn/completed", thread)
	awaitSlots(t, client.home, 0)

	client.sequence++
	reviewID := fmt.Sprintf("client-%d", client.sequence)
	client.send(reviewID, "thread/guardianReview", map[string]any{"threadId": thread})
	rejected := client.response(reviewID)
	if rejected["error"] == nil {
		t.Fatalf("native guardian review unexpectedly succeeded: %s", rpcValue(rejected))
	}
	t.Logf("native guardian review rejection: %s", rpcValue(rejected))
	// The native error reply releases the review's own slot.
	awaitSlots(t, client.home, 0)
	loaded := client.call("thread/loaded/list", map[string]any{})
	if !bytes.Contains(rpcBytes(loaded["data"]), []byte(thread)) {
		t.Fatalf("native review error unloaded the subscribed thread: %s", rpcValue(loaded))
	}
	result := client.call("thread/unsubscribe", map[string]any{"threadId": thread})
	if rpcString(result, "status") != "unsubscribed" {
		t.Fatalf("review error did not keep a native subscription: %s", rpcValue(result))
	}
	awaitThreadNotification(t, client, "thread/closed", thread)
	if code := client.stop(); code != 0 {
		t.Fatalf("proxy exit=%d", code)
	}
}

// An invalid outer request id must reach native byte-for-byte instead of being
// rewritten into a valid request id that would return {}. Native 0.169's
// untagged JSONRPCMessage treats such a message as a notification (Notification
// ignores the extra id), so it emits no reply; if a future build answers with a
// null-id error, that error is checked as the standard invalid request.
func TestNativeAppServerInvalidSampleCancelHeaderIsNotRewritten(t *testing.T) {
	binary := nativeUnsubscribeBinary(t)
	a, _ := localResponses(t, "A", nil, false)
	client := startNativeUnsubscribeApp(t, binary, a.URL, a.URL)
	client.send(false, "thread/sample/cancel", map[string]any{"requestId": "missing-sample"})
	// Order the stream with a valid request: any reply to the invalid header
	// would arrive first and be stashed while this response is awaited.
	client.call("thread/loaded/list", map[string]any{})
	for _, message := range client.stash {
		if rpcString(message, "method") == "" && rpcText(message["id"]) == "false" {
			t.Fatalf("invalid header was rewritten into a request: %s", rpcValue(message))
		}
		if message["error"] != nil && rpcNull(message["id"]) {
			if code := rpcText(rpcObject(message, "error")["code"]); code != "-32600" {
				t.Fatalf("unexpected invalid-header error: %s", rpcValue(message))
			}
		}
	}
	if code := client.stop(); code != 0 {
		t.Fatalf("proxy exit=%d", code)
	}
}
