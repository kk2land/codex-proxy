package proxy

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

type appNativeWait struct {
	done chan struct{}
	err  error
}

func waitAppNative(cmd *exec.Cmd, logger *appLogger) *appNativeWait {
	wait := &appNativeWait{done: make(chan struct{})}
	go func() {
		wait.err = cmd.Wait()
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			logger.Log("error", map[string]any{"stage": "kill child process group after exit", "error": err.Error()})
		}
		close(wait.done)
	}()
	return wait
}

func stopAppNative(cmd *exec.Cmd, stdin io.Closer, wait *appNativeWait, logger *appLogger) {
	if err := stdin.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		logger.Log("error", map[string]any{"stage": "close child input", "error": err.Error()})
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		logger.Log("error", map[string]any{"stage": "terminate child process group", "error": err.Error()})
	}
	select {
	case <-wait.done:
	case <-time.After(500 * time.Millisecond):
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			logger.Log("error", map[string]any{"stage": "kill child process group", "error": err.Error()})
		}
		<-wait.done
	}
}
