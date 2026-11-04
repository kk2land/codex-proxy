package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"
)

func BenchmarkRPCSmallNotification(b *testing.B) {
	line := []byte(`{"method":"item/agentMessage/delta","params":{"threadId":"th_0123456789abcdef","turnId":"tn_1","itemId":"item_2","delta":"hello"}}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		message, err := decodeRPC(line)
		if err != nil {
			b.Fatal(err)
		}
		params := rpcObject(message, "params")
		if rpcString(message, "method") != "item/agentMessage/delta" || rpcString(params, "threadId") == "" || rpcString(params, "delta") != "hello" {
			b.Fatal("notification fields changed")
		}
	}
}

func checkRPCFileJSON(t *testing.T, data []byte) {
	t.Helper()
	spool := testRPCSpool(t)
	file, err := spool.create()
	if err != nil {
		t.Fatal(err)
	}
	if err := rpcWrite(file.file, data); err != nil {
		t.Fatal(err)
	}
	var want map[string]json.RawMessage
	wantErr := json.Unmarshal(data, &want)
	memory := rpcInput(data)
	memory.spool = newRPCSpool("")
	for _, raw := range []rpcRaw{{file: file, length: int64(len(data))}, memory} {
		got, gotErr := rpcObjectValue(raw)
		if raw.store().error() != nil {
			continue // Explicit bounded control-metadata rejection.
		}
		if (wantErr == nil) != (gotErr == nil) {
			t.Fatalf("JSON acceptance differs: input=%q native=%v indexed=%v file=%t", data, wantErr, gotErr, raw.file != nil)
		}
		if wantErr != nil {
			continue
		}
		wantBytes, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		gotBytes, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(gotBytes, wantBytes) {
			t.Fatalf("JSON changed: got=%s want=%s", gotBytes, wantBytes)
		}
	}
}

func TestRPCFileJSONMatchesStandardDecoder(t *testing.T) {
	for _, data := range []string{
		``, `{}`, `null`, `[]`, `true`, `42`, `"text"`,
		` {"id":1,"method":"x","params":{"escaped":"\\\"}{[]\u0061","list":[null,true,false,{},[],0,-0,1e+20,-2.5e-7]}} `,
		`{"method":"ignored","params":{},"method":"last","id":"\ud800\udc00"}`,
		`{"\u0069d":3,"utf8":"中文","surrogate":"\ud800\ud800\udc00\u0000"}`,
		`{"x":}`, `{"x":01}`, `{"x":1.}`, `{"x":1e}`, `{"x":1e+}`, `{"x":-}`, `{"x":.1}`,
		`{"x":truefalse}`, `{"x":nul}`, `{"x":null} false`, `{"x":[1,]}`, `{"x":{},}`, `{"x" 1}`,
		`{"x":"\x00"}`, "{\"x\":\"raw\nline\"}", `{"x":"\u123z"}`, `{"x":"unterminated}`, `{"x":[[}`,
	} {
		t.Run(data, func(t *testing.T) { checkRPCFileJSON(t, []byte(data)) })
	}
}

func FuzzRPCFileJSON(f *testing.F) {
	for _, data := range []string{`{}`, `{"id":1,"params":{"data":["a",null,{"x":-0.2e+3}]}}`, `{"a":"\u0041\\\""}`, `{"bad":01}`} {
		f.Add([]byte(data))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 64<<10 {
			t.Skip()
		}
		checkRPCFileJSON(t, data)
	})
}

func TestRPCStreamingStringMatchesStandardDecoder(t *testing.T) {
	for _, input := range []string{`"plain"`, `"中文😀"`, `"\u0073tatus\u0020code:\u0020429"`, `"\\\/\"\b\f\n\r\t"`, `"\ud800\udc00"`, `"\ud800\ud800\udc00"`, `"\ud800\u0000x"`, `"\udc00\ud800a"`} {
		var want string
		if err := json.Unmarshal([]byte(input), &want); err != nil {
			t.Fatal(err)
		}
		r := newRPCStringReader(rpcInput([]byte(input)))
		var got strings.Builder
		for {
			v, _, err := r.ReadRune()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			got.WriteRune(v)
		}
		if got.String() != want {
			t.Fatalf("input=%s got=%q want=%q", input, got.String(), want)
		}
	}
}

func TestRPCFileJSONDepthBoundary(t *testing.T) {
	for _, depth := range []int{rpcMaxDepth - 1, rpcMaxDepth} {
		data := `{"x":` + strings.Repeat("[", depth) + strings.Repeat("]", depth) + `}`
		checkRPCFileJSON(t, []byte(data))
	}
}

func TestRPCMissingValueRemainsDistinctFromNull(t *testing.T) {
	var number int
	if err := rpcUnmarshal(nil, &number); err == nil {
		t.Fatal("missing value decoded as an explicit null")
	}
	if err := rpcUnmarshal(rpcValue(nil), &number); err != nil {
		t.Fatalf("JSON null rejected: %v", err)
	}
	fields := make(map[string]any)
	logNativeError(fields, rpcValue(map[string]any{"message": "failed"}))
	if _, ok := fields["error_code"]; ok {
		t.Fatal("absent error code logged as zero")
	}
}

func TestRPCOversizedControlCannotBypassAdmission(t *testing.T) {
	s, upstream := newNotificationSession(t)
	s.spool = testRPCSpool(t)
	s.stdin = rpcCheckedWriter{upstream, s.spool}
	frame := testRPCFrame(t, s.spool, rpcLargeReader(`{"id":7,"method":"turn/start","params":{"threadId":"`, rpcMetadataLimit+1, `","input":[{"type":"text","text":"x"}]}}`+"\n"))
	err := s.handle(appServerEvent{source: "client", frame: frame})
	if err == nil || upstream.Len() != 0 {
		t.Fatalf("oversized control field bypassed admission: err=%v bytes=%d", err, upstream.Len())
	}
}

func TestRPCNestedMemoryErrorDoesNotCopyBodies(t *testing.T) {
	const depth = 24
	spool := testRPCSpool(t)
	frame := testRPCFrame(t, spool, rpcLargeReader(`{"code":429,`+strings.Repeat(`"data":{`, depth)+`"blob":"`, 2<<20, `","retry_after":120`+strings.Repeat(`}`, depth)+`}`+"\n"))
	if frame.file != nil {
		t.Fatal("expected in-memory frame")
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	limit := authErrorLimit(frame, time.Unix(1000, 0))
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(frame)
	if limit.Until != 1120 || spool.error() != nil {
		t.Fatalf("nested cooldown was not inspected: limit=%+v err=%v", limit, spool.error())
	}
	retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("2 MiB error at depth 24: %.2f MiB retained, %.2f MiB total allocations", float64(retained)/(1<<20), float64(allocated)/(1<<20))
	if retained > 2<<20 || allocated > 8<<20 {
		t.Fatalf("nested parsing copied bodies: retained=%d allocated=%d", retained, allocated)
	}
}

func TestRPCMetadataBudgetSharedAcrossNestedObjects(t *testing.T) {
	parts := make(map[string]any)
	for i := 0; i < 8; i++ {
		fields := make(map[string]int)
		for j := 0; j < 400; j++ {
			fields[fmt.Sprintf("field%d", j)] = j
		}
		parts[fmt.Sprintf("part%d", i)] = fields
	}
	data, err := json.Marshal(parts)
	if err != nil {
		t.Fatal(err)
	}
	for _, onDisk := range []bool{false, true} {
		t.Run(fmt.Sprintf("disk=%t", onDisk), func(t *testing.T) {
			spool := testRPCSpool(t)
			raw := &rpcJSON{raw: data, spool: spool}
			if onDisk {
				file, err := spool.create()
				if err != nil {
					t.Fatal(err)
				}
				if err := rpcWrite(file.file, data); err != nil {
					t.Fatal(err)
				}
				raw = &rpcJSON{file: file, length: int64(len(data))}
			}
			walkRPCObjects(raw, func(rpcMessage) {})
			if err := spool.error(); err == nil || !strings.Contains(err.Error(), "metadata") {
				t.Fatalf("nested indexes escaped the frame budget: %v", err)
			}
		})
	}
}

func TestRPCSmallQueuedInputDoesNotRetainWholeMemoryFrame(t *testing.T) {
	s, _ := newNotificationSession(t)
	s.spool = testRPCSpool(t)
	s.work("t").running["t:busy"] = true
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	frame := testRPCFrame(t, s.spool, rpcLargeReader(`{"id":1,"method":"thread/queue/add","params":{"threadId":"t","clientUserMessageId":"msg","input":[{"type":"text","text":"tiny"}],"unused":"`, 8<<20, `"}}`+"\n"))
	if err := s.handle(appServerEvent{source: "client", frame: frame}); err != nil {
		t.Fatal(err)
	}
	frame = nil
	runtime.GC()
	runtime.ReadMemStats(&after)
	if len(s.queue("t").items) != 1 || rpcText(s.queue("t").items[0].Input) != `[{"type":"text","text":"tiny"}]` {
		t.Fatal("queued input changed")
	}
	runtime.KeepAlive(s)
	if retained := int64(after.HeapAlloc) - int64(before.HeapAlloc); retained > 2<<20 {
		t.Fatalf("tiny queued input retained %d bytes of enclosing frame", retained)
	}
}

func TestRPCLongIDDoesNotRetainForwardedMemoryBody(t *testing.T) {
	s, _ := newNotificationSession(t)
	s.spool = testRPCSpool(t)
	s.stdin = rpcCheckedWriter{io.Discard, s.spool}
	id := strings.Repeat("i", 8192)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	frame := testRPCFrame(t, s.spool, rpcLargeReader(`{"id":"`+id+`","method":"model/list","params":{"unused":"`, 8<<20, `"}}`+"\n"))
	if err := s.handle(appServerEvent{source: "client", frame: frame}); err != nil {
		t.Fatal(err)
	}
	frame = nil
	runtime.GC()
	runtime.ReadMemStats(&after)
	if len(s.requests) != 1 {
		t.Fatal("forwarded request was not tracked")
	}
	for _, request := range s.requests {
		if rpcText(request.id) != `"`+id+`"` {
			t.Fatal("retained request ID changed")
		}
	}
	runtime.KeepAlive(s)
	if retained := int64(after.HeapAlloc) - int64(before.HeapAlloc); retained > 2<<20 {
		t.Fatalf("pending ID retained %d bytes of forwarded body", retained)
	}
}
