package proxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestStateConfiguredPaths(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		customLock, customState bool
	}{
		{"defaults", false, false},
		{"lock only", true, false},
		{"state only", false, true},
		{"both in separate directories", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			lockPath, statePath := filepath.Join(dir, stateLockFilename), filepath.Join(dir, stateFilename)
			cfg := Config{}
			if tc.customLock {
				lockPath = filepath.Join(t.TempDir(), "custom.lock")
				cfg.StateLockPath = lockPath
			}
			if tc.customState {
				statePath = filepath.Join(t.TempDir(), "custom.json")
				cfg.StatePath = statePath
				// A custom state path must not read a broken default state.
				if err := os.WriteFile(filepath.Join(dir, stateFilename), []byte("broken default"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			gate := newStateStore(dir, cfg, time.Millisecond, func() int64 { return 0 })
			gate.now = func() time.Time { return time.Unix(1000, 0) }
			release := startLockHolder(t, lockPath)
			ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
			defer cancel()
			permit, err := gate.Acquire(ctx)
			if permit != nil {
				permit.Close()
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("configured lock was bypassed: %v", err)
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
			state, err := gate.readState()
			if err != nil {
				t.Fatal(err)
			}
			if stamp, err := state.lastCodexTaskAt(); err != nil || stamp != 1000 {
				t.Fatalf("custom state timestamp = %d, error = %v", stamp, err)
			}
			for _, path := range []string{lockPath, statePath} {
				if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
					t.Fatalf("%s must exist with mode 0600: %v", path, err)
				}
			}
			if tc.customLock {
				if _, err := os.Stat(filepath.Join(dir, stateLockFilename)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("custom lock touched the default lock: %v", err)
				}
			}
			if tc.customState {
				if data, err := os.ReadFile(filepath.Join(dir, stateFilename)); err != nil || string(data) != "broken default" {
					t.Fatalf("custom state changed the default state: %s, %v", data, err)
				}
			}
			if files, err := filepath.Glob(filepath.Join(filepath.Dir(statePath), ".state-*.json")); err != nil || len(files) != 0 {
				t.Fatalf("state temporary files left behind: %v, %v", files, err)
			}
		})
	}
}

func TestStateConfiguredMissingParentDoesNotFallback(t *testing.T) {
	for _, missing := range []string{"lock", "state"} {
		t.Run(missing, func(t *testing.T) {
			dir, customDir := t.TempDir(), t.TempDir()
			writeTaskState(t, dir, 1)
			cfg := Config{StateLockPath: filepath.Join(customDir, "lock"), StatePath: filepath.Join(customDir, "state")}
			if missing == "lock" {
				cfg.StateLockPath = filepath.Join(customDir, "missing", "lock")
			} else {
				cfg.StatePath = filepath.Join(customDir, "missing", "state")
			}
			permit, err := newStateStore(dir, cfg, 0, func() int64 { return 0 }).Acquire(context.Background())
			if permit != nil {
				defer permit.Close()
				err = permit.Record(context.Background())
			}
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("missing custom parent did not fail: %v", err)
			}
			if readTaskState(t, dir) != 1 {
				t.Fatal("custom path failure updated the default state")
			}
			if _, err := os.Stat(filepath.Join(dir, stateLockFilename)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("custom path failure opened the default lock: %v", err)
			}
		})
	}
}

func TestStateRejectsIdenticalPaths(t *testing.T) {
	dir := t.TempDir()
	for _, cfg := range []Config{
		{StateLockPath: filepath.Join(dir, "same"), StatePath: filepath.Join(dir, "same")},
		{StateLockPath: filepath.Join(dir, stateFilename)},
		{StatePath: filepath.Join(dir, stateLockFilename)},
		{StateLockPath: dir + "/sub/../same", StatePath: filepath.Join(dir, "same")},
	} {
		permit, err := newStateStore(dir, cfg, 0, nil).Acquire(context.Background())
		if permit != nil {
			permit.Close()
		}
		if err == nil {
			t.Fatalf("identical lock and state paths accepted: %+v", cfg)
		}
	}
}

func TestStateRejectsAliasedPaths(t *testing.T) {
	for _, alias := range []string{"directory symlink", "hard link", "case alias"} {
		t.Run(alias, func(t *testing.T) {
			dir := t.TempDir()
			writeTaskState(t, dir, 1)
			statePath := filepath.Join(dir, stateFilename)
			var lockPath string
			switch alias {
			case "directory symlink":
				link := filepath.Join(t.TempDir(), "state-dir")
				if err := os.Symlink(dir, link); err != nil {
					t.Fatal(err)
				}
				lockPath = filepath.Join(link, stateFilename)
			case "hard link":
				lockPath = filepath.Join(dir, stateLockFilename)
				if err := os.Link(statePath, lockPath); err != nil {
					t.Fatal(err)
				}
			case "case alias":
				lockPath = filepath.Join(dir, "STATE.JSON")
				if _, err := os.Stat(lockPath); errors.Is(err, os.ErrNotExist) {
					t.Skip("requires a case-insensitive filesystem")
				} else if err != nil {
					t.Fatal(err)
				}
			}
			cfg := Config{StateLockPath: lockPath, StatePath: statePath}
			permit, err := newStateStore(dir, cfg, 0, func() int64 { return 0 }).Acquire(context.Background())
			if permit != nil {
				permit.Close()
			}
			if err == nil {
				t.Fatal("lock and state paths referring to the same file were accepted")
			}
			if readTaskState(t, dir) != 1 {
				t.Fatal("rejected alias changed the state file")
			}
			file, err := os.OpenFile(lockPath, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if locked, err := tryLock(context.Background(), file); err != nil || !locked {
				t.Fatalf("rejected alias leaked the state lock: locked=%v error=%v", locked, err)
			}
		})
	}
}

func TestStateRecordAcrossFilesystems(t *testing.T) {
	dir := t.TempDir()
	stateDir, err := os.MkdirTemp("/dev/shm", "codex-proxy-state-")
	if err != nil {
		t.Skipf("separate memory filesystem unavailable: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(stateDir) })
	baseInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	stateInfo, err := os.Stat(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if baseInfo.Sys().(*syscall.Stat_t).Dev == stateInfo.Sys().(*syscall.Stat_t).Dev {
		t.Skip("state and configuration directories use the same filesystem")
	}
	gate := newStateStore(dir, Config{StatePath: filepath.Join(stateDir, "custom.json")}, 0, func() int64 { return 0 })
	permit, err := gate.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer permit.Close()
	if err := permit.Record(context.Background()); err != nil {
		t.Fatalf("record across filesystems: %v", err)
	}
	state, err := gate.readState()
	if err != nil {
		t.Fatal(err)
	}
	if stamp, err := state.lastCodexTaskAt(); err != nil || stamp <= 0 {
		t.Fatalf("state timestamp = %d, error = %v", stamp, err)
	}
}
