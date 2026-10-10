package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLoggerMetadataAndBoundedRecords(t *testing.T) {
	dir := t.TempDir()
	var diagnostics bytes.Buffer
	logger := NewLogger(dir, "request-123", &diagnostics, func() (Config, error) {
		return Config{LogMaxSize: 256, LogMaxBackups: 5}, nil
	})
	logger.Log("started", map[string]any{
		"timestamp": "spoofed", "request_id": "spoofed", "pid": -1, "event": "spoofed",
		"count": 3, "payload": strings.Repeat("大", 1000),
	})
	records := readLoggerRecords(t, dir, 256)
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	record := records[0]
	if record["request_id"] != "request-123" || record["event"] != "started" || record["pid"] != float64(os.Getpid()) {
		t.Fatalf("required metadata was overwritten: %v", record)
	}
	stamp, ok := record["timestamp"].(string)
	if !ok {
		t.Fatalf("timestamp is not a string: %v", record)
	}
	parsed, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil || parsed.Location() != time.UTC {
		t.Fatalf("timestamp is not UTC RFC3339Nano: %q, %v", stamp, err)
	}
	if record["count"] != float64(3) || record["truncated"] != true {
		t.Fatalf("small fields or truncation marker missing: %v", record)
	}
	if _, found := record["payload"]; found {
		t.Fatalf("oversized payload was retained: %v", record)
	}
	if diagnostics.Len() != 0 {
		t.Fatalf("unexpected diagnostics: %s", diagnostics.String())
	}
	assertLoggerPrivateFiles(t, dir)
}

func TestLoggerBoundsLongCoreStringsAndInvalidFields(t *testing.T) {
	dir := t.TempDir()
	var diagnostics bytes.Buffer
	logger := NewLogger(dir, strings.Repeat("请求", 1000), &diagnostics, func() (Config, error) {
		return Config{LogMaxSize: 256, LogMaxBackups: 5}, nil
	})
	logger.Log(strings.Repeat("<事件>", 1000), map[string]any{"unsupported": make(chan int)})
	records := readLoggerRecords(t, dir, 256)
	if len(records) != 1 || records[0]["truncated"] != true {
		t.Fatalf("bounded record missing: %v", records)
	}
	for _, name := range []string{"timestamp", "request_id", "pid", "event"} {
		if _, found := records[0][name]; !found {
			t.Fatalf("required field %s missing", name)
		}
	}
	if !strings.Contains(diagnostics.String(), "encode log field") {
		t.Fatalf("invalid field diagnostic missing: %s", diagnostics.String())
	}
}

func TestLoggerReloadsSizeAndRetention(t *testing.T) {
	dir := t.TempDir()
	var diagnostics bytes.Buffer
	cfg := Config{LogMaxSize: 512, LogMaxBackups: 3}
	loads := 0
	logger := NewLogger(dir, "retention", &diagnostics, func() (Config, error) {
		loads++
		return cfg, nil
	})
	for i := 0; i < 30; i++ {
		logger.Log("entry", map[string]any{"sequence": i, "detail": strings.Repeat("x", 100)})
	}
	assertLoggerArchiveCount(t, logger, 3)
	readLoggerRecords(t, dir, 512)
	lockBefore, err := os.Stat(filepath.Join(dir, "log.lock"))
	if err != nil {
		t.Fatal(err)
	}

	cfg.LogMaxSize = 256
	cfg.LogMaxBackups = 1
	logger.Log("shrunk", map[string]any{"sequence": 30})
	assertLoggerArchiveCount(t, logger, 1)
	active, err := os.Stat(filepath.Join(dir, loggerFilename))
	if err != nil || active.Size() > cfg.LogMaxSize {
		t.Fatalf("active log did not adopt reduced size: %v, %v", active, err)
	}
	// Historical archives keep their contents when the size setting is reduced.
	readLoggerRecords(t, dir, 512)

	cfg.LogMaxBackups = 0
	for i := 0; i < 10; i++ {
		logger.Log("zero_backups", map[string]any{"sequence": i})
	}
	assertLoggerArchiveCount(t, logger, 0)
	readLoggerRecords(t, dir, 256)
	if loads != 41 {
		t.Fatalf("loaded configuration %d times, want one load for each of 41 records", loads)
	}
	lockAfter, err := os.Stat(filepath.Join(dir, "log.lock"))
	if err != nil || !os.SameFile(lockBefore, lockAfter) {
		t.Fatalf("persistent lock file was replaced: %v", err)
	}
	if diagnostics.Len() != 0 {
		t.Fatalf("unexpected diagnostics: %s", diagnostics.String())
	}
	assertLoggerPrivateFiles(t, dir)
}

func TestLoggerInvalidConfigurationUsesDefaults(t *testing.T) {
	for _, name := range []string{"loader_error", "invalid_size", "invalid_backups"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			var diagnostics bytes.Buffer
			logger := NewLogger(dir, name, &diagnostics, func() (Config, error) {
				switch name {
				case "loader_error":
					return Config{}, errors.New("invalid configuration")
				case "invalid_size":
					return Config{LogMaxSize: 1, LogMaxBackups: 5}, nil
				default:
					return Config{LogMaxSize: 512, LogMaxBackups: -1}, nil
				}
			})
			// This exceeds the invalid requested size and the 512-byte setting,
			// but fits the fallback size and should remain in a single file.
			for i := 0; i < 10; i++ {
				logger.Log("configuration_error", map[string]any{"sequence": i})
			}
			if records := readLoggerRecords(t, dir, 10<<20); len(records) != 10 {
				t.Fatalf("fallback lost records: got %d, want 10", len(records))
			}
			assertLoggerArchiveCount(t, logger, 0)
			if !strings.Contains(diagnostics.String(), "using defaults") {
				t.Fatalf("fallback diagnostic missing: %s", diagnostics.String())
			}
		})
	}
}

func TestLoggerCrossProcessWritesAndRotation(t *testing.T) {
	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const workers, entries = 6, 20
	commands := make([]*exec.Cmd, workers)
	outputs := make([]bytes.Buffer, workers)
	for worker := 0; worker < workers; worker++ {
		command := exec.Command(executable, "-test.run=^TestLoggerSubprocessHelper$")
		command.Env = append(os.Environ(),
			"CODEX_PROXY_LOG_TEST_DIR="+dir,
			"CODEX_PROXY_LOG_TEST_WORKER="+strconv.Itoa(worker),
		)
		command.Stdout = &outputs[worker]
		command.Stderr = &outputs[worker]
		if err := command.Start(); err != nil {
			for _, started := range commands {
				if started != nil {
					_ = started.Process.Kill()
					_ = started.Wait()
				}
			}
			t.Fatal(err)
		}
		commands[worker] = command
	}
	for worker, command := range commands {
		if err := command.Wait(); err != nil {
			t.Errorf("worker %d failed: %v\n%s", worker, err, outputs[worker].String())
		}
	}
	if t.Failed() {
		return
	}
	records := readLoggerRecords(t, dir, 1024)
	if len(records) != workers*entries {
		t.Fatalf("got %d records, want %d", len(records), workers*entries)
	}
	seen := make(map[string]bool, len(records))
	for _, record := range records {
		key := fmt.Sprintf("%v:%v", record["request_id"], record["sequence"])
		if seen[key] {
			t.Fatalf("duplicate record %s", key)
		}
		seen[key] = true
		worker, err := strconv.Atoi(strings.TrimPrefix(record["request_id"].(string), "worker-"))
		if err != nil || worker < 0 || worker >= workers {
			t.Fatalf("invalid worker metadata: %v", record)
		}
		if record["pid"] != float64(commands[worker].Process.Pid) {
			t.Fatalf("record has wrong process ID: %v", record)
		}
	}
	for worker := 0; worker < workers; worker++ {
		for sequence := 0; sequence < entries; sequence++ {
			if !seen[fmt.Sprintf("worker-%d:%d", worker, sequence)] {
				t.Fatalf("missing worker %d sequence %d", worker, sequence)
			}
		}
	}
	assertLoggerPrivateFiles(t, dir)
}

func TestLoggerSubprocessHelper(t *testing.T) {
	dir := os.Getenv("CODEX_PROXY_LOG_TEST_DIR")
	if dir == "" {
		return
	}
	var diagnostics bytes.Buffer
	logger := NewLogger(dir, "worker-"+os.Getenv("CODEX_PROXY_LOG_TEST_WORKER"), &diagnostics, func() (Config, error) {
		return Config{LogMaxSize: 1024, LogMaxBackups: 64}, nil
	})
	for i := 0; i < 20; i++ {
		logger.Log("concurrent", map[string]any{"sequence": i})
	}
	if diagnostics.Len() != 0 {
		t.Fatalf("unexpected diagnostics: %s", diagnostics.String())
	}
}

func TestLoggerReportsWriteFailure(t *testing.T) {
	var diagnostics bytes.Buffer
	logger := NewLogger(filepath.Join(t.TempDir(), "missing"), "failure", &diagnostics, nil)
	logger.Log("failed", nil)
	if !strings.Contains(diagnostics.String(), "open log lock") {
		t.Fatalf("logging failure diagnostic missing: %s", diagnostics.String())
	}
}

func TestLoggerDoesNotHangOnExternallyHeldLock(t *testing.T) {
	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestLoggerLockHolderHelper$")
	command.Env = append(os.Environ(), "CODEX_PROXY_LOG_TEST_LOCK_DIR="+dir)
	command.Stderr = os.Stderr
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = input.Close()
		if err := command.Wait(); err != nil {
			t.Errorf("lock holder failed: %v", err)
		}
	}()
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(output).ReadString('\n')
		if err == nil && strings.TrimSpace(line) != "locked" {
			err = fmt.Errorf("unexpected lock holder response %q", line)
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		_ = command.Process.Kill()
		t.Fatal("lock holder did not acquire lock")
	}

	var diagnostics bytes.Buffer
	logger := NewLogger(dir, "blocked", &diagnostics, func() (Config, error) {
		return Config{LogMaxSize: 512, LogMaxBackups: 5}, nil
	})
	completed := make(chan time.Duration, 1)
	go func() {
		started := time.Now()
		logger.Log("blocked", nil)
		completed <- time.Since(started)
	}()
	select {
	case elapsed := <-completed:
		if elapsed > 500*time.Millisecond {
			t.Fatalf("Log stalled for %v with an externally held lock", elapsed)
		}
	case <-time.After(500 * time.Millisecond):
		// Release the helper's lock so a regression fails without hanging the test.
		_ = input.Close()
		select {
		case <-completed:
		case <-time.After(2 * time.Second):
			_ = command.Process.Kill()
		}
		t.Fatal("Log did not return within 500ms with an externally held lock")
	}
	if !strings.Contains(diagnostics.String(), "timed out waiting for log lock") {
		t.Fatalf("lock timeout diagnostic missing: %s", diagnostics.String())
	}
	if _, err := os.Stat(filepath.Join(dir, loggerFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("timed-out event created a log file: %v", err)
	}
}

func TestLoggerLockHolderHelper(t *testing.T) {
	dir := os.Getenv("CODEX_PROXY_LOG_TEST_LOCK_DIR")
	if dir == "" {
		return
	}
	lock, err := openPrivateLogFile(filepath.Join(dir, "log.lock"), os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	fmt.Fprintln(os.Stdout, "locked")
	_, _ = bufio.NewReader(os.Stdin).ReadByte()
}

func readLoggerRecords(t *testing.T, dir string, maxSize int64) []map[string]any {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, loggerFilename+"*"))
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
			var record map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
				t.Fatalf("invalid JSON record in %s: %v", path, err)
			}
			records = append(records, record)
		}
		if err := scanner.Err(); err != nil {
			t.Fatal(err)
		}
	}
	return records
}

func assertLoggerArchiveCount(t *testing.T, logger *Logger, want int) {
	t.Helper()
	indexes, err := logger.archives()
	if err != nil {
		t.Fatal(err)
	}
	if len(indexes) != want {
		t.Fatalf("got archives %v, want %d", indexes, want)
	}
	for _, index := range indexes {
		if index > want {
			t.Fatalf("unexpected archive index %d, retention is %d", index, want)
		}
	}
}

func assertLoggerPrivateFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("%s has mode %o, want 600", entry.Name(), info.Mode().Perm())
		}
	}
}
