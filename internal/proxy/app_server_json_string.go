package proxy

import (
	"bufio"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Expose decoded JSON strings (including object keys and array elements) in
// an otherwise unchanged token stream. Both external-service exclusion and
// quota matching then see the same text, without materializing large values.
type rpcTextReader struct {
	r        *bufio.Reader
	string   rpcStringReader
	inString bool
}

func newRPCTextReader(value rpcRaw) *rpcTextReader {
	return &rpcTextReader{r: bufio.NewReaderSize(value.reader(), rpcCopyBufferSize)}
}

func (r *rpcTextReader) ReadRune() (rune, int, error) {
	if r.inString {
		v, n, err := r.string.ReadRune()
		if errors.Is(err, io.EOF) && r.string.done {
			r.inString = false
			return '"', 1, nil
		}
		return v, n, err
	}
	v, n, err := r.r.ReadRune()
	if err == nil && v == '"' {
		r.inString = true
		r.string = rpcStringReader{r: r.r, started: true}
	}
	return v, n, err
}

// Read decoded string runes without allocating a potentially huge error
// message. This also finds quota text encoded with JSON unicode escapes.
type rpcStringReader struct {
	r          *bufio.Reader
	started    bool
	done       bool
	pending    rune
	hasPending bool
}

func newRPCStringReader(value rpcRaw) *rpcStringReader {
	return &rpcStringReader{r: bufio.NewReaderSize(value.reader(), rpcCopyBufferSize)}
}

func (r *rpcStringReader) hexRune() (rune, error) {
	var digits [4]byte
	if _, err := io.ReadFull(r.r, digits[:]); err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(string(digits[:]), 16, 16)
	return rune(n), err
}

func (r *rpcStringReader) ReadRune() (rune, int, error) {
	if r.hasPending {
		v := r.pending
		r.hasPending = false
		return r.unicodeUnit(v)
	}
	if r.done {
		return 0, 0, io.EOF
	}
	if !r.started {
		r.started = true
		for {
			b, err := r.r.ReadByte()
			if err != nil {
				return 0, 0, err
			}
			if b == ' ' || b == '\t' || b == '\n' || b == '\r' {
				continue
			}
			if b != '"' {
				return 0, 0, rpcSyntax()
			}
			break
		}
	}
	v, n, err := r.r.ReadRune()
	if err != nil {
		return 0, 0, err
	}
	if v == '"' {
		r.done = true
		return 0, 0, io.EOF
	}
	if v != '\\' {
		return v, n, nil
	}
	b, err := r.r.ReadByte()
	if err != nil {
		return 0, 0, err
	}
	switch b {
	case '"', '\\', '/':
		return rune(b), 1, nil
	case 'b':
		return '\b', 1, nil
	case 'f':
		return '\f', 1, nil
	case 'n':
		return '\n', 1, nil
	case 'r':
		return '\r', 1, nil
	case 't':
		return '\t', 1, nil
	case 'u':
		v, err := r.hexRune()
		if err != nil {
			return 0, 0, err
		}
		return r.unicodeUnit(v)
	}
	return 0, 0, rpcSyntax()
}

func (r *rpcStringReader) unicodeUnit(v rune) (rune, int, error) {
	if v >= 0xD800 && v <= 0xDBFF {
		prefix, _ := r.r.Peek(2)
		if string(prefix) == "\\u" {
			_, _ = r.r.Discard(2)
			low, err := r.hexRune()
			if err != nil {
				return 0, 0, err
			}
			if low >= 0xDC00 && low <= 0xDFFF {
				v = utf16.DecodeRune(v, low)
			} else {
				v = utf8.RuneError
				r.pending = low
				r.hasPending = true
			}
		} else {
			v = utf8.RuneError
		}
	} else if utf16.IsSurrogate(v) {
		v = utf8.RuneError
	}
	return v, utf8.RuneLen(v), nil
}

// Diagnostic previews are bounded independently of the forwarded wire data.
func rpcLogText(raw rpcRaw) string {
	const limit = 4096
	if raw == nil {
		return "null"
	}
	if rpcLen(raw) <= rpcMetadataLimit {
		data, err := rpcRead(raw, rpcMetadataLimit)
		if err != nil {
			return "[unreadable RPC value]"
		}
		return string(data)
	}
	prefix := &rpcPreviewWriter{limit: limit}
	_ = raw.write(prefix, make([]byte, rpcCopyBufferSize))
	return string(prefix.data) + "… [truncated]"
}

type rpcPreviewWriter struct {
	data  []byte
	limit int
}

func (w *rpcPreviewWriter) Write(p []byte) (int, error) {
	n := min(len(p), w.limit-len(w.data))
	w.data = append(w.data, p[:n]...)
	if n < len(p) {
		return n, io.ErrShortWrite
	}
	return n, nil
}

func rpcLogString(raw rpcRaw) string {
	if rpcKind(raw) != '"' {
		return ""
	}
	if rpcLen(raw) <= rpcMetadataLimit {
		var text string
		_ = rpcUnmarshal(raw, &text)
		return text
	}
	r := newRPCStringReader(raw)
	var result strings.Builder
	for result.Len() < 4096 {
		v, _, err := r.ReadRune()
		if err != nil {
			return result.String()
		}
		result.WriteRune(v)
	}
	result.WriteString("… [truncated]")
	return result.String()
}

func rpcLogValue(raw rpcRaw) any {
	if rpcLen(raw) <= rpcMetadataLimit {
		return raw
	}
	return map[string]any{"bytes": rpcLen(raw), "preview": rpcLogText(raw), "truncated": true}
}
