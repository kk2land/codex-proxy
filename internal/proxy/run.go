package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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
	AppServerBin   string                      // Tests may supply the separately built service binary.
	launchOverride func(launchSpec) launchSpec // Package-private test injection.
	taskInterval   func() int64                // Package-private test injection; production always uses 3–15s.
}

func Run(args []string, opts Options) int {
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
	launch, err := prepareLaunch(kind, args)
	if err != nil {
		return fail(err)
	}
	if opts.launchOverride != nil {
		launch = opts.launchOverride(launch)
	}
	loggedArgs = logArgs(launch.args)
	var startup []byte
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
			startup, err = encodeAppServerStartup(appServerStartup{Version: 1, Args: args, Cwd: cwd, Env: os.Environ(), CallerBin: self})
			if err != nil {
				return fail(err)
			}
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
		defer lease.Close() // Never unlock a child that may still be alive.
	}
	cmd := launch.command(cfg.RealBin)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = opts.Stdin, opts.Stdout, opts.Stderr
	var appInput, appWriter *os.File
	if kind == CommandAppServer {
		service := opts.AppServerBin
		if service == "" {
			service = filepath.Join(filepath.Dir(self), "codex-app-server-proxy")
		}
		if err := validateNativeBinary(service, self); err != nil {
			return fail(errors.New("codex-app-server-proxy must be a native binary installed beside fake codex"))
		}
		cmd = exec.Command(service)
		appInput, appWriter, err = os.Pipe()
		if err != nil {
			return fail(errors.New("cannot create app-server startup pipe"))
		}
		defer appInput.Close()
		defer appWriter.Close()
		cmd.Stdin = appInput
		cmd.Stdout, cmd.Stderr = opts.Stdout, opts.Stderr
	}
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: group}
	if lease != nil {
		cmd.ExtraFiles = []*os.File{lease.File()}
	}
	var state *stateLease
	if kind == CommandExec {
		gate := newStateStore(dir, opts.PollInterval, opts.taskInterval)
		state, err = gate.Acquire(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return canceled()
			}
			return fail(err)
		}
		defer state.Close()
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
	}
	processMu.Unlock()
	if state != nil {
		state.Close()
	}
	if err != nil {
		return fail(fmt.Errorf("start real Codex: %w", err))
	}
	if appInput != nil {
		appInput.Close()
		// exec.Cmd must not own this copy goroutine: a connected client may
		// keep stdin open after the service exits, which would block Wait.
		go func() {
			_, _ = io.Copy(appWriter, io.MultiReader(bytes.NewReader(startup), opts.Stdin))
			appWriter.Close()
		}()
	}
	started := time.Now()
	log("started", map[string]any{"child_pid": cmd.Process.Pid, "wait_ms": started.Sub(waitStarted).Milliseconds(), "limited": lease != nil, "sandboxed": launch.profile != ""})
	waitErr := cmd.Wait()
	processMu.Lock()
	process = nil
	processMu.Unlock()
	if cmd.ProcessState == nil {
		return fail(fmt.Errorf("wait for real Codex: %w", waitErr))
	}
	exitCode := cmd.ProcessState.ExitCode()
	var exitSignal syscall.Signal
	if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		exitSignal = status.Signal()
		exitCode = 128 + int(exitSignal)
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
