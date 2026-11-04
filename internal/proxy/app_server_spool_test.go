package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type rpcRepeatReader struct{ value byte }

func (r rpcRepeatReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.value
	}
	return len(p), nil
}

func rpcLargeReader(prefix string, size int, suffix string) io.Reader {
	return io.MultiReader(strings.NewReader(prefix), io.LimitReader(rpcRepeatReader{'x'}, int64(size)), strings.NewReader(suffix))
}

func testRPCFrame(t *testing.T, spool *rpcSpool, reader io.Reader) rpcRaw {
	t.Helper()
	frame, err := newRPCFrameReader(reader, spool).next()
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func testRPCSpool(t *testing.T) *rpcSpool {
	t.Helper()
	s := newRPCSpool(t.TempDir())
	t.Cleanup(s.close)
	return s
}

func TestRPCFrameSpillsAndPreservesFraming(t *testing.T) {
	for _, size := range []int{rpcMemoryThreshold - 1, rpcMemoryThreshold, rpcMemoryThreshold + 1, (32 << 20) + 17} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			spool := testRPCSpool(t)
			reader := newRPCFrameReader(rpcLargeReader("", size, "\r\n\nlast"), spool)
			frame, err := reader.next()
			if err != nil || rpcLen(frame) != size {
				t.Fatalf("length=%d error=%v", rpcLen(frame), err)
			}
			// CR contributes to the buffering threshold even though it is stripped.
			if size >= rpcMemoryThreshold && frame.file == nil {
				t.Fatal("large frame retained in memory")
			}
			if frame.file != nil {
				info, err := frame.file.file.Stat()
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatalf("spool permissions: %v %v", info, err)
				}
				entries, err := os.ReadDir(spool.dir)
				if err != nil || len(entries) != 0 {
					t.Fatalf("spool name retained: %v %v", entries, err)
				}
			}
			hash := sha256.New()
			if _, err := io.Copy(hash, frame.reader()); err != nil {
				t.Fatal(err)
			}
			want := sha256.New()
			_, _ = io.Copy(want, io.LimitReader(rpcRepeatReader{'x'}, int64(size)))
			if !bytes.Equal(hash.Sum(nil), want.Sum(nil)) {
				t.Fatal("frame bytes changed")
			}
			spool.release(frame)
			empty, err := reader.next()
			if err != nil || empty == nil || rpcLen(empty) != 0 {
				t.Fatalf("blank line lost: %v", err)
			}
			last, err := reader.next()
			if err != nil || rpcText(last) != "last" {
				t.Fatalf("EOF tail lost: %v", err)
			}
			if _, err := reader.next(); !errors.Is(err, io.EOF) {
				t.Fatalf("EOF=%v", err)
			}
		})
	}
}

func TestRPCFrame32MiBThreshold(t *testing.T) {
	for _, size := range []int{(1 << 20) + 1, 32 << 20, (32 << 20) + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			spool := testRPCSpool(t)
			frame := testRPCFrame(t, spool, rpcLargeReader("", size, "\n"))
			if (frame.file != nil) != (size > 32<<20) || rpcLen(frame) != size {
				t.Fatalf("wrong spill boundary: size=%d spooled=%t length=%d", size, frame.file != nil, rpcLen(frame))
			}
		})
	}
}

func TestRPCMemoryResponseAboveMetadataLimit(t *testing.T) {
	for _, size := range []int{(2 << 20) + 1, (32 << 20) - 256} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			s, upstream := newNotificationSession(t)
			s.spool = testRPCSpool(t)
			hash := sha256.New()
			s.stdout = rpcCheckedWriter{hash, s.spool}
			if err := s.client(rpcValue(map[string]any{"id": "client", "method": "thread/read", "params": map[string]any{"threadId": "t"}})); err != nil {
				t.Fatal(err)
			}
			request, err := decodeRPC(upstream.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			frame := testRPCFrame(t, s.spool, rpcLargeReader(`{"result":{"blob":"`, size, `"},"id":`+rpcText(request["id"])+`}`+"\n"))
			if frame.file != nil {
				t.Fatal("response below 32 MiB was spooled")
			}
			if err := s.handle(appServerEvent{source: "server", frame: frame}); err != nil {
				t.Fatal(err)
			}
			want := sha256.New()
			_, _ = io.Copy(want, rpcLargeReader(`{"id":"client","result":{"blob":"`, size, `"}}`+"\n"))
			if !bytes.Equal(hash.Sum(nil), want.Sum(nil)) {
				t.Fatal("in-memory response or restored ID changed")
			}
		})
	}
}

func TestRPCSpoolLargeResponseBoundedAllocations(t *testing.T) {
	s, upstream := newNotificationSession(t)
	s.spool = testRPCSpool(t)
	hash := sha256.New()
	s.stdout = rpcCheckedWriter{hash, s.spool}
	if err := s.client(rpcValue(map[string]any{"id": "client", "method": "thread/read", "params": map[string]any{"threadId": "t"}})); err != nil {
		t.Fatal(err)
	}
	request, err := decodeRPC(upstream.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	const size = (40 << 20) + 117
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	// ID after the large result exercises JSON field order independence.
	frame := testRPCFrame(t, s.spool, rpcLargeReader(`{"result":{"blob":"`, size, `","tail":"kept"},"id":`+rpcText(request["id"])+`,"zz":true}`+"\n"))
	file := frame.file
	if err := s.handle(appServerEvent{source: "server", frame: frame}); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	// Geometric growth of the in-memory prefix allocates less than twice the
	// threshold. Forwarding must not allocate another complete response.
	if allocated > uint64(2*rpcMemoryThreshold+(16<<20)) {
		t.Fatalf("forwarding allocated %d bytes for a %d-byte body", allocated, size)
	}
	t.Logf("40 MiB response: %.2f MiB total Go allocations", float64(allocated)/(1<<20))
	want := sha256.New()
	_, _ = io.Copy(want, rpcLargeReader(`{"id":"client","result":{"blob":"`, size, `","tail":"kept"},"zz":true}`+"\n"))
	if !bytes.Equal(hash.Sum(nil), want.Sum(nil)) {
		t.Fatal("large response or restored ID changed")
	}
	if _, err := file.file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("completed spool still open: %v", err)
	}
}

func TestRPCSpoolResumeWaitsForPolicyAndReleases(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(fmt.Sprintf("reject=%t", reject), func(t *testing.T) {
			s, upstream := newNotificationSession(t)
			s.spool = testRPCSpool(t)
			out, err := os.CreateTemp(t.TempDir(), "response")
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			s.stdout = rpcCheckedWriter{out, s.spool}
			if err := s.client(rpcValue(map[string]any{"id": "resume", "method": "thread/resume", "params": map[string]any{"threadId": "thread"}})); err != nil {
				t.Fatal(err)
			}
			request, err := decodeRPC(upstream.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			upstream.Reset()
			const size = (32 << 20) + 19
			frame := testRPCFrame(t, s.spool, rpcLargeReader(`{"result":{"thread":{"turns":["`, size, `"],"id":"thread"},"sandbox":"read-only","approvalPolicy":"on-request"},"id":`+rpcText(request["id"])+"}\n"))
			file := frame.file
			if err := s.handle(appServerEvent{source: "server", frame: frame}); err != nil {
				t.Fatal(err)
			}
			if info, _ := out.Stat(); info.Size() != 0 {
				t.Fatal("resume response forwarded before policy confirmation")
			}
			if _, err := file.file.Stat(); err != nil {
				t.Fatalf("pending response released: %v", err)
			}
			policy, err := decodeRPC(upstream.Bytes())
			if err != nil || rpcString(policy, "method") != "thread/settings/update" {
				t.Fatalf("missing policy request: %v", err)
			}
			upstream.Reset()
			reply := rpcMessage{"id": policy["id"], "result": rpcValue(map[string]any{})}
			if reject {
				delete(reply, "result")
				reply["error"] = rpcValue(map[string]any{"code": -32000, "message": "policy rejected"})
			}
			if err := s.handle(appServerEvent{source: "server", frame: rpcValue(reply)}); err != nil {
				t.Fatal(err)
			}
			if !reject {
				if info, _ := out.Stat(); info.Size() != 0 {
					t.Fatal("resume forwarded before goal initialization")
				}
				goal, err := decodeRPC(upstream.Bytes())
				if err != nil || rpcString(goal, "method") != "thread/goal/get" {
					t.Fatalf("missing goal request: %v", err)
				}
				if err := s.handle(appServerEvent{source: "server", frame: rpcValue(rpcMessage{"id": goal["id"], "result": rpcValue(map[string]any{"goal": nil})})}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := file.file.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("deferred response leaked file: %v", err)
			}
			if _, err := out.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			checkStore := testRPCSpool(t)
			response, err := decodeRPC(testRPCFrame(t, checkStore, out))
			if err != nil || rpcString(response, "id") != "resume" {
				t.Fatalf("bad resume reply: %v", err)
			}
			if reject {
				if response["error"] == nil {
					t.Fatal("policy failure became success")
				}
				return
			}
			result := rpcObject(response, "result")
			if rpcString(result, "approvalPolicy") != "never" || rpcString(rpcObject(result, "sandbox"), "type") != "externalSandbox" {
				t.Fatal("policy overlay lost")
			}
			history := rpcObject(result, "thread")["turns"]
			if history.file == nil || rpcLen(history) != size+4 {
				t.Fatalf("history was lost or expanded: %d", rpcLen(history))
			}
			hash, want := sha256.New(), sha256.New()
			_, _ = io.Copy(hash, history.reader())
			_, _ = io.Copy(want, rpcLargeReader(`["`, size, `"]`))
			if !bytes.Equal(hash.Sum(nil), want.Sum(nil)) {
				t.Fatal("history was modified")
			}
		})
	}
}

func TestRPCSpoolPendingClientRequestAndCancellation(t *testing.T) {
	s, _ := newNotificationSession(t)
	s.spool = testRPCSpool(t)
	frame := testRPCFrame(t, s.spool, rpcLargeReader(`{"id":"queued","method":"turn/start","params":{"input":[{"type":"text","text":"`, rpcMemoryThreshold+19, `"}],"threadId":"t"}}`+"\n"))
	s.spool.adopt(frame)
	message, err := decodeRPC(frame)
	if err != nil {
		t.Fatal(err)
	}
	work := s.work("t")
	ctx, cancel := context.WithCancel(s.ctx)
	work.pending = &appAdmission{work: work, thread: "t", method: "turn/start", message: message, ctx: ctx, cancel: cancel}
	s.collectSpools()
	if _, err := frame.file.file.Stat(); err != nil {
		t.Fatal("waiting task lost its input")
	}
	if err := s.cancelAdmission(work); err != nil {
		t.Fatal(err)
	}
	s.collectSpools()
	if _, err := frame.file.file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("canceled input retained file")
	}
}

func TestRPCSpoolLargeClientRequestPreservesPolicyAndID(t *testing.T) {
	s, _ := newNotificationSession(t)
	s.spool = testRPCSpool(t)
	hash := sha256.New()
	s.stdin = rpcCheckedWriter{hash, s.spool}
	const size = (32 << 20) + 29
	frame := testRPCFrame(t, s.spool, rpcLargeReader(`{"params":{"blob":"`, size, `","threadId":"t","permissions":{"allow":"unsafe"},"permissionProfile":"ignored"},"method":"command/exec","id":"client","unknown":42}`+"\n"))
	if err := s.handle(appServerEvent{source: "client", frame: frame}); err != nil {
		t.Fatal(err)
	}
	if len(s.requests) != 1 {
		t.Fatalf("missing request mapping: %d", len(s.requests))
	}
	for id, request := range s.requests {
		if rpcText(request.id) != `"client"` {
			t.Fatal("original client ID lost")
		}
		want := sha256.New()
		_, _ = io.Copy(want, rpcLargeReader(`{"id":`+id+`,"method":"command/exec","params":{"blob":"`, size, `","sandboxPolicy":{"networkAccess":"enabled","type":"externalSandbox"},"threadId":"t"},"unknown":42}`+"\n"))
		if !bytes.Equal(hash.Sum(nil), want.Sum(nil)) {
			t.Fatal("large request, policy overlay, or ID changed")
		}
	}
	if _, err := frame.file.file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("forwarded client payload retained file: %v", err)
	}
}

func TestRPCSpoolDeferredReviewEventReplaysAndReleases(t *testing.T) {
	s, _ := newNotificationSession(t)
	s.spool = testRPCSpool(t)
	hash := sha256.New()
	s.stdout = rpcCheckedWriter{hash, s.spool}
	s.threadStarted("child")
	s.reviewSources["child"] = "parent"
	const size = (32 << 20) + 31
	prefix := `{"method":"turn/completed","params":{"threadId":"child","turn":{"id":"done","status":"completed","blob":"`
	suffix := `"}}}` + "\n"
	frame := testRPCFrame(t, s.spool, rpcLargeReader(prefix, size, suffix))
	if err := s.handle(appServerEvent{source: "server", frame: frame}); err != nil {
		t.Fatal(err)
	}
	if len(s.reviewEvents["child"]) != 1 || s.reviewEventSize != rpcMetadataLimit {
		t.Fatal("large lifecycle event was not deferred within its memory budget")
	}
	if _, err := frame.file.file.Stat(); err != nil {
		t.Fatal("deferred lifecycle event lost its body")
	}
	delete(s.reviewSources, "child")
	if err := s.replayReviewEvents("child"); err != nil {
		t.Fatal(err)
	}
	s.collectSpools()
	want := sha256.New()
	_, _ = io.Copy(want, rpcLargeReader(prefix, size, suffix))
	if !bytes.Equal(hash.Sum(nil), want.Sum(nil)) || s.reviewEventSize != 0 {
		t.Fatal("deferred lifecycle event was not replayed intact")
	}
	if _, err := frame.file.file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("replayed event retained file: %v", err)
	}
}

type rpcTestReaderFunc func([]byte) (int, error)

func (f rpcTestReaderFunc) Read(p []byte) (int, error) { return f(p) }

func TestRPCSpoolDiskFailureAndDrainDeadline(t *testing.T) {
	t.Run("write failure", func(t *testing.T) {
		spool := testRPCSpool(t)
		reader := io.MultiReader(io.LimitReader(rpcRepeatReader{'x'}, 2*rpcMemoryThreshold), rpcTestReaderFunc(func(p []byte) (int, error) {
			for file := range spool.files {
				_ = file.file.Close()
			}
			p[0] = '\n'
			return 1, io.EOF
		}))
		if _, err := newRPCFrameReader(reader, spool).next(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("disk write failure not reported: %v", err)
		}
		if len(spool.files) != 0 {
			t.Fatal("failed spool retained")
		}
	})
	t.Run("bounded shutdown scan", func(t *testing.T) {
		for _, size := range []int{rpcMetadataLimit + 1, rpcMemoryThreshold + 1} {
			spool := testRPCSpool(t)
			frame := testRPCFrame(t, spool, rpcLargeReader(`{"code":429,"message":"`, size, ` MCP tool execution"}`+"\n"))
			spool.setReadDeadline(time.Now().Add(-time.Second))
			if authRateLimited(frame) {
				t.Fatal("interrupted external-service scan marked account rate limited")
			}
			if !errors.Is(spool.error(), os.ErrDeadlineExceeded) {
				t.Fatalf("drain deadline ignored: %v", spool.error())
			}
		}
	})
}

func TestRPCSpoolLogSnapshotOutlivesCanceledInput(t *testing.T) {
	spool := testRPCSpool(t)
	id := strings.Repeat("i", 8192)
	frame := testRPCFrame(t, spool, rpcLargeReader(`{"id":"`+id+`","params":{"blob":"`, rpcMemoryThreshold+1, `"}}`+"\n"))
	message, err := decodeRPC(frame)
	if err != nil {
		t.Fatal(err)
	}
	fields := make(map[string]any)
	logRPCID(fields, message["id"])
	spool.release(frame)
	data, err := json.Marshal(fields)
	if err != nil || string(data) != `{"rpc_id":"`+id+`"}` {
		t.Fatalf("worker log snapshot depends on canceled input: %v", err)
	}
}

func TestRPCSpoolLargeErrorRecognizesQuotaAndExternalFailures(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(fmt.Sprint(external), func(t *testing.T) {
			spool := testRPCSpool(t)
			tail := ` status\u0020code:\u0020429","resetsAt":5000}`
			if external {
				tail = ` status code: 429 \u004d\u0043\u0050","resetsAt":5000}`
			}
			frame := testRPCFrame(t, spool, rpcLargeReader(`{"message":"`, (32<<20)+7, tail+"\n"))
			if authRateLimited(frame) == external {
				t.Fatal("large error attribution changed")
			}
			if got := authErrorLimit(frame, time.Unix(1000, 0)).Until; got != 5000 {
				t.Fatalf("reset=%d", got)
			}
			fields := make(map[string]any)
			logNativeError(fields, frame)
			encoded, err := json.Marshal(fields)
			if err != nil || len(encoded) > 32<<10 {
				t.Fatalf("unbounded error log: bytes=%d err=%v", len(encoded), err)
			}
			if err := spool.error(); err != nil {
				t.Fatalf("diagnostic expanded body: %v", err)
			}
		})
	}
}

func TestRPCQuotaEscapedTimestamp(t *testing.T) {
	stamp := "2026-11-03T12:34:56.123456789+08:00"
	var encoded strings.Builder
	for _, c := range stamp {
		fmt.Fprintf(&encoded, `\u%04x`, c)
	}
	raw := rpcInput([]byte(`{"resetsAt":"` + encoded.String() + `"}`))
	want, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		t.Fatal(err)
	}
	if got := authErrorLimit(raw, time.Unix(1000, 0)).Until; got != want.Unix() {
		t.Fatalf("escaped reset timestamp changed: got=%d want=%d", got, want.Unix())
	}
}

func TestRPCSpoolErrorsAndUndeliveredFrameCleanup(t *testing.T) {
	spool := newRPCSpool(t.TempDir() + "/missing")
	if _, err := newRPCFrameReader(rpcLargeReader("", rpcMemoryThreshold+1, "\n"), spool).next(); err == nil {
		t.Fatal("spool creation failure ignored")
	}
	spool = testRPCSpool(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	readAppFrames(ctx, rpcLargeReader(`{"text":"`, rpcMemoryThreshold+1, "\"}\n"), spool, make(chan appServerEvent), "server")
	if len(spool.files) != 0 {
		t.Fatal("undelivered frame leaked")
	}
}

func TestAppServerForwardsResponseLargerThan32MiB(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	client := startApp(t, home)
	client.timeout = 30 * time.Second
	const size = (32 << 20) + 193
	response := client.call("test/largeReply", map[string]any{"bytes": size})
	blob := response["blob"]
	if blob == nil || blob.file == nil || rpcLen(blob) != size+2 || rpcString(response, "tail") != "kept" {
		t.Fatal("large child response was truncated or expanded")
	}
	hash, want := sha256.New(), sha256.New()
	_, _ = io.Copy(hash, blob.reader())
	_, _ = io.Copy(want, rpcLargeReader(`"`, size, `"`))
	if !bytes.Equal(hash.Sum(nil), want.Sum(nil)) {
		t.Fatal("child payload was changed")
	}
	client.call("model/list", map[string]any{})
	if code := client.stop(); code != 0 {
		t.Fatalf("app-server exited %d after large response", code)
	}
}

func TestAppServerForwardsRequestLargerThan32MiB(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	client := startApp(t, home)
	client.timeout = 30 * time.Second
	thread := client.thread()
	prompt := strings.Repeat("x", (32<<20)+193)
	client.call("turn/start", taskParams(thread, prompt))
	client.call("model/list", map[string]any{})
	client.finish(thread)
	if code := client.stop(); code != 0 {
		t.Fatalf("app-server exited %d after large request", code)
	}

	capture, err := os.Open(filepath.Join(client.dir, "rpc.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Close()
	decoder := json.NewDecoder(capture)
	for {
		var message struct {
			Method string `json:"method"`
			Params struct {
				Input []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"input"`
				SandboxPolicy struct {
					Type string `json:"type"`
				} `json:"sandboxPolicy"`
			} `json:"params"`
		}
		if err := decoder.Decode(&message); err != nil {
			t.Fatalf("read captured turn/start: %v", err)
		}
		if message.Method != "turn/start" {
			continue
		}
		if len(message.Params.Input) != 1 || message.Params.Input[0].Type != "text" || message.Params.Input[0].Text != prompt {
			t.Fatal("large client input was changed or truncated")
		}
		if message.Params.SandboxPolicy.Type != "externalSandbox" {
			t.Fatal("large request lost its policy overlay")
		}
		break
	}
}
