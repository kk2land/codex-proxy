package proxy

import (
	"bytes"
	"testing"
	"time"
)

func TestUnsubscribeForwardsNativeReplyAndClose(t *testing.T) {
	for _, status := range []string{"unsubscribed", "notSubscribed", "notLoaded", "error"} {
		t.Run(status, func(t *testing.T) {
			s, upstream := newNotificationSession(t)
			output := s.stdout.(*bytes.Buffer)
			s.threadCreated("thread", s.nextThreadEpoch())
			if err := s.unsubscribeThread(rpcValue("client-id"), "thread"); err != nil {
				t.Fatal(err)
			}
			request, err := decodeRPC(upstream.Bytes())
			if err != nil || rpcString(request, "method") != "thread/unsubscribe" || rpcString(rpcObject(request, "params"), "threadId") != "thread" {
				t.Fatalf("native unsubscribe missing: %v: %s", err, upstream.Bytes())
			}
			if output.Len() != 0 {
				t.Fatalf("idle unsubscribe replied before native: %s", output.Bytes())
			}
			response := rpcMessage{"id": request["id"], "result": rpcValue(map[string]any{"status": status})}
			if status == "error" {
				delete(response, "result")
				response["error"] = rpcValue(map[string]any{"code": -32603, "message": "native unsubscribe failed", "data": "native detail"})
			}
			reviewNative(t, s, response)
			response["id"] = rpcValue("client-id")
			if !bytes.Equal(bytes.TrimSpace(output.Bytes()), rpcValue(response)) {
				t.Fatalf("native response was changed: %s", output.Bytes())
			}
			if status == "error" {
				if !s.subs["thread"].subscribed {
					t.Fatal("failed unsubscribe cannot be retried")
				}
				return
			}
			if (status == "notLoaded") != s.closedTasks["thread"] {
				t.Fatalf("unexpected lifecycle after %s", status)
			}
			// Only a native notification may tell the client the thread closed.
			output.Reset()
			closed := closeMessage("thread/closed", "thread")
			closed["emittedAtMs"] = rpcValue(12345)
			reviewNative(t, s, closed)
			if !bytes.Equal(bytes.TrimSpace(output.Bytes()), rpcValue(closed)) {
				t.Fatalf("native close notification was changed: %s", output.Bytes())
			}
		})
	}
}

func TestUnsubscribeChildUnloadsAfterItsTurnWithoutReleasingParent(t *testing.T) {
	s, upstream := newNotificationSession(t)
	work := s.work("parent")
	s.threads["child"] = work
	work.lease = testLease(t)
	work.running["parent:parent-turn"], work.running["child:child-turn"] = true, true
	s.threadCreated("parent", s.nextThreadEpoch())
	s.threadCreated("child", s.nextThreadEpoch())
	if err := s.unsubscribeThread(rpcValue("unsub-child"), "child"); err != nil {
		t.Fatal(err)
	}
	if upstream.Len() != 0 {
		t.Fatalf("running child unsubscribed too early: %s", upstream.Bytes())
	}
	reviewNative(t, s, rpcMessage{"method": rpcValue("turn/completed"), "params": rpcValue(map[string]any{"threadId": "child", "turn": map[string]any{"id": "child-turn", "status": "completed"}})})
	request, err := decodeRPC(upstream.Bytes())
	if err != nil || rpcString(request, "method") != "thread/unsubscribe" || rpcString(rpcObject(request, "params"), "threadId") != "child" {
		t.Fatalf("child was not unloaded after its turn: %v: %s", err, upstream.Bytes())
	}
	reviewNative(t, s, rpcMessage{"id": request["id"], "result": rpcValue(map[string]any{"status": "unsubscribed"})})
	reviewNative(t, s, closeMessage("thread/closed", "child"))
	if work.lease == nil || !work.running["parent:parent-turn"] || !s.subs["parent"].subscribed {
		t.Fatal("child unload changed the parent's task, slot, or subscription")
	}
	reviewNative(t, s, rpcMessage{"method": rpcValue("turn/completed"), "params": rpcValue(map[string]any{"threadId": "parent", "turn": map[string]any{"id": "parent-turn", "status": "completed"}})})
	if work.lease != nil {
		t.Fatal("shared slot was not released after parent completion")
	}
}

func TestUnsubscribeResumeCancelsDeferredUnload(t *testing.T) {
	s, upstream := newNotificationSession(t)
	work := s.work("thread")
	work.running["thread:turn"] = true
	s.threadCreated("thread", s.nextThreadEpoch())
	if err := s.unsubscribeThread(rpcValue("unsub"), "thread"); err != nil {
		t.Fatal(err)
	}
	s.threadCreated("thread", s.nextThreadEpoch())
	reviewNative(t, s, rpcMessage{"method": rpcValue("turn/completed"), "params": rpcValue(map[string]any{"threadId": "thread", "turn": map[string]any{"id": "turn", "status": "completed"}})})
	if upstream.Len() != 0 || !s.subs["thread"].subscribed || s.subs["thread"].unsubscribePending {
		t.Fatal("successful resume did not cancel the older deferred unsubscribe")
	}
}

func TestUnsubscribeDeferredNativeFailureIsReported(t *testing.T) {
	s, upstream := newNotificationSession(t)
	work := s.work("thread")
	work.lease = testLease(t)
	work.running["thread:turn"] = true
	s.threadCreated("thread", s.nextThreadEpoch())
	if err := s.unsubscribeThread(rpcValue("unsub"), "thread"); err != nil {
		t.Fatal(err)
	}
	reviewNative(t, s, rpcMessage{"method": rpcValue("turn/completed"), "params": rpcValue(map[string]any{"threadId": "thread", "turn": map[string]any{"id": "turn", "status": "failed"}})})
	request, err := decodeRPC(upstream.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	output := s.stdout.(*bytes.Buffer)
	output.Reset()
	reviewNative(t, s, rpcMessage{"id": request["id"], "error": rpcValue(map[string]any{"code": -32603, "message": "native unload failure"})})
	notice, err := decodeRPC(output.Bytes())
	if err != nil || rpcString(notice, "method") != "error" || rpcString(rpcObject(notice, "params"), "threadId") != "thread" {
		t.Fatalf("deferred native error was lost: %v: %s", err, output.Bytes())
	}
	if work.lease != nil || !s.subs["thread"].subscribed {
		t.Fatal("failed deferred unload leaked a slot or prevented retry")
	}
}

func TestAppServerUnsubscribePausesAndPreservesQueue(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	client := startApp(t, home)
	thread := client.thread()
	client.call("turn/start", taskParams(thread, "first turn"))
	client.call("thread/queue/add", map[string]any{"threadId": thread, "clientUserMessageId": "queued", "input": []any{map[string]any{"type": "text", "text": "preserved queued turn"}}})
	client.call("thread/unsubscribe", map[string]any{"threadId": thread})
	client.finish(thread)
	awaitThreadNotification(t, client, "thread/closed", thread)
	awaitSlots(t, home, 0)
	queue := client.call("thread/queue/list", map[string]any{"threadId": thread})
	if !bytes.Contains(rpcValue(queue), []byte("preserved queued turn")) {
		t.Fatalf("unload discarded queued input: %s", rpcValue(queue))
	}
	client.call("thread/resume", map[string]any{"threadId": thread})
	awaitSlots(t, home, 0)
	client.call("thread/queue/start", map[string]any{"threadId": thread})
	awaitSlots(t, home, 1)
	client.finish(thread)
	awaitThreadNotification(t, client, "turn/completed", thread)
	awaitSlots(t, home, 0)
	if code := client.stop(); code != 0 {
		t.Fatalf("proxy exit=%d", code)
	}
}

func TestAppServerUnsubscribeAndNonTaskRPCBypassAdmission(t *testing.T) {
	for _, stage := range []string{"slot", "interval"} {
		t.Run(stage, func(t *testing.T) {
			home := t.TempDir()
			limit := 1
			if stage == "interval" {
				limit = 2
			}
			configureAppProxy(t, home, fakeCodex(t), limit, "5s")
			client := startAppEnv(t, home, []string{"CODEX_PROXY_TEST_TASK_INTERVAL=10"})
			busy, waiting, idle := client.thread(), client.thread(), client.thread()
			client.call("turn/start", taskParams(busy, "hold the first slot"))
			client.send("waiting", "turn/start", taskParams(waiting, "wait for admission"))
			if stage == "interval" {
				awaitStateLock(t, client.home+"/.codex-proxy")
			} else {
				awaitLog(t, home, "waiting")
			}
			started := time.Now()
			// The fixture implements test/launch; it is outside the task
			// whitelist and must reach native while another request waits.
			if result := client.call("test/launch", map[string]any{}); result["parentPid"] == nil {
				t.Fatal("non-whitelisted RPC did not reach native")
			}
			client.call("thread/unsubscribe", map[string]any{"threadId": idle})
			awaitThreadNotification(t, client, "thread/closed", idle)
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("control requests waited on %s admission: %v", stage, elapsed)
			}
			client.send(nil, "$/cancelRequest", map[string]any{"id": "waiting"})
			if response := client.response("waiting"); string(rpcObject(response, "error")["code"]) != "-32800" {
				t.Fatalf("waiting task bypassed admission: %s", rpcValue(response))
			}
			client.finish(busy)
			awaitSlots(t, home, 0)
			if code := client.stop(); code != 0 {
				t.Fatalf("proxy exit=%d", code)
			}
		})
	}
}
