package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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

func runAppServer(opts Options, startup appServerStartup, onStart func(int, bool)) int {
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
	if err != nil || !filepath.IsAbs(home) {
		fmt.Fprintln(opts.Stderr, "codex-proxy app-server: cannot resolve system account")
		return 2
	}
	dir := filepath.Join(home, ".codex-proxy")
	if err := ensurePrivateDir(dir); err != nil {
		fmt.Fprintln(opts.Stderr, "codex-proxy app-server: cannot open state directory")
		return 2
	}
	self, err := os.Executable()
	if err != nil {
		return 2
	}
	load := func() (Config, error) { return LoadConfig(dir, self) }
	// File diagnostics may include filesystem paths, never protocol payloads.
	logger := newNamedLogger(dir, appServerLogFilename, newRequestID(), opts.Stderr, load)
	fail := func(stage string) int {
		logger.Log("error", map[string]any{"stage": stage})
		fmt.Fprintln(opts.Stderr, "codex-proxy app-server:", stage)
		return 2
	}
	scanner := appScanner(opts.Stdin)
	if !filepath.IsAbs(startup.Cwd) {
		return fail("invalid app-server working directory")
	}
	mode, err := appServerMode(startup.Args)
	if err != nil || mode != appServerStdio {
		return fail("invalid stdio launch arguments")
	}
	cfg, err := load()
	if err != nil {
		return fail("invalid configuration")
	}
	launch, err := prepareAppServerLaunch(startup)
	if err != nil {
		return fail("invalid app-server sandbox configuration")
	}
	if opts.launchOverride != nil {
		launch = opts.launchOverride(launch)
	}
	// Install handlers before spawning the child, including the interval spent
	// starting the server and writing its first log record.
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	cmd := launch.command(cfg.RealBin)
	cmd.Dir, cmd.Env, cmd.Stderr = startup.Cwd, startup.Env, opts.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fail("create child input")
	}
	defer stdin.Close()
	stdout, childOutput, err := os.Pipe()
	if err != nil {
		return fail("create child output")
	}
	defer stdout.Close()
	cmd.Stdout = childOutput
	if err := cmd.Start(); err != nil {
		childOutput.Close()
		return fail("start real app-server")
	}
	childOutput.Close()
	started := time.Now()
	logger.Log("started", map[string]any{"child_pid": cmd.Process.Pid, "sandboxed": launch.profile != ""})
	if onStart != nil {
		onStart(cmd.Process.Pid, launch.profile != "")
	}
	waited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		// Tools in this process group must not keep stdio open after Codex exits.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		close(waited)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan appServerEvent, 32)
	go readAppLines(ctx, scanner, events, "client")
	go readAppLines(ctx, appScanner(stdout), events, "server")
	session := newAppSession(ctx, dir, load, opts.PollInterval, logger, stdin, opts.Stdout, events)
	session.stateStore = newStateStore(dir, opts.PollInterval, opts.taskInterval)
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
			logger.Log("shutdown", map[string]any{"signal": int(sig.(syscall.Signal))})
			loop = false
		case event := <-events:
			if event.source == "client" && event.line == nil {
				normalShutdown = true
				if event.err != nil {
					exitCode = fail("client message exceeds limit or input failed")
				}
				logger.Log("shutdown", map[string]any{"reason": "client_closed"})
				loop = false
			} else if event.source == "server" && event.line == nil {
				if event.err != nil {
					exitCode = fail("child message exceeds limit or output failed")
				}
				loop = false
			} else if err := session.handle(event); err != nil {
				exitCode = fail("protocol forwarding failed")
				loop = false
			}
			idle.update(event.source == "client", session.busy())
		}
	}
	cancel()
	stdin.Close()
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-waited:
	case <-time.After(500 * time.Millisecond):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-waited
	}
	// All wait workers must return before releasing their reservations. An
	// acquired but undelivered lease is closed by its worker on cancellation.
	session.workers.Wait()
	for {
		select {
		case event := <-events:
			if event.lease != nil {
				event.lease.Close()
			}
			if event.state != nil {
				event.state.Close()
			}
		default:
			goto drained
		}
	}
drained:
	session.close()
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
	if err := json.Unmarshal(line, &message); err != nil || message == nil {
		return nil, errors.New("invalid JSON-RPC message")
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
		return err
	}
	data = append(data, '\n')
	n, err := writer.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return err
}
