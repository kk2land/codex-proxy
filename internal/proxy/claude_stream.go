package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

type claudeAdmission struct {
	id           string
	line         []byte
	ctx          context.Context
	cancel       context.CancelFunc
	canceled     bool // Owned by the protocol loop, never read by the worker.
	started      time.Time
	slotWait     time.Duration
	intervalWait time.Duration
	lease        *Lease
	state        *stateLease
	cfg          Config
	err          error
}

func (job *claudeAdmission) close() {
	if job.state != nil {
		job.state.Close()
		job.state = nil
	}
	if job.lease != nil {
		job.lease.Close()
		job.lease = nil
	}
}

// Keep a single process-level reservation from its first admitted model input
// through native exit. A result is deliberately NOT a release signal: background
// agents, Monitor and scheduled follow-up turns may still issue model requests.
func runClaudeStream(parent context.Context, dir string, load ConfigLoader, opts Options, child *claudeChild, log func(string, map[string]any)) int {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	input := make(chan appServerEvent, 32)
	completed := make(chan *claudeAdmission, 1)
	go readAppLines(ctx, appScanner(opts.Stdin), input, "client")
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
		for {
			select {
			case job := <-completed:
				job.close()
			default:
				return
			}
		}
	}()
	var queue []*claudeAdmission
	var current *claudeAdmission
	pendingBytes := 0
	inputEnded, inputClosed := false, false
	fail := func(err error, stage string) int {
		log("error", map[string]any{"stage": stage, "timeout": errors.Is(err, ErrWaitTimeout)})
		if errors.Is(err, ErrWaitTimeout) {
			fmt.Fprintln(opts.Stderr, "claude-proxy: timed out waiting for a Claude task slot")
		} else {
			// Never print a decoder error that could contain protocol content.
			fmt.Fprintln(opts.Stderr, "claude-proxy:", stage)
		}
		return claudeErrorCode(err)
	}
	forward := func(line []byte) error {
		data := append(line, '\n')
		n, err := child.stdin.Write(data)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
		return err
	}
	for {
		if current == nil && len(queue) > 0 {
			current, queue = queue[0], queue[1:]
			job := current
			job.ctx, job.cancel = context.WithCancel(ctx)
			held := child.lease != nil
			workers.Add(1)
			go func() {
				defer workers.Done()
				began := time.Now()
				if held {
					job.cfg, job.err = load()
				} else {
					limiter := Limiter{Dir: dir, Load: load, PollInterval: opts.PollInterval, OnWait: func(cfg Config, elapsed time.Duration) {
						log("waiting", map[string]any{"submission_id": job.id, "limit": cfg.MaxConcurrency, "slot_wait_ms": elapsed.Milliseconds()})
					}}
					job.lease, job.cfg, job.err = limiter.Acquire(job.ctx)
				}
				job.slotWait = time.Since(began)
				if job.err == nil {
					began = time.Now()
					job.state, job.err = newClaudeStateStore(dir, opts.PollInterval, opts.taskInterval).Acquire(job.ctx)
					job.intervalWait = time.Since(began)
				}
				select {
				case completed <- job:
				case <-ctx.Done():
					job.close()
				}
			}()
		}
		if inputEnded && current == nil && len(queue) == 0 && !inputClosed {
			// stdin EOF means "finish these inputs", unlike app-server disconnect.
			// Drain admitted messages before half-closing, then wait for results.
			child.stdin.Close()
			inputClosed = true
		}
		select {
		case <-ctx.Done():
			log("canceled", map[string]any{"reason": "session stopped"})
			return 2
		case <-child.guardExited:
			return fail(errors.New("guard exited"), "task guard exited unexpectedly")
		case <-child.done:
			return claudeChildExit(child.cmd)
		case job := <-completed:
			pendingBytes -= len(job.line)
			current = nil
			if job.canceled || job.ctx.Err() != nil {
				job.close()
				job.cancel()
				log("canceled", map[string]any{"submission_id": job.id, "wait_ms": time.Since(job.started).Milliseconds()})
				continue
			}
			if job.err != nil {
				job.close()
				job.cancel()
				return fail(job.err, "task admission failed")
			}
			if job.lease != nil {
				if err := child.protect(job.lease); err != nil {
					job.close()
					job.cancel()
					return fail(err, "cannot protect task slot")
				}
				job.lease = nil // Ownership transferred to child.close().
			}
			err := job.state.Record(job.ctx)
			if err == nil {
				err = forward(job.line)
			}
			job.close()
			job.cancel()
			if err != nil {
				return fail(err, "model input forwarding failed")
			}
			log("submitted", map[string]any{"submission_id": job.id, "child_pid": child.cmd.Process.Pid, "guard_pid": child.guard.cmd.Process.Pid, "limit": job.cfg.MaxConcurrency, "slot_wait_ms": job.slotWait.Milliseconds(), "interval_wait_ms": job.intervalWait.Milliseconds(), "wait_ms": time.Since(job.started).Milliseconds()})
		case event := <-input:
			if event.line == nil {
				if event.err != nil {
					return fail(event.err, "client input failed or exceeded the message limit")
				}
				inputEnded = true
				continue
			}
			if len(bytes.TrimSpace(event.line)) == 0 {
				continue
			}
			message, err := decodeRPC(event.line)
			if err != nil {
				return fail(err, "invalid stream-json input")
			}
			if rpcString(message, "type") == "control_request" {
				switch rpcString(rpcObject(message, "request"), "subtype") {
				case "remote_control", "channel_enable", "ultrareview_launch":
					return fail(errors.New("unsupported remote execution"), "remote or detached control requests are not supported")
				}
			}
			if rpcString(message, "type") == "control_request" && rpcString(rpcObject(message, "request"), "subtype") == "interrupt" {
				for _, job := range queue {
					pendingBytes -= len(job.line)
					log("canceled", map[string]any{"submission_id": job.id, "reason": "interrupt before admission"})
				}
				queue = nil
				if current != nil {
					current.canceled = true
					current.cancel()
				}
			}
			if !claudeInputLimited(message) {
				if err := forward(event.line); err != nil {
					return fail(err, "control input forwarding failed")
				}
				continue
			}
			pendingCount := len(queue)
			if current != nil {
				pendingCount++
			}
			if pendingCount >= 128 || pendingBytes+len(event.line) > maxAppServerMessage {
				return fail(errors.New("queue capacity exceeded"), "Claude input queue capacity exceeded")
			}
			job := &claudeAdmission{id: newRequestID(), line: event.line, started: time.Now()}
			queue = append(queue, job)
			pendingBytes += len(job.line)
			messageType := rpcString(message, "type")
			if messageType != "user" && messageType != "control_request" {
				messageType = "other"
			}
			log("received", map[string]any{"submission_id": job.id, "type": messageType, "queue_size": pendingCount + 1})
		}
	}
}

// These are host/CLI coordination, not independent model submissions. Unknown
// messages and future control methods are conservatively admitted as tasks.
func claudeInputLimited(message rpcMessage) bool {
	switch rpcString(message, "type") {
	case "control_response", "control_cancel_request", "keep_alive", "update_environment_variables":
		return false
	case "control_request":
		switch rpcString(rpcObject(message, "request"), "subtype") {
		case "initialize", "interrupt", "set_permission_mode", "set_model", "set_max_thinking_tokens", "mcp_status", "mcp_message", "mcp_reconnect", "mcp_toggle", "mcp_set_servers", "mcp_authenticate", "mcp_clear_auth", "mcp_oauth_callback_url", "rewind_files", "read_file", "seed_read_state", "get_context_usage", "get_settings", "reload_plugins", "reload_skills", "supported_commands", "supported_models", "supported_agents", "account_info", "reconnect_mcp_server", "toggle_mcp_server", "stop_task", "background_tasks", "submit_feedback", "message_rated", "claude_authenticate", "claude_oauth_callback", "claude_oauth_wait_for_completion":
			return false
		}
	}
	return true
}
