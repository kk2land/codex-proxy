package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"syscall"
	"time"
)

const claudeMaxPendingFrames = 128
const claudeFrameWriteTimeout = 30 * time.Second

var errClaudeQueueFull = errors.New("Claude frame queue capacity exceeded")

// Reserve before copying a scanned frame. The same reservation follows it
// through reading, admission and writing, including control frames. A scanner
// may additionally hold one frame, but cannot prebuffer 32 independent 32 MiB
// copies outside this budget. Overload fails closed rather than blocking the
// reader indefinitely ahead of a cancellation message.
type claudeByteBudget struct {
	mu    sync.Mutex
	bytes int
	count int
	space chan struct{} // Output readers may wait for the writer to free space.
}

type claudeFrame struct {
	line     []byte // Includes its final newline; never mutated while being written.
	budget   *claudeByteBudget
	released sync.Once
}

func (budget *claudeByteBudget) copy(line []byte) (*claudeFrame, error) {
	size := len(line) + 1
	budget.mu.Lock()
	if budget.count >= claudeMaxPendingFrames || size > maxAppServerMessage-budget.bytes {
		budget.mu.Unlock()
		return nil, errClaudeQueueFull
	}
	budget.bytes += size
	budget.count++
	budget.mu.Unlock()
	data := make([]byte, size)
	copy(data, line)
	data[len(line)] = '\n'
	return &claudeFrame{line: data, budget: budget}, nil
}

func (budget *claudeByteBudget) copyWait(ctx context.Context, line []byte) (*claudeFrame, error) {
	if len(line)+1 > maxAppServerMessage {
		return nil, errClaudeQueueFull
	}
	for {
		frame, err := budget.copy(line)
		if err == nil {
			return frame, nil
		}
		select {
		case <-budget.space:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (frame *claudeFrame) release() {
	frame.released.Do(func() {
		frame.budget.mu.Lock()
		frame.budget.bytes -= len(frame.line)
		frame.budget.count--
		frame.budget.mu.Unlock()
		frame.line = nil // Retired queue entries must not retain released bodies.
		select {
		case frame.budget.space <- struct{}{}:
		default:
		}
	})
}

type claudeReadEvent struct {
	frame *claudeFrame
	err   error
}

func readClaudeFrames(ctx context.Context, reader io.Reader, budget *claudeByteBudget, events chan<- claudeReadEvent) {
	scanner := appScanner(reader)
	for scanner.Scan() {
		frame, err := budget.copy(scanner.Bytes())
		if err != nil {
			select {
			case events <- claudeReadEvent{err: err}:
			case <-ctx.Done():
			}
			return
		}
		select {
		case events <- claudeReadEvent{frame: frame}:
		case <-ctx.Done():
			frame.release()
			return
		}
	}
	select {
	case events <- claudeReadEvent{err: scanner.Err()}:
	case <-ctx.Done():
	}
}

func duplicateClaudeFile(file *os.File) (*os.File, error) {
	connection, err := file.SyscallConn()
	if err != nil {
		return nil, err
	}
	fd := -1
	var dupErr error
	err = connection.Control(func(original uintptr) {
		syscall.ForkLock.RLock()
		defer syscall.ForkLock.RUnlock()
		fd, dupErr = syscall.Dup(int(original))
		if dupErr == nil {
			syscall.CloseOnExec(fd)
		}
	})
	if err != nil {
		if fd >= 0 {
			syscall.Close(fd)
		}
		return nil, err
	}
	if dupErr != nil {
		return nil, dupErr
	}
	// NewFile only registers an existing descriptor with Go's poller when
	// O_NONBLOCK is already set. Without this, Close cannot interrupt an
	// inherited stdin read and stdout deadlines do not work on SDK pipes.
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	if kind := stat.Mode & syscall.S_IFMT; kind == syscall.S_IFIFO || kind == syscall.S_IFSOCK || kind == syscall.S_IFCHR {
		if err := syscall.SetNonblock(fd, true); err != nil {
			syscall.Close(fd)
			return nil, err
		}
	}
	return os.NewFile(uintptr(fd), file.Name()), nil
}

func writeClaudeFrame(file *os.File, line []byte, timeout time.Duration) error {
	if err := file.SetWriteDeadline(time.Now().Add(timeout)); err != nil && !errors.Is(err, os.ErrNoDeadline) {
		return err
	}
	n, err := file.Write(line)
	if err == nil && n != len(line) {
		return io.ErrShortWrite
	}
	return err
}

// Native stdout and proxy cancellation responses share one framed writer.
// Neither producer can interleave bytes with another JSON message, or block
// the protocol loop on a host that has stopped consuming stdout.
type claudeOutput struct {
	ctx       context.Context
	cancel    context.CancelFunc
	native    *os.File
	host      *os.File
	frames    chan *claudeFrame
	budget    claudeByteBudget
	readDone  chan struct{}
	writeDone chan struct{}
	errors    chan error
	errorMu   sync.Mutex
	err       error
}

func startClaudeOutput(ctx context.Context, native, host *os.File, timeout time.Duration) (*claudeOutput, error) {
	writer, err := duplicateClaudeFile(host)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	output := &claudeOutput{ctx: ctx, cancel: cancel, native: native, host: writer, frames: make(chan *claudeFrame, claudeMaxPendingFrames), readDone: make(chan struct{}), writeDone: make(chan struct{}), errors: make(chan error, 1)}
	output.budget.space = make(chan struct{}, 1)
	go func() {
		defer close(output.readDone)
		scanner := appScanner(native)
		for scanner.Scan() {
			frame, err := output.budget.copyWait(ctx, scanner.Bytes())
			if err == nil {
				err = output.enqueue(frame)
			}
			if err != nil {
				output.fail(err)
				return
			}
		}
		if err := scanner.Err(); err != nil {
			output.fail(err)
		}
	}()
	go func() {
		defer close(output.writeDone)
		for {
			select {
			case frame, ok := <-output.frames:
				if !ok {
					return
				}
				err := writeClaudeFrame(writer, frame.line, timeout)
				frame.release()
				if err != nil {
					output.fail(err)
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return output, nil
}

func (output *claudeOutput) send(line []byte) error {
	frame, err := output.budget.copy(line)
	if err != nil {
		return err
	}
	return output.enqueue(frame)
}

func (output *claudeOutput) enqueue(frame *claudeFrame) error {
	select {
	case output.frames <- frame:
		return nil
	case <-output.ctx.Done():
		frame.release()
		return output.ctx.Err()
	}
}

func (output *claudeOutput) cancelAsyncResponse(requestID string) error {
	return output.controlResponse(map[string]any{"subtype": "success", "request_id": requestID, "response": map[string]any{"cancelled": true}})
}

func (output *claudeOutput) cancelControlResponse(requestID string) error {
	return output.controlResponse(map[string]any{"subtype": "error", "request_id": requestID, "error": "Request canceled before submission"})
}

func (output *claudeOutput) controlResponse(response map[string]any) error {
	line, err := json.Marshal(map[string]any{"type": "control_response", "response": response})
	if err != nil {
		return err
	}
	return output.send(line)
}

func (output *claudeOutput) fail(err error) {
	output.errorMu.Lock()
	defer output.errorMu.Unlock()
	if output.err == nil {
		output.err = err
		output.errors <- err
	}
}

func (output *claudeOutput) failure() error {
	output.errorMu.Lock()
	defer output.errorMu.Unlock()
	return output.err
}

func (output *claudeOutput) stop() {
	output.cancel()
	output.host.Close()
	output.native.Close()
	<-output.readDone
	<-output.writeDone
	for {
		select {
		case frame, ok := <-output.frames:
			if !ok {
				return
			}
			frame.release()
		default:
			return
		}
	}
}
