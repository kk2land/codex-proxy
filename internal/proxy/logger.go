package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	loggerDefaultMaxSize    int64 = 10 << 20
	loggerDefaultMaxBackups       = 5
	loggerMinimumMaxSize    int64 = 256
	loggerFilename                = "codex-proxy.log"
	loggerLockTimeout             = 100 * time.Millisecond
	loggerLockPollInterval        = 10 * time.Millisecond
)

// Logger writes bounded JSON Lines records and coordinates rotation across processes.
type Logger struct {
	dir       string
	requestID string
	stderr    io.Writer
	load      ConfigLoader
	mu        sync.Mutex
}

func NewLogger(dir, requestID string, stderr io.Writer, load ConfigLoader) *Logger {
	if stderr == nil {
		stderr = io.Discard
	}
	return &Logger{dir: dir, requestID: requestID, stderr: stderr, load: load}
}

// Log reports logging failures to stderr without affecting the proxied command.
func (l *Logger) Log(event string, fields map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()

	lock, err := openPrivateLogFile(filepath.Join(l.dir, "log.lock"), os.O_CREATE|os.O_RDWR)
	if err != nil {
		l.diagnostic("open log lock", err)
		return
	}
	defer func() {
		if err := lock.Close(); err != nil {
			l.diagnostic("close log lock", err)
		}
	}()
	if err := acquireLoggerLock(lock); err != nil {
		l.diagnostic("acquire log lock", err)
		return
	}
	defer func() {
		if err := loggerFlock(lock, syscall.LOCK_UN); err != nil {
			l.diagnostic("release log lock", err)
		}
	}()

	maxSize, maxBackups := l.limits()
	record, err := l.record(event, fields, maxSize)
	if err != nil {
		l.diagnostic("encode log record", err)
		return
	}
	if err := l.pruneArchives(maxBackups); err != nil {
		l.diagnostic("prune log archives", err)
		return
	}

	path := filepath.Join(l.dir, loggerFilename)
	logFile, err := openPrivateLogFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND)
	if err != nil {
		l.diagnostic("open log", err)
		return
	}
	info, err := logFile.Stat()
	if err != nil {
		_ = logFile.Close()
		l.diagnostic("inspect log", err)
		return
	}
	if info.Size() > maxSize-int64(len(record)) {
		if err := logFile.Close(); err != nil {
			l.diagnostic("close log before rotation", err)
			return
		}
		if err := l.rotate(maxBackups); err != nil {
			l.diagnostic("rotate log", err)
			return
		}
		logFile, err = openPrivateLogFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND)
		if err != nil {
			l.diagnostic("open rotated log", err)
			return
		}
	}
	n, err := logFile.Write(record)
	if err == nil && n != len(record) {
		err = io.ErrShortWrite
	}
	if err != nil {
		l.diagnostic("append log record", err)
	}
	if err := logFile.Close(); err != nil {
		l.diagnostic("close log", err)
	}
}

func (l *Logger) diagnostic(operation string, err error) {
	_, _ = fmt.Fprintf(l.stderr, "codex-proxy: %s: %v\n", operation, err)
}

// Configuration is loaded while holding the same lock as append and rotation.
func (l *Logger) limits() (int64, int) {
	if l.load == nil {
		return loggerDefaultMaxSize, loggerDefaultMaxBackups
	}
	cfg, err := l.load()
	if err != nil {
		l.diagnostic("load log configuration (using defaults)", err)
		return loggerDefaultMaxSize, loggerDefaultMaxBackups
	}
	if cfg.LogMaxSize < loggerMinimumMaxSize || cfg.LogMaxBackups < 0 {
		l.diagnostic("load log configuration (using defaults)", errors.New("invalid log size or backup count"))
		return loggerDefaultMaxSize, loggerDefaultMaxBackups
	}
	return cfg.LogMaxSize, cfg.LogMaxBackups
}

func openPrivateLogFile(path string, flags int) (*os.File, error) {
	f, err := os.OpenFile(path, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("log path is not a regular file")
	}
	if err == nil {
		err = f.Chmod(0600)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func loggerFlock(file *os.File, operation int) error {
	for {
		err := syscall.Flock(int(file.Fd()), operation)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

func acquireLoggerLock(file *os.File) error {
	deadline := time.Now().Add(loggerLockTimeout)
	for {
		if time.Until(deadline) <= 0 {
			return errors.New("timed out waiting for log lock")
		}
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			return err
		}
		delay := time.Until(deadline)
		if delay > loggerLockPollInterval {
			delay = loggerLockPollInterval
		}
		if delay > 0 {
			time.Sleep(delay)
		}
	}
}

type logFieldSize struct {
	name string
	size int
}

func (l *Logger) record(event string, fields map[string]any, maxSize int64) ([]byte, error) {
	values := make(map[string]json.RawMessage, len(fields)+6)
	core := map[string]any{
		"timestamp":  time.Now().UTC().Format(time.RFC3339Nano),
		"request_id": l.requestID,
		"pid":        os.Getpid(),
		"event":      event,
	}
	for name, value := range core {
		encoded, _ := json.Marshal(value)
		values[name] = encoded
	}
	candidates := make([]logFieldSize, 0, len(fields))
	truncated := false
	for name, value := range fields {
		if _, reserved := core[name]; reserved || name == "truncated" {
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			l.diagnostic("encode log field", err)
			truncated = true
			continue
		}
		values[name] = encoded
		encodedName, _ := json.Marshal(name)
		candidates = append(candidates, logFieldSize{name: name, size: len(encodedName) + len(encoded)})
	}
	if truncated {
		values["truncated"] = json.RawMessage("true")
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	if int64(len(encoded)+1) <= maxSize {
		return append(encoded, '\n'), nil
	}

	values["truncated"] = json.RawMessage("true")
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].size == candidates[j].size {
			return candidates[i].name < candidates[j].name
		}
		return candidates[i].size > candidates[j].size
	})
	for _, candidate := range candidates {
		delete(values, candidate.name)
		encoded, err = json.Marshal(values)
		if err != nil {
			return nil, err
		}
		if int64(len(encoded)+1) <= maxSize {
			return append(encoded, '\n'), nil
		}
	}

	// Normal event names and request IDs fit the minimum size. Bound unusually
	// long core strings as well so an individual line can never exceed the limit.
	for int64(len(encoded)+1) > maxSize {
		name := "event"
		if len(values["request_id"]) > len(values[name]) {
			name = "request_id"
		}
		var value string
		if err := json.Unmarshal(values[name], &value); err != nil {
			return nil, err
		}
		if value == "" {
			return nil, errors.New("log size is too small for required metadata")
		}
		runes := []rune(value)
		values[name], _ = json.Marshal(string(runes[:len(runes)/2]))
		encoded, err = json.Marshal(values)
		if err != nil {
			return nil, err
		}
	}
	return append(encoded, '\n'), nil
}

func (l *Logger) archives() ([]int, error) {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return nil, err
	}
	var indexes []int
	for _, entry := range entries {
		suffix, found := strings.CutPrefix(entry.Name(), loggerFilename+".")
		if !found {
			continue
		}
		index, err := strconv.Atoi(suffix)
		if err == nil && index > 0 && strconv.Itoa(index) == suffix {
			indexes = append(indexes, index)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(indexes)))
	return indexes, nil
}

func (l *Logger) archivePath(index int) string {
	return filepath.Join(l.dir, loggerFilename+"."+strconv.Itoa(index))
}

func (l *Logger) pruneArchives(maxBackups int) error {
	indexes, err := l.archives()
	if err != nil {
		return err
	}
	for _, index := range indexes {
		if index > maxBackups {
			if err := os.Remove(l.archivePath(index)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (l *Logger) rotate(maxBackups int) error {
	path := filepath.Join(l.dir, loggerFilename)
	if maxBackups == 0 {
		return os.Remove(path)
	}
	indexes, err := l.archives()
	if err != nil {
		return err
	}
	for _, index := range indexes {
		if index >= maxBackups {
			if err := os.Remove(l.archivePath(index)); err != nil {
				return err
			}
			continue
		}
		if err := os.Rename(l.archivePath(index), l.archivePath(index+1)); err != nil {
			return err
		}
	}
	return os.Rename(path, l.archivePath(1))
}
