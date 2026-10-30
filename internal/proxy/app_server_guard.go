package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
)

// runAppServerGuard owns an inherited slot FD. If its parent's control pipe
// closes unexpectedly, it stops the native process group before dropping the
// slot. This covers SIGKILL, for which the main proxy cannot run cleanup.
func runAppServerGuard(pid int) int {
	if pid <= 1 {
		fmt.Fprintln(os.Stderr, "codex-proxy app-server guard: invalid child pid", pid)
		return 2
	}
	lock := os.NewFile(3, "task-slot")
	if lock == nil {
		fmt.Fprintln(os.Stderr, "codex-proxy app-server guard: task slot file descriptor is unavailable")
		return 2
	}
	defer func() {
		if err := lock.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			fmt.Fprintln(os.Stderr, "codex-proxy app-server guard: close task slot:", err)
		}
	}()
	var control [1]byte
	n, err := os.Stdin.Read(control[:])
	if err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintln(os.Stderr, "codex-proxy app-server guard: read control channel:", err)
	}
	if n == 1 && control[0] == 'R' {
		return 0
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		fmt.Fprintln(os.Stderr, "codex-proxy app-server guard: terminate native process group:", err)
	}
	time.Sleep(500 * time.Millisecond)
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		fmt.Fprintln(os.Stderr, "codex-proxy app-server guard: kill native process group:", err)
	}
	// Keep the reservation until the group has disappeared. If orphan zombies
	// are not reaped promptly, conservatively retain the slot rather than admit
	// overlapping work.
	for {
		err := syscall.Kill(-pid, 0)
		if err != nil {
			if !errors.Is(err, syscall.ESRCH) {
				fmt.Fprintln(os.Stderr, "codex-proxy app-server guard: wait for native process group:", err)
			}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 0
}

type appTaskGuard struct {
	control *os.File
	cmd     *exec.Cmd
	waited  chan struct{}
	waitErr error
	normal  atomic.Bool
}

func (s *appSession) startGuard(lease *Lease) (*appTaskGuard, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(s.selfBin, "--proxy-app-server-guard", strconv.Itoa(s.childPID))
	cmd.Stdin, cmd.Stdout = reader, io.Discard
	stderr := s.guardStderr()
	cmd.Stderr = stderr
	cmd.ExtraFiles = []*os.File{lease.File()}
	if s.authHome != nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, s.authHome)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return nil, err
	}
	if err := reader.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		s.logger.Log("error", map[string]any{"stage": "close task lock guardian input", "error": err.Error()})
	}
	guard := &appTaskGuard{control: writer, cmd: cmd, waited: make(chan struct{})}
	go func() {
		guard.waitErr = cmd.Wait()
		close(guard.waited)
		if !guard.normal.Load() {
			cause := stageError("task lock guardian exited unexpectedly", guard.waitErr)
			select {
			case s.events <- appServerEvent{source: "guard", err: cause}:
			case <-s.ctx.Done():
			}
		}
		// Wait has already captured and logged the diagnostics. A blocked
		// downstream must not hold up the exit event or reservation release.
		if _, err := io.Copy(stderr.stderr, &stderr.buffer); err != nil && stderr.logger != nil {
			stderr.logger.Log("error", map[string]any{"stage": "forward task lock guardian stderr", "error": err.Error()})
		}
	}()
	return guard, nil
}

// guardStderr logs and buffers the guardian's small diagnostic stream. Logger
// failures use the same buffer, so they cannot block Wait on the downstream.
func (s *appSession) guardStderr() *appGuardStderr {
	stderr := &appGuardStderr{stderr: os.Stderr}
	if s.logger != nil && s.logger.Logger != nil && s.logger.stderr != nil {
		stderr.stderr = s.logger.stderr
		stderr.logger = &appLogger{
			Logger: newNamedLogger(s.logger.dir, s.logger.filename, s.logger.requestID, &stderr.buffer, s.logger.load),
			user:   s.logger.user, cwd: s.logger.cwd, childPID: s.logger.childPID,
		}
	}
	return stderr
}

type appGuardStderr struct {
	logger *appLogger
	stderr io.Writer
	buffer bytes.Buffer
}

func (w *appGuardStderr) Write(data []byte) (int, error) {
	if len(data) > 0 && w.logger != nil {
		w.logger.Log("error", map[string]any{"stage": "task lock guardian stderr", "error": string(data)})
	}
	return w.buffer.Write(data)
}

// release asks the guard to drop the reservation and reports real I/O or wait
// failures. A repeated Close reporting ErrClosed is the only expected error.
func (g *appTaskGuard) release() error {
	g.normal.Store(true)
	_, writeErr := g.control.Write([]byte{'R'})
	closeErr := g.control.Close()
	<-g.waited
	if errors.Is(closeErr, os.ErrClosed) {
		closeErr = nil
	}
	if writeErr != nil {
		writeErr = fmt.Errorf("write guardian release: %w", writeErr)
	}
	if closeErr != nil {
		closeErr = fmt.Errorf("close guardian control: %w", closeErr)
	}
	// g.waitErr is published before close(g.waited) and is read-only after
	// that; wrap a local copy so release never races with the waiter.
	waitErr := g.waitErr
	if waitErr != nil {
		waitErr = fmt.Errorf("wait for task lock guardian: %w", waitErr)
	}
	return errors.Join(writeErr, closeErr, waitErr)
}
