package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTaskState(t *testing.T, dir string, stamp int64) {
	t.Helper()
	data, err := json.Marshal(proxyState{lastCodexTaskKey: rpcValue(stamp)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFilename), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func readTaskState(t *testing.T, dir string) int64 {
	t.Helper()
	state, err := newStateStore(dir, Config{}, 0, nil).readState()
	if err != nil {
		t.Fatal(err)
	}
	stamp, err := state.lastCodexTaskAt()
	if err != nil {
		t.Fatal(err)
	}
	return stamp
}

func TestStateRandomIntervalRange(t *testing.T) {
	seen := make(map[int64]bool)
	for i := 0; i < 10000; i++ {
		interval := randomTaskInterval()
		if interval < 3 || interval > 15 {
			t.Fatalf("interval out of range: %d", interval)
		}
		seen[interval] = true
	}
	if len(seen) != 13 {
		t.Fatalf("expected all integer intervals from 3 through 15, got %v", seen)
	}
}

func TestStateWaitDifferenceAndPersist(t *testing.T) {
	for _, tc := range []struct {
		name string
		last int64
		wait time.Duration
	}{
		{"first submission", 0, 0},
		{"old submission", 980, 0},
		{"exact interval", 995, 0},
		{"recent submission", 998, 3 * time.Second},
		{"same second", 1000, 5 * time.Second},
		{"clock rollback", 1010, 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.last != 0 {
				writeTaskState(t, dir, tc.last)
			}
			now := time.Unix(1000, 700000000)
			draws := 0
			gate := newStateStore(dir, Config{}, time.Millisecond, func() int64 { draws++; return 5 })
			gate.now = func() time.Time { return now }
			var slept time.Duration
			gate.sleep = func(ctx context.Context, delay time.Duration) error {
				slept += delay
				now = now.Add(delay)
				return ctx.Err()
			}
			permit, err := gate.Acquire(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer permit.Close()
			if slept != tc.wait || draws != 1 {
				t.Fatalf("slept=%v want=%v draws=%d", slept, tc.wait, draws)
			}
			if stamp := readTaskState(t, dir); stamp != tc.last {
				t.Fatalf("acquisition recorded an unsubmitted task: %d", stamp)
			}
			if err := permit.Record(context.Background()); err != nil {
				t.Fatal(err)
			}
			if stamp := readTaskState(t, dir); stamp != now.Unix() {
				t.Fatalf("timestamp=%d want=%d", stamp, now.Unix())
			}
			lockBefore, err := os.Stat(filepath.Join(dir, stateLockFilename))
			if err != nil {
				t.Fatal(err)
			}
			if err := permit.Close(); err != nil {
				t.Fatal(err)
			}
			now = now.Add(5 * time.Second)
			next, err := gate.Acquire(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			if err := next.Record(context.Background()); err != nil {
				t.Fatal(err)
			}
			lockAfter, err := os.Stat(filepath.Join(dir, stateLockFilename))
			if err != nil || !os.SameFile(lockBefore, lockAfter) {
				t.Fatalf("persistent lock inode changed: %v", err)
			}
			for _, name := range []string{stateLockFilename, stateFilename} {
				info, err := os.Stat(filepath.Join(dir, name))
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatalf("%s must have mode 0600: %v", name, err)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 2 {
				t.Fatalf("temporary state files left behind: %v, %v", entries, err)
			}
		})
	}
}

func TestStateRecordUsesCurrentTimeAndPreservesOtherFields(t *testing.T) {
	for _, tc := range []struct {
		name     string
		last     int64
		metadata bool
		wait     time.Duration
	}{
		{"after sleep", 998, true, 3 * time.Second},
		{"other state before first task", 0, true, 0},
		{"empty state before first task", 0, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			original := make(proxyState)
			if tc.last != 0 {
				original[lastCodexTaskKey] = rpcValue(tc.last)
			}
			if tc.metadata {
				original["future_state"] = json.RawMessage(`{"enabled":true,"items":[1,null,"value"]}`)
				original["large_integer"] = json.RawMessage(`9007199254740993`)
				original["text"] = rpcValue(strings.Repeat("x", 8192))
			}
			data, err := json.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, stateFilename), data, 0600); err != nil {
				t.Fatal(err)
			}
			now := time.Unix(1000, 0)
			gate := newStateStore(dir, Config{}, 0, func() int64 { return 5 })
			gate.now = func() time.Time { return now }
			var slept time.Duration
			gate.sleep = func(ctx context.Context, delay time.Duration) error {
				slept += delay
				now = now.Add(delay)
				return ctx.Err()
			}
			permit, err := gate.Acquire(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer permit.Close()
			if slept != tc.wait {
				t.Fatalf("slept=%v want=%v", slept, tc.wait)
			}
			if readTaskState(t, dir) != tc.last {
				t.Fatal("waiting wrote a task timestamp before submission")
			}
			// The protocol loop may take more time after waiting. Record must
			// sample again at submission, rather than remember either old time.
			now = now.Add(7 * time.Second)
			if err := permit.Record(context.Background()); err != nil {
				t.Fatal(err)
			}
			if stamp := readTaskState(t, dir); stamp != now.Unix() {
				t.Fatalf("recorded stale time=%d want=%d", stamp, now.Unix())
			}
			updated, err := gate.readState()
			if err != nil {
				t.Fatal(err)
			}
			for key, value := range original {
				if key != lastCodexTaskKey && !bytes.Equal(updated[key], value) {
					t.Fatalf("other state field changed: %s=%s want=%s", key, updated[key], value)
				}
			}
			expectedFields := len(original)
			if tc.last == 0 {
				expectedFields++
			}
			if len(updated) != expectedFields {
				t.Fatalf("unexpected state fields: %v", updated)
			}
		})
	}
}

func TestStateCancellationDuringSleep(t *testing.T) {
	dir := t.TempDir()
	writeTaskState(t, dir, 1000)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gate := newStateStore(dir, Config{}, time.Millisecond, func() int64 { return 15 })
	gate.now = func() time.Time { return time.Unix(1000, 0) }
	gate.sleep = func(ctx context.Context, delay time.Duration) error {
		if delay != 15*time.Second {
			t.Errorf("incorrect sleep duration: %v", delay)
		}
		cancel()
		return ctx.Err()
	}
	permit, err := gate.Acquire(ctx)
	if permit != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation without permit: %v", err)
	}
	if stamp := readTaskState(t, dir); stamp != 1000 {
		t.Fatalf("canceled task updated timestamp: %d", stamp)
	}
	next, err := newStateStore(dir, Config{}, 0, func() int64 { return 0 }).Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if err := next.Record(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("record ignored cancellation: %v", err)
	}
	if stamp := readTaskState(t, dir); stamp != 1000 {
		t.Fatalf("canceled record updated timestamp: %d", stamp)
	}
}

func TestStateCrossProcessLock(t *testing.T) {
	dir := t.TempDir()
	release := startLockHolder(t, filepath.Join(dir, stateLockFilename))
	draws := 0
	gate := newStateStore(dir, Config{}, time.Millisecond, func() int64 { draws++; return 3 })
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	permit, err := gate.Acquire(ctx)
	if permit != nil || !errors.Is(err, context.DeadlineExceeded) || draws != 1 {
		t.Fatalf("external lock bypassed or interval resampled: err=%v draws=%d", err, draws)
	}
	if stamp := readTaskState(t, dir); stamp != 0 {
		t.Fatalf("lock waiter updated timestamp: %d", stamp)
	}
	release()
	permit, err = gate.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer permit.Close()
	if err := permit.Record(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStateRejectsInvalidStateAndPaths(t *testing.T) {
	for _, data := range []string{"", "null", "[]", `{"last_codex_task_at":-1}`, `{"last_codex_task_at":null}`, `{"last_codex_task_at":"123"}`, `{"last_codex_task_at":1} trailing`} {
		t.Run(data, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, stateFilename), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			permit, err := newStateStore(dir, Config{}, 0, nil).Acquire(context.Background())
			if permit != nil || err == nil {
				t.Fatalf("invalid state accepted: %q", data)
			}
			writeTaskState(t, dir, 1)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			next, err := newStateStore(dir, Config{}, 0, nil).Acquire(ctx)
			if err != nil {
				t.Fatalf("state failure leaked lock: %v", err)
			}
			next.Close()
		})
	}
	for _, name := range []string{stateLockFilename, stateFilename} {
		t.Run(name+" symlink", func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(target, []byte(`{"last_codex_task_at":1}`), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
			permit, err := newStateStore(dir, Config{}, 0, nil).Acquire(context.Background())
			if permit != nil || err == nil {
				t.Fatalf("symlink accepted for %s", name)
			}
		})
	}
}

func awaitStateLock(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		file, err := os.OpenFile(filepath.Join(dir, stateLockFilename), os.O_RDWR, 0)
		if err == nil {
			locked, err := tryLock(context.Background(), file)
			file.Close()
			if err != nil {
				t.Fatal(err)
			}
			if !locked {
				return
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("state lock was not acquired")
}
