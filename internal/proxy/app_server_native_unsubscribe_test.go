package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Opt in with CODEX_PROXY_NATIVE_BIN=/absolute/path/to/native/codex.
// Only the Responses endpoints are mocked: the existing process fixture runs
// the real proxy Run entrypoint and launches the supplied native app-server.
func nativeUnsubscribeBinary(t *testing.T) string {
	t.Helper()
	binary := os.Getenv("CODEX_PROXY_NATIVE_BIN")
	if binary == "" {
		t.Skip("set CODEX_PROXY_NATIVE_BIN to run the native app-server regression")
	}
	version, err := exec.Command(binary, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("native version: %v: %s", err, version)
	}
	t.Log(strings.TrimSpace(string(version)))
	return binary
}

func localResponses(t *testing.T, provider string, gate <-chan struct{}, fail bool) (*httptest.Server, <-chan rpcMessage) {
	t.Helper()
	requests := make(chan rpcMessage, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		var request rpcMessage
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		select {
		case requests <- request:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		item := map[string]any{"type": "message", "id": "msg_" + provider, "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": provider + "-answer", "annotations": []any{}}}}
		events := []any{
			map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_" + provider, "status": "in_progress", "output": []any{}}},
			map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item},
			map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_" + provider, "status": "completed", "output": []any{item},
				"usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}}},
		}
		if fail {
			events = []any{map[string]any{"type": "response.failed", "response": map[string]any{"id": "resp_failed", "status": "failed",
				"error": map[string]any{"code": "invalid_request_error", "message": "local regression failure"}}}}
		}
		for _, event := range events {
			fmt.Fprintf(w, "data: %s\n\n", rpcValue(event))
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(server.Close)
	return server, requests
}

func startNativeUnsubscribeApp(t *testing.T, binary, providerA, providerB string) *appClient {
	t.Helper()
	home := t.TempDir()
	configureAppProxy(t, home, binary, 2, "5s")
	codexHome := filepath.Join(home, "sandbox-user", ".codex")
	if err := os.MkdirAll(codexHome, 0700); err != nil {
		t.Fatal(err)
	}
	config := "model = \"gpt-5.4\"\nmodel_provider = \"local_a\"\n"
	for i, url := range []string{providerA, providerB} {
		config += fmt.Sprintf("\n[model_providers.local_%c]\nname = \"Local %c\"\nbase_url = %q\nwire_api = \"responses\"\nrequires_openai_auth = false\nsupports_websockets = false\nrequest_max_retries = 0\nstream_max_retries = 0\n", 'a'+i, 'A'+i, url+"/v1")
	}
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	// The proxy must override the native inactivity grace period, including
	// an explicit CLI value, without relying on a client's longer timeout.
	return startApp(t, home, "app-server", "-c", "thread_unload_delay_secs=900")
}

func nativeTestThread(t *testing.T, client *appClient) string {
	t.Helper()
	result := client.call("thread/start", map[string]any{"modelProvider": "local_a", "historyMode": "legacy", "cwd": filepath.Join(client.home, "sandbox-user")})
	return rpcString(rpcObject(result, "thread"), "id")
}

func awaitThreadNotification(t *testing.T, client *appClient, method, thread string) rpcMessage {
	t.Helper()
	return client.await(func(message rpcMessage) bool {
		return rpcString(message, "method") == method && rpcString(rpcObject(message, "params"), "threadId") == thread
	})
}

func awaitResponsesRequest(t *testing.T, requests <-chan rpcMessage) rpcMessage {
	t.Helper()
	select {
	case request := <-requests:
		return request
	case <-time.After(5 * time.Second):
		t.Fatal("native Codex did not request the local Responses service")
		return nil
	}
}

func TestNativeAppServerUnsubscribeProviderSwitch(t *testing.T) {
	binary := nativeUnsubscribeBinary(t)
	a, requestsA := localResponses(t, "A", nil, false)
	b, requestsB := localResponses(t, "B", nil, false)
	client := startNativeUnsubscribeApp(t, binary, a.URL, b.URL)
	thread, other := nativeTestThread(t, client), nativeTestThread(t, client)
	turn := func(id, prompt string, requests <-chan rpcMessage) rpcMessage {
		t.Helper()
		client.call("turn/start", taskParams(id, prompt))
		request := awaitResponsesRequest(t, requests)
		completed := awaitThreadNotification(t, client, "turn/completed", id)
		if status := rpcString(rpcObject(rpcObject(completed, "params"), "turn"), "status"); status != "completed" {
			t.Fatalf("native turn status=%s: %s", status, rpcValue(completed))
		}
		awaitSlots(t, client.home, 0)
		return request
	}
	turn(thread, "provider-switch-first-question", requestsA)
	turn(other, "other-session-first-question", requestsA)
	if result := client.call("thread/unsubscribe", map[string]any{"threadId": thread}); rpcString(result, "status") != "unsubscribed" {
		t.Fatalf("unsubscribe: %s", rpcValue(result))
	}
	awaitThreadNotification(t, client, "thread/closed", thread)
	loaded := client.call("thread/loaded/list", map[string]any{})
	if bytes.Contains(loaded["data"], []byte(thread)) || !bytes.Contains(loaded["data"], []byte(other)) {
		t.Fatalf("unexpected loaded threads after native close: %s", rpcValue(loaded))
	}
	if result := client.call("thread/unsubscribe", map[string]any{"threadId": thread}); rpcString(result, "status") != "notLoaded" {
		t.Fatalf("closed thread unsubscribe: %s", rpcValue(result))
	}
	resumed := client.call("thread/resume", map[string]any{"threadId": thread, "modelProvider": "local_b"})
	if rpcString(rpcObject(resumed, "thread"), "id") != thread || rpcString(resumed, "modelProvider") != "local_b" {
		t.Fatalf("provider switch did not keep the original thread: %s", rpcValue(resumed))
	}
	requestB := turn(thread, "provider-switch-second-question", requestsB)
	for _, history := range []string{"provider-switch-first-question", "A-answer", "provider-switch-second-question"} {
		if !bytes.Contains(requestB["input"], []byte(history)) {
			t.Fatalf("B request is missing history %q: %s", history, requestB["input"])
		}
	}
	requestA := turn(other, "other-session-second-question", requestsA)
	if !bytes.Contains(requestA["input"], []byte("other-session-first-question")) || bytes.Contains(requestA["input"], []byte("provider-switch-first-question")) {
		t.Fatalf("other A session lost its isolated history: %s", requestA["input"])
	}
	if len(requestsA) != 0 || len(requestsB) != 0 {
		t.Fatal("unexpected extra model requests")
	}
	if code := client.stop(); code != 0 {
		t.Fatalf("proxy exit=%d", code)
	}
	t.Log("A first turn -> native close -> same thread on B with history; other thread stays on A")
}

func TestNativeAppServerUnsubscribeRunningTurn(t *testing.T) {
	binary := nativeUnsubscribeBinary(t)
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed=%t", failed), func(t *testing.T) {
			gate := make(chan struct{})
			a, requests := localResponses(t, "A", gate, failed)
			client := startNativeUnsubscribeApp(t, binary, a.URL, a.URL)
			thread := nativeTestThread(t, client)
			client.call("turn/start", taskParams(thread, "unsubscribe-during-running-turn"))
			awaitResponsesRequest(t, requests)
			awaitSlots(t, client.home, 1)
			if result := client.call("thread/unsubscribe", map[string]any{"threadId": thread}); rpcString(result, "status") != "unsubscribed" {
				t.Fatalf("unsubscribe: %s", rpcValue(result))
			}
			if result := client.call("thread/unsubscribe", map[string]any{"threadId": thread}); rpcString(result, "status") != "notSubscribed" {
				t.Fatalf("repeated unsubscribe: %s", rpcValue(result))
			}
			awaitSlots(t, client.home, 1)
			awaitAbsent(t, client, "thread/closed")
			close(gate)
			awaitThreadNotification(t, client, "thread/closed", thread)
			awaitSlots(t, client.home, 0)
			awaitAbsent(t, client, "turn/completed")
			// Read native persisted history after unloading to distinguish a
			// completed/failed turn from cancellation by an early unsubscribe.
			read := client.call("thread/read", map[string]any{"threadId": thread, "includeTurns": true})
			var turns []rpcMessage
			if err := json.Unmarshal(rpcObject(read, "thread")["turns"], &turns); err != nil || len(turns) != 1 {
				t.Fatalf("native turn history: %v: %s", err, rpcValue(read))
			}
			want := "completed"
			if failed {
				want = "failed"
			}
			if rpcString(turns[0], "status") != want {
				t.Fatalf("native turn status=%s, want %s", rpcString(turns[0], "status"), want)
			}
			if code := client.stop(); code != 0 {
				t.Fatalf("proxy exit=%d", code)
			}
		})
	}
}
