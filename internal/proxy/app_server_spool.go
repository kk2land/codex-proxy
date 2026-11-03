package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Readers own pending files until the protocol loop adopts them. Thereafter
// sweep keeps only files reachable from queued requests, deferred responses,
// or buffered lifecycle events. File names are unlinked immediately, including
// on macOS, so even SIGKILL cannot leave conversation data in the directory.
type rpcSpool struct {
	mu           sync.Mutex
	dir          string
	files        map[*rpcSpoolFile]bool
	err          error
	closed       bool
	readDeadline time.Time
}

func (s *rpcSpool) setReadDeadline(deadline time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readDeadline = deadline
}

type rpcSpoolReader struct {
	reader io.Reader
	store  *rpcSpool
}

func (r *rpcSpoolReader) Read(data []byte) (int, error) {
	r.store.mu.Lock()
	deadline, previous := r.store.readDeadline, r.store.err
	r.store.mu.Unlock()
	if previous != nil {
		return 0, previous
	}
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		r.store.fail(os.ErrDeadlineExceeded)
		return 0, os.ErrDeadlineExceeded
	}
	n, err := r.reader.Read(data)
	if err != nil && !errors.Is(err, io.EOF) {
		r.store.fail(err)
	}
	return n, err
}

type rpcSpoolFile struct {
	file   *os.File
	store  *rpcSpool
	budget rpcMetadataBudget
}

// All object indexes and detached scalars from a frame share this budget,
// regardless of whether the original body lives in memory or on disk.
type rpcMetadataBudget struct {
	mu   sync.Mutex
	used int
}

func (b *rpcMetadataBudget) reserve(n int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > rpcMetadataLimit-b.used {
		b.used = rpcMetadataLimit
		return errors.New("RPC control metadata exceeds memory budget")
	}
	b.used += n
	return nil
}

func newRPCSpool(dir string) *rpcSpool {
	return &rpcSpool{dir: dir, files: make(map[*rpcSpoolFile]bool)}
}

func (s *rpcSpool) create() (*rpcSpoolFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, os.ErrClosed
	}
	if len(s.files) >= 512 {
		return nil, errors.New("too many retained app-server message spools")
	}
	f, err := os.CreateTemp(s.dir, ".app-message-*")
	if err != nil {
		return nil, fmt.Errorf("create app-server message spool: %w", err)
	}
	if err := os.Remove(f.Name()); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("unlink app-server message spool: %w", err)
	}
	file := &rpcSpoolFile{file: f, store: s}
	s.files[file] = true
	return file, nil
}

func (s *rpcSpool) fail(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

func (s *rpcSpool) error() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *rpcSpool) adopt(v rpcRaw) {
	if v == nil || v.file == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.files[v.file]; ok {
		s.files[v.file] = false
	}
}

func (s *rpcSpool) release(v rpcRaw) {
	if v == nil || v.file == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.files[v.file]; ok {
		_ = v.file.file.Close()
		delete(s.files, v.file)
	}
}

func (s *rpcSpool) sweep(live map[*rpcSpoolFile]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for file, pending := range s.files {
		if pending || live[file] {
			continue
		}
		_ = file.file.Close()
		delete(s.files, file)
	}
}

func (s *rpcSpool) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for file := range s.files {
		_ = file.file.Close()
		delete(s.files, file)
	}
}

type rpcCheckedWriter struct {
	io.Writer
	spool *rpcSpool
}

func (w rpcCheckedWriter) Write(data []byte) (int, error) {
	if err := w.spool.error(); err != nil {
		return 0, err
	}
	return w.Writer.Write(data)
}

type rpcFrameReader struct {
	reader *bufio.Reader
	spool  *rpcSpool
	err    error
}

func newRPCFrameReader(reader io.Reader, spool *rpcSpool) *rpcFrameReader {
	return &rpcFrameReader{reader: bufio.NewReaderSize(reader, rpcCopyBufferSize), spool: spool}
}

func (r *rpcFrameReader) next() (result rpcRaw, resultErr error) {
	if r.err != nil {
		return nil, r.err
	}
	data := make([]byte, 0, 4096)
	var file *rpcSpoolFile
	var size int64
	defer func() {
		if resultErr != nil && file != nil {
			r.spool.release(&rpcJSON{file: file})
		}
	}()
	for {
		part, err := r.reader.ReadSlice('\n')
		end := len(part) > 0 && part[len(part)-1] == '\n'
		if end {
			part = part[:len(part)-1]
		}
		if file == nil && len(data)+len(part) > rpcMemoryThreshold {
			file, resultErr = r.spool.create()
			if resultErr != nil {
				return nil, resultErr
			}
			if err := rpcWrite(file.file, data); err != nil {
				return nil, fmt.Errorf("write app-server message spool: %w", err)
			}
			data = nil
		}
		if file != nil {
			if err := rpcWrite(file.file, part); err != nil {
				return nil, fmt.Errorf("write app-server message spool: %w", err)
			}
		} else {
			if needed := len(data) + len(part); needed > cap(data) {
				// Grow geometrically without allocating beyond the spill limit.
				grown := make([]byte, len(data), min(rpcMemoryThreshold, max(needed, 2*cap(data))))
				copy(grown, data)
				data = grown
			}
			data = append(data, part...)
		}
		size += int64(len(part))
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			r.err = err
		}
		if !end && size == 0 {
			return nil, err
		}
		// Match ScanLines, including an EOF-terminated last line and CRLF.
		if file == nil {
			if len(data) > 0 && data[len(data)-1] == '\r' {
				data = data[:len(data)-1]
			}
			return &rpcJSON{raw: data, spool: r.spool}, nil
		}
		if size > 0 {
			var last [1]byte
			if _, err := file.file.ReadAt(last[:], size-1); err != nil {
				return nil, err
			}
			if last[0] == '\r' {
				size--
			}
		}
		return &rpcJSON{file: file, length: size}, nil
	}
}

func readAppFrames(ctx context.Context, reader io.Reader, spool *rpcSpool, events chan<- appServerEvent, source string) {
	r := newRPCFrameReader(reader, spool)
	for {
		frame, err := r.next()
		if errors.Is(err, io.EOF) {
			err = nil
		}
		select {
		case events <- appServerEvent{source: source, frame: frame, err: err}:
		case <-ctx.Done():
			spool.release(frame)
			return
		}
		if frame == nil {
			return
		}
	}
}

func (s *appSession) collectSpools() {
	if s.spool == nil {
		return
	}
	live := make(map[*rpcSpoolFile]bool)
	visit := func(file *rpcSpoolFile) { live[file] = true }
	for work := range s.works {
		if work.pending != nil {
			rpcVisit(work.pending.message, visit)
		}
	}
	for _, requests := range []map[string]*appRPCRequest{s.requests, s.serverRequests, s.processes} {
		for _, request := range requests {
			rpcVisit(request.id, visit)
			rpcVisit(request.retained, visit)
			rpcVisit(request.metadata, visit)
		}
	}
	for _, queue := range s.queues {
		for _, item := range queue.items {
			rpcVisit(item.Input, visit)
		}
	}
	for _, events := range s.reviewEvents {
		for _, frame := range events {
			rpcVisit(frame, visit)
		}
	}
	s.spool.sweep(live)
}
