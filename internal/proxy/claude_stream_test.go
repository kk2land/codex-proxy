package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func awaitClaudeOutput(t *testing.T, path string, match func(rpcMessage) bool) rpcMessage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(path)
		lines := bytes.Split(data, []byte{'\n'})
		for _, line := range lines[:len(lines)-1] {
			message, err := decodeRPC(line)
			if err == nil && match(message) {
				return message
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("expected Claude output was not delivered")
	return nil
}

func awaitClaudeResponse(t *testing.T, path, id string) rpcMessage {
	t.Helper()
	message := awaitClaudeOutput(t, path, func(message rpcMessage) bool {
		return rpcString(message, "type") == "control_response" && rpcString(rpcObject(message, "response"), "request_id") == id
	})
	return rpcObject(message, "response")
}

func TestClaudeTargetedCancellationBeforeAdmission(t *testing.T) {
	for _, kind := range []string{"user", "control"} {
		for _, position := range []string{"current", "queued"} {
			t.Run(kind+"/"+position, func(t *testing.T) {
				home, dir := t.TempDir(), t.TempDir()
				configureClaude(t, home, 1, "5s")
				holder := startClaude(t, home, "hold", dir, "", nil, "-p", "hold")
				pid := awaitStarts(t, dir, 1)[0]
				stream, input, output := startClaudePipe(t, home, "stream", dir, nil)
				target := `{"type":"user","uuid":"target","message":{"role":"user","content":"cancel me"}}`
				cancel := `{"type":"control_request","request_id":"cancel","request":{"subtype":"cancel_async_message","message_uuid":"target"}}`
				responseID, responseType := "cancel", "success"
				if kind == "control" {
					target = `{"type":"control_request","request_id":"target","request":{"subtype":"generate_session_title","description":"cancel me"}}`
					cancel = `{"type":"control_cancel_request","request_id":"target"}`
					responseID, responseType = "target", "error"
				}
				survivor := `{"type":"user","uuid":"survivor","message":{"role":"user","content":"keep me"}}`
				if position == "queued" {
					fmt.Fprintln(input, survivor)
				}
				fmt.Fprintln(input, target)
				if position == "current" {
					fmt.Fprintln(input, survivor)
				}
				fmt.Fprintln(input, cancel)
				fmt.Fprintln(input, `{"type":"control_request","request_id":"check","request":{"subtype":"get_settings"}}`)
				response := awaitClaudeResponse(t, output, responseID)
				if rpcString(response, "subtype") != responseType {
					t.Fatalf("wrong cancellation response: %s", rpcValue(response))
				}
				if kind == "user" && string(rpcObject(response, "response")["cancelled"]) != "true" {
					t.Fatalf("missing SDK cancelled flag: %s", rpcValue(response))
				}
				awaitClaudeResponse(t, output, "check") // No slot is available yet.
				input.Close()
				releaseNative(t, dir, pid)
				if code := childExit(t, holder, 5*time.Second); code != 0 {
					t.Fatalf("holder=%d", code)
				}
				if code := childExit(t, stream, 5*time.Second); code != 0 {
					t.Fatalf("stream=%d stderr=%s", code, stream.stderr.String())
				}
				paths := awaitClaudeFiles(t, dir, "*-user-*.json", 1)
				data, _ := os.ReadFile(paths[0])
				if !bytes.Contains(data, []byte(`"uuid":"survivor"`)) {
					t.Fatal("targeted cancellation submitted the target or removed the other input")
				}
				for _, pattern := range []string{"*-generate_session_title.json", "*-cancel_async_message.json", "*-control_cancel_request.json"} {
					paths, _ := filepath.Glob(filepath.Join(dir, pattern))
					if len(paths) != 0 {
						t.Fatalf("locally canceled request reached native Claude: %s", pattern)
					}
				}
				canceled := 0
				for _, record := range claudeLogRecords(t, home) {
					if record["event"] == "canceled" {
						canceled++
					}
					if record["event"] == "error" {
						t.Fatalf("local cancellation was logged again as an error: %v", record)
					}
				}
				if canceled != 1 {
					t.Fatalf("got %d cancellation logs, want one", canceled)
				}
			})
		}
	}
}

func TestClaudeTargetedCancellationAfterForwarding(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	configureClaude(t, home, 1, "5s")
	stream, input, output := startClaudePipe(t, home, "stream", dir, nil)
	fmt.Fprintln(input, `{"type":"user","uuid":"sent","message":{"role":"user","content":"already submitted"}}`)
	awaitClaudeFiles(t, dir, "*-user-1.json", 1)
	fmt.Fprintln(input, `{"type":"control_request","request_id":"cancel","request":{"subtype":"cancel_async_message","message_uuid":"sent"}}`)
	response := awaitClaudeResponse(t, output, "cancel")
	if string(rpcObject(response, "response")["cancelled"]) != "false" {
		t.Fatal("proxy claimed to cancel an input already owned by the CLI")
	}
	awaitClaudeFiles(t, dir, "*-cancel_async_message.json", 1)
	input.Close()
	if code := childExit(t, stream, 5*time.Second); code != 0 {
		t.Fatalf("stream=%d stderr=%s", code, stream.stderr.String())
	}
}

func awaitClaudeTaskStamp(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(filepath.Join(dir, stateFilename))
		if bytes.Contains(data, []byte(lastClaudeTaskKey)) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("model input did not reach the write stage")
}

func TestClaudeBlockedInputWriteStopsAndReleasesLocks(t *testing.T) {
	for _, cancellation := range []string{"interrupt", "message", "control", "timeout"} {
		t.Run(cancellation, func(t *testing.T) {
			home, dir := t.TempDir(), t.TempDir()
			proxyDir := configureClaude(t, home, 1, "5s")
			extra := []string{"CLAUDE_PROXY_TEST_IO_TIMEOUT=5s"}
			if cancellation == "timeout" {
				extra = []string{"CLAUDE_PROXY_TEST_IO_TIMEOUT=200ms"}
			}
			stream, input, _ := startClaudePipe(t, home, "hold", dir, extra)
			pid := awaitStarts(t, dir, 1)[0]
			content := strings.Repeat("private blocked prompt", 100000)
			message := map[string]any{"type": "user", "uuid": "target", "message": map[string]any{"role": "user", "content": content}}
			if cancellation == "control" {
				message = map[string]any{"type": "control_request", "request_id": "target", "request": map[string]any{"subtype": "generate_session_title", "description": content}}
			}
			if err := json.NewEncoder(input).Encode(message); err != nil {
				t.Fatal(err)
			}
			awaitClaudeTaskStamp(t, proxyDir)
			switch cancellation {
			case "interrupt":
				fmt.Fprintln(input, `{"type":"control_request","request_id":"cancel","request":{"subtype":"interrupt"}}`)
			case "message":
				fmt.Fprintln(input, `{"type":"control_request","request_id":"cancel","request":{"subtype":"cancel_async_message","message_uuid":"target"}}`)
			case "control":
				fmt.Fprintln(input, `{"type":"control_cancel_request","request_id":"target"}`)
			}
			// Keep host stdin open: cleanup must unblock its inherited reader.
			if code := childExit(t, stream, 3*time.Second); code != 2 {
				t.Fatalf("blocked writer exit=%d stderr=%s", code, stream.stderr.String())
			}
			awaitClaudeFiles(t, dir, "*.terminated", 1)
			var nativePID int
			fmt.Sscan(pid, &nativePID)
			if err := syscall.Kill(nativePID, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("slot freed while native Claude was alive: %v", err)
			}
			lock, err := os.OpenFile(filepath.Join(proxyDir, stateLockFilename), os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Fatalf("shared state lock stayed blocked: %v", err)
			}
			syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			next := startClaude(t, home, "echo", "", "", nil, "-p", "after cleanup")
			if code := childExit(t, next, 3*time.Second); code != 0 {
				t.Fatalf("task slot leaked: %d %s", code, next.stderr.String())
			}
			if strings.Contains(stream.stderr.String(), "private blocked prompt") {
				t.Fatal("diagnostic exposed protocol content")
			}
		})
	}
}

func TestClaudeReaderBudgetCoversAllRetainedFrames(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		n    int
	}{
		{"bytes", strings.Repeat("a", 2<<20) + "\n", 20},
		{"count", "{}\n", claudeMaxPendingFrames + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			budget := &claudeByteBudget{}
			events := make(chan claudeReadEvent, 32)
			done := make(chan struct{})
			go func() {
				defer close(done)
				readClaudeFrames(ctx, strings.NewReader(strings.Repeat(tc.line, tc.n)), budget, events)
			}()
			// Transfer frames to a simulated admission/write queue without
			// releasing them: the reader's allowance must not reset on dequeue.
			var held []*claudeFrame
			for {
				select {
				case event := <-events:
					if event.frame == nil {
						if !errors.Is(event.err, errClaudeQueueFull) {
							t.Fatalf("reader exceeded its shared budget: %v", event.err)
						}
						goto exhausted
					}
					held = append(held, event.frame)
				case <-time.After(5 * time.Second):
					t.Fatal("overloaded reader blocked ahead of cancellation")
				}
			}
		exhausted:
			<-done
			if budget.bytes > maxAppServerMessage || budget.count > claudeMaxPendingFrames {
				t.Fatalf("retained bytes=%d count=%d", budget.bytes, budget.count)
			}
			for _, frame := range held {
				frame.release()
			}
			if budget.bytes != 0 || budget.count != 0 {
				t.Fatal("frame reservations leaked")
			}
		})
	}
}

func TestClaudeOutputSerializesCompleteFrames(t *testing.T) {
	native, nativeWriter, _ := os.Pipe()
	hostReader, host, _ := os.Pipe()
	defer nativeWriter.Close()
	defer hostReader.Close()
	defer host.Close()
	output, err := startClaudeOutput(context.Background(), native, host, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer output.stop()
	large := []byte(`{"type":"result","result":"` + strings.Repeat("a", 2<<20) + `"}`)
	if _, err := nativeWriter.Write(large[:4096]); err != nil {
		t.Fatal(err)
	}
	// The native producer is midway through a frame when a local response
	// is generated. Both must reach stdout as separate valid JSON lines.
	if err := output.cancelAsyncResponse("cancel"); err != nil {
		t.Fatal(err)
	}
	wrote := make(chan error, 1)
	go func() {
		_, err := nativeWriter.Write(append(large[4096:], '\n'))
		nativeWriter.Close()
		wrote <- err
	}()
	scanner := appScanner(hostReader)
	types := map[string]bool{}
	for len(types) < 2 && scanner.Scan() {
		message, err := decodeRPC(scanner.Bytes())
		if err != nil {
			t.Fatal("native and local JSON frames interleaved")
		}
		types[rpcString(message, "type")] = true
	}
	if !types["result"] || !types["control_response"] {
		t.Fatalf("missing output frames: %v", types)
	}
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	<-output.readDone
	close(output.frames)
	<-output.writeDone
	if err := output.failure(); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeNativeOutputBurstWaitsForBudget(t *testing.T) {
	native, producer, _ := os.Pipe()
	reader, host, _ := os.Pipe()
	defer producer.Close()
	defer reader.Close()
	defer host.Close()
	output, err := startClaudeOutput(context.Background(), native, host, 5*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer output.stop()
	produced := make(chan error, 1)
	go func() {
		encoder := json.NewEncoder(producer)
		err := encoder.Encode(map[string]any{"type": "result", "result": strings.Repeat("a", 2<<20)})
		for i := 0; i < 200 && err == nil; i++ {
			err = encoder.Encode(map[string]any{"type": "stream_event", "index": i})
		}
		producer.Close()
		produced <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	full := false
	for time.Now().Before(deadline) {
		output.budget.mu.Lock()
		full = output.budget.count == claudeMaxPendingFrames
		output.budget.mu.Unlock()
		if full {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !full {
		t.Fatal("native output never reached the queue budget")
	}
	if err := output.failure(); err != nil {
		t.Fatalf("normal output burst was rejected: %v", err)
	}
	scanner := appScanner(reader)
	for i := 0; i < 201; i++ {
		if !scanner.Scan() || !json.Valid(scanner.Bytes()) {
			t.Fatalf("burst output lost frame %d: %v", i, scanner.Err())
		}
	}
	if err := <-produced; err != nil {
		t.Fatal(err)
	}
	<-output.readDone
	close(output.frames)
	<-output.writeDone
	if err := output.failure(); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeBlockedStdoutStopsNativeBeforeSlotRelease(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	configureClaude(t, home, 1, "5s")
	self, _ := os.Executable()
	cmd := exec.Command(self, append([]string{"-test.run=^TestRunHelperProcess$", "--"}, claudeStreamArgs...)...)
	cmd.Env = append(os.Environ(), "CLAUDE_PROXY_TEST_PROCESS=1", "CODEX_PROXY_TEST_HOME="+home, "CODEX_PROXY_TEST_TASK_INTERVAL=0", "CODEX_PROXY_TEST_NO_SANDBOX=1", "CLAUDE_PROXY_TEST_IO_TIMEOUT=200ms", "FAKE_CLAUDE_MODE=stream-hold", "FAKE_CLAUDE_DIR="+dir, "FAKE_CLAUDE_RESULT_BYTES=2097152")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	reader, writer, _ := os.Pipe()
	defer reader.Close()
	defer writer.Close()
	cmd.Stdout = writer // Keep the host reader open without consuming stdout.
	diagnostic, _ := os.Create(filepath.Join(t.TempDir(), "stderr"))
	defer diagnostic.Close()
	cmd.Stderr = diagnostic
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := &proxyChild{cmd: cmd, result: make(chan error, 1)}
	go func() { child.result <- cmd.Wait() }()
	t.Cleanup(func() {
		input.Close()
		cmd.Process.Kill()
		for _, pid := range startedPIDs(t, dir) {
			var value int
			fmt.Sscan(pid, &value)
			syscall.Kill(-value, syscall.SIGKILL)
		}
	})
	fmt.Fprintln(input, `{"type":"user","message":{"role":"user","content":"produce large output"}}`)
	if code := childExit(t, child, 5*time.Second); code != 2 {
		data, _ := os.ReadFile(diagnostic.Name())
		t.Fatalf("stdout backpressure exit=%d stderr=%s", code, data)
	}
	awaitClaudeFiles(t, dir, "*.terminated", 1)
	next := startClaude(t, home, "echo", "", "", nil, "-p", "after stdout failure")
	if code := childExit(t, next, 3*time.Second); code != 0 {
		t.Fatalf("stdout failure leaked slot: %d %s", code, next.stderr.String())
	}
	// Cleanup must not close the caller's host file descriptor.
	if _, err := writer.Write([]byte{}); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("caller stdout was closed: %v", err)
	}
}

func TestClaudeCancellationDuringSubmissionInterval(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	configureClaude(t, home, 1, "5s")
	stream, input, output := startClaudePipe(t, home, "stream", dir, []string{"CODEX_PROXY_TEST_TASK_INTERVAL=2"})
	fmt.Fprintln(input, `{"type":"user","uuid":"first","message":{"role":"user","content":"one"}}`)
	awaitClaudeFiles(t, dir, "*-user-1.json", 1)
	fmt.Fprintln(input, `{"type":"user","uuid":"second","message":{"role":"user","content":"cancel during interval"}}`)
	fmt.Fprintln(input, `{"type":"control_request","request_id":"cancel","request":{"subtype":"cancel_async_message","message_uuid":"second"}}`)
	response := awaitClaudeResponse(t, output, "cancel")
	if string(rpcObject(response, "response")["cancelled"]) != "true" {
		t.Fatal("interval wait ignored targeted cancellation")
	}
	input.Close()
	if code := childExit(t, stream, 5*time.Second); code != 0 {
		t.Fatalf("interval cancellation stuck: %d %s", code, stream.stderr.String())
	}
	awaitClaudeFiles(t, dir, "*-user-*.json", 1)
}

func TestClaudeNativeExitDrainsLastLargeResult(t *testing.T) {
	home := t.TempDir()
	configureClaude(t, home, 1, "5s")
	stream := startClaude(t, home, "stream", "", `{"type":"user","message":{"role":"user","content":"large result"}}`+"\n", []string{"FAKE_CLAUDE_RESULT_BYTES=2097152"}, claudeStreamArgs...)
	if code := childExit(t, stream, 5*time.Second); code != 0 {
		t.Fatalf("native exit=%d stderr=%s", code, stream.stderr.String())
	}
	message, err := decodeRPC(bytes.TrimSpace(stream.stdout.Bytes()))
	if err != nil || len(rpcString(message, "result")) != 2<<20 {
		t.Fatalf("native exit truncated the last result: err=%v bytes=%d", err, stream.stdout.Len())
	}
}

func TestClaudeAgentSDKCancellationCompatibility(t *testing.T) {
	if os.Getenv("CLAUDE_PROXY_TEST_SDK_MODULE") == "" {
		t.Skip("set CLAUDE_PROXY_TEST_SDK_MODULE to an installed SDK's sdk.mjs")
	}
	home, dir := t.TempDir(), t.TempDir()
	configureClaude(t, home, 1, "5s")
	holder := startClaude(t, home, "hold", dir, "", nil, "-p", "hold SDK quota")
	pid := awaitStarts(t, dir, 1)[0]
	self, _ := os.Executable()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "./testdata/sdk_cancel_compat.mjs")
	cmd.Env = append(os.Environ(), "CLAUDE_PROXY_SDK_TEST_PROCESS=1", "CODEX_PROXY_TEST_HOME="+home, "CODEX_PROXY_TEST_TASK_INTERVAL=0", "CODEX_PROXY_TEST_NO_SANDBOX=1", "CLAUDE_PROXY_TEST_BINARY="+self, "FAKE_CLAUDE_MODE=stream", "FAKE_CLAUDE_DIR="+dir)
	output, err := cmd.CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte(`"cancelled":true`)) {
		t.Fatalf("SDK cancellation compatibility: %v\n%s", err, output)
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "*-user-*.json"))
	if len(paths) != 0 {
		t.Fatal("SDK canceled input reached native Claude")
	}
	releaseNative(t, dir, pid)
	if code := childExit(t, holder, 5*time.Second); code != 0 {
		t.Fatalf("holder=%d", code)
	}
}
