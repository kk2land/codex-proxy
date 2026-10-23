package proxy

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var auth429Text = regexp.MustCompile(`(?i)(?:\b(?:http(?: status)?|status(?: code)?|unexpected status)\s*[:=]?\s*429\b|\b429\s+too many requests\b|\byou(?:'ve| have) hit your usage limit\b|\busage limit (?:has been )?(?:reached|exceeded)\b|\brate limit (?:has been )?(?:reached|exceeded)\b)`)
var externalAuthErrorText = regexp.MustCompile(`(?i)\b(?:mcp|oauth|connector|tool call|tool execution)\b`)

// Inspect native error payloads only, never arbitrary successful results or tool
// output containing a number like 429.
func authRateLimited(raw json.RawMessage) bool {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return false
	}
	// Native diagnostics may wrap an external service's HTTP failure. A tool
	// or MCP/OAuth 429 must not disable the GPT account or stop native Codex.
	if externalAuthErrorText.Match(raw) {
		return false
	}
	var inspect func(any, string) bool
	inspect = func(value any, key string) bool {
		switch v := value.(type) {
		case map[string]any:
			for k, item := range v {
				if inspect(item, k) {
					return true
				}
			}
		case string:
			switch key {
			case "code", "type", "codexErrorInfo":
				if v == "usageLimitExceeded" || v == "rateLimitExceeded" || v == "rate_limit_exceeded" || v == "usage_limit_reached" || v == "usage_limit_exceeded" || v == "429" {
					return true
				}
			case "message", "":
				return auth429Text.MatchString(v)
			}
		case json.Number:
			return (key == "httpStatusCode" || key == "status" || key == "status_code" || key == "code") && string(v) == "429"
		}
		return false
	}
	return inspect(value, "")
}

func authErrorLimit(raw json.RawMessage, now time.Time) authLimit {
	limit := authLimit{At: now.Unix()}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	_ = decoder.Decode(&value)
	var inspect func(any)
	inspect = func(value any) {
		if v, ok := value.(map[string]any); ok {
			for key, item := range v {
				var n int64
				switch x := item.(type) {
				case json.Number:
					n, _ = x.Int64()
				case string:
					n, _ = strconv.ParseInt(x, 10, 64)
					if stamp, err := time.Parse(time.RFC3339, x); err == nil {
						n = stamp.Unix()
					}
				}
				switch key {
				case "resetsAt", "reset_at", "resetAt", "resets_at", "cooldown_until":
					if n > now.Unix() {
						limit.Until = max(limit.Until, n)
					}
				case "retry_after", "retryAfter", "retry_after_secs", "retryAfterSeconds":
					if n > 0 && n <= 365*24*60*60 {
						limit.Until = max(limit.Until, now.Unix()+n)
					}
				}
				inspect(item)
			}
		}
	}
	inspect(value)
	if limit.Until == 0 {
		limit = defaultAuthLimit(now)
	}
	return limit
}

// Native rate-limit notifications are hints for the next actual 429, not
// themselves a reason to disable an account. Only exhausted windows count.
func authResetHint(raw json.RawMessage, now int64) int64 {
	var value any
	_ = json.Unmarshal(raw, &value)
	var until int64
	var inspect func(any)
	inspect = func(value any) {
		if v, ok := value.(map[string]any); ok {
			used, _ := v["usedPercent"].(float64)
			reset, _ := v["resetsAt"].(float64)
			if used >= 100 && reset > float64(now) {
				until = max(until, int64(reset))
			}
			for _, item := range v {
				inspect(item)
			}
		}
	}
	inspect(value)
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
	var raw json.RawMessage
	if json.Unmarshal(line, &event) == nil {
		switch rpcString(event, "type") {
		case "error", "turn.failed":
			raw = rpcErrorPayload(event)
			if raw == nil {
				raw = event["message"]
			}
		}
	} else if o.stderr {
		text := strings.TrimSpace(string(line))
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
