package proxy

import (
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
		return 2
	}
	lock := os.NewFile(3, "task-slot")
	if lock == nil {
		return 2
	}
	defer lock.Close()
	var control [1]byte
	if n, _ := os.Stdin.Read(control[:]); n == 1 && control[0] == 'R' {
		return 0
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	time.Sleep(500 * time.Millisecond)
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	// Keep the reservation until the group has disappeared. If orphan zombies
	// are not reaped promptly, conservatively retain the slot rather than admit
	// overlapping work.
	for syscall.Kill(-pid, 0) == nil {
		time.Sleep(50 * time.Millisecond)
	}
	return 0
}

type appTaskGuard struct {
	control *os.File
	cmd     *exec.Cmd
	waited  chan struct{}
	normal  atomic.Bool
}

func (s *appSession) startGuard(lease *Lease) (*appTaskGuard, error) {
	return startSlotGuard(s.selfBin, []string{"--proxy-app-server-guard", strconv.Itoa(s.childPID)}, lease, func() {
		select {
		case s.events <- appServerEvent{source: "guard"}:
		case <-s.ctx.Done():
		}
	})
}

func startSlotGuard(self string, args []string, lease *Lease, onExit func()) (*appTaskGuard, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(self, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = reader, io.Discard, io.Discard
	cmd.ExtraFiles = []*os.File{lease.File()}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		reader.Close()
		writer.Close()
		return nil, err
	}
	reader.Close()
	guard := &appTaskGuard{control: writer, cmd: cmd, waited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(guard.waited)
		if !guard.normal.Load() {
			onExit()
		}
	}()
	return guard, nil
}

func (g *appTaskGuard) release() {
	g.normal.Store(true)
	_, _ = g.control.Write([]byte{'R'})
	g.control.Close()
	<-g.waited
}
