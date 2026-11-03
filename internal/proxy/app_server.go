package proxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

type appServerEvent struct {
	source string
	frame  rpcRaw
	err    error
	job    *appAdmission
	lease  *Lease
	cfg    Config
	state  *stateLease
}

func runAppServer(opts Options, startup appServerStartup, requestID string, onStart func(int, bool)) int {
	launchStarted := time.Now()
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
	home, err := opts.AccountHome()
	if err != nil {
		fmt.Fprintln(opts.Stderr, "codex-proxy app-server:", stageErrorText("cannot resolve system account", err))
		return 2
	}
	if !filepath.IsAbs(home) {
		fmt.Fprintln(opts.Stderr, "codex-proxy app-server: cannot resolve system account: home directory is not absolute:", home)
		return 2
	}
	dir := filepath.Join(home, ".codex-proxy")
	if err := ensurePrivateDir(dir); err != nil {
		fmt.Fprintln(opts.Stderr, "codex-proxy app-server:", stageErrorText("cannot open state directory", err))
		return 2
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(opts.Stderr, "codex-proxy app-server:", stageErrorText("cannot resolve proxy executable", err))
		return 2
	}
	load := func() (Config, error) { return LoadConfig(dir, self) }
	logger := &appLogger{Logger: newNamedLogger(dir, appServerLogFilename, requestID, opts.Stderr, load), cwd: startup.Cwd, user: codexHomeUser(environmentValue(startup.Env, "CODEX_HOME"))}
	fail := func(stage string, cause error) int {
		reason := stageErrorText(stage, cause)
		logger.Log("error", map[string]any{"stage": stage, "error": reason})
		fmt.Fprintln(opts.Stderr, "codex-proxy app-server:", reason)
		return 2
	}
	// Closing an already closed descriptor is the only expected close error.
	// Diagnostics never override the startup or exit status.
	closeLog := func(stage string, err error) {
		if err != nil && !errors.Is(err, os.ErrClosed) {
			logger.Log("error", map[string]any{"stage": stage, "error": err.Error()})
		}
	}
	spool := newRPCSpool(dir)
	defer spool.close()
	if !filepath.IsAbs(startup.Cwd) {
		return fail("invalid app-server working directory", fmt.Errorf("cwd %q is not absolute", startup.Cwd))
	}
	mode, err := appServerMode(startup.Args)
	if err != nil || mode != appServerStdio {
		if err == nil {
			err = fmt.Errorf("unsupported app-server mode %d", mode)
		}
		return fail("invalid stdio launch arguments", err)
	}
	cfg, err := load()
	if err != nil {
		return fail("invalid configuration", err)
	}
	stateStore := newStateStore(dir, cfg, opts.PollInterval, opts.taskInterval)
	pool, err := newAuthPool(cfg, environmentValue(startup.Env, "CODEX_HOME"), stateStore)
	if err != nil {
		return fail("invalid account configuration", err)
	}
	var authHome *os.File
	defer func() {
		if authHome != nil {
			closeLog("close authentication reader", releaseAuthHome(authHome))
		}
	}()
	if pool != nil {
		// Do not launch a native process or reserve a task slot when exhausted.
		authHome, err = pool.acquireHome(context.Background(), cfg.WaitTimeout)
		if errors.Is(err, errNoAuthAccount) {
			fmt.Fprintln(opts.Stderr, "codex-proxy app-server:", err)
			// Startup has no client request id yet. Send a transport-level
			// failure and exit immediately, even if the caller sends no input.
			if writeErr := writeRPC(opts.Stdout, rpcMessage{"id": rpcValue(nil), "error": rpcValue(map[string]any{"code": 429, "message": err.Error()})}); writeErr != nil {
				logger.Log("error", map[string]any{"stage": "account_rejection", "error": writeErr.Error()})
			}
			return authLimitExitCode
		}
		if err != nil {
			return fail("cannot pin Codex account", err)
		}
	}
	launch, err := prepareAppServerLaunch(startup, dir)
	if err != nil {
		return fail("invalid app-server sandbox configuration", err)
	}
	if opts.launchOverride != nil {
		launch = opts.launchOverride(launch)
	}
	if pool != nil {
		launch.args = authFileArgs(launch.args)
		launch, err = authLaunch(launch, pool)
		if err != nil {
			return fail("invalid account sandbox configuration", err)
		}
	}
	// Install handlers before spawning the child, including the interval spent
	// starting the server and writing its first log record.
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	cmd := launch.command(cfg.RealBin)
	cmd.Dir, cmd.Env, cmd.Stderr = startup.Cwd, startup.Env, opts.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if authHome != nil {
		// Protect idle native processes too if the proxy is killed abruptly.
		cmd.ExtraFiles = append(cmd.ExtraFiles, authHome)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fail("create child input", err)
	}
	defer func() { closeLog("close child input pipe", stdin.Close()) }()
	stdout, childOutput, err := os.Pipe()
	if err != nil {
		return fail("create child output", err)
	}
	defer func() { closeLog("close child output pipe", stdout.Close()) }()
	cmd.Stdout = childOutput
	if err := cmd.Start(); err != nil {
		closeLog("close child output after failed start", childOutput.Close())
		return fail("start real app-server", err)
	}
	closeLog("close child output", childOutput.Close())
	started := time.Now()
	logger.childPID = cmd.Process.Pid
	if pool != nil && pool.active() != "" {
		logger.Log("auth_selected", map[string]any{"auth_file": pool.active()})
	}
	// sandboxed reports wrapper use; inherited false is accepted in prepareSandboxLaunch.
	logger.Log("started", map[string]any{"sandboxed": launch.profile != "", "limited": false, "wait_ms": started.Sub(launchStarted).Milliseconds(), "limit": cfg.MaxConcurrency, "wait_timeout_ms": cfg.WaitTimeout.Milliseconds(), "idle_timeout_ms": cfg.AppServerIdleTimeout.Milliseconds()})
	if onStart != nil {
		onStart(cmd.Process.Pid, launch.profile != "")
	}
	nativeWait := waitAppNative(cmd, logger)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The client reader and worker deliveries follow the protocol lifetime.
	// Native output needs its own handle: canceling it together with the client
	// would discard pipe/reader bytes that already hold a 429, and the
	// shutdown drain must observe the tail native wrote before it stopped.
	// serverDone reports that the reader has left the pipe.
	serverCtx, cancelServer := context.WithCancel(context.Background())
	defer cancelServer()
	events := make(chan appServerEvent, 32)
	go readAppFrames(ctx, opts.Stdin, spool, events, "client")
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		readAppFrames(serverCtx, stdout, spool, events, "server")
	}()
	session := newAppSession(ctx, dir, load, opts.PollInterval, logger, rpcCheckedWriter{stdin, spool}, rpcCheckedWriter{opts.Stdout, spool}, events)
	session.spool = spool
	session.stateStore, session.authPool = stateStore, pool
	session.authHome = authHome
	authHome = nil // session owns the reader until native shutdown completes.
	session.childPID, session.selfBin = cmd.Process.Pid, self
	exitCode := 0
	normalShutdown := false
	idle := newAppServerIdleTimer(cfg.AppServerIdleTimeout)
	defer idle.stop()
	loop := true
	for loop {
		select {
		case <-idle.channel():
			// Pending input already read from either pipe takes precedence.
			if len(events) > 0 {
				idle.update(true, session.busy())
				continue
			}
			normalShutdown = true
			logger.Log("shutdown", map[string]any{"reason": "idle_timeout", "idle_ms": cfg.AppServerIdleTimeout.Milliseconds()})
			loop = false
		case sig := <-signals:
			exitCode = 128 + int(sig.(syscall.Signal))
			logger.Log("shutdown", map[string]any{"reason": "signal", "signal": int(sig.(syscall.Signal))})
			loop = false
		case event := <-events:
			if event.source == "client" && event.frame == nil {
				normalShutdown = true
				if event.err != nil {
					exitCode = fail("read client message failed", event.err)
				}
				logger.Log("shutdown", map[string]any{"reason": "client_closed"})
				loop = false
			} else if event.source == "server" && event.frame == nil {
				if event.err != nil {
					exitCode = fail("read child message failed", event.err)
				}
				logger.Log("shutdown", map[string]any{"reason": "child_closed"})
				loop = false
			} else if err := session.handle(event); err != nil {
				exitCode = fail("protocol forwarding failed", err)
				loop = false
			}
			if session.authLimited {
				exitCode = authLimitExitCode
				fmt.Fprintln(opts.Stderr, "codex-proxy app-server: 429: Codex account rate limited; caller must restart")
				logger.Log("shutdown", map[string]any{"reason": "account_429", "auth_file": pool.active()})
				loop = false
			}
			// Client queries such as thread/loaded/list refresh the idle window,
			// even when no threads are loaded and no model task is running.
			idle.update(event.source == "client", session.busy())
		}
	}
	cancel()
	stopAppNative(cmd, stdin, nativeWait, logger)
	waitErr := nativeWait.err
	// nativeWait.done is closed after cmd.Wait, so waitErr is safe to read here.
	// A *exec.ExitError is an ordinary native exit (whatever the shutdown
	// reason) and keeps its existing exit-code handling. Any other Wait error
	// is a real system failure regardless of how shutdown started, so it is
	// always diagnosed; an existing non-zero exit code is preserved.
	var exited *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exited) {
		reason := stageErrorText("wait for real app-server", waitErr)
		logger.Log("error", map[string]any{"stage": "wait for real app-server", "error": reason})
		fmt.Fprintln(opts.Stderr, "codex-proxy app-server:", reason)
		if exitCode == 0 {
			exitCode = 2
		}
	}
	// All wait workers must return before releasing their reservations. An
	// acquired but undelivered lease is closed by its worker on cancellation.
	session.workers.Wait()
	// Native has stopped, but its output reader may still hold a second 429 for
	// another running thread in the pipe, the input buffer, or this channel.
	// Observe that tail before releasing the auth reader and rotating, or the
	// longest cooldown could be lost. The wait is bounded so a background
	// process inheriting the pipe cannot pin shutdown.
	observeDrained := func(event appServerEvent) {
		defer spool.release(event.frame)
		if event.lease != nil {
			if err := event.lease.Close(); err != nil {
				logger.Log("error", map[string]any{"stage": "close undelivered task slot", "error": err.Error()})
			}
		}
		if event.state != nil {
			// Release before authRecords.Wait: this event owns state.lock and
			// would otherwise block its own cooldown writer.
			if err := event.state.Close(); err != nil {
				logger.Log("error", map[string]any{"stage": "close undelivered state lease", "error": err.Error()})
			}
		}
		if event.source != "server" || event.frame == nil {
			return
		}
		// Observe only: never s.server/handle here, so the drain starts no new
		// task, runs no policy RPC, and never writes to a disconnected client.
		message, err := decodeRPC(event.frame)
		if err != nil {
			return
		}
		limited := session.authLimited
		session.observeAuthMessage(message)
		if session.authLimited && !limited {
			// A 429 first observed during the drain still reports the documented
			// rate-limit exit and diagnostic, exactly like the protocol loop.
			exitCode = authLimitExitCode
			fmt.Fprintln(opts.Stderr, "codex-proxy app-server: 429: Codex account rate limited; caller must restart")
			logger.Log("shutdown", map[string]any{"reason": "account_429", "auth_file": pool.active(), "stage": "drain"})
		}
	}
	stopServerReader := func() {
		cancelServer()
		// stdout is the read end of os.Pipe: a pollable descriptor. Closing it
		// cancels pending I/O, and the deadline unblocks a reader that a
		// detached background process would otherwise pin behind an open write
		// end. The join is real; the drain above stays bounded by its timer.
		closeLog("expire child output before close", stdout.SetReadDeadline(time.Now()))
		closeLog("close child output during shutdown drain", stdout.Close())
		<-serverDone
	}
	spool.setReadDeadline(time.Now().Add(500 * time.Millisecond))
	drain := time.NewTimer(500 * time.Millisecond)
	defer drain.Stop()
	readerDone := false
drainLoop:
	for {
		if readerDone {
			// The reader can no longer produce, so the queue empties quickly.
			for {
				select {
				case event := <-events:
					observeDrained(event)
				default:
					break drainLoop
				}
			}
		}
		select {
		case event := <-events:
			observeDrained(event)
		case <-serverDone:
			readerDone = true
		case <-drain.C:
			stopServerReader()
			readerDone = true
		}
	}
	session.authRecords.Wait()
	session.close() // Native has stopped; release task slots and the auth reader.
	if session.authLimited {
		locked, name, err := pool.rotate()
		if err != nil && !errors.Is(err, errNoAuthAccount) {
			logger.Log("error", map[string]any{"stage": "account_rotation", "error": err.Error()})
		} else {
			logger.Log("auth_rotation", map[string]any{"writer_acquired": locked, "auth_file": name})
		}
	}
	if exitCode == 0 && !normalShutdown && cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
		if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			// EOF and idle expiry are normal shutdown even when we terminate
			// the child. A child-initiated failure preserves the signal exit.
			exitCode = 128 + int(status.Signal())
		}
	}
	logger.Log("finished", map[string]any{"child_pid": cmd.Process.Pid, "run_ms": time.Since(started).Milliseconds(), "exit_code": exitCode})
	return exitCode
}
