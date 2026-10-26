package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// decodeLogLine accepts the current text records and historical JSON Lines so
// shared test helpers can read files written across a format change.
func decodeLogLine(line []byte) (map[string]any, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil, nil
	}
	if line[0] == '{' {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, err
		}
		return record, nil
	}
	tokens := splitLogTextTokens(string(line))
	if len(tokens) < 2 {
		return nil, strconv.ErrSyntax
	}
	record := map[string]any{"timestamp": unquoteLogText(tokens[0]), "event": unquoteLogText(tokens[1])}
	for _, token := range tokens[2:] {
		name, value, found := cutLogTextToken(token)
		if !found {
			return nil, errors.New("malformed log field")
		}
		if name == "truncated" && value == "true" {
			record["truncated"] = true
			continue
		}
		decoded, err := decodeLogTextValue(value)
		if err != nil {
			return nil, err
		}
		record[unquoteLogText(name)] = decoded
	}
	return record, nil
}

func cutLogTextToken(token string) (string, string, bool) {
	quoted, escaped := false, false
	for index := 0; index < len(token); index++ {
		switch {
		case escaped:
			escaped = false
		case quoted && token[index] == '\\':
			escaped = true
		case token[index] == '"':
			quoted = !quoted
		case token[index] == '=' && !quoted:
			return token[:index], token[index+1:], true
		}
	}
	return "", "", false
}

func splitLogTextTokens(line string) []string {
	var tokens []string
	start, quoted, escaped := 0, false, false
	for index := 0; index < len(line); index++ {
		switch {
		case escaped:
			escaped = false
		case quoted && line[index] == '\\':
			escaped = true
		case line[index] == '"':
			quoted = !quoted
		case line[index] == ' ' && !quoted:
			tokens = append(tokens, line[start:index])
			start = index + 1
		}
	}
	return append(tokens, line[start:])
}

func unquoteLogText(value string) string {
	if strings.HasPrefix(value, `"`) {
		if unquoted, err := strconv.Unquote(value); err == nil {
			return unquoted
		}
	}
	return value
}

func decodeLogTextValue(value string) (any, error) {
	if strings.HasPrefix(value, `"`) {
		return strconv.Unquote(value)
	}
	var decoded any
	if err := json.Unmarshal([]byte(value), &decoded); err == nil {
		return decoded, nil
	}
	return value, nil
}

func readLogRecordsAt(t *testing.T, dir, filename string, maxSize int64) []map[string]any {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, filename+"*"))
	if err != nil {
		t.Fatal(err)
	}
	var records []map[string]any
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(data)) > maxSize {
			t.Fatalf("log %s is %d bytes, limit is %d", path, len(data), maxSize)
		}
		if len(data) > 0 && data[len(data)-1] != '\n' {
			t.Fatalf("log %s has an incomplete final record", path)
		}
		scanner := bufio.NewScanner(bytes.NewReader(data))
		scanner.Buffer(make([]byte, 1024), int(maxSize)+1)
		for scanner.Scan() {
			record, err := decodeLogLine(scanner.Bytes())
			if err != nil {
				t.Fatalf("invalid record in %s: %v", path, err)
			}
			if record != nil {
				records = append(records, record)
			}
		}
		if err := scanner.Err(); err != nil {
			t.Fatal(err)
		}
	}
	return records
}

func TestLoggerTextFormatDefaultsReadable(t *testing.T) {
	dir := t.TempDir()
	var diagnostics bytes.Buffer
	logger := NewLogger(dir, "req-文本 1", &diagnostics, func() (Config, error) {
		return Config{LogMaxSize: 1 << 20, LogMaxBackups: 2}, nil
	})
	logger.Log("started", map[string]any{
		"user": "张三", "status": "completed", "error": "boom\ncodex-proxy: injected",
		"count": 3, "ratio": 1.5, "ok": true, "payload": map[string]any{"reason": "上游 timeout", "retry_after_ms": 50},
	})
	data, err := os.ReadFile(filepath.Join(dir, loggerFilename))
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics.Len() != 0 {
		t.Fatalf("unexpected diagnostics: %s", diagnostics.String())
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("text record is not single-line: %q", data)
	}
	line := lines[0]
	stamp, rest, found := strings.Cut(line, " ")
	if !found {
		t.Fatalf("missing timestamp: %q", line)
	}
	if _, err := time.Parse(time.RFC3339Nano, stamp); err != nil {
		t.Fatalf("timestamp %q is not RFC3339 with explicit offset: %v", stamp, err)
	}
	if !strings.HasPrefix(rest, "started ") {
		t.Fatalf("event is not readable after the timestamp: %q", rest)
	}
	// Priority fields appear first, then stable name ordering.
	userIndex := strings.Index(rest, "user=张三")
	statusIndex := strings.Index(rest, "status=completed")
	errorIndex := strings.Index(rest, `error="boom\ncodex-proxy: injected"`)
	countIndex := strings.Index(rest, "count=3")
	if userIndex < 0 || statusIndex < 0 || errorIndex < 0 || countIndex < 0 {
		t.Fatalf("missing or unreadable fields: %q", rest)
	}
	if !(userIndex < statusIndex && statusIndex < errorIndex && errorIndex < countIndex) {
		t.Fatalf("field priority or ordering unstable: %q", rest)
	}
	if strings.Contains(line, "\n") || strings.Contains(line, "\x00") {
		t.Fatalf("control characters were not escaped: %q", line)
	}
	if !strings.Contains(rest, `payload={"reason":"上游 timeout","retry_after_ms":50}`) {
		t.Fatalf("complex value is not compact JSON: %q", rest)
	}
	record, err := decodeLogLine([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if record["error"] != "boom\ncodex-proxy: injected" || record["user"] != "张三" || record["count"] != float64(3) {
		t.Fatalf("text record does not round-trip: %v", record)
	}
}

func TestLoggerTextFormatByteLimit(t *testing.T) {
	dir := t.TempDir()
	logger := NewLogger(dir, "req-limit", nil, func() (Config, error) {
		return Config{LogMaxSize: 256, LogMaxBackups: 0}, nil
	})
	logger.Log("large", map[string]any{
		"user": "sandbox-user", "status": "completed", "sequence": 7,
		"payload": strings.Repeat("大", 500),
	})
	records := readLoggerRecords(t, dir, 256)
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	record := records[0]
	if record["truncated"] != true {
		t.Fatalf("oversized text record is not marked truncated: %v", record)
	}
	if record["sequence"] != float64(7) || record["user"] != "sandbox-user" || record["status"] != "completed" {
		t.Fatalf("small fields were dropped: %v", record)
	}
	if _, found := record["payload"]; found {
		t.Fatalf("oversized field was retained: %v", record)
	}
	data, err := os.ReadFile(filepath.Join(dir, loggerFilename))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 256 || data[len(data)-1] != '\n' {
		t.Fatalf("text record violates the byte limit: %d bytes", len(data))
	}
}
