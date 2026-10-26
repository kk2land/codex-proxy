package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Text records start with a UTC timestamp carrying an explicit timezone, followed by the event and stable key=value fields. Values keep
// printable Unicode readable; ambiguous or multiline values are Go-quoted so a
// record can never inject an extra line.
const logTextTimeLayout = "2006-01-02T15:04:05.000Z07:00"

// Fields useful for skimming come first; everything else is sorted by name.
var logTextPriority = []string{
	"user", "status", "method", "error", "error_code", "stage", "reason",
	"wait_ms", "run_ms", "held_ms", "queue_wait_ms", "slot_wait_ms",
	"interval_wait_ms", "idle_ms", "wait_timeout_ms", "idle_timeout_ms",
	"request_id", "pid", "child_pid", "task_id", "submission_id",
	"thread_id", "turn_id", "rpc_id", "rpc_sequence", "queue_id", "auth_file",
	"process_handle", "session_cwd", "cwd", "args", "limit", "exit_code", "signal",
}

var logTextRank = func() map[string]int {
	ranks := make(map[string]int, len(logTextPriority))
	for index, name := range logTextPriority {
		ranks[name] = index
	}
	return ranks
}()

type logTextField struct {
	name string
	size int
}

// recordText renders one bounded text record. The limit is checked against the
// final encoded bytes, including the trailing newline.
func (l *Logger) recordText(event string, fields map[string]any, maxSize int64) ([]byte, error) {
	now := time.Now().UTC()
	stamp := now.Format(logTextTimeLayout)
	coreEvent, coreRequestID := event, l.requestID

	values := make(map[string]string, len(fields)+2)
	values["request_id"] = textString(l.requestID)
	values["pid"] = strconv.Itoa(os.Getpid())
	truncated := false
	for name, value := range fields {
		if name == "timestamp" || name == "request_id" || name == "pid" || name == "event" || name == "truncated" {
			continue
		}
		rendered, err := textValue(value)
		if err != nil {
			l.diagnostic("encode log field", err)
			truncated = true
			continue
		}
		values[name] = rendered
	}

	build := func() []byte {
		return buildTextLine(stamp, coreEvent, values, truncated)
	}
	line := build()
	if int64(len(line)) <= maxSize {
		return line, nil
	}

	// Drop the largest optional fields first. Required metadata is only
	// shortened below, so every retained record remains correlateable.
	truncated = true
	candidates := make([]logTextField, 0, len(values))
	for name, value := range values {
		if name == "request_id" || name == "pid" {
			continue
		}
		candidates = append(candidates, logTextField{name: name, size: len(textKey(name)) + 1 + len(value)})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].size == candidates[j].size {
			return candidates[i].name < candidates[j].name
		}
		return candidates[i].size > candidates[j].size
	})
	for _, candidate := range candidates {
		delete(values, candidate.name)
		line = build()
		if int64(len(line)) <= maxSize {
			return line, nil
		}
	}

	// Normal events and request IDs fit the minimum size. Bound unusually long
	// core strings as well so an individual line can never exceed the limit.
	for int64(len(line)) > maxSize {
		name := "event"
		if len(coreRequestID) > len(coreEvent) {
			name = "request_id"
		}
		if name == "event" {
			if coreEvent == "" {
				return nil, errors.New("log size is too small for required metadata")
			}
			coreEvent = shortenText(coreEvent)
		} else {
			if coreRequestID == "" {
				return nil, errors.New("log size is too small for required metadata")
			}
			coreRequestID = shortenText(coreRequestID)
			values["request_id"] = textString(coreRequestID)
		}
		line = build()
	}
	return line, nil
}

func shortenText(value string) string {
	runes := []rune(value)
	return string(runes[:len(runes)/2])
}

func buildTextLine(stamp, event string, values map[string]string, truncated bool) []byte {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		left, leftOK := logTextRank[names[i]]
		right, rightOK := logTextRank[names[j]]
		if leftOK != rightOK {
			return leftOK
		}
		if leftOK && left != right {
			return left < right
		}
		return names[i] < names[j]
	})

	var buffer bytes.Buffer
	buffer.Grow(64 + len(names)*16)
	buffer.WriteString(stamp)
	buffer.WriteByte(' ')
	buffer.WriteString(textString(event))
	for _, name := range names {
		buffer.WriteByte(' ')
		buffer.WriteString(textKey(name))
		buffer.WriteByte('=')
		buffer.WriteString(values[name])
	}
	if truncated {
		buffer.WriteString(" truncated=true")
	}
	buffer.WriteByte('\n')
	return buffer.Bytes()
}

// textKey keeps keys parseable even if a caller supplies an unusual name.
func textKey(name string) string {
	for _, r := range name {
		if r <= ' ' || r == '=' || r == '"' || r == '\\' || r == 0x7f || !unicode.IsPrint(r) {
			return strconv.Quote(name)
		}
	}
	if name == "" {
		return `""`
	}
	return name
}

// textString leaves simple values bare for readability. Values that are empty,
// contain whitespace/control characters, would look like JSON, or contain
// quoting/assignment syntax are Go-quoted. Go quoting escapes newlines and
// other controls, which prevents line injection.
func textString(value string) string {
	if !textNeedsQuote(value) {
		return value
	}
	return strconv.Quote(value)
}

func textNeedsQuote(value string) bool {
	if value == "" || !utf8.ValidString(value) {
		return true
	}
	for _, r := range value {
		if r <= ' ' || r == 0x7f || !unicode.IsPrint(r) || r == '=' || r == '"' || r == '\\' {
			return true
		}
	}
	// A bare string that is itself valid JSON would decode back as a number,
	// boolean, array or object instead of a string.
	return json.Valid([]byte(value))
}

// textValue renders scalars directly and complex values as compact JSON.
func textValue(value any) (string, error) {
	switch typed := value.(type) {
	case nil:
		return "null", nil
	case string:
		return textString(typed), nil
	case bool:
		return strconv.FormatBool(typed), nil
	case int:
		return strconv.Itoa(typed), nil
	case int8:
		return strconv.FormatInt(int64(typed), 10), nil
	case int16:
		return strconv.FormatInt(int64(typed), 10), nil
	case int32:
		return strconv.FormatInt(int64(typed), 10), nil
	case int64:
		return strconv.FormatInt(typed, 10), nil
	case uint:
		return strconv.FormatUint(uint64(typed), 10), nil
	case uint8:
		return strconv.FormatUint(uint64(typed), 10), nil
	case uint16:
		return strconv.FormatUint(uint64(typed), 10), nil
	case uint32:
		return strconv.FormatUint(uint64(typed), 10), nil
	case uint64:
		return strconv.FormatUint(typed, 10), nil
	case float32:
		return formatTextFloat(float64(typed), 32)
	case float64:
		return formatTextFloat(typed, 64)
	case json.RawMessage:
		return textJSON(typed)
	default:
		encoded, err := marshalTextJSON(value)
		if err != nil {
			return "", err
		}
		return textJSON(encoded)
	}
}

func formatTextFloat(value float64, bits int) (string, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "", errors.New("unsupported floating-point value")
	}
	return strconv.FormatFloat(value, 'g', -1, bits), nil
}

func marshalTextJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

// textJSON emits compact JSON and escapes Unicode line separators.
// Spaces inside JSON strings remain readable.
func textJSON(raw []byte) (string, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return "", err
	}
	value := compact.String()
	if !utf8.ValidString(value) {
		return "", errors.New("log value is not valid UTF-8")
	}
	var escaped strings.Builder
	escaped.Grow(len(value))
	for _, r := range value {
		switch r {
		case '\u2028':
			escaped.WriteString(`\u2028`)
		case '\u2029':
			escaped.WriteString(`\u2029`)
		case 0x85:
			escaped.WriteString(`\u0085`)
		default:
			escaped.WriteRune(r)
		}
	}
	return escaped.String(), nil
}
