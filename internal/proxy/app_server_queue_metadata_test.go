package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// queueMetadataSession keeps the thread's queue local by holding a running
// turn, so add/update requests stay enqueued instead of admitting.
func queueMetadataSession(t *testing.T) *appSession {
	t.Helper()
	s, _ := newNotificationSession(t)
	w := s.work("thread")
	w.lease = testLease(t)
	w.running["thread:turn"] = true
	s.threadCreated("thread", s.nextThreadEpoch())
	t.Cleanup(func() { s.release(w) })
	return s
}

func queueRPC(t *testing.T, s *appSession, id, method string, params map[string]any) rpcMessage {
	t.Helper()
	if err := s.client(rpcValue(map[string]any{"id": id, "method": method, "params": params})); err != nil {
		t.Fatal(err)
	}
	return controlResponse(t, s, id)
}

func queueParams(thread, clientID, text string) map[string]any {
	params := taskParams(thread, text)
	params["clientUserMessageId"] = clientID
	return params
}

func queueList(t *testing.T, s *appSession, id, thread string) []rpcMessage {
	t.Helper()
	result := rpcObject(queueRPC(t, s, id, "thread/queue/list", map[string]any{"threadId": thread}), "result")
	var raw []json.RawMessage
	if err := rpcUnmarshal(result["data"], &raw); err != nil {
		t.Fatal(err)
	}
	items := make([]rpcMessage, len(raw))
	for i, item := range raw {
		message, err := decodeRPC(item)
		if err != nil {
			t.Fatal(err)
		}
		items[i] = message
	}
	return items
}

func lastTurnStart(t *testing.T, upstream *bytes.Buffer) rpcMessage {
	t.Helper()
	var found rpcMessage
	for _, line := range bytes.Split(upstream.Bytes(), []byte{'\n'}) {
		message, err := decodeRPC(line)
		if err == nil && rpcString(message, "method") == "turn/start" {
			found = message
		}
	}
	if found == nil {
		t.Fatal("no turn/start reached native")
	}
	return found
}

func TestQueueMetadataAddListAndUpdate(t *testing.T) {
	s := queueMetadataSession(t)
	params := queueParams("thread", "corr-a", "first")
	params["responsesapiClientMetadata"] = map[string]string{"a": "1", "b": "2"}
	response := queueRPC(t, s, "add-a", "thread/queue/add", params)
	if response["error"] != nil {
		t.Fatalf("add failed: %s", rpcValue(response))
	}
	first := rpcObject(rpcObject(response, "result"), "queuedSubmission")
	id := rpcString(first, "id")
	if id == "" || rpcString(first, "clientUserMessageId") != "corr-a" {
		t.Fatalf("add response lost identifiers: %s", rpcValue(first))
	}
	if got := rpcText(first["responsesapiClientMetadata"]); got != `{"a":"1","b":"2"}` {
		t.Fatalf("add metadata = %s", got)
	}
	if !bytes.Contains(rpcBytes(first["input"]), []byte("first")) {
		t.Fatalf("add response lost input: %s", rpcValue(first))
	}

	response = queueRPC(t, s, "add-empty", "thread/queue/add", func() map[string]any {
		params := queueParams("thread", "corr-empty", "empty")
		params["responsesapiClientMetadata"] = map[string]string{}
		return params
	}())
	if response["error"] != nil {
		t.Fatalf("add with empty metadata failed: %s", rpcValue(response))
	}
	empty := rpcObject(rpcObject(response, "result"), "queuedSubmission")
	if got := rpcText(empty["responsesapiClientMetadata"]); got != "{}" {
		t.Fatalf("explicit empty add metadata encoded as %q, want {}", got)
	}

	response = queueRPC(t, s, "add-b", "thread/queue/add", queueParams("thread", "corr-b", "second"))
	if response["error"] != nil {
		t.Fatalf("add without metadata failed: %s", rpcValue(response))
	}
	plain := rpcObject(rpcObject(response, "result"), "queuedSubmission")
	if got := rpcText(plain["responsesapiClientMetadata"]); got != "null" {
		t.Fatalf("absent metadata encoded as %q, want null", got)
	}

	params = queueParams("thread", "corr-null", "null")
	params["responsesapiClientMetadata"] = json.RawMessage("null")
	response = queueRPC(t, s, "add-null", "thread/queue/add", params)
	if response["error"] != nil {
		t.Fatalf("add with null metadata failed: %s", rpcValue(response))
	}
	null := rpcObject(rpcObject(response, "result"), "queuedSubmission")
	if got := rpcText(null["responsesapiClientMetadata"]); got != "null" {
		t.Fatalf("null add metadata encoded as %q, want null", got)
	}

	items := queueList(t, s, "list", "thread")
	if len(items) != 4 {
		t.Fatalf("listed %d entries, want 4", len(items))
	}
	if rpcString(items[0], "id") != id || rpcText(items[0]["responsesapiClientMetadata"]) != `{"a":"1","b":"2"}` {
		t.Fatalf("list lost metadata: %s", rpcValue(items[0]))
	}
	if got := rpcText(items[1]["responsesapiClientMetadata"]); got != "{}" {
		t.Fatalf("list encoded explicit empty metadata as %s", got)
	}
	if got := rpcText(items[2]["responsesapiClientMetadata"]); got != "null" {
		t.Fatalf("list encoded absent metadata as %s", got)
	}
	if got := rpcText(items[3]["responsesapiClientMetadata"]); got != "null" {
		t.Fatalf("list encoded null metadata as %s", got)
	}

	// Omission, null, and an empty map preserve metadata; a non-empty map replaces it.
	for _, tc := range []struct {
		name     string
		metadata any
		want     string
	}{
		{"omitted", nil, `{"a":"1","b":"2"}`},
		{"null", json.RawMessage("null"), `{"a":"1","b":"2"}`},
		{"empty", map[string]string{}, `{"a":"1","b":"2"}`},
		{"replace", map[string]string{"c": "3"}, `{"c":"3"}`},
	} {
		params := queueParams("thread", "corr-a", "first-"+tc.name)
		params["queuedSubmissionId"] = id
		if tc.metadata != nil {
			params["responsesapiClientMetadata"] = tc.metadata
		}
		response := queueRPC(t, s, "update-"+tc.name, "thread/queue/update", params)
		if response["error"] != nil {
			t.Fatalf("%s update failed: %s", tc.name, rpcValue(response))
		}
		updated := rpcObject(rpcObject(response, "result"), "queuedSubmission")
		if got := rpcText(updated["responsesapiClientMetadata"]); got != tc.want {
			t.Fatalf("%s update metadata = %s, want %s", tc.name, got, tc.want)
		}
		if !bytes.Contains(rpcBytes(updated["input"]), []byte("first-"+tc.name)) {
			t.Fatalf("%s update lost input: %s", tc.name, rpcValue(updated))
		}
	}
}

func TestQueueMetadataRejectsInvalidValues(t *testing.T) {
	for name, metadata := range map[string]json.RawMessage{
		"null value": json.RawMessage(`{"a":null}`),
		"number":     json.RawMessage(`{"a":1}`),
		"boolean":    json.RawMessage(`{"a":true}`),
		"array":      json.RawMessage(`{"a":[]}`),
		"scalar":     json.RawMessage(`"a"`),
		"top-level":  json.RawMessage(`["a"]`),
	} {
		t.Run(name, func(t *testing.T) {
			s := queueMetadataSession(t)
			params := queueParams("thread", "corr", "text")
			params["responsesapiClientMetadata"] = metadata
			if response := queueRPC(t, s, "bad-add", "thread/queue/add", params); response["error"] == nil {
				t.Fatalf("invalid metadata accepted: %s", rpcValue(response))
			}
			if state := s.queueState("thread"); state != nil && len(state.items) != 0 {
				t.Fatalf("rejected add left %d entries: %s", len(state.items), rpcValue(state.items[0].Input))
			}
			if s.queuedSize != 0 {
				t.Fatalf("rejected add kept %d queued bytes", s.queuedSize)
			}

			valid := queueParams("thread", "corr", "text")
			valid["responsesapiClientMetadata"] = map[string]string{"keep": "1"}
			response := queueRPC(t, s, "add", "thread/queue/add", valid)
			if response["error"] != nil {
				t.Fatalf("valid add failed: %s", rpcValue(response))
			}
			entry := rpcObject(rpcObject(response, "result"), "queuedSubmission")
			size := s.queuedSize
			params = queueParams("thread", "corr", "changed")
			params["queuedSubmissionId"] = rpcString(entry, "id")
			params["responsesapiClientMetadata"] = metadata
			if response := queueRPC(t, s, "bad-update", "thread/queue/update", params); response["error"] == nil {
				t.Fatalf("invalid metadata update accepted: %s", rpcValue(response))
			}
			if s.queuedSize != size {
				t.Fatalf("rejected update moved accounting from %d to %d", size, s.queuedSize)
			}
			items := queueList(t, s, "list", "thread")
			if len(items) != 1 {
				t.Fatalf("rejected update changed the queue to %d entries", len(items))
			}
			if got := rpcText(items[0]["responsesapiClientMetadata"]); got != `{"keep":"1"}` {
				t.Fatalf("rejected update changed metadata to %s", got)
			}
			if !bytes.Contains(rpcBytes(items[0]["input"]), []byte("text")) {
				t.Fatalf("rejected update changed input: %s", rpcValue(items[0]))
			}
		})
	}
}

func TestQueueMetadataDispatchesTurnStart(t *testing.T) {
	s, w, upstream := sharedParentChildWork(t)
	params := queueParams("parent", "corr-parent", "parent input")
	params["responsesapiClientMetadata"] = map[string]string{"a": "1"}
	if response := queueRPC(t, s, "parent-add", "thread/queue/add", params); response["error"] != nil {
		t.Fatalf("add failed: %s", rpcValue(response))
	}
	if w.pending != nil {
		t.Fatal("queued input was admitted while the slot was busy")
	}
	delete(w.running, "parent:turn")
	if err := s.finish(w); err != nil {
		t.Fatal(err)
	}
	if w.pending == nil || w.pending.queueID == "" {
		t.Fatal("queued entry was not scheduled")
	}
	if err := s.dispatch(w, w.pending); err != nil {
		t.Fatal(err)
	}
	turn := rpcObject(lastTurnStart(t, upstream), "params")
	if got := rpcText(turn["responsesapiClientMetadata"]); got != `{"a":"1"}` {
		t.Fatalf("turn/start metadata = %s", got)
	}
	if got := rpcString(turn, "clientUserMessageId"); got != "corr-parent" {
		t.Fatalf("turn/start clientUserMessageId = %q", got)
	}
	if !bytes.Contains(rpcBytes(turn["input"]), []byte("parent input")) {
		t.Fatalf("turn/start lost input: %s", rpcValue(turn))
	}
}

// TestQueueMetadataUpdateWhileWaitingDispatchesLatest depends on the shared
// stateReady change that refreshes a waiting entry's metadata before dispatch.
func TestQueueMetadataUpdateWhileWaitingDispatchesLatest(t *testing.T) {
	s, w, upstream := sharedParentChildWork(t)
	params := queueParams("parent", "corr-parent", "before")
	params["responsesapiClientMetadata"] = map[string]string{"stage": "before"}
	response := queueRPC(t, s, "parent-add", "thread/queue/add", params)
	if response["error"] != nil {
		t.Fatalf("add failed: %s", rpcValue(response))
	}
	delete(w.running, "parent:turn")
	if err := s.finish(w); err != nil {
		t.Fatal(err)
	}
	job := w.pending
	if job == nil {
		t.Fatal("queued entry was not waiting for a slot")
	}
	// The entry has not reached native yet: update it while it waits.
	params = queueParams("parent", "corr-parent", "after")
	params["queuedSubmissionId"] = job.queueID
	params["responsesapiClientMetadata"] = map[string]string{"stage": "after"}
	if response := queueRPC(t, s, "parent-update", "thread/queue/update", params); response["error"] != nil {
		t.Fatalf("update while waiting failed: %s", rpcValue(response))
	}
	state := &stateLease{gate: s.stateStore, state: proxyState{}}
	if err := s.stateReady(appServerEvent{source: "state_ready", job: job, state: state}); err != nil {
		t.Fatal(err)
	}
	turn := rpcObject(lastTurnStart(t, upstream), "params")
	if got := rpcText(turn["responsesapiClientMetadata"]); got != `{"stage":"after"}` {
		t.Fatalf("turn/start metadata = %s, want the latest update", got)
	}
	if !bytes.Contains(rpcBytes(turn["input"]), []byte("after")) {
		t.Fatalf("turn/start did not use the latest input: %s", rpcValue(turn))
	}
}

func TestQueueMetadataBudgetAndDeleteRelease(t *testing.T) {
	s := queueMetadataSession(t)
	const floor = maxAppServerRetainedBytes - 5000
	s.queuedSize = floor
	params := queueParams("thread", "corr-big", "text")
	params["responsesapiClientMetadata"] = map[string]string{"blob": strings.Repeat("v", 8192)}
	if response := queueRPC(t, s, "big-add", "thread/queue/add", params); response["error"] == nil {
		t.Fatal("metadata was not counted in the queue budget")
	}
	if state := s.queueState("thread"); state != nil && len(state.items) != 0 {
		t.Fatalf("rejected add left %d entries", len(state.items))
	}
	if s.queuedSize != floor {
		t.Fatalf("rejected add changed accounting to %d", s.queuedSize)
	}
	response := queueRPC(t, s, "plain-add", "thread/queue/add", queueParams("thread", "corr-plain", "text"))
	if response["error"] != nil {
		t.Fatalf("input without metadata did not fit: %s", rpcValue(response))
	}
	id := rpcString(rpcObject(rpcObject(response, "result"), "queuedSubmission"), "id")
	if response := queueRPC(t, s, "plain-delete", "thread/queue/delete", map[string]any{"threadId": "thread", "queuedSubmissionId": id}); response["error"] != nil {
		t.Fatalf("delete failed: %s", rpcValue(response))
	}
	if s.queuedSize != floor {
		t.Fatalf("delete left %d bytes, want the pre-add size %d", s.queuedSize, floor)
	}

	// Metadata retained with an accepted entry is released by deletion too.
	s = queueMetadataSession(t)
	params = queueParams("thread", "corr-meta", "text")
	params["responsesapiClientMetadata"] = map[string]string{"blob": strings.Repeat("v", 8192)}
	response = queueRPC(t, s, "meta-add", "thread/queue/add", params)
	if response["error"] != nil {
		t.Fatalf("add failed: %s", rpcValue(response))
	}
	if s.queuedSize < 8192 {
		t.Fatalf("metadata missing from queued bytes: %d", s.queuedSize)
	}
	id = rpcString(rpcObject(rpcObject(response, "result"), "queuedSubmission"), "id")
	if response := queueRPC(t, s, "meta-delete", "thread/queue/delete", map[string]any{"threadId": "thread", "queuedSubmissionId": id}); response["error"] != nil {
		t.Fatalf("delete failed: %s", rpcValue(response))
	}
	if s.queuedSize != 0 {
		t.Fatalf("delete kept %d bytes of metadata", s.queuedSize)
	}
}
