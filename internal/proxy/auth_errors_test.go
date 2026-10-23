package proxy

import (
	"testing"
	"time"
)

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
