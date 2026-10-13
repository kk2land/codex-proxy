package proxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
)

// RunClaude is the entry point for bin/claude. Admission is independent of the
// Codex quota, but shares its system-account lookup, flock scheduler and state gate.
func RunClaude(args []string, opts Options) int {
	if len(args) >= 2 && args[0] == "--proxy-claude-launch" {
		return runClaudeLauncher(args[1:])
	}
	if len(args) == 3 && args[0] == "--proxy-claude-guard" {
		pid, err := strconv.Atoi(args[1])
		if err != nil || (args[2] != "group" && args[2] != "process") {
			return 2
		}
		return runClaudeGuard(pid, args[2] == "group")
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
	fail := func(err error) int {
		if errors.Is(err, ErrWaitTimeout) {
			fmt.Fprintln(opts.Stderr, "claude-proxy: timed out waiting for a Claude task slot")
		} else {
			fmt.Fprintln(opts.Stderr, "claude-proxy:", err)
		}
		return claudeErrorCode(err)
	}
	home, err := opts.AccountHome()
	if err != nil {
		return fail(err)
	}
	if !filepath.IsAbs(home) {
		return fail(errors.New("account home must be absolute"))
	}
	dir := filepath.Join(home, ".claude-proxy")
	if err := ensurePrivateDir(dir); err != nil {
		return fail(err)
	}
	self, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	load := func() (Config, error) { return LoadClaudeConfig(dir, self) }
	logger := newNamedLogger(dir, "claude-proxy.log", newRequestID(), opts.Stderr, load)
	cwd, _ := os.Getwd()
	user := ""
	if configDir := os.Getenv("CLAUDE_CONFIG_DIR"); filepath.IsAbs(configDir) && filepath.Base(filepath.Clean(configDir)) == ".claude" && safeSandboxPath(configDir) {
		user = filepath.Base(filepath.Dir(filepath.Clean(configDir)))
	}
	kind, err := classifyClaudeArgs(args)
	mode := "task"
	if kind == claudeStream {
		mode = "stream-json"
	} else if kind == claudePassThrough {
		mode = "passthrough"
	}
	log := func(event string, fields map[string]any) {
		// No argv or protocol bodies: either can contain prompts, settings,
		// credentials or hook input. Keep only bounded lifecycle metadata.
		fields["mode"] = mode
		fields["user"] = user
		fields["session_cwd"] = cwd
		logger.Log(event, fields)
	}
	if err != nil {
		log("rejected", map[string]any{"reason": "unsupported execution mode"})
		return fail(err)
	}
	cfg, err := load()
	if err != nil {
		log("error", map[string]any{"stage": "configuration"})
		return fail(err)
	}
	launch, err := prepareClaudeLaunch(kind, args)
	if err != nil {
		log("error", map[string]any{"stage": "sandbox"})
		return fail(err)
	}
	if opts.launchOverride != nil {
		launch = opts.launchOverride(launch)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	var received atomic.Int32
	go func() {
		select {
		case sig := <-signals:
			received.Store(int32(sig.(syscall.Signal)))
			cancel()
		case <-ctx.Done():
		}
	}()
	canceledCode := func() int {
		if sig := received.Load(); sig != 0 {
			return 128 + int(sig)
		}
		return 2
	}
	executionFailure := func(err error, stage string) int {
		log("error", map[string]any{"stage": stage})
		if received.Load() != 0 {
			return canceledCode()
		}
		return fail(err)
	}
	var lease *Lease
	var state *stateLease
	startedWaiting := time.Now()
	if kind == claudeTask {
		limiter := Limiter{Dir: dir, Load: load, PollInterval: opts.PollInterval, OnWait: func(cfg Config, elapsed time.Duration) {
			log("waiting", map[string]any{"limit": cfg.MaxConcurrency, "wait_ms": elapsed.Milliseconds()})
		}}
		lease, cfg, err = limiter.Acquire(ctx)
		if err == nil {
			state, err = newClaudeStateStore(dir, opts.PollInterval, opts.taskInterval).Acquire(ctx)
		}
		if err != nil {
			if lease != nil {
				lease.Close()
			}
			if ctx.Err() != nil {
				log("canceled", map[string]any{"signal": received.Load()})
				return canceledCode()
			}
			log("error", map[string]any{"stage": "admission", "timeout": errors.Is(err, ErrWaitTimeout)})
			return fail(err)
		}
		defer state.Close()
	}
	child, err := startClaudeChild(ctx, self, cfg.RealBin, launch, kind, opts, lease)
	if err != nil {
		if lease != nil {
			lease.Close()
		}
		return executionFailure(err, "start")
	}
	defer child.close()
	if state != nil {
		// The child is still a Go launcher, blocked on its bootstrap pipe.
		// A guard owns the slot before native Claude can issue a model call.
		if err := state.Record(ctx); err != nil {
			return executionFailure(err, "state")
		}
		if err := ctx.Err(); err != nil {
			return executionFailure(err, "launch canceled")
		}
		if err := child.begin(); err != nil {
			return executionFailure(err, "launch")
		}
		state.Close()
	}
	started := time.Now()
	guardPID := 0
	if child.guard != nil {
		guardPID = child.guard.cmd.Process.Pid
	}
	log("started", map[string]any{"child_pid": child.cmd.Process.Pid, "guard_pid": guardPID, "limited": lease != nil, "sandboxed": launch.profile != "", "wait_ms": started.Sub(startedWaiting).Milliseconds()})
	code := 0
	if kind == claudeStream {
		code = runClaudeStream(ctx, dir, load, opts, child, log)
	} else {
		select {
		case <-ctx.Done():
			code = canceledCode()
		case <-child.guardExited:
			log("error", map[string]any{"stage": "guard"})
			code = 2
		case <-child.done:
			code = claudeChildExit(child.cmd)
		}
	}
	if received.Load() != 0 {
		code = canceledCode()
	}
	// Terminate/reap native Claude before releasing the guardian and slot.
	child.close()
	log("finished", map[string]any{"child_pid": child.cmd.Process.Pid, "run_ms": time.Since(started).Milliseconds(), "exit_code": code, "signal": received.Load()})
	return code
}

func claudeErrorCode(err error) int {
	if errors.Is(err, ErrWaitTimeout) {
		return 124
	}
	return 2
}

func claudeChildExit(cmd *exec.Cmd) int {
	if cmd.ProcessState == nil {
		return 2
	}
	if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return cmd.ProcessState.ExitCode()
}

type claudeChild struct {
	cmd         *exec.Cmd
	done        chan struct{}
	stdin       *os.File
	stdout      *os.File
	bootstrap   *os.File
	lease       *Lease
	guard       *appTaskGuard
	guardExited chan struct{}
	self        string
	group       bool
	closed      bool
}

func startClaudeChild(ctx context.Context, self, realBin string, launch launchSpec, kind claudeCommand, opts Options, lease *Lease) (*claudeChild, error) {
	cmd := launch.command(realBin)
	var bootReader, bootWriter *os.File
	if lease != nil {
		var err error
		bootReader, bootWriter, err = os.Pipe()
		if err != nil {
			return nil, err
		}
		defer bootReader.Close()
		// The bootstrap FD is 3; slot FD 4 is also inherited by native Claude.
		cmd = exec.Command(self, append([]string{"--proxy-claude-launch", cmd.Path}, cmd.Args[1:]...)...)
		cmd.ExtraFiles = []*os.File{bootReader, lease.File()}
	}
	child := &claudeChild{cmd: cmd, self: self, lease: lease, bootstrap: bootWriter, done: make(chan struct{}), guardExited: make(chan struct{}), group: kind == claudeStream || !isTerminal(opts.Stdin.Fd())}
	cmd.Stdout, cmd.Stderr, cmd.Env = opts.Stdout, opts.Stderr, os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: child.group}
	if kind == claudeStream {
		// Own both pipe ends instead of StdoutPipe: cmd.Wait must not close
		// the output reader before the last result has been drained.
		input, writer, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		defer input.Close()
		child.stdin, cmd.Stdin = writer, input
		reader, output, err := os.Pipe()
		if err != nil {
			writer.Close()
			return nil, err
		}
		defer output.Close()
		child.stdout, cmd.Stdout = reader, output
	} else {
		cmd.Stdin = opts.Stdin
	}
	if err := ctx.Err(); err != nil {
		if child.stdin != nil {
			child.stdin.Close()
			child.stdout.Close()
		}
		if bootWriter != nil {
			bootWriter.Close()
		}
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		if child.stdin != nil {
			child.stdin.Close()
			child.stdout.Close()
		}
		if bootWriter != nil {
			bootWriter.Close()
		}
		return nil, err
	}
	go func() {
		_ = cmd.Wait()
		if child.group {
			// Descendants must not retain stdout after native Claude exits.
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		close(child.done)
	}()
	// A canceled session must also unblock a pipe write, even when native
	// Claude stops reading stdin. This goroutine never releases the slot.
	go func() {
		select {
		case <-ctx.Done():
			pid := child.cmd.Process.Pid
			if child.group {
				pid = -pid
			}
			_ = syscall.Kill(pid, syscall.SIGTERM)
			select {
			case <-child.done:
			case <-time.After(500 * time.Millisecond):
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		case <-child.done:
		}
	}()
	if lease != nil {
		if err := child.protect(lease); err != nil {
			child.close()
			return nil, err
		}
	}
	return child, nil
}

func (child *claudeChild) protect(lease *Lease) error {
	mode := "process"
	if child.group {
		mode = "group"
	}
	guard, err := startSlotGuard(child.self, []string{"--proxy-claude-guard", strconv.Itoa(child.cmd.Process.Pid), mode}, lease, func() { close(child.guardExited) })
	if err != nil {
		return err
	}
	child.lease, child.guard = lease, guard
	return nil
}

func (child *claudeChild) begin() error {
	_, err := child.bootstrap.Write([]byte{'L'})
	child.bootstrap.Close()
	child.bootstrap = nil
	return err
}

func (child *claudeChild) close() {
	if child.closed {
		return
	}
	child.closed = true
	if child.bootstrap != nil {
		child.bootstrap.Close()
	}
	if child.stdin != nil {
		child.stdin.Close()
	}
	signalChild := func(sig syscall.Signal) {
		if child.group {
			_ = syscall.Kill(-child.cmd.Process.Pid, sig)
		} else {
			_ = child.cmd.Process.Signal(sig)
		}
	}
	select {
	case <-child.done:
	default:
		signalChild(syscall.SIGTERM)
		select {
		case <-child.done:
		case <-time.After(500 * time.Millisecond):
			signalChild(syscall.SIGKILL)
			<-child.done
		}
	}
	if child.group {
		signalChild(syscall.SIGKILL)
	}
	if child.stdout != nil {
		child.stdout.Close()
	}
	if child.guard != nil {
		child.guard.release()
	}
	if child.lease != nil {
		child.lease.Release()
	}
}

func runClaudeLauncher(args []string) int {
	if len(args) == 0 || !filepath.IsAbs(args[0]) {
		return 2
	}
	bootstrap := os.NewFile(3, "claude-bootstrap")
	var token [1]byte
	n, err := bootstrap.Read(token[:])
	bootstrap.Close()
	if err != nil || n != 1 || token[0] != 'L' {
		return 2
	}
	if err := syscall.Exec(args[0], args, os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, "claude-proxy: native launch failed:", err)
		return 2
	}
	return 0
}

func runClaudeGuard(pid int, group bool) int {
	if group {
		return runAppServerGuard(pid)
	}
	if pid <= 1 {
		return 2
	}
	lock := os.NewFile(3, "claude-slot")
	defer lock.Close()
	var control [1]byte
	if n, _ := os.Stdin.Read(control[:]); n == 1 && control[0] == 'R' {
		return 0
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	time.Sleep(500 * time.Millisecond)
	_ = syscall.Kill(pid, syscall.SIGKILL)
	for syscall.Kill(pid, 0) == nil {
		time.Sleep(50 * time.Millisecond)
	}
	return 0
}
