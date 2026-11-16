package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// RPC bodies are immutable byte ranges. Large ranges refer to an unlinked
// spool file; inspecting an object indexes only its immediate members. Arrays
// and strings in history/tool/image payloads are never decoded just to forward
// them. Generated objects contain references to the original ranges as well.
type rpcRaw = *rpcJSON

type rpcJSON struct {
	raw       []byte
	file      *rpcSpoolFile
	spool     *rpcSpool
	budget    *rpcMetadataBudget
	offset    int64
	length    int64
	object    rpcMessage
	array     []*rpcJSON
	indexed   bool
	indexErr  error
	encodeErr error
}

type rpcMessage map[string]*rpcJSON

const (
	rpcMemoryThreshold = 32 << 20
	rpcCopyBufferSize  = 64 << 10
	rpcMetadataLimit   = 1 << 20
	rpcMaxDepth        = 10000
)

func rpcInput(value any) rpcRaw {
	switch v := value.(type) {
	case nil:
		return nil
	case rpcRaw:
		return v
	case []byte:
		if v == nil {
			v = []byte{}
		}
		return &rpcJSON{raw: v}
	case json.RawMessage:
		if v == nil {
			v = json.RawMessage{}
		}
		return &rpcJSON{raw: v}
	default:
		panic("invalid RPC input")
	}
}

func rpcValue(value any) rpcRaw {
	switch v := value.(type) {
	case rpcRaw:
		return v
	case json.RawMessage:
		if v == nil {
			return rpcValue(nil)
		}
		return &rpcJSON{raw: v}
	case rpcMessage:
		if v == nil {
			return rpcValue(nil)
		}
		m := make(rpcMessage, len(v))
		for key, item := range v {
			m[key] = item
		}
		return &rpcJSON{object: m, indexed: true}
	case map[string]any:
		if v == nil {
			return rpcValue(nil)
		}
		m := make(rpcMessage, len(v))
		for key, item := range v {
			m[key] = rpcValue(item)
		}
		return &rpcJSON{object: m, indexed: true}
	case []rpcRaw:
		if v == nil {
			return rpcValue(nil)
		}
		return &rpcJSON{array: append([]rpcRaw{}, v...)}
	case []any:
		if v == nil {
			return rpcValue(nil)
		}
		a := make([]rpcRaw, len(v))
		for i, item := range v {
			a[i] = rpcValue(item)
		}
		return &rpcJSON{array: a}
	case *appQueuedInput:
		if v == nil {
			return rpcValue(nil)
		}
		return rpcValue(map[string]any{"id": v.ID, "clientUserMessageId": v.ClientUserMessageID, "input": v.Input, "responsesapiClientMetadata": v.ClientMetadata})
	case []*appQueuedInput:
		if v == nil {
			return rpcValue(nil)
		}
		a := make([]rpcRaw, len(v))
		for i, item := range v {
			a[i] = rpcValue(item)
		}
		return &rpcJSON{array: a}
	default:
		data, err := json.Marshal(value)
		if err != nil {
			return &rpcJSON{encodeErr: err}
		}
		return &rpcJSON{raw: data}
	}
}

func (v *rpcJSON) reader() io.Reader {
	if v == nil {
		return strings.NewReader("null")
	}
	if v.file != nil {
		return &rpcSpoolReader{reader: io.NewSectionReader(v.file.file, v.offset, v.length), store: v.file.store}
	}
	if v.raw != nil {
		if v.spool != nil {
			return &rpcSpoolReader{reader: bytes.NewReader(v.raw), store: v.spool}
		}
		return bytes.NewReader(v.raw)
	}
	if v.encodeErr != nil {
		return rpcErrorReader{v.encodeErr}
	}
	var parts []io.Reader
	if v.object != nil {
		keys := make([]string, 0, len(v.object))
		for key := range v.object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts = append(parts, strings.NewReader("{"))
		for i, key := range keys {
			if i > 0 {
				parts = append(parts, strings.NewReader(","))
			}
			encoded, _ := json.Marshal(key)
			parts = append(parts, bytes.NewReader(append(encoded, ':')), v.object[key].reader())
		}
		parts = append(parts, strings.NewReader("}"))
	} else if v.array != nil {
		parts = append(parts, strings.NewReader("["))
		for i, item := range v.array {
			if i > 0 {
				parts = append(parts, strings.NewReader(","))
			}
			parts = append(parts, item.reader())
		}
		parts = append(parts, strings.NewReader("]"))
	} else {
		return strings.NewReader("null")
	}
	return io.MultiReader(parts...)
}

type rpcErrorReader struct{ err error }

func (r rpcErrorReader) Read([]byte) (int, error) { return 0, r.err }

func rpcLen(v rpcRaw) int {
	if v == nil {
		return 0
	}
	if v.file != nil {
		return int(v.length)
	}
	if v.raw != nil {
		return len(v.raw)
	}
	n := 2
	if v.object != nil {
		for key, item := range v.object {
			encoded, _ := json.Marshal(key)
			n += len(encoded) + 1 + rpcWireLen(item) + 1
		}
		if len(v.object) > 0 {
			n--
		}
	} else if v.array != nil {
		for _, item := range v.array {
			n += rpcWireLen(item) + 1
		}
		if len(v.array) > 0 {
			n--
		}
	}
	return n
}

func rpcWireLen(v rpcRaw) int {
	if v == nil {
		return 4
	}
	return rpcLen(v)
}

func (v *rpcJSON) String() string { return rpcLogText(v) }

func (v *rpcJSON) slice(offset, length int64) (rpcRaw, error) {
	budget := v.metadataBudget()
	if v.file == nil {
		data := v.raw[offset : offset+length : offset+length]
		// Detach small scalar IDs/statuses so retaining one does not pin the
		// entire input. Containers always share their body: copying each small
		// nested object would recreate the depth-dependent memory expansion.
		if length > 0 && length <= 4096 && data[0] != '{' && data[0] != '[' {
			if err := v.reserveMetadata(int(length) + 128); err != nil {
				return nil, err
			}
			data = bytes.Clone(data)
		}
		return &rpcJSON{raw: data, spool: v.spool, budget: budget}, nil
	}
	// Detach small IDs/statuses from a large file so an outstanding request
	// does not keep its already-forwarded payload on disk.
	if length <= 4096 {
		if err := v.reserveMetadata(int(length) + 128); err != nil {
			return nil, err
		}
		data := make([]byte, int(length))
		_, err := v.file.file.ReadAt(data, v.offset+offset)
		if err != nil {
			return nil, v.fail(err)
		}
		return &rpcJSON{raw: data, spool: v.file.store, budget: budget}, nil
	}
	return &rpcJSON{file: v.file, offset: v.offset + offset, length: length}, nil
}

func (v *rpcJSON) metadataBudget() *rpcMetadataBudget {
	if v.file != nil {
		return &v.file.budget
	}
	if v.budget == nil {
		v.budget = &rpcMetadataBudget{}
	}
	return v.budget
}

func (v *rpcJSON) reserveMetadata(n int) error {
	return v.fail(v.metadataBudget().reserve(n))
}

// Queue entries keep only their input after the enclosing request is gone.
// Copy that one retained memory range so a tiny input cannot pin an otherwise
// discarded large frame. File-backed ranges are managed by collectSpools.
func rpcRetainInput(v rpcRaw) rpcRaw {
	if v == nil || v.file != nil || v.raw == nil {
		return v
	}
	return &rpcJSON{raw: bytes.Clone(v.raw), spool: v.spool}
}

// Pending RPCs need the original ID after the forwarded request body is gone.
// Detach bounded IDs from both memory frames and disk spools.
func rpcRetainID(v rpcRaw) (rpcRaw, error) {
	if v == nil {
		return nil, nil
	}
	data, err := rpcRead(v, rpcMetadataLimit)
	if err != nil {
		return nil, v.fail(err)
	}
	return rpcInput(bytes.Clone(data)), nil
}

func (v *rpcJSON) store() *rpcSpool {
	if v == nil {
		return nil
	}
	if v.file != nil {
		return v.file.store
	}
	return v.spool
}

func (v *rpcJSON) fail(err error) error {
	if store := v.store(); store != nil {
		store.fail(err)
	}
	return err
}

func rpcRead(v rpcRaw, limit int) ([]byte, error) {
	if v == nil {
		return []byte("null"), nil
	}
	if rpcLen(v) > limit {
		return nil, fmt.Errorf("RPC control value exceeds %d bytes", limit)
	}
	if v.raw != nil {
		return v.raw, nil
	}
	var out bytes.Buffer
	if err := v.write(&out, make([]byte, min(rpcLen(v), rpcCopyBufferSize))); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// rpcBytes is for bounded metadata and tests. Wire output always uses writeRPC.
func rpcBytes(v rpcRaw) []byte {
	if v == nil {
		return nil
	}
	data, err := rpcRead(v, rpcMetadataLimit)
	if err != nil {
		v.fail(err)
		return nil
	}
	return data
}

func rpcText(v rpcRaw) string { return string(rpcBytes(v)) }

func (v *rpcJSON) MarshalJSON() ([]byte, error) {
	return rpcRead(v, rpcMetadataLimit)
}

func (v *rpcJSON) UnmarshalJSON(data []byte) error {
	*v = rpcJSON{raw: append([]byte(nil), data...)}
	return nil
}

func rpcUnmarshal(v rpcRaw, dest any) error {
	if v == nil {
		// An absent field is not a supplied JSON null. Preserve the old
		// RawMessage decoding error for callers that validate required fields.
		return json.Unmarshal(nil, dest)
	}
	if object, ok := dest.(*rpcMessage); ok {
		m, err := rpcObjectValue(v)
		*object = m
		return err
	}
	data, err := rpcRead(v, rpcMetadataLimit)
	if err != nil {
		return v.fail(err)
	}
	return json.Unmarshal(data, dest)
}

func rpcString(message rpcMessage, field string) string {
	var result string
	_ = rpcUnmarshal(message[field], &result)
	return result
}

func rpcObject(message rpcMessage, field string) rpcMessage {
	result, _ := rpcObjectValue(message[field])
	if result == nil {
		result = make(rpcMessage)
	}
	return result
}

func rpcObjectValue(v rpcRaw) (rpcMessage, error) {
	if v == nil {
		return nil, nil
	}
	if !v.indexed {
		v.indexed = true
		v.object, v.indexErr = indexRPCObject(v)
	}
	if v.indexErr != nil {
		return nil, v.indexErr
	}
	if v.object == nil {
		return nil, nil
	}
	result := make(rpcMessage, len(v.object))
	for key, item := range v.object {
		result[key] = item
	}
	return result, nil
}

func decodeRPC(data any) (rpcMessage, error) {
	message, err := rpcObjectValue(rpcInput(data))
	if err != nil {
		return nil, fmt.Errorf("invalid JSON-RPC message: %w", err)
	}
	if message == nil {
		return nil, errors.New("invalid JSON-RPC message: null object")
	}
	return message, nil
}

func writeRPC(writer io.Writer, message rpcMessage) error {
	// Full JSON marshaling has its own budget: a message can remain in RAM
	// up to the spill threshold while its body is still forwarded in pieces.
	if rpcLen(rpcValue(message)) <= rpcMetadataLimit {
		data, err := json.Marshal(message)
		if err != nil {
			return fmt.Errorf("encode JSON-RPC message: %w", err)
		}
		if err := rpcWrite(writer, append(data, '\n')); err != nil {
			return fmt.Errorf("write JSON-RPC message: %w", err)
		}
		return nil
	}
	if err := validateRPCValue(rpcValue(message)); err != nil {
		return fmt.Errorf("encode JSON-RPC message: %w", err)
	}
	if err := rpcValue(message).write(writer, make([]byte, rpcCopyBufferSize)); err != nil {
		return fmt.Errorf("write JSON-RPC message: %w", err)
	}
	return rpcWrite(writer, []byte{'\n'})
}

func validateRPCValue(v rpcRaw) error {
	if v == nil {
		return nil
	}
	if store := v.store(); store != nil {
		if err := store.error(); err != nil {
			return err
		}
	}
	if v.file != nil {
		return nil
	}
	if v.raw != nil {
		if !json.Valid(v.raw) {
			return rpcSyntax()
		}
		return nil
	}
	if v.encodeErr != nil {
		return v.encodeErr
	}
	for _, item := range v.object {
		if err := validateRPCValue(item); err != nil {
			return err
		}
	}
	for _, item := range v.array {
		if err := validateRPCValue(item); err != nil {
			return err
		}
	}
	return nil
}

func rpcWrite(w io.Writer, data []byte) error {
	n, err := w.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

func (v *rpcJSON) write(w io.Writer, buffer []byte) error {
	if v == nil {
		return rpcWrite(w, []byte("null"))
	}
	// Raw ranges remain authoritative even after their object index is cached.
	if v.file != nil {
		_, err := io.CopyBuffer(w, io.NewSectionReader(v.file.file, v.offset, v.length), buffer)
		return err
	}
	if v.raw != nil {
		return rpcWrite(w, v.raw)
	}
	if v.encodeErr != nil {
		return v.encodeErr
	}
	if v.object != nil {
		keys := make([]string, 0, len(v.object))
		for key := range v.object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if err := rpcWrite(w, []byte{'{'}); err != nil {
			return err
		}
		for i, key := range keys {
			if i > 0 {
				if err := rpcWrite(w, []byte{','}); err != nil {
					return err
				}
			}
			encoded, _ := json.Marshal(key)
			if err := rpcWrite(w, append(encoded, ':')); err != nil {
				return err
			}
			if err := v.object[key].write(w, buffer); err != nil {
				return err
			}
		}
		return rpcWrite(w, []byte{'}'})
	}
	if v.array != nil {
		if err := rpcWrite(w, []byte{'['}); err != nil {
			return err
		}
		for i, item := range v.array {
			if i > 0 {
				if err := rpcWrite(w, []byte{','}); err != nil {
					return err
				}
			}
			if err := item.write(w, buffer); err != nil {
				return err
			}
		}
		return rpcWrite(w, []byte{']'})
	}
	return rpcWrite(w, []byte("null"))
}

// The scanner validates JSON without allocating strings/arrays. Offsets let
// callers index selected objects without copying their large child values.
type rpcJSONScanner struct {
	r   *bufio.Reader
	pos int64
}

func newRPCJSONScanner(v rpcRaw) *rpcJSONScanner {
	return &rpcJSONScanner{r: bufio.NewReaderSize(v.reader(), min(rpcLen(v), rpcCopyBufferSize))}
}

func (s *rpcJSONScanner) take() (byte, error) {
	b, err := s.r.ReadByte()
	if err == nil {
		s.pos++
	}
	return b, err
}

func (s *rpcJSONScanner) peek() (byte, error) {
	b, err := s.r.Peek(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (s *rpcJSONScanner) space() error {
	for {
		b, err := s.peek()
		if err != nil {
			return err
		}
		if b != ' ' && b != '\t' && b != '\r' && b != '\n' {
			return nil
		}
		_, _ = s.take()
	}
}

func rpcSyntax() error { return errors.New("invalid JSON syntax") }

func (s *rpcJSONScanner) string() error {
	b, err := s.take()
	if err != nil {
		return err
	}
	if b != '"' {
		return rpcSyntax()
	}
	for {
		b, err = s.take()
		if err != nil {
			return err
		}
		if b == '"' {
			return nil
		}
		if b < 0x20 {
			return rpcSyntax()
		}
		if b != '\\' {
			continue
		}
		b, err = s.take()
		if err != nil {
			return err
		}
		switch b {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
		case 'u':
			for i := 0; i < 4; i++ {
				b, err = s.take()
				if err != nil {
					return err
				}
				if !(b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F') {
					return rpcSyntax()
				}
			}
		default:
			return rpcSyntax()
		}
	}
}

func (s *rpcJSONScanner) value(depth int) error {
	if err := s.space(); err != nil {
		return err
	}
	b, err := s.peek()
	if err != nil {
		return err
	}
	switch b {
	case '"':
		return s.string()
	case '{', '[':
		if depth >= rpcMaxDepth {
			return errors.New("JSON nesting depth exceeded")
		}
		_, _ = s.take()
		close := byte(']')
		if b == '{' {
			close = '}'
		}
		if err := s.space(); err != nil {
			return err
		}
		next, err := s.peek()
		if err != nil {
			return err
		}
		if next == close {
			_, _ = s.take()
			return nil
		}
		for {
			if b == '{' {
				if err := s.string(); err != nil {
					return err
				}
				if err := s.space(); err != nil {
					return err
				}
				c, err := s.take()
				if err != nil {
					return err
				}
				if c != ':' {
					return rpcSyntax()
				}
			}
			if err := s.value(depth + 1); err != nil {
				return err
			}
			if err := s.space(); err != nil {
				return err
			}
			c, err := s.take()
			if err != nil {
				return err
			}
			if c == close {
				return nil
			}
			if c != ',' {
				return rpcSyntax()
			}
			if err := s.space(); err != nil {
				return err
			}
		}
	case 't':
		return s.literal("true")
	case 'f':
		return s.literal("false")
	case 'n':
		return s.literal("null")
	default:
		return s.number()
	}
}

func (s *rpcJSONScanner) literal(want string) error {
	for i := range want {
		b, err := s.take()
		if err != nil {
			return err
		}
		if b != want[i] {
			return rpcSyntax()
		}
	}
	return nil
}

func (s *rpcJSONScanner) number() error {
	b, err := s.peek()
	if err != nil {
		return err
	}
	if b == '-' {
		_, _ = s.take()
		b, err = s.peek()
		if err != nil {
			return err
		}
	}
	if b == '0' {
		_, _ = s.take()
	} else {
		if b < '1' || b > '9' {
			return rpcSyntax()
		}
		s.digits()
	}
	b, _ = s.peek()
	if b == '.' {
		_, _ = s.take()
		if s.digits() == 0 {
			return rpcSyntax()
		}
	}
	b, _ = s.peek()
	if b == 'e' || b == 'E' {
		_, _ = s.take()
		b, _ = s.peek()
		if b == '+' || b == '-' {
			_, _ = s.take()
		}
		if s.digits() == 0 {
			return rpcSyntax()
		}
	}
	return nil
}

func (s *rpcJSONScanner) digits() int {
	n := 0
	for {
		b, err := s.peek()
		if err != nil || b < '0' || b > '9' {
			return n
		}
		_, _ = s.take()
		n++
	}
}

func indexRPCObject(v rpcRaw) (rpcMessage, error) {
	if v.raw != nil && !json.Valid(v.raw) {
		// Preserve encoding/json's detailed malformed-input diagnostics without
		// decoding valid subtrees into copying RawMessages.
		var invalid json.RawMessage
		return nil, json.Unmarshal(v.raw, &invalid)
	}
	s := newRPCJSONScanner(v)
	if err := s.space(); err != nil {
		return nil, err
	}
	b, err := s.take()
	if err != nil {
		return nil, err
	}
	if b != '{' {
		// Preserve encoding/json's acceptance of null for map destinations.
		if b == 'n' {
			if err := s.literal("ull"); err == nil && errors.Is(s.space(), io.EOF) {
				return nil, nil
			}
		}
		if v.raw != nil {
			var object map[string]json.RawMessage
			return nil, json.Unmarshal(v.raw, &object)
		}
		return nil, errors.New("JSON value is not an object")
	}
	object := make(rpcMessage)
	budget := 0
	if err := s.space(); err != nil {
		return nil, err
	}
	b, err = s.peek()
	if err != nil {
		return nil, err
	}
	if b == '}' {
		_, _ = s.take()
	} else {
		for {
			start := s.pos
			if err := s.string(); err != nil {
				return nil, err
			}
			keySize := s.pos - start
			budget += int(keySize) + 64
			if budget > rpcMetadataLimit {
				return nil, v.fail(errors.New("RPC object metadata exceeds memory budget"))
			}
			if err := v.reserveMetadata(int(keySize) + 128); err != nil {
				return nil, err
			}
			keyRaw, err := v.slice(start, keySize)
			if err != nil {
				return nil, err
			}
			var key string
			if err := rpcUnmarshal(keyRaw, &key); err != nil {
				return nil, err
			}
			if err := s.space(); err != nil {
				return nil, err
			}
			b, err := s.take()
			if err != nil {
				return nil, err
			}
			if b != ':' {
				return nil, rpcSyntax()
			}
			if err := s.space(); err != nil {
				return nil, err
			}
			start = s.pos
			if err := s.value(1); err != nil {
				return nil, err
			}
			item, err := v.slice(start, s.pos-start)
			if err != nil {
				return nil, err
			}
			object[key] = item
			if err := s.space(); err != nil {
				return nil, err
			}
			b, err = s.take()
			if err != nil {
				return nil, err
			}
			if b == '}' {
				break
			}
			if b != ',' {
				return nil, rpcSyntax()
			}
			if err := s.space(); err != nil {
				return nil, err
			}
		}
	}
	if err := s.space(); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, v.fail(err)
		}
		return nil, rpcSyntax()
	}
	return object, nil
}

// A spooled deferred event reserves its metadata budget. Raising the spill
// threshold must not reduce the number of deferred disk bodies we can retain.
func rpcMemoryCharge(v rpcRaw) int {
	if v != nil && v.file != nil {
		return rpcMetadataLimit
	}
	return rpcLen(v)
}

// Keep reflection out of the forwarding path. This helper is only used for
// visiting explicitly retained metadata maps during spool-file collection.
func rpcVisit(value any, visit func(*rpcSpoolFile)) {
	switch v := value.(type) {
	case rpcRaw:
		if v == nil {
			return
		}
		if v.file != nil {
			visit(v.file)
			return
		}
		if v.object != nil {
			rpcVisit(v.object, visit)
		}
		for _, item := range v.array {
			rpcVisit(item, visit)
		}
	case rpcMessage:
		for _, item := range v {
			rpcVisit(item, visit)
		}
	case map[string]any:
		for _, item := range v {
			rpcVisit(item, visit)
		}
	}
}

func rpcKind(v rpcRaw) byte {
	if v == nil {
		return 'n'
	}
	if v.file == nil && v.raw == nil {
		if v.object != nil {
			return '{'
		}
		if v.array != nil {
			return '['
		}
		return 'n'
	}
	r := bufio.NewReader(v.reader())
	for {
		b, err := r.ReadByte()
		if err != nil {
			return 0
		}
		if b != ' ' && b != '\n' && b != '\r' && b != '\t' {
			return b
		}
	}
}

func rpcNull(v rpcRaw) bool { return v == nil || rpcKind(v) == 'n' }

func rpcNonemptyArray(v rpcRaw) bool {
	if v == nil {
		return false
	}
	if v.array != nil {
		return len(v.array) > 0
	}
	s := newRPCJSONScanner(v)
	if s.space() != nil {
		return false
	}
	b, err := s.take()
	if err != nil || b != '[' || s.space() != nil {
		return false
	}
	b, err = s.peek()
	if err != nil || b == ']' {
		return false
	}
	validate := newRPCJSONScanner(v)
	return validate.value(0) == nil && errors.Is(validate.space(), io.EOF)
}
