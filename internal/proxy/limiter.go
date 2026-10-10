package proxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var ErrWaitTimeout = errors.New("timed out waiting for a codex exec slot")

// Limiter coordinates all invocations using the same private directory.
// OnWait runs once, outside the admission lock, when an acquisition must wait.
type Limiter struct {
	Dir          string
	PollInterval time.Duration
	Load         ConfigLoader
	OnWait       func(Config, time.Duration)
}

// Lease owns one locked file description. A native child can inherit File via
// exec.Cmd.ExtraFiles and keep the slot occupied if its supervisor dies.
type Lease struct {
	file   *os.File
	mu     sync.Mutex
	closed bool
}

func (l *Lease) File() *os.File {
	return l.file
}

// Close drops this descriptor without unlocking a descriptor held by a child.
func (l *Lease) Close() error {
	return l.close(false)
}

// Release unlocks all descriptors sharing this lock, then closes this one.
// Call it only after confirming that the native child has exited.
func (l *Lease) Release() error {
	return l.close(true)
}

func (l *Lease) close(unlock bool) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	var unlockErr error
	if unlock {
		for {
			unlockErr = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
			if !errors.Is(unlockErr, syscall.EINTR) {
				break
			}
		}
	}
	return errors.Join(unlockErr, l.file.Close())
}

func (l *Limiter) Acquire(ctx context.Context) (*Lease, Config, error) {
	started := time.Now()
	var cfg Config
	if l.Load == nil {
		return nil, cfg, errors.New("config loader is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, cfg, err
	}
	locksDir := filepath.Join(l.Dir, "locks")
	if err := os.MkdirAll(locksDir, 0700); err != nil {
		return nil, cfg, fmt.Errorf("create locks directory: %w", err)
	}
	poll := l.PollInterval
	if poll <= 0 {
		poll = 100 * time.Millisecond
	}
	waitReported := false
	for {
		var err error
		cfg, err = l.Load()
		if err != nil {
			return nil, cfg, err
		}
		if err := acquisitionError(ctx, cfg, started); err != nil {
			return nil, cfg, err
		}
		gate, err := os.OpenFile(filepath.Join(locksDir, "admission.lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, cfg, fmt.Errorf("open admission lock: %w", err)
		}
		locked, err := tryLock(ctx, gate)
		if err != nil {
			gate.Close()
			return nil, cfg, fmt.Errorf("lock admission: %w", err)
		}
		var lease *Lease
		if locked {
			lease, cfg, err = l.acquireUnderGate(ctx, locksDir, started)
		}
		closeErr := gate.Close()
		if err != nil || closeErr != nil {
			if lease != nil {
				lease.Close()
			}
			return nil, cfg, errors.Join(err, closeErr)
		}
		if lease != nil {
			return lease, cfg, nil
		}
		if !waitReported {
			waitReported = true
			if l.OnWait != nil {
				l.OnWait(cfg, time.Since(started))
			}
		}
		if err := acquisitionError(ctx, cfg, started); err != nil {
			return nil, cfg, err
		}
		delay := poll
		if cfg.WaitTimeout > 0 {
			remaining := cfg.WaitTimeout - time.Since(started)
			if remaining < delay {
				delay = remaining
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, cfg, ctx.Err()
		case <-timer.C:
		}
	}
}

func (l *Limiter) acquireUnderGate(ctx context.Context, locksDir string, started time.Time) (*Lease, Config, error) {
	cfg, err := l.Load()
	if err != nil {
		return nil, cfg, err
	}
	if err := acquisitionError(ctx, cfg, started); err != nil {
		return nil, cfg, err
	}
	lease, err := reserveSlot(ctx, locksDir, cfg.MaxConcurrency)
	if err != nil {
		return nil, cfg, err
	}
	if err := acquisitionError(ctx, cfg, started); err != nil {
		if lease != nil {
			lease.Close()
		}
		return nil, cfg, err
	}
	return lease, cfg, nil
}

func acquisitionError(ctx context.Context, cfg Config, started time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cfg.MaxConcurrency < 1 {
		return errors.New("maximum concurrency must be positive")
	}
	if cfg.WaitTimeout < 0 {
		return errors.New("wait timeout must not be negative")
	}
	if cfg.WaitTimeout > 0 && time.Since(started) >= cfg.WaitTimeout {
		return ErrWaitTimeout
	}
	return nil
}

// The caller holds admission.lock. Every historical slot counts, including
// slots whose number exceeds a newly lowered limit. Free candidates are held
// until this scan finishes so that reservation and counting are one decision.
func reserveSlot(ctx context.Context, locksDir string, limit int) (*Lease, error) {
	entries, err := os.ReadDir(locksDir)
	if err != nil {
		return nil, fmt.Errorf("read slots: %w", err)
	}
	var candidate *os.File
	defer func() {
		if candidate != nil {
			candidate.Close()
		}
	}()
	occupied := 0
	maxIndex := -1
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		index, ok := slotIndex(entry.Name())
		if !ok {
			continue
		}
		if index > maxIndex {
			maxIndex = index
		}
		file, err := os.OpenFile(filepath.Join(locksDir, entry.Name()), os.O_RDWR, 0)
		if err != nil {
			return nil, fmt.Errorf("open slot %s: %w", entry.Name(), err)
		}
		locked, err := tryLock(ctx, file)
		if err != nil {
			file.Close()
			return nil, fmt.Errorf("lock slot %s: %w", entry.Name(), err)
		}
		if !locked {
			occupied++
			file.Close()
		} else if candidate == nil {
			candidate = file
		} else {
			file.Close()
		}
	}
	if occupied >= limit {
		return nil, nil
	}
	if candidate == nil {
		if maxIndex == int(^uint(0)>>1) {
			return nil, errors.New("slot index overflow")
		}
		name := fmt.Sprintf("slot-%d.lock", maxIndex+1)
		candidate, err = os.OpenFile(filepath.Join(locksDir, name), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if err != nil {
			return nil, fmt.Errorf("create slot: %w", err)
		}
		locked, err := tryLock(ctx, candidate)
		if err != nil {
			return nil, fmt.Errorf("lock new slot: %w", err)
		}
		if !locked {
			return nil, errors.New("new slot was unexpectedly locked")
		}
	}
	lease := &Lease{file: candidate}
	candidate = nil
	return lease, nil
}

func slotIndex(name string) (int, bool) {
	if !strings.HasPrefix(name, "slot-") || !strings.HasSuffix(name, ".lock") {
		return 0, false
	}
	value := strings.TrimSuffix(strings.TrimPrefix(name, "slot-"), ".lock")
	index, err := strconv.Atoi(value)
	return index, err == nil && index >= 0 && value == strconv.Itoa(index)
}

func tryLock(ctx context.Context, file *os.File) (bool, error) {
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK), errors.Is(err, syscall.EAGAIN):
			return false, nil
		default:
			return false, err
		}
	}
}
