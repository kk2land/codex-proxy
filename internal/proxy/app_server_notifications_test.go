package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func startAppOptOut(t *testing.T, home string, methods ...string) *appClient {
	t.Helper()
	return startAppInitialize(t, home, nil, map[string]any{
		"clientInfo":   map[string]any{"name": "proxy-test", "version": "1"},
		"capabilities": map[string]any{"optOutNotificationMethods": methods},
	})
}

func awaitMethod(t *testing.T, client *appClient, method string) rpcMessage {
	t.Helper()
	return client.await(func(message rpcMessage) bool { return rpcString(message, "method") == method })
}

// awaitAbsent asserts that no matching message arrives within a short window
// while unrelated traffic keeps flowing.
func awaitAbsent(t *testing.T, client *appClient, method string) {
	t.Helper()
	client.send("probe", "model/list", map[string]any{})
	client.response("probe")
	for _, message := range client.stash {
		if rpcString(message, "method") == method {
			t.Fatalf("client received suppressed notification %s", method)
		}
	}
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case message := <-client.messages:
			if rpcString(message, "method") == method {
				t.Fatalf("client received suppressed notification %s", method)
			}
			client.stash = append(client.stash, message)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestAppServerNotificationOptOutStillReleasesSlot(t *testing.T) {
	for _, test := range []struct {
		name    string
		methods []string
	}{
		{name: "turn completed", methods: []string{"turn/completed"}},
		{name: "prediction updated", methods: []string{"thread/prediction/updated"}},
		{name: "realtime closed", methods: []string{"thread/realtime/closed"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			configureAppProxy(t, home, fakeCodex(t), 1, "5s")
			client := startAppOptOut(t, home, test.methods...)
			thread := client.thread()
			switch test.name {
			case "prediction updated":
				client.call("thread/prediction/request", map[string]any{"threadId": thread, "sourceTurnId": "turn-source"})
				awaitSlots(t, home, 0)
				awaitAbsent(t, client, "thread/prediction/updated")
			case "realtime closed":
				client.call("thread/realtime/start", map[string]any{"threadId": thread, "outputModality": "text"})
				awaitSlots(t, home, 1)
				client.call("thread/realtime/stop", map[string]any{"threadId": thread})
				awaitSlots(t, home, 0)
				awaitAbsent(t, client, "thread/realtime/closed")
			default:
				client.call("turn/start", taskParams(thread, "suppressed completion"))
				awaitSlots(t, home, 1)
				client.finish(thread)
				awaitSlots(t, home, 0)
				awaitAbsent(t, client, "turn/completed")
			}
			// The client still sees allowed lifecycle notifications and can
			// issue further requests; RPC responses were never filtered.
			client.call("model/list", map[string]any{})
			if code := client.stop(); code != 0 {
				t.Fatalf("exit=%d", code)
			}
		})
	}
}

func TestAppServerOptOutKeepsClientSideAndServerRequests(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	client := startAppOptOut(t, home, "turn/completed", "turn/started", "thread/started", "thread/queue/changed")
	thread := client.thread()
	client.call("turn/start", taskParams(thread, "suppressed lifecycle"))
	awaitSlots(t, home, 1)
	client.call("test/toolRequest", map[string]any{"threadId": thread})
	tool := awaitMethod(t, client, "item/tool/call")
	if err := writeRPC(client.input, rpcMessage{"id": tool["id"], "result": rpcValue(map[string]any{"content": "ok"})}); err != nil {
		t.Fatal(err)
	}
	awaitMethod(t, client, "test/toolReply")
	client.finish(thread)
	awaitSlots(t, home, 0)
	for _, suppressed := range []string{"turn/completed", "turn/started", "thread/started"} {
		awaitAbsent(t, client, suppressed)
	}
	// Proxy-generated queue notifications obey the same preference.
	client.call("thread/queue/add", map[string]any{"threadId": thread, "clientUserMessageId": "queued", "input": []any{map[string]any{"type": "text", "text": "queued"}}})
	awaitAbsent(t, client, "thread/queue/changed")
	awaitSlots(t, home, 1)
	client.finish(thread)
	awaitSlots(t, home, 0)
	if code := client.stop(); code != 0 {
		t.Fatalf("exit=%d", code)
	}
}

func TestAppServerUnsubscribeKeepsInternalTracking(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	client := startApp(t, home)
	thread := client.thread()
	client.call("turn/start", taskParams(thread, "unsubscribed task"))
	awaitSlots(t, home, 1)

	status := client.call("thread/unsubscribe", map[string]any{"threadId": thread})
	if rpcString(status, "status") != "unsubscribed" {
		t.Fatalf("unsubscribe status: %v", status)
	}
	// Broadcast thread status and metadata remain visible while unsubscribed.
	client.call("test/threadStatus", map[string]any{"threadId": thread, "status": "idle"})
	awaitMethod(t, client, "thread/status/changed")
	// Turn/item events are hidden while the task completes internally.
	client.finish(thread)
	awaitSlots(t, home, 0)
	awaitAbsent(t, client, "turn/completed")

	// A second unsubscribe reports notSubscribed; unknown threads are notLoaded.
	again := client.call("thread/unsubscribe", map[string]any{"threadId": thread})
	if rpcString(again, "status") != "notSubscribed" {
		t.Fatalf("second unsubscribe: %v", again)
	}
	unknown := client.call("thread/unsubscribe", map[string]any{"threadId": "never-loaded"})
	if rpcString(unknown, "status") != "notLoaded" {
		t.Fatalf("unknown unsubscribe: %v", unknown)
	}
	// Queries for an unknown thread must not mark it loaded.
	client.call("thread/queue/list", map[string]any{"threadId": thread})
	client.call("model/list", map[string]any{})
	// Resume restores the subscription and turn events become visible again.
	client.call("thread/resume", map[string]any{"threadId": thread})
	client.call("turn/start", taskParams(thread, "resubscribed task"))
	awaitSlots(t, home, 1)
	client.finish(thread)
	awaitMethod(t, client, "turn/completed")
	awaitSlots(t, home, 0)
	if code := client.stop(); code != 0 {
		t.Fatalf("exit=%d", code)
	}
}

func TestAppServerUnsubscribeFailureStillReleases(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	client := startApp(t, home)
	thread := client.thread()
	client.call("turn/start", taskParams(thread, "failing unsubscribed task"))
	awaitSlots(t, home, 1)
	if rpcString(client.call("thread/unsubscribe", map[string]any{"threadId": thread}), "status") != "unsubscribed" {
		t.Fatal("unsubscribe failed")
	}
	// The failing turn is observed internally even though its notification is
	// hidden, so the slot must be released.
	client.call("test/finish", map[string]any{"threadId": thread, "status": "failed"})
	awaitSlots(t, home, 0)
	awaitAbsent(t, client, "turn/completed")
	if code := client.stop(); code != 0 {
		t.Fatalf("exit=%d", code)
	}
}

func TestAppServerUnsubscribeChildDoesNotAffectParent(t *testing.T) {
	s, _ := newNotificationSession(t)
	shared := s.work("parent")
	s.threads["child"] = shared
	s.threadCreated("parent", 1)
	s.threadCreated("child", 2)
	if err := s.unsubscribeThread(rpcValue("unsub-child"), "child"); err != nil {
		t.Fatal(err)
	}
	if !s.subs["parent"].subscribed {
		t.Fatal("child unsubscribe changed the parent subscription")
	}
	if s.subs["child"].subscribed {
		t.Fatal("child remained subscribed")
	}
	if s.notificationVisible(rpcMessage{"method": rpcValue("turn/started"), "params": rpcValue(map[string]any{"threadId": "child"})}) {
		t.Fatal("child events leaked after unsubscribe")
	}
	if !s.notificationVisible(rpcMessage{"method": rpcValue("turn/started"), "params": rpcValue(map[string]any{"threadId": "parent"})}) {
		t.Fatal("shared parent events were suppressed")
	}
}

func TestAppServerLatePrivateSettingsDoNotResubscribe(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	client := startApp(t, home)
	source := client.thread()
	client.call("test/deferNextSettings", map[string]any{})
	client.send("late-fork", "thread/fork", map[string]any{"threadId": source, "ephemeral": true, "excludeTurns": true})
	child := ""
	client.await(func(message rpcMessage) bool {
		if rpcString(message, "method") != "thread/started" {
			return false
		}
		info := rpcObject(rpcObject(message, "params"), "thread")
		if rpcString(info, "forkedFromId") != source {
			return false
		}
		child = rpcString(info, "id")
		return true
	})
	if child == "" {
		t.Fatal("fork start missing")
	}
	// Unsubscribe before the fork's private settings ACK is emitted.
	client.send("unsub", "thread/unsubscribe", map[string]any{"threadId": child})
	if rpcString(rpcObject(client.response("unsub"), "result"), "status") != "unsubscribed" {
		t.Fatal("early unsubscribe failed")
	}
	client.call("test/flushSettings", map[string]any{})
	client.response("late-fork")
	// The late ACK must not restore the subscription.
	client.call("turn/start", taskParams(child, "child task"))
	awaitSlots(t, home, 1)
	client.finish(child)
	awaitSlots(t, home, 0)
	awaitAbsent(t, client, "turn/completed")

	// A new explicit resume is a later client intent and restores it.
	client.call("thread/resume", map[string]any{"threadId": child})
	client.call("turn/start", taskParams(child, "resumed child task"))
	awaitSlots(t, home, 1)
	client.finish(child)
	awaitMethod(t, client, "turn/completed")
	awaitSlots(t, home, 0)
	if code := client.stop(); code != 0 {
		t.Fatalf("exit=%d", code)
	}
}

func TestAppServerMalformedUnsubscribeDoesNotChangeState(t *testing.T) {
	s, upstream := newNotificationSession(t)
	s.threadCreated("thread-a", 1)
	if err := s.client(rpcValue(map[string]any{"method": "thread/unsubscribe", "params": map[string]any{"threadId": "thread-a"}})); err != nil {
		t.Fatal(err)
	}
	if !s.subs["thread-a"].subscribed {
		t.Fatal("notification without id changed subscription state")
	}
	if upstream.Len() != 0 {
		t.Fatalf("malformed unsubscribe was forwarded: %s", upstream.Bytes())
	}
}

func TestAppServerInitializeKeepsOtherCapabilities(t *testing.T) {
	s, upstream := newNotificationSession(t)
	line := rpcValue(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{
		"clientInfo": map[string]any{"name": "test", "version": "1"},
		"capabilities": map[string]any{
			"experimentalApi":           false,
			"requestAttestation":        true,
			"optOutNotificationMethods": []string{"turn/completed", "thread/status/changed"},
		},
	}})
	if err := s.client(line); err != nil {
		t.Fatal(err)
	}
	message, err := decodeRPC(upstream.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	capabilities := rpcObject(rpcObject(message, "params"), "capabilities")
	if _, exists := capabilities["optOutNotificationMethods"]; exists {
		t.Fatal("internal connection kept client opt-out")
	}
	if !rpcBoolTrue(capabilities, "experimentalApi") || !rpcBoolTrue(capabilities, "requestAttestation") {
		t.Fatalf("capabilities were lost: %s", rpcValue(capabilities))
	}
	if len(s.optOut) != 0 {
		t.Fatal("client preferences changed before successful initialization")
	}
	if err := s.server(rpcValue(rpcMessage{"id": message["id"], "result": rpcValue(map[string]any{})})); err != nil {
		t.Fatal(err)
	}
	if !s.optOut["turn/completed"] || !s.optOut["thread/status/changed"] {
		t.Fatal("client opt-out was not saved")
	}
}

func TestAppServerRejectedInitializeKeepsNotificationPreferences(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	client := startAppOptOut(t, home, "turn/completed")
	client.send("reinitialize", "initialize", map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}})
	if response := client.response("reinitialize"); response["error"] == nil {
		t.Fatalf("duplicate initialize accepted: %s", rpcValue(response))
	}
	thread := client.thread()
	client.call("turn/start", taskParams(thread, "after rejected initialize"))
	client.finish(thread)
	awaitSlots(t, home, 0)
	awaitAbsent(t, client, "turn/completed")
	if code := client.stop(); code != 0 {
		t.Fatalf("exit=%d", code)
	}
}

func TestNotificationOptOutKeepsLongPreferenceLists(t *testing.T) {
	names := make([]string, 300)
	for i := range names {
		names[i] = fmt.Sprintf("unknown/%d", i)
	}
	names = append(names, "turn/completed")
	if !notificationOptOut(rpcMessage{"optOutNotificationMethods": rpcValue(names)})["turn/completed"] {
		t.Fatal("valid notification preference was dropped")
	}
}

func TestLateThreadResponseDoesNotUndoUnsubscribe(t *testing.T) {
	s, _ := newNotificationSession(t)
	epoch := s.nextThreadEpoch()
	s.threadCreated("child", epoch)
	if err := s.unsubscribeThread(rpcValue("unsubscribe"), "child"); err != nil {
		t.Fatal(err)
	}
	s.threadCreated("child", epoch)
	if s.subs["child"].subscribed {
		t.Fatal("late thread response undid a newer unsubscribe")
	}
}

func TestAppServerQueryDoesNotLoadUnknownThread(t *testing.T) {
	s, _ := newNotificationSession(t)
	var stdout countingBuffer
	s.stdout = &stdout
	if err := s.client(rpcValue(map[string]any{"id": 1, "method": "model/list", "params": map[string]any{"threadId": "unknown"}})); err != nil {
		t.Fatal(err)
	}
	if s.subs["unknown"] != nil && s.subs["unknown"].loaded {
		t.Fatal("query marked an unknown thread loaded")
	}
}

func TestNotificationFilterHonorsOptOutAndSubscription(t *testing.T) {
	s, _ := newNotificationSession(t)
	s.optOut["turn/completed"] = true
	s.threadCreated("parent", 1)
	s.threadCreated("child", 2)
	if err := s.unsubscribeThread(rpcValue("unsub"), "child"); err != nil {
		t.Fatal(err)
	}
	visible := func(method, thread string) bool {
		return s.notificationVisible(rpcMessage{"method": rpcValue(method), "params": rpcValue(map[string]any{"threadId": thread})})
	}
	if visible("turn/completed", "parent") {
		t.Fatal("client opt-out ignored")
	}
	if visible("turn/started", "child") || visible("item/started", "child") {
		t.Fatal("unsubscribed child events leaked")
	}
	if !visible("thread/status/changed", "child") || !visible("thread/realtime/closed", "child") {
		t.Fatal("broadcast lifecycle events were suppressed with the subscription")
	}
	if !visible("turn/started", "parent") {
		t.Fatal("sibling subscription was affected")
	}
}

func TestAppServerOptOutConfigFromInitializeWire(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	client := startAppOptOut(t, home, "thread/status/changed")
	thread := client.thread()
	// thread/started was not suppressed; thread/status/changed is.
	client.call("test/notify", map[string]any{"method": "thread/status/changed", "params": map[string]any{"threadId": thread, "status": map[string]any{"type": "idle"}}})
	awaitAbsent(t, client, "thread/status/changed")
	if code := client.stop(); code != 0 {
		t.Fatalf("exit=%d", code)
	}
	capture, err := os.ReadFile(filepath.Join(client.dir, "rpc.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(capture), "optOutNotificationMethods") {
		t.Fatalf("client opt-out leaked to native connection: %s", capture)
	}
}

func newNotificationSession(t *testing.T) (*appSession, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	upstream := new(bytes.Buffer)
	clientOutput := new(bytes.Buffer)
	logger := &appLogger{Logger: NewLogger(dir, "notification-test", io.Discard, nil)}
	s := newAppSession(context.Background(), dir, nil, time.Millisecond, logger, upstream, clientOutput, nil)
	return s, upstream
}

// countingBuffer is a minimal io.Writer used to assert filtered output stays
// empty without depending on bytes.Buffer internals.
type countingBuffer struct {
	bytes.Buffer
}

var _ io.Writer = (*countingBuffer)(nil)
