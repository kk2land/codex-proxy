package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const (
	appServerSocketFilename = "app-server.sock"
	appServerPIDFilename    = "app-server.pid"
	appServerDaemonLock     = "app-server.lock"
)

func appServerStateDir(opts Options) (string, error) {
	resolve := opts.AccountHome
	if resolve == nil {
		resolve = SystemHome
	}
	home, err := resolve()
	if err != nil || !filepath.IsAbs(home) {
		return "", errors.New("cannot resolve system account")
	}
	return filepath.Join(home, ".codex-proxy"), nil
}

// RunAppServerDaemon owns per-connection workers. Clients transfer their three
// stdio descriptors over a private local socket, so protocol traffic and real
// Codex stderr still go directly to the caller rather than daemon logs.
func RunAppServerDaemon(opts Options) int {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	fail := func(stage string) int {
		fmt.Fprintln(stderr, "codex-app-server-proxy:", stage)
		return 2
	}
	dir, err := appServerStateDir(opts)
	if err != nil {
		return fail("cannot resolve system account")
	}
	if err := ensurePrivateDir(dir); err != nil {
		return fail("cannot open state directory")
	}
	self, err := os.Executable()
	if err != nil {
		return fail("cannot resolve executable")
	}
	load := func() (Config, error) { return LoadConfig(dir, self) }
	if _, err := load(); err != nil {
		return fail("invalid configuration")
	}
	lock, err := openPrivateLogFile(filepath.Join(dir, appServerDaemonLock), os.O_CREATE|os.O_RDWR)
	if err != nil {
		return fail("cannot open daemon lock")
	}
	defer lock.Close()
	if locked, err := tryLock(context.Background(), lock); err != nil || !locked {
		return fail("daemon is already running or its lock is unavailable")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	socket := filepath.Join(dir, appServerSocketFilename)
	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fail("socket path is not a socket")
		}
		if err := os.Remove(socket); err != nil {
			return fail("cannot remove stale socket")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail("cannot inspect socket")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return fail("cannot listen on local socket")
	}
	defer listener.Close()
	if err := os.Chmod(socket, 0600); err != nil {
		return fail("cannot set socket permissions")
	}
	pidPath := filepath.Join(dir, appServerPIDFilename)
	pidFile, err := openPrivateLogFile(pidPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		return fail("cannot write daemon pid")
	}
	_, writeErr := fmt.Fprintln(pidFile, os.Getpid())
	closeErr := pidFile.Close()
	defer os.Remove(pidPath)
	if writeErr != nil || closeErr != nil {
		return fail("cannot write daemon pid")
	}
	logger := newNamedLogger(dir, appServerLogFilename, newRequestID(), stderr, load)
	logger.Log("daemon_started", map[string]any{"pid": os.Getpid()})
	go func() {
		<-ctx.Done()
		listener.Close()
	}()
	var workers sync.WaitGroup
	code := 0
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() == nil {
				code = fail("accept client connection")
			}
			break
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			runAppServerWorker(ctx, conn, self, logger)
		}()
	}
	cancel()
	workers.Wait()
	logger.Log("daemon_finished", map[string]any{"pid": os.Getpid()})
	return code
}

func receiveAppServerFiles(conn *net.UnixConn) ([]*os.File, error) {
	var version [1]byte
	oob := make([]byte, syscall.CmsgSpace(3*4))
	n, oobn, flags, _, err := conn.ReadMsgUnix(version[:], oob)
	if err != nil {
		return nil, err
	}
	messages, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, err
	}
	var files []*os.File
	for _, message := range messages {
		fds, err := syscall.ParseUnixRights(&message)
		if err != nil {
			closeAppServerFiles(files)
			return nil, err
		}
		for _, fd := range fds {
			syscall.CloseOnExec(fd)
			files = append(files, os.NewFile(uintptr(fd), "client-stdio"))
		}
	}
	if n != 1 || version[0] != 1 || flags&syscall.MSG_CTRUNC != 0 || len(files) != 3 {
		closeAppServerFiles(files)
		return nil, errors.New("invalid client descriptors")
	}
	return files, nil
}

func closeAppServerFiles(files []*os.File) {
	for _, file := range files {
		file.Close()
	}
}

func runAppServerWorker(ctx context.Context, conn *net.UnixConn, self string, logger *Logger) {
	defer conn.Close()
	// An incomplete handshake must not hold up daemon shutdown.
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	stopHandshake := context.AfterFunc(ctx, func() { conn.Close() })
	files, err := receiveAppServerFiles(conn)
	stopHandshake()
	if err != nil {
		return
	}
	defer closeAppServerFiles(files)
	conn.SetReadDeadline(time.Time{})
	if ctx.Err() != nil {
		return
	}
	cmd := exec.Command(self)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = files[0], files[1], files[2]
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		logger.Log("error", map[string]any{"stage": "start app-server worker"})
		return
	}
	// The worker owns copies now; retain no client pipes in the daemon.
	closeAppServerFiles(files)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	control := make(chan struct{})
	go func() {
		defer close(control)
		var value [1]byte
		for {
			if _, err := io.ReadFull(conn, value[:]); err != nil {
				return
			}
			switch syscall.Signal(value[0]) {
			case syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP:
				_ = cmd.Process.Signal(syscall.Signal(value[0]))
			}
		}
	}()
	var waitErr error
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		_ = cmd.Process.Signal(syscall.SIGTERM)
		waitErr = <-done
	case <-control:
		_ = cmd.Process.Signal(syscall.SIGTERM)
		waitErr = <-done
	}
	code := 0
	if waitErr != nil {
		code = 2
		if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
			code = status.ExitStatus()
			if status.Signaled() {
				code = 128 + int(status.Signal())
			}
		}
	}
	_, _ = conn.Write([]byte{byte(code)})
}

// RunAppServerClient uses the daemon if one is listening, otherwise preserving
// the original per-connection stdio service without requiring a background job.
func RunAppServerClient(opts Options) int {
	dir, err := appServerStateDir(opts)
	if err != nil {
		return RunAppServer(opts)
	}
	socket := filepath.Join(dir, appServerSocketFilename)
	if _, err := os.Lstat(socket); errors.Is(err, os.ErrNotExist) {
		return RunAppServer(opts)
	}
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socket, Net: "unix"})
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return RunAppServer(opts)
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	fail := func() int {
		fmt.Fprintln(stderr, "codex-app-server-proxy: daemon connection failed")
		return 2
	}
	if err != nil {
		return fail()
	}
	defer conn.Close()
	stdin, stdout := opts.Stdin, opts.Stdout
	if stdin == nil {
		stdin = os.Stdin
	}
	if stdout == nil {
		stdout = os.Stdout
	}
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	if _, _, err := conn.WriteMsgUnix([]byte{1}, syscall.UnixRights(int(stdin.Fd()), int(stdout.Fd()), int(stderr.Fd())), nil); err != nil {
		return fail()
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case value := <-signals:
				_, _ = conn.Write([]byte{byte(value.(syscall.Signal))})
			case <-done:
				return
			}
		}
	}()
	var code [1]byte
	if _, err := io.ReadFull(conn, code[:]); err != nil {
		return fail()
	}
	return int(code[0])
}
