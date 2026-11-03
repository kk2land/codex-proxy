package proxy

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var auth429Text = regexp.MustCompile(`(?i)(?:\b(?:http(?: status)?|status(?: code)?|unexpected status)\s*[:=]?\s*429\b|\b429\s+too many requests\b|\byou(?:'ve| have) hit your usage limit\b|\busage limit (?:has been )?(?:reached|exceeded)\b|\brate limit (?:has been )?(?:reached|exceeded)\b)`)
var externalAuthErrorText = regexp.MustCompile(`(?i)\b(?:mcp|oauth|connector|tool call|tool execution)\b`)
var authANSICSI = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

// Inspect native error payloads only, never arbitrary successful results or tool
// output containing a number like 429.
func authRateLimited(input any) bool {
	raw := rpcInput(input)
	// Scan the error payload only; a tool/MCP failure is not an account 429.
	if externalAuthErrorText.MatchReader(newRPCTextReader(raw)) {
		return false
	}
	if store := raw.store(); store != nil && store.error() != nil {
		return false
	}
	var inspect func(rpcRaw, string) bool
	inspect = func(value rpcRaw, key string) bool {
		switch rpcKind(value) {
		case '{':
			object, err := rpcObjectValue(value)
			if err != nil {
				return false
			}
			for k, item := range object {
				if inspect(item, k) {
					return true
				}
			}
		case '"':
			switch key {
			case "code", "type", "codexErrorInfo":
				if rpcLen(value) > 128 {
					return false
				}
				var text string
				if rpcUnmarshal(value, &text) != nil {
					return false
				}
				return text == "usageLimitExceeded" || text == "rateLimitExceeded" || text == "rate_limit_exceeded" || text == "usage_limit_reached" || text == "usage_limit_exceeded" || text == "429"
			case "message", "":
				return auth429Text.MatchReader(newRPCStringReader(value))
			}
		default:
			if rpcLen(value) <= 64 && (key == "httpStatusCode" || key == "status" || key == "status_code" || key == "code") {
				return rpcText(value) == "429"
			}
		}
		return false
	}
	limited := inspect(raw, "")
	if store := raw.store(); store != nil && store.error() != nil {
		return false
	}
	return limited
}

func authErrorLimit(input any, now time.Time) authLimit {
	limit := authLimit{At: now.Unix()}
	walkRPCObjects(rpcInput(input), func(object rpcMessage) {
		for key, item := range object {
			switch key {
			case "resetsAt", "reset_at", "resetAt", "resets_at", "cooldown_until":
				if n := rpcAuthTimestamp(item); n > now.Unix() {
					limit.Until = max(limit.Until, n)
				}
			case "retry_after", "retryAfter", "retry_after_secs", "retryAfterSeconds":
				if n := rpcAuthTimestamp(item); n > 0 && n <= 365*24*60*60 {
					limit.Until = max(limit.Until, now.Unix()+n)
				}
			}
		}
	})
	if limit.Until == 0 {
		limit = defaultAuthLimit(now)
	}
	return limit
}

func rpcAuthTimestamp(raw rpcRaw) int64 {
	// Allow a complete RFC3339Nano timestamp even when every character uses
	// a six-byte JSON unicode escape.
	if raw == nil || rpcLen(raw) > 256 {
		return 0
	}
	text := rpcText(raw)
	if rpcKind(raw) == '"' {
		_ = rpcUnmarshal(raw, &text)
	}
	n, _ := strconv.ParseInt(text, 10, 64)
	if stamp, err := time.Parse(time.RFC3339, text); err == nil {
		n = stamp.Unix()
	}
	return n
}

func walkRPCObjects(raw rpcRaw, visit func(rpcMessage)) {
	if rpcKind(raw) != '{' {
		return
	}
	object, err := rpcObjectValue(raw)
	if err != nil {
		return
	}
	visit(object)
	for _, item := range object {
		walkRPCObjects(item, visit)
	}
}

// A rate-limit notification extends a recorded cooldown but cannot start one.
func authResetHint(input any, now int64) int64 {
	var until int64
	walkRPCObjects(rpcInput(input), func(object rpcMessage) {
		var used, reset float64
		if rpcLen(object["resetsAt"]) <= 128 {
			_ = rpcUnmarshal(object["resetsAt"], &reset)
		}
		if rpcLen(object["usedPercent"]) <= 128 {
			_ = rpcUnmarshal(object["usedPercent"], &used)
		}
		if used >= 100 && reset > float64(now) {
			until = max(until, int64(reset))
		}
	})
	return until
}

// exec retains byte-for-byte stdout/stderr while observing bounded complete
// lines. The callback only schedules persistence and shutdown; it must not wait
// for state.lock, since the launching goroutine may still hold that lock.
type authOutputObserver struct {
	mu      sync.Mutex
	buffer  []byte
	drop    bool
	stderr  bool
	now     func() time.Time
	limit   authLimit
	onLimit func(authLimit)
}

func (o *authOutputObserver) Write(data []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, b := range data {
		if b == '\n' {
			if !o.drop {
				o.line(o.buffer)
			}
			o.buffer, o.drop = o.buffer[:0], false
		} else if !o.drop {
			if len(o.buffer) >= 1<<20 {
				o.buffer, o.drop = o.buffer[:0], true
			} else {
				o.buffer = append(o.buffer, b)
			}
		}
	}
	return len(data), nil
}

func (o *authOutputObserver) line(line []byte) {
	var event rpcMessage
	var raw rpcRaw
	if json.Unmarshal(line, &event) == nil {
		switch rpcString(event, "type") {
		case "error", "turn.failed":
			raw = rpcErrorPayload(event)
			if raw == nil {
				raw = event["message"]
			}
		}
	} else if o.stderr {
		// --color always styles the ERROR label and may style its message too.
		// Normalize only the detection copy; stdout/stderr are forwarded intact.
		text := strings.TrimSpace(authANSICSI.ReplaceAllString(string(line), ""))
		if strings.HasPrefix(text, "Error:") || strings.HasPrefix(text, "ERROR:") || strings.Contains(text, " ERROR ") {
			raw = rpcValue(text)
		}
	}
	if authRateLimited(raw) {
		limit := authErrorLimit(raw, o.now())
		o.limit = authLimit{At: max(o.limit.At, limit.At), Until: max(o.limit.Until, limit.Until)}
		if o.onLimit != nil {
			o.onLimit(o.limit)
		}
	}
}

func (o *authOutputObserver) result() authLimit {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.drop && len(o.buffer) != 0 {
		o.line(o.buffer)
		o.buffer = nil
	}
	return o.limit
}
