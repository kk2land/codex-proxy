package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Options allows tests to supply an account resolver without a production
// environment-variable escape hatch that could split a user's shared quota.
type Options struct {
	AccountHome    func() (string, error)
	Stdin          *os.File
	Stdout         *os.File
	Stderr         *os.File
	PollInterval   time.Duration
	launchOverride func(launchSpec) launchSpec // Package-private test injection.
	taskInterval   func() int64                // Package-private test injection; production always uses 3–15s.
}

func Run(args []string, opts Options) int {
	if len(args) == 2 && args[0] == "--proxy-app-server-guard" {
		pid, err := strconv.Atoi(args[1])
		if err != nil {
			return 2
		}
		return runAppServerGuard(pid)
	}
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	if opts.AccountHome == nil {
		opts.AccountHome = SystemHome
	}
	kind, classifyErr := ClassifyArgs(args)
	home, err := opts.AccountHome()
	if err != nil {
		fmt.Fprintln(opts.Stderr, "codex-proxy:", err)
		if kind == CommandAppServer {
			return rejectAppServer(opts.Stderr)
		}
		return 2
	}
	if !filepath.IsAbs(home) {
		fmt.Fprintln(opts.Stderr, "codex-proxy: account home must be absolute")
		return 2
	}
	dir := filepath.Join(home, ".codex-proxy")
	if err := ensurePrivateDir(dir); err != nil {
		fmt.Fprintln(opts.Stderr, "codex-proxy:", err)
		if kind == CommandAppServer {
			return rejectAppServer(opts.Stderr)
		}
		return 2
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(opts.Stderr, "codex-proxy:", err)
		return 2
	}
	load := func() (Config, error) { return LoadConfig(dir, self) }
	logger := NewLogger(dir, newRequestID(), opts.Stderr, load)
	loggedArgs := logArgs(args)
	loggedUser := codexHomeUser(os.Getenv("CODEX_HOME"))
	log := func(event string, fields map[string]any) {
		fields["args"] = loggedArgs
		fields["user"] = loggedUser
		logger.Log(event, fields)
	}
	fail := func(err error) int {
		log("error", map[string]any{"error": err.Error()})
		fmt.Fprintln(opts.Stderr, "codex-proxy:", err)
		if errors.Is(err, errNoAuthAccount) {
			return authLimitExitCode
		}
		return 2
	}
	if classifyErr != nil {
		return fail(classifyErr)
	}
	var cfg Config
	if kind == CommandAppServer {
		cfg, err = loadAppServerConfig(dir, self)
	} else {
		cfg, err = load()
	}
	if err != nil {
		return fail(err)
	}
	if kind == CommandAppServer && !cfg.AppServerEnabled {
		log("rejected", map[string]any{"reason": "app-server is not supported"})
		return rejectAppServer(opts.Stderr)
	}
	launch, err := prepareLaunch(kind, args, dir)
	if err != nil {
		return fail(err)
	}
	if opts.launchOverride != nil {
		launch = opts.launchOverride(launch)
	}
	loggedArgs = logArgs(launch.args)
	if kind == CommandAppServer {
		mode, modeErr := appServerMode(args)
		if modeErr != nil {
			return fail(modeErr)
		}
		if mode == appServerTooling {
			kind = CommandPassThrough
		} else {
			cwd, cwdErr := os.Getwd()
			if cwdErr != nil {
				return fail(errors.New("cannot resolve app-server working directory"))
			}
			// Own the native stdio child in this process. No companion binary
			// or daemon is needed for protocol admission or lifecycle control.
			started := time.Now()
			childPID := 0
			code := runAppServer(opts, appServerStartup{Args: args, Cwd: cwd, Env: os.Environ()}, logger.requestID, func(pid int, sandboxed bool) {
				childPID, started = pid, time.Now()
				log("started", map[string]any{"child_pid": pid, "limited": false, "sandboxed": sandboxed})
			})
			if childPID != 0 {
				log("finished", map[string]any{"child_pid": childPID, "run_ms": time.Since(started).Milliseconds(), "exit_code": code})
			}
			return code
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 16)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	stopSignals := make(chan struct{})
	signalsStopped := make(chan struct{})
	var processMu sync.Mutex
	var process *os.Process
	var received syscall.Signal
	group := !isTerminal(opts.Stdin.Fd())
	go func() {
		defer close(signalsStopped)
		for {
			select {
			case <-stopSignals:
				return
			case value := <-signals:
				sig := value.(syscall.Signal)
				processMu.Lock()
				if received == 0 {
					received = sig
				}
				cancel()
				if process != nil {
					if group {
						_ = syscall.Kill(-process.Pid, sig)
					} else {
						_ = process.Signal(sig)
					}
				}
				processMu.Unlock()
			}
		}
	}()
	defer func() {
		signal.Stop(signals)
		close(stopSignals)
		<-signalsStopped
	}()
	canceled := func() int {
		processMu.Lock()
		sig := received
		processMu.Unlock()
		log("canceled", map[string]any{"signal": int(sig)})
		if sig == 0 {
			return 2
		}
		return 128 + int(sig)
	}
	waitStarted := time.Now()
	// Freeze state paths for this invocation, independently of hot-reloaded
	// admission settings. Changing paths requires restarting all proxies.
	stateGate := newStateStore(dir, cfg, opts.PollInterval, opts.taskInterval)
	var pool *authPool
	var authHome *os.File
	var authWait time.Duration
	authChildStarted, authChildReaped := false, false
	authRotate := false
	if kind == CommandExec {
		pool, err = newAuthPool(cfg, os.Getenv("CODEX_HOME"), stateGate)
		if err != nil {
			return fail(err)
		}
		if pool != nil {
			// Reject exhausted pools before either slot or submission sleep.
			if _, err := pool.choose(); err != nil {
				return fail(err)
			}
			// Keep the deadline sampled before the limiter can reload
			// configuration while this invocation waits for a slot.
			authWait = cfg.WaitTimeout
			launch.args = authFileArgs(launch.args)
		}
	}
	var lease *Lease
	if kind == CommandExec {
		limiter := Limiter{Dir: dir, Load: load, PollInterval: opts.PollInterval, OnWait: func(cfg Config, elapsed time.Duration) {
			log("waiting", map[string]any{"limit": cfg.MaxConcurrency, "wait_ms": elapsed.Milliseconds()})
		}}
		lease, cfg, err = limiter.Acquire(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return canceled()
			}
			if errors.Is(err, ErrWaitTimeout) {
				log("timeout", map[string]any{"wait_ms": time.Since(waitStarted).Milliseconds()})
				fmt.Fprintln(opts.Stderr, "codex-proxy:", err)
				return 124
			}
			return fail(err)
		}
		defer func() {
			// Never unlock a child that may still be alive; a close failure is
			// diagnostic only and must not change the exit status.
			if err := lease.Close(); err != nil {
				fmt.Fprintln(opts.Stderr, "codex-proxy: close slot lease:", err)
				log("error", map[string]any{"error": "close slot lease: " + err.Error()})
			}
		}()
		if pool != nil {
			// Acquire the shared authentication reader only after the slot is
			// ours. A process still waiting for admission must never pin
			// CODEX_HOME credentials and block an exiting owner from rotating.
			homeLease, err := pool.acquireHome(ctx, authWait)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return canceled()
				}
				if errors.Is(err, ErrWaitTimeout) {
					log("timeout", map[string]any{"wait_ms": time.Since(waitStarted).Milliseconds()})
					fmt.Fprintln(opts.Stderr, "codex-proxy:", err)
					return 124
				}
				return fail(err)
			}
			authHome = homeLease
			defer func() {
				if !authChildStarted || authChildReaped {
					if err := releaseAuthHome(homeLease); err != nil {
						log("error", map[string]any{"error": "release authentication reader: " + err.Error()})
						return
					}
					if authRotate {
						locked, name, err := pool.rotate()
						if err != nil && !errors.Is(err, errNoAuthAccount) {
							log("error", map[string]any{"error": "rotate Codex account: " + err.Error()})
						} else {
							log("auth_rotation", map[string]any{"writer_acquired": locked, "auth_file": name})
						}
					}
				} else {
					_ = homeLease.Close()
				}
			}()
		}
	}
	cmd := launch.command(cfg.RealBin)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = opts.Stdin, opts.Stdout, opts.Stderr
	cmd.Env = os.Environ()
	var outObserver, errObserver *authOutputObserver
	authLimited := make(chan struct{})
	var authOnce sync.Once
	var authWrites sync.WaitGroup
	recordAuth := func(limit authLimit) {
		name := pool.active()
		recordCtx, recordCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer recordCancel()
		if err := pool.record(recordCtx, name, limit); err != nil {
			fmt.Fprintln(opts.Stderr, "codex-proxy:", err)
			log("error", map[string]any{"auth_file": name, "error": err.Error()})
		} else {
			log("auth_rate_limited", map[string]any{"auth_file": name, "cooldown_until": limit.Until})
		}
	}
	if pool != nil {
		observe := func(limit authLimit) {
			pool.note(pool.active(), limit)
			authOnce.Do(func() {
				// Persist immediately without blocking output on the launch gate.
				authWrites.Add(1)
				go func() { defer authWrites.Done(); recordAuth(limit) }()
				close(authLimited)
			})
		}
		outObserver = &authOutputObserver{now: stateGate.now, onLimit: observe}
		errObserver = &authOutputObserver{now: stateGate.now, stderr: true, onLimit: observe}
		cmd.Stdout = io.MultiWriter(opts.Stdout, outObserver)
		cmd.Stderr = io.MultiWriter(opts.Stderr, errObserver)
		// Observation uses exec's copying pipes. Once native Codex exits,
		// background tools retaining those pipes must not pin its reservations.
		// WaitDelay closes the pipes and joins their writers before Wait returns.
		cmd.WaitDelay = 500 * time.Millisecond
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: group}
	if lease != nil {
		cmd.ExtraFiles = []*os.File{lease.File()}
	}
	if authHome != nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, authHome)
	}
	var state *stateLease
	if kind == CommandExec {
		state, err = stateGate.Acquire(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return canceled()
			}
			return fail(err)
		}
		defer func() {
			if err := state.Close(); err != nil {
				fmt.Fprintln(opts.Stderr, "codex-proxy: close state lease:", err)
				log("error", map[string]any{"error": "close state lease: " + err.Error()})
			}
		}()
		if pool != nil {
			if err := pool.checkCurrent(state.state); err != nil {
				authRotate = errors.Is(err, errNoAuthAccount)
				return fail(err)
			}
			launch, err = authLaunch(launch, pool)
			if err != nil {
				return fail(err)
			}
			authCmd := launch.command(cfg.RealBin)
			cmd.Path, cmd.Args = authCmd.Path, authCmd.Args
			log("auth_selected", map[string]any{"auth_file": pool.active()})
		}
	}
	processMu.Lock()
	if ctx.Err() != nil {
		processMu.Unlock()
		return canceled()
	}
	if state != nil {
		if err := state.Record(ctx); err != nil {
			processMu.Unlock()
			if errors.Is(err, context.Canceled) {
				return canceled()
			}
			return fail(err)
		}
	}
	err = cmd.Start()
	if err == nil {
		process = cmd.Process
		authChildStarted = true
	}
	processMu.Unlock()
	if state != nil {
		// A cancellation does not make an actual Close failure ignorable.
		if err := state.Close(); err != nil {
			log("error", map[string]any{"error": "close state lease: " + err.Error()})
		}
	}
	if err != nil {
		return fail(fmt.Errorf("start real Codex: %w", err))
	}
	started := time.Now()
	log("started", map[string]any{"child_pid": cmd.Process.Pid, "wait_ms": started.Sub(waitStarted).Milliseconds(), "limited": lease != nil, "sandboxed": launch.profile != ""})
	nativeDone := make(chan error, 1)
	go func() { nativeDone <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-nativeDone:
	case <-authLimited:
		// Stop native Codex before releasing its lifetime authentication reader.
		kill := func(sig syscall.Signal) {
			if group {
				_ = syscall.Kill(-cmd.Process.Pid, sig)
			} else {
				_ = cmd.Process.Signal(sig)
			}
		}
		kill(syscall.SIGTERM)
		select {
		case waitErr = <-nativeDone:
		case <-time.After(500 * time.Millisecond):
			kill(syscall.SIGKILL)
			waitErr = <-nativeDone
		}
		if group {
			kill(syscall.SIGKILL) // Also stop tools retaining inherited pipe/lock FDs.
		}
	}
	authChildReaped = cmd.ProcessState != nil
	processMu.Lock()
	process = nil
	processMu.Unlock()
	if pool != nil {
		a, b := outObserver.result(), errObserver.result()
		limit := authLimit{At: max(a.At, b.At), Until: max(a.Until, b.Until)}
		// Wait has drained both output writers, so no callback can add workers.
		authWrites.Wait()
		if limit.At != 0 {
			pool.note(pool.active(), limit)
			recordAuth(limit) // Merge any larger reset hint emitted during shutdown.
			authRotate = true
			fmt.Fprintln(opts.Stderr, "codex-proxy: 429: Codex account rate limited; caller must retry")
		}
	}
	if cmd.ProcessState == nil {
		return fail(fmt.Errorf("wait for real Codex: %w", waitErr))
	}
	// Preserve the native exit status even when Wait reports ErrWaitDelay
	// because a background tool kept an observed output pipe open.
	exitCode := cmd.ProcessState.ExitCode()
	var exitSignal syscall.Signal
	if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		exitSignal = status.Signal()
		exitCode = 128 + int(exitSignal)
	}
	if authRotate {
		exitCode = authLimitExitCode
	}
	if lease != nil {
		// Wait has confirmed the native process is gone. Unlock also frees FDs
		// inherited by background tools, which do not count as codex exec.
		if err := lease.Release(); err != nil {
			fmt.Fprintln(opts.Stderr, "codex-proxy: release slot:", err)
			log("error", map[string]any{"error": "release slot: " + err.Error()})
		}
	}
	log("finished", map[string]any{"child_pid": cmd.Process.Pid, "run_ms": time.Since(started).Milliseconds(), "exit_code": exitCode, "signal": int(exitSignal)})
	return exitCode
}

func rejectAppServer(stderr *os.File) int {
	fmt.Fprintln(stderr, "codex-proxy: app-server is not supported")
	return 1
}

func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("state directory must be a directory, not a symlink")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Getuid()) {
		return errors.New("state directory must be owned by the current UID")
	}
	return os.Chmod(dir, 0700)
}

func newRequestID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err == nil {
		return hex.EncodeToString(id[:])
	}
	return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
}
