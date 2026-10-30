package proxy

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestAppServerGuardBlockedStderrDoesNotDelayExitOrRelease(t *testing.T) {
	s, _ := newNotificationSession(t)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close(); writer.Close() })
	raw, err := writer.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var fillErr error
	if err := raw.Control(func(fd uintptr) {
		data := make([]byte, 4096)
		for {
			_, err := syscall.Write(int(fd), data)
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
				return
			}
			if err != nil {
				fillErr = err
				return
			}
		}
	}); err != nil || fillErr != nil {
		t.Fatalf("fill stderr pipe: %v, %v", err, fillErr)
	}
	s.logger.stderr, s.logger.filename = writer, appServerLogFilename
	events := make(chan appServerEvent, 1)
	s.events = events
	s.selfBin, s.childPID = os.Args[0], 0
	limiter := testLimiter(s.dir, 1, 100*time.Millisecond)
	lease, _, err := limiter.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lease.Close() })
	guard, err := s.startGuard(lease)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		reader.Close()
		guard.cmd.Process.Kill()
		guard.control.Close()
		select {
		case <-guard.waited:
		case <-time.After(2 * time.Second):
			t.Error("guardian cleanup timed out")
		}
	})
	select {
	case event := <-events:
		if event.source != "guard" || event.err == nil || !strings.Contains(event.err.Error(), "exit status 2") {
			t.Fatalf("guardian exit changed: %+v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked stderr delayed guardian exit notification")
	}
	if !strings.Contains(errorTestLogs(t, s), "invalid child pid 0") {
		t.Fatal("guardian diagnostic was not logged before exit notification")
	}
	work := s.work("blocked-stderr")
	work.guard, work.lease = guard, lease
	released := make(chan struct{})
	go func() { s.release(work); close(released) }()
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked stderr delayed task slot release")
	}
	next, _, err := limiter.Acquire(context.Background())
	if err != nil {
		t.Fatalf("released slot could not be acquired: %v", err)
	}
	next.Close()
}
