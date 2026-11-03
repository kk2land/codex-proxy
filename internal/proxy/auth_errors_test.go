package proxy

import (
	"bytes"
	"testing"
	"time"
)

func TestAuthEscapedExternalErrorText(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		want bool
	}{
		{"escaped MCP", `"\u004d\u0043\u0050 HTTP status 429 Too Many Requests"`, false},
		{"escaped OAuth", `{"message":"HTTP status 429 from \u004fAuth"}`, false},
		{"escaped connector key", `{"code":429,"\u0063onnector":{"message":"failed"}}`, false},
		{"escaped array marker", `{"code":429,"details":["\u006d\u0063\u0070"]}`, false},
		{"escaped tool call", `{"message":"tool\u0020call failed: HTTP 429"}`, false},
		{"escaped tool execution", `{"code":429,"message":"tool\u0020execution failed"}`, false},
		{"escaped quote", `{"message":"HTTP 429 from \"\u004dCP\""}`, false},
		{"escaped quota", `{"message":"status\u0020code:\u0020429"}`, true},
		{"literal unicode spelling", `"\\u004d\\u0043\\u0050 HTTP 429"`, true},
		{"substring is not MCP", `"x\u004d\u0043\u0050x HTTP 429"`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := rpcInput([]byte(test.raw))
			if got := authRateLimited(raw); got != test.want {
				t.Fatalf("rate limited=%t want=%t", got, test.want)
			}
			var forwarded bytes.Buffer
			if err := writeRPC(&forwarded, rpcMessage{"id": rpcValue(1), "error": raw}); err != nil {
				t.Fatal(err)
			}
			message, err := decodeRPC(forwarded.Bytes())
			if err != nil || rpcText(message["error"]) != test.raw {
				t.Fatalf("auth inspection changed the error payload: %v", err)
			}
		})
	}
}

func TestAuthEscapedMCPNotificationDoesNotLimitAccount(t *testing.T) {
	message, err := decodeRPC([]byte(`{"method":"thread/realtime/error","params":{"message":"\u004d\u0043\u0050 tool HTTP status 429 Too Many Requests"}}`))
	if err != nil {
		t.Fatal(err)
	}
	raw := notificationRPCError("thread/realtime/error", rpcObject(message, "params"))
	if raw == nil || authRateLimited(raw) {
		t.Fatal("escaped external notification was treated as an account 429")
	}
}

func TestAuthOutputObserverColoredDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name   string
		text   string
		stdout bool
		want   bool
	}{
		{name: "native usage limit", text: "\x1b[31m\x1b[1mERROR:\x1b[0m You've hit your usage limit.\n", want: true},
		{name: "colored status", text: "\x1b[1;38;2;255;0;0mERROR:\x1b[0m unexpected status \x1b[31m429\x1b[0m Too Many Requests\n", want: true},
		{name: "tracing label", text: "2026-10-24T00:00:00Z \x1b[31mERROR\x1b[0m HTTP status 429 Too Many Requests\n", want: true},
		{name: "unterminated error", text: "\x1b[31mERROR:\x1b[0m HTTP status 429 Too Many Requests", want: true},
		{name: "external service", text: "\x1b[31mERROR:\x1b[0m \x1b[34mMCP\x1b[0m OAuth HTTP status 429 Too Many Requests\n"},
		{name: "other error", text: "\x1b[31mERROR:\x1b[0m HTTP status 401 Unauthorized\n"},
		{name: "ordinary text", text: "Example: \x1b[31mHTTP status 429 Too Many Requests\x1b[0m\n"},
		{name: "stdout text", text: "\x1b[31mERROR:\x1b[0m HTTP status 429 Too Many Requests\n", stdout: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Unix(2000, 0)
			observer := &authOutputObserver{stderr: !tc.stdout, now: func() time.Time { return now }}
			// Pipe reads may split the escape sequence or error label anywhere.
			for offset := 0; offset < len(tc.text); offset += 3 {
				if _, err := observer.Write([]byte(tc.text[offset:min(offset+3, len(tc.text))])); err != nil {
					t.Fatal(err)
				}
			}
			limit := observer.result()
			if got := limit.At != 0; got != tc.want {
				t.Fatalf("limit=%+v want detected=%v", limit, tc.want)
			}
			if tc.want && limit.Until != 2060 {
				t.Fatalf("unexpected cooldown: %+v", limit)
			}
		})
	}
}
