package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestLimiterProcessHelper(t *testing.T) {
	if os.Getenv("CODEX_PROXY_LIMITER_TEST_HELPER") != "1" {
		return
	}
	file, err := os.OpenFile(os.Getenv("CODEX_PROXY_LIMITER_TEST_LOCK"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(os.Stdout, "locked")
	bufio.NewReader(os.Stdin).ReadString('\n')
}

func startLockHolder(t *testing.T, path string) func() {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	command := exec.CommandContext(ctx, executable, "-test.run=^TestLimiterProcessHelper$")
	command.Env = append(os.Environ(), "CODEX_PROXY_LIMITER_TEST_HELPER=1", "CODEX_PROXY_LIMITER_TEST_LOCK="+path)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdin, err := command.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			io.WriteString(stdin, "release\n")
			stdin.Close()
			if err := command.Wait(); err != nil {
				t.Errorf("lock helper failed: %v; stderr: %s", err, stderr.String())
			}
			cancel()
		})
	}
	t.Cleanup(release)
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "locked\n" {
		release()
		t.Fatalf("lock helper not ready: %q, %v; stderr: %s", line, err, stderr.String())
	}
	return release
}

func testLimiter(dir string, cap int, timeout time.Duration) *Limiter {
	return &Limiter{
		Dir:          dir,
		PollInterval: 5 * time.Millisecond,
		Load: func() (Config, error) {
			return Config{MaxConcurrency: cap, WaitTimeout: timeout}, nil
		},
	}
}

type acquireResult struct {
	lease *Lease
	cfg   Config
	err   error
}

func acquireAsync(l *Limiter, ctx context.Context) <-chan acquireResult {
	result := make(chan acquireResult, 1)
	go func() {
		lease, cfg, err := l.Acquire(ctx)
		result <- acquireResult{lease: lease, cfg: cfg, err: err}
	}()
	return result
}

func receiveAcquisition(t *testing.T, results <-chan acquireResult) acquireResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(time.Second):
		t.Fatal("acquisition did not finish")
		return acquireResult{}
	}
}

func requireWaiting(t *testing.T, results <-chan acquireResult) {
	t.Helper()
	select {
	case result := <-results:
		if result.lease != nil {
			result.lease.Close()
		}
		t.Fatalf("acquisition finished while capacity was occupied: %v", result.err)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestLimiterCrossProcessSlot(t *testing.T) {
	dir := t.TempDir()
	release := startLockHolder(t, filepath.Join(dir, "locks", "slot-8.lock"))
	limiter := testLimiter(dir, 1, time.Second)
	waited := make(chan struct{}, 1)
	var callbacks atomic.Int64
	limiter.OnWait = func(cfg Config, elapsed time.Duration) {
		callbacks.Add(1)
		if cfg.MaxConcurrency != 1 || elapsed < 0 {
			t.Errorf("unexpected wait callback: %+v, %v", cfg, elapsed)
		}
		waited <- struct{}{}
	}
	results := acquireAsync(limiter, context.Background())
	select {
	case <-waited:
	case <-time.After(time.Second):
		t.Fatal("wait callback not invoked")
	}
	requireWaiting(t, results)
	release()
	result := receiveAcquisition(t, results)
	if result.err != nil {
		t.Fatal(result.err)
	}
	defer result.lease.Close()
	if result.cfg.MaxConcurrency != 1 || callbacks.Load() != 1 {
		t.Fatalf("unexpected config/callback count: %+v, %d", result.cfg, callbacks.Load())
	}
	if filepath.Base(result.lease.File().Name()) != "slot-8.lock" {
		t.Fatalf("existing free slot was not reused: %s", result.lease.File().Name())
	}
}

func TestLimiterDynamicCapDrains(t *testing.T) {
	dir := t.TempDir()
	var cap atomic.Int64
	cap.Store(2)
	limiter := testLimiter(dir, 2, time.Second)
	limiter.Load = func() (Config, error) {
		return Config{MaxConcurrency: int(cap.Load()), WaitTimeout: time.Second}, nil
	}
	first, _, err := limiter.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, _, err := limiter.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	cap.Store(1)
	results := acquireAsync(limiter, context.Background())
	requireWaiting(t, results)
	first.Close()
	requireWaiting(t, results)
	second.Close()
	result := receiveAcquisition(t, results)
	if result.err != nil {
		t.Fatal(result.err)
	}
	defer result.lease.Close()
	if result.cfg.MaxConcurrency != 1 {
		t.Fatalf("admitted with stale cap: %+v", result.cfg)
	}
}

func TestLimiterDynamicCapIncrease(t *testing.T) {
	dir := t.TempDir()
	var cap atomic.Int64
	cap.Store(1)
	limiter := testLimiter(dir, 1, time.Second)
	limiter.Load = func() (Config, error) {
		return Config{MaxConcurrency: int(cap.Load()), WaitTimeout: time.Second}, nil
	}
	first, _, err := limiter.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	results := acquireAsync(limiter, context.Background())
	requireWaiting(t, results)
	cap.Store(2)
	result := receiveAcquisition(t, results)
	if result.err != nil {
		t.Fatal(result.err)
	}
	defer result.lease.Close()
	if result.cfg.MaxConcurrency != 2 || first.File().Name() == result.lease.File().Name() {
		t.Fatalf("cap increase did not reserve a distinct slot: %+v", result.cfg)
	}
}

func TestLimiterReloadsInsideAdmission(t *testing.T) {
	dir := t.TempDir()
	startLockHolder(t, filepath.Join(dir, "locks", "slot-0.lock"))
	var calls atomic.Int64
	limiter := testLimiter(dir, 2, 60*time.Millisecond)
	limiter.Load = func() (Config, error) {
		cap := 2
		if calls.Add(1)%2 == 0 {
			cap = 1
		}
		return Config{MaxConcurrency: cap, WaitTimeout: 60 * time.Millisecond}, nil
	}
	lease, _, err := limiter.Acquire(context.Background())
	if lease != nil {
		lease.Close()
	}
	if !errors.Is(err, ErrWaitTimeout) || calls.Load() < 2 {
		t.Fatalf("gate used outside config: calls=%d, err=%v", calls.Load(), err)
	}
}

func TestLimiterTimeoutIncludesGateWait(t *testing.T) {
	dir := t.TempDir()
	startLockHolder(t, filepath.Join(dir, "locks", "admission.lock"))
	limiter := testLimiter(dir, 1, 60*time.Millisecond)
	started := time.Now()
	lease, _, err := limiter.Acquire(context.Background())
	if lease != nil {
		lease.Close()
	}
	if !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("expected gate timeout, got %v", err)
	}
	if elapsed := time.Since(started); elapsed < 60*time.Millisecond || elapsed > time.Second {
		t.Fatalf("timeout reset or returned early: %v", elapsed)
	}
}

func TestLimiterCancellation(t *testing.T) {
	for _, name := range []string{"slot-0.lock", "admission.lock"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			startLockHolder(t, filepath.Join(dir, "locks", name))
			limiter := testLimiter(dir, 1, 0)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			limiter.OnWait = func(Config, time.Duration) { cancel() }
			lease, _, err := limiter.Acquire(ctx)
			if lease != nil {
				lease.Close()
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("expected cancellation, got %v", err)
			}
		})
	}
}

func TestLeaseSharedDescriptorCleanup(t *testing.T) {
	for _, explicitRelease := range []bool{false, true} {
		t.Run(fmt.Sprintf("release=%v", explicitRelease), func(t *testing.T) {
			dir := t.TempDir()
			limiter := testLimiter(dir, 1, 40*time.Millisecond)
			lease, _, err := limiter.Acquire(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			fd, err := syscall.Dup(int(lease.File().Fd()))
			if err != nil {
				t.Fatal(err)
			}
			duplicate := os.NewFile(uintptr(fd), "inherited-slot")
			defer duplicate.Close()
			if explicitRelease {
				err = lease.Release()
			} else {
				err = lease.Close()
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := lease.Close(); err != nil {
				t.Fatalf("close not idempotent: %v", err)
			}
			next, _, err := limiter.Acquire(context.Background())
			if next != nil {
				defer next.Close()
			}
			if explicitRelease && err != nil {
				t.Fatalf("explicit release left inherited lock occupied: %v", err)
			}
			if !explicitRelease && !errors.Is(err, ErrWaitTimeout) {
				t.Fatalf("close unlocked inherited descriptor: %v", err)
			}
		})
	}
}

func TestLimiterConfigFailureDoesNotReserve(t *testing.T) {
	dir := t.TempDir()
	problem := errors.New("invalid edited configuration")
	limiter := testLimiter(dir, 1, time.Second)
	limiter.Load = func() (Config, error) { return Config{}, problem }
	lease, _, err := limiter.Acquire(context.Background())
	if lease != nil || !errors.Is(err, problem) {
		t.Fatalf("expected config failure without lease: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "locks"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("bad config reserved a file: %v", entries)
	}
}
