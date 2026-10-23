package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	stateLockFilename = "state.lock"
	stateFilename     = "state.json"
	lastCodexTaskKey  = "last_codex_task_at"
	maxStateSize      = 1 << 20
)

// Keep unknown values as raw JSON so future state fields survive timestamp
// updates, including nested objects and integers too large for float64.
type proxyState map[string]json.RawMessage

func (s proxyState) lastCodexTaskAt() (int64, error) {
	raw, exists := s[lastCodexTaskKey]
	if !exists {
		return 0, nil
	}
	var stamp int64
	if err := json.Unmarshal(raw, &stamp); err != nil {
		return 0, fmt.Errorf("invalid Codex task timestamp in state: %w", err)
	}
	if stamp <= 0 {
		return 0, fmt.Errorf("invalid Codex task timestamp in state: got %q", string(raw))
	}
	return stamp, nil
}

// stateStore is independent of slot admission. Call Acquire only after
// reserving a slot, and keep its permit until the process starts or RPC is sent.
type stateStore struct {
	lockPath  string
	statePath string
	poll      time.Duration
	interval  func() int64
	now       func() time.Time
	sleep     func(context.Context, time.Duration) error
}

func newStateStore(dir string, cfg Config, poll time.Duration, interval func() int64) *stateStore {
	lockPath, statePath := cfg.StateLockPath, cfg.StatePath
	if lockPath == "" {
		lockPath = filepath.Join(dir, stateLockFilename)
	}
	if statePath == "" {
		statePath = filepath.Join(dir, stateFilename)
	}
	if poll <= 0 {
		poll = 100 * time.Millisecond
	}
	if interval == nil {
		interval = randomTaskInterval
	}
	return &stateStore{lockPath: filepath.Clean(lockPath), statePath: filepath.Clean(statePath), poll: poll, interval: interval, now: time.Now, sleep: sleepTaskInterval}
}

func randomTaskInterval() int64 {
	return int64(3 + rand.IntN(13))
}

func sleepTaskInterval(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (g *stateStore) Acquire(ctx context.Context) (*stateLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if g.lockPath == g.statePath {
		return nil, errors.New("state lock and state file must use different paths")
	}
	// Sample once per submission, including time spent waiting for this lock.
	interval := g.interval()
	file, err := openStateFile(g.lockPath, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return nil, fmt.Errorf("open state lock: %w", err)
	}
	permit := &stateLease{gate: g, file: file}
	ok := false
	defer func() {
		if !ok {
			permit.Close()
		}
	}()
	for {
		locked, err := tryLock(ctx, file)
		if err != nil {
			return nil, fmt.Errorf("lock state: %w", err)
		}
		if locked {
			break
		}
		if err := g.sleep(ctx, g.poll); err != nil {
			return nil, err
		}
	}
	// Different path strings can name the same file through directory aliases,
	// case folding, or hard links. Replacing that file must never replace the
	// locked inode and let another process acquire a different state lock.
	if stateInfo, err := os.Stat(g.statePath); err == nil {
		lockInfo, err := file.Stat()
		if err != nil {
			return nil, fmt.Errorf("stat state lock: %w", err)
		}
		if os.SameFile(lockInfo, stateInfo) {
			return nil, errors.New("state lock and state file must refer to different files")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stat state: %w", err)
	}
	state, err := g.readState()
	if err != nil {
		return nil, err
	}
	last, err := state.lastCodexTaskAt()
	if err != nil {
		return nil, err
	}
	permit.state = state
	if last > g.now().Unix() {
		// A wall-clock rollback must not produce an unbounded delay. Wait a
		// full sampled interval from now before replacing the old timestamp.
		last = g.now().Unix()
	}
	for last > 0 {
		remaining := interval - (g.now().Unix() - last)
		if remaining <= 0 {
			break
		}
		if err := g.sleep(ctx, time.Duration(remaining)*time.Second); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ok = true
	return permit, nil
}

func (g *stateStore) readState() (proxyState, error) {
	file, err := openStateFile(g.statePath, os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return make(proxyState), nil
	}
	if err != nil {
		return nil, fmt.Errorf("open state: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxStateSize+1))
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	if len(data) > maxStateSize {
		return nil, fmt.Errorf("invalid state: exceeds maximum size of %d bytes", maxStateSize)
	}
	var state proxyState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("invalid state: %w", err)
	}
	if state == nil {
		return nil, errors.New("invalid state: object is required")
	}
	return state, nil
}

type stateLease struct {
	gate  *stateStore
	file  *os.File
	state proxyState
}

// Record reads the current time after interval waiting has finished, immediately
// before submitting. Update only the task timestamp under state.lock. A failed
// launch/write may conservatively leave a timestamp, but cannot allow a burst.
func (p *stateLease) Record(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stamp, _ := json.Marshal(p.gate.now().Unix())
	p.state[lastCodexTaskKey] = stamp
	data, err := json.Marshal(p.state)
	if err != nil {
		return err
	}
	if len(data)+1 > maxStateSize {
		return errors.New("state exceeds maximum size")
	}
	file, err := os.CreateTemp(filepath.Dir(p.gate.statePath), ".state-*.json")
	if err != nil {
		return fmt.Errorf("create state: %w", err)
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := file.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync state: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), p.gate.statePath); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	return nil
}

func (p *stateLease) Close() error {
	if p.file == nil {
		return nil
	}
	err := p.file.Close() // Closing the sole descriptor releases flock.
	p.file = nil
	return err
}

func openStateFile(path string, flags int) (*os.File, error) {
	file, err := os.OpenFile(path, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("state path must be a regular file")
	}
	if err == nil {
		err = file.Chmod(0600)
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}
