package proxy

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func claudeLogRecords(t *testing.T, home string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".claude-proxy", "claude-proxy.log"))
	if err != nil {
		t.Fatal(err)
	}
	var records []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

func TestClaudeNativeErrorMetadataIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want map[string]any
	}{
		{"stream", `{"type":"stream_event","event":{"delta":{"text":"private output text"}}}`, nil},
		{"notification", `{"type":"system","subtype":"notification","text":"private output text"}`, nil},
		{"successful control", `{"type":"control_response","response":{"subtype":"success","request_id":"private output text"}}`, nil},
		{"successful result", `{"type":"result","subtype":"success","is_error":false,"result":"private output text"}`, nil},
		{"null error", `{"type":"assistant","error":null,"message":{"content":"private output text"}}`, nil},
		{"empty auth error", `{"type":"auth_status","error":"","output":["private output text"]}`, nil},
		{"unknown healthy", `{"type":"private output text","error":null}`, nil},
		{"invalid JSON", `private output text`, nil},
		{"assistant error", `{"type":"assistant","error":"rate_limit","message":{"content":"private output text"}}`, map[string]any{"type": "assistant", "error_type": "rate_limit"}},
		{"retry", `{"type":"system","subtype":"api_retry","error":"overloaded","error_status":503,"uuid":"private output text"}`, map[string]any{"type": "system", "subtype": "api_retry", "error_type": "overloaded", "http_status": int64(503)}},
		{"result error", `{"type":"result","subtype":"error_max_turns","is_error":true,"errors":["private output text"]}`, map[string]any{"type": "result", "subtype": "error_max_turns"}},
		{"flagged result", `{"type":"result","subtype":"success","is_error":true,"api_error_status":500,"result":"private output text"}`, map[string]any{"type": "result", "subtype": "success", "http_status": int64(500)}},
		{"mirror error", `{"type":"system","subtype":"mirror_error","error":"private output text"}`, map[string]any{"type": "system", "subtype": "mirror_error", "error_type": "unknown"}},
		{"control error", `{"type":"control_response","response":{"subtype":"error","request_id":"private output text","error":"private output text"}}`, map[string]any{"type": "control_response", "stage": "control_response", "subtype": "error", "error_type": "unknown"}},
		{"integer code", `{"type":"error","error":{"code":-32000,"message":"private output text"}}`, map[string]any{"type": "error", "error_code": int64(-32000)}},
		{"unknown error category", `{"type":"private output text","subtype":"private output text","error":{"code":-32002,"message":"private output text"}}`, map[string]any{"type": "other", "subtype": "unknown", "error_code": int64(-32002)}},
		{"invalid numeric metadata", `{"type":"error","error_status":999,"error":{"code":"private output text"}}`, map[string]any{"type": "error"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := claudeOutputErrorFields([]byte(tc.line))
			if tc.want != nil {
				if tc.want["stage"] == nil {
					tc.want["stage"] = "notification"
				}
				tc.want["status"], tc.want["direction"] = "failed", "server_to_client"
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("metadata=%v want=%v", got, tc.want)
			}
			data, _ := json.Marshal(got)
			if strings.Contains(string(data), "private output text") {
				t.Fatal("native output entered log metadata")
			}
		})
	}
}

func TestClaudeStreamLogsOnlyErrorsAndTaskLifecycle(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	configureClaude(t, home, 1, "5s")
	const privateText = "private native notification and error text"
	var events bytes.Buffer
	encoder := json.NewEncoder(&events)
	for i := 0; i < 100; i++ {
		if err := encoder.Encode(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "delta": map[string]any{"text": privateText}}, "uuid": privateText}); err != nil {
			t.Fatal(err)
		}
	}
	for _, message := range []map[string]any{
		{"type": "system", "subtype": "status", "status": "compacting"},
		{"type": "system", "subtype": "notification", "text": privateText},
		{"type": "assistant", "error": nil, "message": map[string]any{"content": privateText}},
		{"type": "control_request", "request_id": privateText, "request": map[string]any{"subtype": "can_use_tool", "input": privateText}},
		{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": privateText, "response": privateText}},
		{"type": "result", "subtype": "success", "is_error": false, "result": privateText},
		{"type": "assistant", "error": "rate_limit", "message": map[string]any{"content": privateText}},
		{"type": "system", "subtype": "api_retry", "error": "rate_limit", "error_status": 429, "uuid": privateText},
		{"type": "system", "subtype": "mirror_error", "error": privateText},
		{"type": "result", "subtype": "error_max_turns", "is_error": true, "errors": []string{privateText}},
		{"type": "result", "subtype": "success", "is_error": true, "api_error_status": 500, "result": privateText},
		{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": privateText, "error": privateText}},
		{"type": "error", "error": map[string]any{"code": -32000, "message": privateText}},
		{"type": privateText, "subtype": privateText, "error": map[string]any{"code": -32002, "message": privateText}},
	} {
		if err := encoder.Encode(message); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "native-events.jsonl")
	if err := os.WriteFile(path, events.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	input := `{"type":"control_request","request_id":"init","request":{"subtype":"initialize"}}` + "\n" + `{"type":"user","message":{"role":"user","content":"private native notification and error text"}}` + "\n"
	stream := startClaude(t, home, "stream", dir, input, []string{"FAKE_CLAUDE_EVENT_FILE=" + path}, claudeStreamArgs...)
	if code := childExit(t, stream, 5*time.Second); code != 0 {
		t.Fatalf("native errors changed process exit: %d %s", code, stream.stderr.String())
	}
	if !bytes.Contains(stream.stdout.Bytes(), events.Bytes()) {
		t.Fatal("log filtering changed or lost protocol output")
	}
	records := claudeLogRecords(t, home)
	started := findAppLog(t, records, "started", "", nil)
	counts := make(map[string]int)
	for _, record := range records {
		event, _ := record["event"].(string)
		counts[event]++
		switch event {
		case "notification", "rpc_received", "rpc_started", "rpc_finished":
			t.Fatalf("unexpected per-message log: %v", record)
		case "error":
			if record["status"] != "failed" || record["direction"] != "server_to_client" || record["child_pid"] != started["child_pid"] || record["request_id"] != started["request_id"] || record["mode"] != "stream-json" || record["user"] != "sandbox-user" {
				t.Fatalf("missing native error correlation: %v", record)
			}
		}
	}
	if counts["error"] != 8 || counts["received"] != 1 || counts["submitted"] != 1 || counts["finished"] != 1 {
		t.Fatalf("noisy or missing lifecycle/error logs: %v", counts)
	}
	if retry := findAppLog(t, records, "error", "subtype", "api_retry"); retry["http_status"] != float64(429) || retry["error_type"] != "rate_limit" {
		t.Fatalf("missing retry error metadata: %v", retry)
	}
	if failure := findAppLog(t, records, "error", "type", "error"); failure["error_code"] != float64(-32000) {
		t.Fatalf("missing native error code: %v", failure)
	}
	if control := findAppLog(t, records, "error", "type", "control_response"); control["stage"] != "control_response" {
		t.Fatalf("wrong control response stage: %v", control)
	}
	logData, _ := json.Marshal(records)
	if bytes.Contains(logData, []byte(privateText)) {
		t.Fatal("logs leaked native output or error bodies")
	}
}
