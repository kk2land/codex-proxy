package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

type appServerEvent struct {
	source string
	line   []byte
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
	scanner := appScanner(opts.Stdin)
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
	launch, err := prepareAppServerLaunch(startup)
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
	logger.Log("started", map[string]any{"sandboxed": launch.profile != "", "limited": false, "wait_ms": started.Sub(launchStarted).Milliseconds(), "limit": cfg.MaxConcurrency, "wait_timeout_ms": cfg.WaitTimeout.Milliseconds(), "idle_timeout_ms": cfg.AppServerIdleTimeout.Milliseconds()})
	if onStart != nil {
		onStart(cmd.Process.Pid, launch.profile != "")
	}
	nativeWait := waitAppNative(cmd, logger)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan appServerEvent, 32)
	go readAppLines(ctx, scanner, events, "client")
	go readAppLines(ctx, appScanner(stdout), events, "server")
	session := newAppSession(ctx, dir, load, opts.PollInterval, logger, stdin, opts.Stdout, events)
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
			if event.source == "client" && event.line == nil {
				normalShutdown = true
				if event.err != nil {
					exitCode = fail("client message exceeds limit or input failed", event.err)
				}
				logger.Log("shutdown", map[string]any{"reason": "client_closed"})
				loop = false
			} else if event.source == "server" && event.line == nil {
				if event.err != nil {
					exitCode = fail("child message exceeds limit or output failed", event.err)
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
	for {
		select {
		case event := <-events:
			if event.lease != nil {
				if err := event.lease.Close(); err != nil {
					logger.Log("error", map[string]any{"stage": "close undelivered task slot", "error": err.Error()})
				}
			}
			if event.state != nil {
				if err := event.state.Close(); err != nil {
					logger.Log("error", map[string]any{"stage": "close undelivered state lease", "error": err.Error()})
				}
			}
		default:
			goto drained
		}
	}
drained:
	// A state-ready event may own state.lock. Drain those leases before waiting
	// for independent cooldown writers, or shutdown could block its own record.
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

func appScanner(reader io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), maxAppServerMessage)
	return scanner
}

func readAppLines(ctx context.Context, scanner *bufio.Scanner, events chan<- appServerEvent, source string) {
	for scanner.Scan() {
		line := append([]byte{}, scanner.Bytes()...)
		select {
		case events <- appServerEvent{source: source, line: line}:
		case <-ctx.Done():
			return
		}
	}
	select {
	case events <- appServerEvent{source: source, err: scanner.Err()}:
	case <-ctx.Done():
	}
}

type rpcMessage map[string]json.RawMessage

func decodeRPC(line []byte) (rpcMessage, error) {
	var message rpcMessage
	if err := json.Unmarshal(line, &message); err != nil {
		return nil, fmt.Errorf("invalid JSON-RPC message: %w", err)
	}
	if message == nil {
		return nil, errors.New("invalid JSON-RPC message: null object")
	}
	return message, nil
}

func rpcString(message rpcMessage, field string) string {
	var result string
	_ = json.Unmarshal(message[field], &result)
	return result
}

func rpcObject(message rpcMessage, field string) rpcMessage {
	var result rpcMessage
	_ = json.Unmarshal(message[field], &result)
	if result == nil {
		result = make(rpcMessage)
	}
	return result
}

func rpcValue(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}

func writeRPC(writer io.Writer, message rpcMessage) error {
	data, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode JSON-RPC message: %w", err)
	}
	data = append(data, '\n')
	n, err := writer.Write(data)
	if err != nil {
		return fmt.Errorf("write JSON-RPC message: %w", err)
	}
	if n != len(data) {
		return fmt.Errorf("write JSON-RPC message: %w", io.ErrShortWrite)
	}
	return nil
}
