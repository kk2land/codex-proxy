package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type claudeAdmission struct {
	id, uuid, requestID string
	frame               *claudeFrame
	ctx                 context.Context
	cancel              context.CancelFunc
	canceled            bool // Owned by the protocol loop, never read by a worker.
	ready               bool // Admission worker has returned; state is owned by the loop.
	started             time.Time
	slotWait            time.Duration
	intervalWait        time.Duration
	lease               *Lease
	state               *stateLease
	cfg                 Config
	err                 error
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
	job.frame.release()
}

type claudeForward struct {
	frame     *claudeFrame
	job       *claudeAdmission
	requestID string
	finished  atomic.Bool
	err       error // Published through the writer completion channel.
}

// Keep a single process-level reservation from its first admitted model input
// through native exit. A result is deliberately NOT a release signal: background
// agents, Monitor and scheduled follow-up turns may still issue model requests.
func runClaudeStream(parent context.Context, dir string, load ConfigLoader, opts Options, child *claudeChild, log func(string, map[string]any)) int {
	ctx, cancel := context.WithCancel(parent)
	timeout := opts.claudeIOTimeout
	if timeout <= 0 {
		timeout = claudeFrameWriteTimeout
	}
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
	client, err := duplicateClaudeFile(opts.Stdin)
	if err != nil {
		cancel()
		return fail(err, "cannot open client input")
	}
	output, err := startClaudeOutput(ctx, child.stdout, opts.Stdout, timeout)
	if err != nil {
		cancel()
		client.Close()
		return fail(err, "cannot open client output")
	}
	input := make(chan claudeReadEvent)
	inputDone := make(chan struct{})
	budget := &claudeByteBudget{}
	go func() {
		defer close(inputDone)
		readClaudeFrames(ctx, client, budget, input)
	}()
	completed := make(chan *claudeAdmission, 1)
	writer := make(chan *claudeForward)
	written := make(chan *claudeForward, 1)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case request := <-writer:
				if request.job != nil {
					request.err = request.job.state.Record(request.job.ctx)
				}
				if request.err == nil {
					request.err = writeClaudeFrame(child.stdin, request.frame.line, timeout)
				}
				request.finished.Store(true)
				select {
				case written <- request:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	var workers sync.WaitGroup
	var queue []*claudeAdmission
	var controls []*claudeForward
	var current *claudeAdmission
	var writing *claudeForward
	defer func() {
		cancel()
		client.Close()
		// Reap native Claude before releasing a state lock involved in a
		// partial frame. Later submissions must not overtake a native process
		// that could still consume that frame.
		child.close()
		<-writerDone
		workers.Wait()
		<-inputDone
		output.stop()
		if current != nil {
			current.close()
		}
		for _, job := range queue {
			job.close()
		}
		if writing != nil {
			writing.frame.release()
		}
		for _, request := range controls {
			request.frame.release()
		}
		for {
			select {
			case job := <-completed:
				job.close()
			default:
				return
			}
		}
	}()
	cancelPending := func(match func(*claudeAdmission) bool) (bool, bool, error) {
		canceled, partial := false, false
		var responseErr error
		cancelJob := func(job *claudeAdmission) {
			canceled = true
			job.canceled = true
			if job.cancel != nil {
				job.cancel()
			}
			log("canceled", map[string]any{"submission_id": job.id, "reason": "canceled before submission"})
			if job.requestID != "" {
				if err := output.cancelControlResponse(job.requestID); err != nil {
					responseErr = err
				}
			}
		}
		kept := queue[:0]
		for _, job := range queue {
			if match(job) {
				cancelJob(job)
				job.close()
			} else {
				kept = append(kept, job)
			}
		}
		clear(queue[len(kept):])
		queue = kept
		if current != nil && !current.canceled && match(current) {
			if writing != nil && writing.job == current {
				// A complete frame belongs to the CLI; an unfinished write may
				// be partial, so cancellation must abort the connection.
				partial = !writing.finished.Load()
			} else {
				cancelJob(current)
				if current.ready {
					current.close()
					current = nil
				}
			}
		}
		return canceled, partial, responseErr
	}
	inputChannel := (<-chan claudeReadEvent)(input)
	childDone := (<-chan struct{})(child.done)
	outputRead := (<-chan struct{})(output.readDone)
	outputDone := (<-chan struct{})(output.writeDone)
	inputEnded, inputClosed := false, false
	exited, outputEnded, outputClosed, outputWritten := false, false, false, false
	for {
		if exited && outputEnded && !outputClosed {
			close(output.frames) // Both producers have stopped.
			outputClosed = true
		}
		if exited && outputWritten {
			if err := output.failure(); err != nil {
				return fail(err, "native output forwarding failed")
			}
			return claudeChildExit(child.cmd)
		}
		if !exited && current == nil && len(queue) > 0 && writing == nil && len(controls) == 0 {
			current = queue[0]
			queue[0] = nil
			queue = queue[1:]
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
		if inputEnded && current == nil && len(queue) == 0 && len(controls) == 0 && writing == nil && !inputClosed {
			child.stdin.Close()
			inputClosed = true
		}
		var next *claudeForward
		var dispatch chan *claudeForward
		if !exited && writing == nil {
			if current != nil && current.ready {
				next = &claudeForward{frame: current.frame, job: current, requestID: current.requestID}
			} else if len(controls) > 0 {
				next = controls[0]
			}
			if next != nil {
				dispatch = writer
			}
		}
		select {
		case <-ctx.Done():
			log("canceled", map[string]any{"reason": "session stopped"})
			return 2
		case <-child.guardExited:
			return fail(errors.New("guard exited"), "task guard exited unexpectedly")
		case <-childDone:
			exited, childDone, inputChannel = true, nil, nil
			client.Close()
			child.stdin.Close()
			if current != nil {
				current.cancel()
				if current.ready && (writing == nil || writing.job != current) {
					current.close()
					current = nil
				}
			}
			for _, job := range queue {
				job.close()
			}
			queue = nil
			for _, request := range controls {
				request.frame.release()
			}
			controls = nil
		case <-outputRead:
			outputEnded, outputRead = true, nil
			if err := output.failure(); err != nil {
				return fail(err, "native output forwarding failed")
			}
		case <-outputDone:
			outputWritten, outputDone = true, nil
			if err := output.failure(); err != nil {
				return fail(err, "native output forwarding failed")
			}
		case err := <-output.errors:
			return fail(err, "native output forwarding failed")
		case dispatch <- next:
			writing = next
			if next.job == nil {
				controls[0] = nil
				controls = controls[1:]
			}
		case request := <-written:
			writing = nil
			if request.err != nil && !exited {
				// Defer owns the job/state until the child has been reaped.
				writing = request
				return fail(request.err, "input forwarding failed or timed out")
			}
			if job := request.job; job != nil {
				job.close()
				job.cancel()
				current = nil
				if request.err == nil && !exited {
					log("submitted", map[string]any{"submission_id": job.id, "child_pid": child.cmd.Process.Pid, "guard_pid": child.guard.cmd.Process.Pid, "limit": job.cfg.MaxConcurrency, "slot_wait_ms": job.slotWait.Milliseconds(), "interval_wait_ms": job.intervalWait.Milliseconds(), "wait_ms": time.Since(job.started).Milliseconds()})
				}
			} else {
				request.frame.release()
			}
		case job := <-completed:
			if job.canceled || job.ctx.Err() != nil || exited {
				job.close()
				job.cancel()
				current = nil
				continue
			}
			if job.err != nil {
				return fail(job.err, "task admission failed")
			}
			if job.lease != nil {
				if err := child.protect(job.lease); err != nil {
					return fail(err, "cannot protect task slot")
				}
				job.lease = nil // Ownership transferred to child.close().
			}
			job.ready = true
		case event := <-inputChannel:
			if event.frame == nil {
				if event.err != nil {
					return fail(event.err, "client input failed or exceeded the queue/message limit")
				}
				inputEnded, inputChannel = true, nil
				continue
			}
			frame := event.frame
			if len(bytes.TrimSpace(frame.line)) == 0 {
				frame.release()
				continue
			}
			message, err := decodeRPC(frame.line)
			if err != nil {
				frame.release()
				return fail(err, "invalid stream-json input")
			}
			kind := rpcString(message, "type")
			requestID, subtype := "", ""
			if kind == "control_request" {
				requestID = rpcString(message, "request_id")
				subtype = rpcString(rpcObject(message, "request"), "subtype")
				switch subtype {
				case "remote_control", "channel_enable", "ultrareview_launch":
					frame.release()
					return fail(errors.New("unsupported remote execution"), "remote or detached control requests are not supported")
				}
			}
			var match func(*claudeAdmission) bool
			switch {
			case subtype == "interrupt":
				if writing != nil && !writing.finished.Load() {
					frame.release()
					return fail(errors.New("incomplete frame interrupted"), "interrupted during input forwarding")
				}
				match = func(*claudeAdmission) bool { return true }
			case subtype == "cancel_async_message":
				uuid := rpcString(rpcObject(message, "request"), "message_uuid")
				match = func(job *claudeAdmission) bool { return uuid != "" && job.uuid == uuid }
			case kind == "control_cancel_request":
				target := rpcString(message, "request_id")
				match = func(job *claudeAdmission) bool { return target != "" && job.requestID == target }
			}
			if match != nil {
				canceled, partial, err := cancelPending(match)
				if err != nil || partial {
					frame.release()
					return fail(errors.New("cancellation forwarding failed"), "canceled during input forwarding or response failed")
				}
				if kind == "control_cancel_request" {
					target := rpcString(message, "request_id")
					if writing != nil && target != "" && writing.requestID == target && !writing.finished.Load() {
						frame.release()
						return fail(errors.New("incomplete control frame canceled"), "canceled during input forwarding")
					}
					kept := controls[:0]
					safeCanceled := false
					for _, request := range controls {
						if target != "" && request.requestID == target {
							safeCanceled = true
							request.frame.release()
						} else {
							kept = append(kept, request)
						}
					}
					clear(controls[len(kept):])
					controls = kept
					if safeCanceled {
						canceled = true
						if err := output.cancelControlResponse(target); err != nil {
							frame.release()
							return fail(err, "cancellation response forwarding failed")
						}
					}
				}
				if canceled && subtype == "cancel_async_message" {
					frame.release()
					if err := output.cancelAsyncResponse(requestID); err != nil {
						return fail(err, "cancellation response forwarding failed")
					}
					continue
				}
				if canceled && kind == "control_cancel_request" {
					frame.release()
					continue
				}
			}
			if !claudeInputLimited(message) {
				controls = append(controls, &claudeForward{frame: frame, requestID: requestID})
				continue
			}
			job := &claudeAdmission{id: newRequestID(), frame: frame, requestID: requestID, started: time.Now()}
			if kind == "user" {
				job.uuid = rpcString(message, "uuid")
			}
			queue = append(queue, job)
			if kind != "user" && kind != "control_request" {
				kind = "other"
			}
			pendingCount := len(queue)
			if current != nil {
				pendingCount++
			}
			log("received", map[string]any{"submission_id": job.id, "type": kind, "queue_size": pendingCount})
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
		case "initialize", "interrupt", "cancel_async_message", "set_permission_mode", "set_model", "set_max_thinking_tokens", "mcp_status", "mcp_message", "mcp_reconnect", "mcp_toggle", "mcp_set_servers", "mcp_authenticate", "mcp_clear_auth", "mcp_oauth_callback_url", "rewind_files", "read_file", "seed_read_state", "get_context_usage", "get_settings", "reload_plugins", "reload_skills", "supported_commands", "supported_models", "supported_agents", "account_info", "reconnect_mcp_server", "toggle_mcp_server", "stop_task", "background_tasks", "submit_feedback", "message_rated", "claude_authenticate", "claude_oauth_callback", "claude_oauth_wait_for_completion":
			return false
		}
	}
	return true
}
