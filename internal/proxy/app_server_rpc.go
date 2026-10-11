package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

type appRPCRequest struct {
	id               json.RawMessage
	method           string
	work             *appWork
	callback         func(rpcMessage) error
	started          bool
	task             bool
	awaitStart       bool
	sequence         uint64
	previousGoal     bool
	previousRealtime bool
	initial          bool
}

type appAdmission struct {
	work    *appWork
	message rpcMessage
	method  string
	queueID string
	ctx     context.Context
	initial bool
	cancel  context.CancelFunc
	started time.Time
}

type appWork struct {
	thread      string
	lease       *Lease
	guard       *appTaskGuard
	taskID      string
	started     time.Time
	pending     *appAdmission
	policyReady bool
	running     map[string]bool
	completed   map[string]bool
	inflight    int
	awaiting    int
	goal        bool
	realtime    bool
	shell       int
	queue       []*appQueuedInput
	queuePaused bool
}

type appSession struct {
	ctx             context.Context
	dir             string
	load            ConfigLoader
	poll            time.Duration
	logger          *Logger
	stdin           io.Writer
	stdout          io.Writer
	events          chan<- appServerEvent
	workers         sync.WaitGroup
	threads         map[string]*appWork
	works           map[*appWork]bool
	requests        map[string]*appRPCRequest
	detached        map[string][]*appWork
	reviewSources   map[string]string
	reviewEvents    map[string][][]byte
	reviewEventSize int
	nonce           string
	sequence        uint64
	queuedSize      int
	childPID        int
	selfBin         string
	stateStore      *stateStore
}

func newAppSession(ctx context.Context, dir string, load ConfigLoader, poll time.Duration, logger *Logger, stdin, stdout io.Writer, events chan<- appServerEvent) *appSession {
	return &appSession{ctx: ctx, dir: dir, load: load, poll: poll, logger: logger, stdin: stdin, stdout: stdout, events: events, stateStore: newStateStore(dir, poll, nil),
		threads: make(map[string]*appWork), works: make(map[*appWork]bool), requests: make(map[string]*appRPCRequest), detached: make(map[string][]*appWork), reviewSources: make(map[string]string), reviewEvents: make(map[string][][]byte), nonce: newRequestID()}
}

func (s *appSession) work(thread string) *appWork {
	if work := s.threads[thread]; work != nil {
		return work
	}
	work := &appWork{thread: thread, running: make(map[string]bool), completed: make(map[string]bool)}
	s.threads[thread] = work
	s.works[work] = true
	return work
}

func (s *appSession) handle(event appServerEvent) error {
	switch event.source {
	case "admitted":
		return s.admitted(event)
	case "state_ready":
		return s.stateReady(event)
	case "guard":
		return errors.New("task lock guardian exited unexpectedly")
	case "client":
		return s.client(event.line)
	case "server":
		return s.server(event.line)
	}
	return nil
}

func (s *appSession) send(message rpcMessage, request *appRPCRequest) error {
	if request != nil {
		s.sequence++
		request.sequence = s.sequence
		id := "codex-proxy:" + s.nonce + ":" + strconv.FormatUint(s.sequence, 10)
		message["id"] = rpcValue(id)
		s.requests[string(message["id"])] = request
	}
	return writeRPC(s.stdin, message)
}

func (s *appSession) forward(message rpcMessage, method string, work *appWork) error {
	var request *appRPCRequest
	if id := message["id"]; len(id) > 0 && string(id) != "null" {
		request = &appRPCRequest{id: append(json.RawMessage(nil), id...), method: method, work: work}
	}
	return s.send(message, request)
}

func (s *appSession) internal(method string, params rpcMessage, callback func(rpcMessage) error) error {
	return s.send(rpcMessage{"method": rpcValue(method), "params": rpcValue(params)}, &appRPCRequest{method: method, callback: callback})
}

func (s *appSession) reply(id json.RawMessage, result any) error {
	if len(id) == 0 || string(id) == "null" {
		return nil
	}
	return writeRPC(s.stdout, rpcMessage{"id": id, "result": rpcValue(result)})
}

func (s *appSession) rpcError(id json.RawMessage, code int, message string) error {
	if len(id) == 0 || string(id) == "null" {
		return nil
	}
	return writeRPC(s.stdout, rpcMessage{"id": id, "error": rpcValue(map[string]any{"code": code, "message": message})})
}

func (s *appSession) notify(method string, params any) error {
	return writeRPC(s.stdout, rpcMessage{"method": rpcValue(method), "params": rpcValue(params)})
}

func externalSandbox() json.RawMessage {
	return rpcValue(map[string]string{"type": "externalSandbox", "networkAccess": "enabled"})
}

func enforceAppPolicy(message rpcMessage) {
	method := rpcString(message, "method")
	params := rpcObject(message, "params")
	switch method {
	case "turn/start", "thread/settings/update", "command/exec":
		delete(params, "permissions")
		delete(params, "permissionProfile")
		params["sandboxPolicy"] = externalSandbox()
		if method != "command/exec" {
			params["approvalPolicy"] = rpcValue("never")
		}
	case "thread/start", "thread/resume", "thread/fork":
		delete(params, "permissions")
		params["sandbox"] = rpcValue("danger-full-access")
		params["approvalPolicy"] = rpcValue("never")
		config := rpcObject(params, "config")
		config["sandbox_mode"] = rpcValue("danger-full-access")
		config["approval_policy"] = rpcValue("never")
		params["config"] = rpcValue(config)
		if method == "thread/fork" {
			// A fork must not start an automatic goal before task admission.
			params["deferGoalContinuation"] = rpcValue(true)
		}
	default:
		return
	}
	message["params"] = rpcValue(params)
}

func goalStartsWork(params rpcMessage) bool {
	status := rpcString(params, "status")
	return status == "active" || (status == "" && rpcString(params, "objective") != "")
}

func modelTask(method string, params rpcMessage) bool {
	switch method {
	case "turn/start", "review/start", "thread/compact/start", "thread/queue/start", "thread/approveGuardianDeniedAction", "thread/prediction/request", "thread/realtime/start":
		return true
	case "thread/goal/set":
		return goalStartsWork(params)
	}
	return false
}

func (s *appSession) client(line []byte) error {
	message, err := decodeRPC(line)
	if err != nil {
		_, err = s.stdin.Write(append(line, '\n')) // Codex owns protocol validation.
		return err
	}
	method := rpcString(message, "method")
	if method == "" {
		// Responses to server-initiated approvals/tools retain the server's id.
		return writeRPC(s.stdin, message)
	}
	params := rpcObject(message, "params")
	thread := rpcString(params, "threadId")
	if method == "initialize" {
		// thread/settings/update currently requires experimentalApi. Enable it
		// on the private native connection so stable clients can still use the
		// proxy's mandatory externalSandbox setup. Preserve other capabilities.
		capabilities := rpcObject(params, "capabilities")
		capabilities["experimentalApi"] = rpcValue(true)
		params["capabilities"] = rpcValue(capabilities)
		message["params"] = rpcValue(params)
	}
	if method == "remoteControl/enable" || method == "remoteControl/pairing/start" {
		return s.rpcError(message["id"], -32602, "remote task ingress is unavailable through the stdio proxy")
	}
	if strings.HasPrefix(method, "thread/queue/") {
		return s.queueRequest(message, params, method, thread)
	}
	if method == "$/cancelRequest" {
		for work := range s.works {
			if work.pending != nil && len(work.pending.message["id"]) > 0 && string(work.pending.message["id"]) != "null" && string(work.pending.message["id"]) == string(params["id"]) {
				return s.cancelAdmission(work)
			}
		}
		for nativeID, request := range s.requests {
			if len(request.id) > 0 && string(request.id) == string(params["id"]) {
				params["id"] = json.RawMessage(nativeID)
				message["params"] = rpcValue(params)
				break
			}
		}
		return s.forward(message, method, nil)
	}
	if method == "turn/interrupt" && thread != "" {
		work := s.work(thread)
		work.queuePaused = true
		if work.pending != nil {
			if err := s.cancelAdmission(work); err != nil {
				return err
			}
			if work.lease == nil {
				return s.reply(message["id"], map[string]any{})
			}
		}
	}
	if thread != "" && ((method == "thread/goal/set" && !goalStartsWork(params)) || method == "thread/goal/clear" || method == "thread/realtime/stop") {
		work := s.work(thread)
		if work.pending != nil && ((strings.HasPrefix(method, "thread/goal/") && work.pending.method == "thread/goal/set") || (method == "thread/realtime/stop" && work.pending.method == "thread/realtime/start")) {
			if err := s.cancelAdmission(work); err != nil {
				return err
			}
		}
	}
	if modelTask(method, params) && thread != "" {
		if s.reviewSources[thread] != "" {
			return s.rpcError(message["id"], -32000, "detached review thread is still starting")
		}
		if len(message["id"]) == 0 || string(message["id"]) == "null" {
			return s.rpcError(rpcValue(nil), -32600, "task methods require a request id")
		}
		work := s.work(thread)
		if method == "review/start" && rpcString(params, "delivery") == "detached" {
			work = &appWork{thread: thread, running: make(map[string]bool), completed: make(map[string]bool)}
			s.works[work] = true
			// Detached reviews are independent jobs, even if the source is busy.
			return s.begin(work, message, method, "")
		}
		if work.pending != nil {
			return s.rpcError(message["id"], -32000, "thread already has a task waiting for a slot")
		}
		return s.begin(work, message, method, "")
	}
	enforceAppPolicy(message)
	var work *appWork
	if thread != "" {
		work = s.work(thread)
	}
	if method == "thread/shellCommand" && work != nil && len(work.running) == 0 {
		work.shell++
	}
	return s.forward(message, method, work)
}

func (s *appSession) begin(work *appWork, message rpcMessage, method, queueID string) error {
	waiting := 0
	for existing := range s.works {
		if existing.pending != nil {
			waiting++
		}
	}
	if waiting >= 128 {
		return s.rpcError(message["id"], -32000, "too many tasks waiting for slots")
	}
	ctx, cancel := context.WithCancel(s.ctx)
	job := &appAdmission{work: work, message: message, method: method, queueID: queueID, ctx: ctx, initial: work.lease == nil, cancel: cancel, started: time.Now()}
	work.pending = job
	if !job.initial {
		return s.dispatchAdmission(job)
	}
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		limiter := Limiter{Dir: s.dir, Load: s.load, PollInterval: s.poll, OnWait: func(cfg Config, elapsed time.Duration) {
			// Only constants and numeric metadata enter the log.
			s.logger.Log("waiting", map[string]any{"method": method, "limit": cfg.MaxConcurrency, "wait_ms": elapsed.Milliseconds()})
		}}
		lease, cfg, err := limiter.Acquire(ctx)
		select {
		case s.events <- appServerEvent{source: "admitted", job: job, lease: lease, cfg: cfg, err: err}:
		case <-s.ctx.Done():
			if lease != nil {
				lease.Close()
			}
		}
	}()
	return nil
}

func (s *appSession) admitted(event appServerEvent) error {
	job, work := event.job, event.job.work
	if work.pending != job || s.ctx.Err() != nil {
		if event.lease != nil {
			event.lease.Close()
		}
		return nil
	}
	if event.err != nil {
		code, reason := -32000, "cannot acquire Codex task slot"
		eventName := "error"
		if errors.Is(event.err, ErrWaitTimeout) {
			code, reason, eventName = -32001, "timed out waiting for a Codex task slot", "timeout"
		} else if errors.Is(event.err, context.Canceled) {
			code, reason, eventName = -32800, "Codex task canceled before execution", "canceled"
		}
		work.pending = nil
		job.cancel()
		s.logger.Log(eventName, map[string]any{"method": job.method, "wait_ms": time.Since(job.started).Milliseconds()})
		return s.admissionError(job, code, reason)
	}
	work.lease = event.lease
	work.taskID, work.started = newRequestID(), time.Now()
	guard, err := s.startGuard(event.lease)
	if err != nil {
		work.pending = nil
		job.cancel()
		s.release(work)
		return s.admissionError(job, -32000, "cannot start task lock guardian")
	}
	work.guard = guard
	s.logger.Log("admitted", map[string]any{"task_id": work.taskID, "method": job.method, "limit": event.cfg.MaxConcurrency, "wait_ms": time.Since(job.started).Milliseconds()})
	if work.policyReady {
		return s.prepareAdmission(job)
	}
	return s.internal("thread/settings/update", rpcMessage{"threadId": rpcValue(work.thread), "sandboxPolicy": externalSandbox(), "approvalPolicy": rpcValue("never")}, func(response rpcMessage) error {
		if work.pending != job {
			return nil
		}
		if response["error"] != nil {
			work.pending = nil
			job.cancel()
			s.release(work)
			return s.admissionError(job, -32000, "real app-server cannot apply externalSandbox policy")
		}
		work.policyReady = true
		return s.prepareAdmission(job)
	})
}

func (s *appSession) prepareAdmission(job *appAdmission) error {
	if job.method != "turn/start" {
		return s.dispatchAdmission(job)
	}
	// A resumed/forked thread can carry an active persisted goal. Its next
	// explicit turn also owns all automatic continuations, even when this
	// connection has not received a goal update yet.
	return s.internal("thread/goal/get", rpcMessage{"threadId": rpcValue(job.work.thread)}, func(response rpcMessage) error {
		if job.work.pending != job {
			return nil
		}
		if response["error"] == nil {
			job.work.goal = rpcString(rpcObject(rpcObject(response, "result"), "goal"), "status") == "active"
		}
		return s.dispatchAdmission(job)
	})
}

func (s *appSession) dispatchAdmission(job *appAdmission) error {
	// Policy/goal preparation has completed and the work already owns a slot.
	// Only this worker sleeps; the protocol loop stays responsive to queries,
	// approvals, interruption, and cancellation while holding the reservation.
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		state, err := s.stateStore.Acquire(job.ctx)
		select {
		case s.events <- appServerEvent{source: "state_ready", job: job, state: state, err: err}:
		case <-s.ctx.Done():
			if state != nil {
				state.Close()
			}
		}
	}()
	return nil
}

func (s *appSession) stateReady(event appServerEvent) error {
	job, work := event.job, event.job.work
	if event.state != nil {
		defer event.state.Close()
	}
	if work.pending != job || s.ctx.Err() != nil {
		return nil
	}
	err := event.err
	if err == nil {
		err = event.state.Record(job.ctx)
	}
	if err != nil {
		work.pending = nil
		job.cancel()
		if job.initial {
			work.goal = false
			s.release(work)
		} else if err := s.finish(work); err != nil {
			return err
		}
		code, reason := -32000, "cannot apply Codex task submission interval"
		if errors.Is(err, context.Canceled) {
			code, reason = -32800, "Codex task canceled before execution"
		}
		s.logger.Log("error", map[string]any{"method": job.method, "stage": "task_interval"})
		return s.admissionError(job, code, reason)
	}
	work.pending = nil
	job.cancel()
	if job.queueID != "" {
		entry := s.removeQueued(work, job.queueID)
		if entry == nil {
			s.release(work)
			return s.rpcError(job.message["id"], -32800, "queued submission was removed")
		}
		params := rpcObject(job.message, "params")
		params["input"], params["clientUserMessageId"] = entry.Input, rpcValue(entry.ClientUserMessageID)
		job.message["params"] = rpcValue(params)
		if err := s.queueChanged(work); err != nil {
			return err
		}
	}
	return s.dispatch(work, job.message, job.method, job.initial)
}

func (s *appSession) dispatch(work *appWork, message rpcMessage, method string, initial bool) error {
	enforceAppPolicy(message)
	work.inflight++
	previousGoal, previousRealtime := work.goal, work.realtime
	awaitStart := false
	if method == "thread/goal/set" {
		work.goal = true
	} else if method == "thread/realtime/start" {
		work.realtime = true
	} else if !(method == "turn/start" && len(work.running) > 0) {
		work.awaiting++
		awaitStart = true
	}
	if method == "review/start" && rpcString(rpcObject(message, "params"), "delivery") == "detached" {
		s.detached[work.thread] = append(s.detached[work.thread], work)
	}
	return s.send(message, &appRPCRequest{id: append(json.RawMessage(nil), message["id"]...), method: method, work: work, task: true, awaitStart: awaitStart, previousGoal: previousGoal, previousRealtime: previousRealtime, initial: initial})
}

func (s *appSession) cancelAdmission(work *appWork) error {
	job := work.pending
	if job == nil {
		return nil
	}
	work.pending = nil
	job.cancel()
	work.queuePaused = true
	if job.initial && work.lease != nil && len(work.running) == 0 {
		work.goal = false
		s.release(work)
	} else if err := s.finish(work); err != nil {
		return err
	}
	s.logger.Log("canceled", map[string]any{"method": job.method})
	return s.rpcError(job.message["id"], -32800, "Codex task canceled before execution")
}

func (s *appSession) server(line []byte) error {
	message, err := decodeRPC(line)
	if err != nil {
		return errors.New("real app-server emitted invalid JSON")
	}
	method := rpcString(message, "method")
	if method == "" && len(message["id"]) > 0 {
		request := s.requests[string(message["id"])]
		if request != nil {
			delete(s.requests, string(message["id"]))
			if request.callback != nil {
				return request.callback(message)
			}
			message["id"] = request.id
			return s.response(request, message)
		}
	}
	params := rpcObject(message, "params")
	thread := rpcString(params, "threadId")
	if method == "thread/started" {
		info := rpcObject(params, "thread")
		id := rpcString(info, "id")
		parent := rpcString(info, "forkedFromId")
		if pending := s.detached[parent]; len(pending) > 0 {
			// Multiple reviews or an ordinary fork can emit indistinguishable
			// thread/started notifications. Bind the reservation using the
			// review response's authoritative reviewThreadId, not arrival order.
			s.reviewSources[id] = parent
			s.work(id)
		} else if parent := rpcString(info, "parentThreadId"); parent != "" && s.threads[parent] != nil && s.threads[parent].lease != nil {
			// Native subagents belong to their submitting task, like codex exec.
			s.threads[id] = s.threads[parent]
		} else if id != "" {
			s.work(id)
		}
	}
	if thread != "" {
		work := s.work(thread)
		if s.reviewSources[thread] != "" && work.lease == nil && (method == "turn/started" || method == "turn/completed" || method == "thread/goal/updated" || method == "thread/goal/cleared" || method == "thread/closed") {
			if s.reviewEventSize+len(line) > maxAppServerMessage {
				return errors.New("too many detached review lifecycle events")
			}
			s.reviewEvents[thread] = append(s.reviewEvents[thread], line)
			s.reviewEventSize += len(line)
			return nil
		}
		switch method {
		case "turn/started":
			turnID := rpcString(rpcObject(params, "turn"), "id")
			if work.shell > 0 && len(work.running) == 0 {
				work.shell--
				work.running[thread+":"+turnID] = false
			} else {
				if work.lease == nil {
					return errors.New("unadmitted autonomous Codex turn")
				}
				work.running[thread+":"+turnID] = true
				if work.awaiting > 0 {
					work.awaiting--
					s.markStarted(work)
				}
				work.queuePaused = false
				s.logger.Log("task_started", map[string]any{"task_id": work.taskID})
			}
		case "turn/completed":
			turn := rpcObject(params, "turn")
			key := thread + ":" + rpcString(turn, "id")
			modeled := work.running[key]
			delete(work.running, key)
			work.completed[key] = true
			if len(work.completed) > 256 {
				// Completed ids only suppress an acknowledgement arriving after
				// its completion notification; do not retain conversation history.
				work.completed = map[string]bool{key: true}
			}
			status := rpcString(turn, "status")
			if thread == work.thread && (status == "interrupted" || status == "failed") {
				work.goal = false
				work.queuePaused = true
			}
			if modeled {
				s.logger.Log("task_finished", map[string]any{"task_id": work.taskID, "status": logTurnStatus(status)})
			}
		case "thread/goal/updated":
			goal := rpcObject(params, "goal")
			// An active persisted goal on an idle loaded thread does not own a
			// slot until an explicit request has admitted its execution chain.
			if thread == work.thread {
				work.goal = rpcString(goal, "status") == "active" && work.lease != nil
			}
		case "thread/goal/cleared":
			if thread == work.thread {
				work.goal = false
			}
		case "thread/closed", "thread/deleted", "thread/archived":
			// Native shutdown has ended this thread's work. Shared child-agent
			// reservations remain until the other threads in that task end.
			if thread == work.thread {
				work.goal, work.realtime = false, false
			}
			for key := range work.running {
				if strings.HasPrefix(key, thread+":") {
					delete(work.running, key)
				}
			}
			work.queuePaused = true
		case "thread/realtime/closed":
			work.realtime = false
		case "thread/prediction/updated":
			if work.awaiting > 0 {
				work.awaiting--
			}
		}
		if err := writeRPC(s.stdout, message); err != nil {
			return err
		}
		return s.finish(work)
	}
	return writeRPC(s.stdout, message)
}

func (s *appSession) response(request *appRPCRequest, message rpcMessage) error {
	method, work := request.method, request.work
	result := rpcObject(message, "result")
	if method == "thread/start" || method == "thread/resume" || method == "thread/fork" {
		id := rpcString(rpcObject(result, "thread"), "id")
		if message["error"] == nil && id != "" {
			work := s.work(id)
			// An ordinary fork can share the review's source. Its response
			// disambiguates it before any explicit task is allowed to start.
			delete(s.reviewSources, id)
			if err := s.replayReviewEvents(id); err != nil {
				return err
			}
			return s.internal("thread/settings/update", rpcMessage{"threadId": rpcValue(id), "sandboxPolicy": externalSandbox(), "approvalPolicy": rpcValue("never")}, func(response rpcMessage) error {
				if response["error"] != nil {
					return s.rpcError(request.id, -32000, "real app-server cannot apply externalSandbox policy")
				}
				work.policyReady = true
				result["sandbox"], result["approvalPolicy"], result["activePermissionProfile"] = externalSandbox(), rpcValue("never"), rpcValue(nil)
				message["result"] = rpcValue(result)
				if err := writeRPC(s.stdout, message); err != nil {
					return err
				}
				return s.finish(work)
			})
		}
	}
	if work != nil {
		if method == "thread/shellCommand" && message["error"] != nil && work.shell > 0 {
			work.shell--
		}
		if method == "thread/goal/clear" && message["error"] == nil {
			work.goal = false
		}
		if method == "thread/goal/set" && message["error"] == nil {
			work.goal = rpcString(rpcObject(result, "goal"), "status") == "active" && work.lease != nil
		}
		if request.task {
			work.inflight--
			if method == "review/start" {
				s.dropDetached(work)
				if id := rpcString(result, "reviewThreadId"); id != "" {
					if placeholder := s.threads[id]; placeholder != nil && placeholder != work {
						work.queue, work.queuePaused = placeholder.queue, placeholder.queuePaused
						placeholder.queue = nil
						delete(s.works, placeholder)
					}
					work.thread = id
					s.threads[id] = work
					delete(s.reviewSources, id)
					if err := s.replayReviewEvents(id); err != nil {
						return err
					}
				}
			}
			turn := rpcObject(result, "turn")
			if turnID := rpcString(turn, "id"); turnID != "" {
				key := work.thread + ":" + turnID
				if !work.completed[key] {
					work.running[key] = true
				}
				if request.awaitStart && !request.started && work.awaiting > 0 {
					work.awaiting--
				}
			} else if message["error"] != nil {
				if request.awaitStart && !request.started && work.awaiting > 0 {
					work.awaiting--
				}
				if method == "thread/goal/set" {
					work.goal = request.previousGoal
				}
				if request.initial && method == "turn/start" && !request.started && len(work.running) == 0 {
					// A rejected explicit turn did not begin the persisted goal's
					// continuation chain, so it must not retain a fresh reservation.
					work.goal = false
				}
				if method == "thread/realtime/start" {
					work.realtime = request.previousRealtime
				}
			}
		}
	}
	if len(request.id) > 0 {
		if err := writeRPC(s.stdout, message); err != nil {
			return err
		}
	}
	if work != nil {
		return s.finish(work)
	}
	return nil
}

func (s *appSession) dropDetached(work *appWork) {
	for parent, pending := range s.detached {
		for i, candidate := range pending {
			if candidate == work {
				s.detached[parent] = append(pending[:i], pending[i+1:]...)
				break
			}
		}
	}
}

func (s *appSession) replayReviewEvents(thread string) error {
	events := s.reviewEvents[thread]
	delete(s.reviewEvents, thread)
	for _, line := range events {
		s.reviewEventSize -= len(line)
		if err := s.server(line); err != nil {
			return err
		}
	}
	return nil
}

func (s *appSession) finish(work *appWork) error {
	if work.pending == nil && len(work.running) == 0 && work.inflight == 0 && work.awaiting == 0 && !work.goal && !work.realtime {
		s.release(work)
		return s.startQueue(work, nil, "")
	}
	return nil
}

func (s *appSession) release(work *appWork) {
	if work.guard != nil {
		work.guard.release()
		work.guard = nil
	}
	if work.lease != nil {
		_ = work.lease.Close()
		work.lease = nil
		s.logger.Log("released", map[string]any{"task_id": work.taskID, "run_ms": time.Since(work.started).Milliseconds()})
	}
}

func (s *appSession) close() {
	for work := range s.works {
		if work.pending != nil {
			work.pending.cancel()
		}
		s.release(work)
	}
}

func logTurnStatus(status string) string {
	switch status {
	case "completed", "failed", "interrupted":
		return status
	}
	return "unknown"
}

func (s *appSession) admissionError(job *appAdmission, code int, reason string) error {
	if job.queueID != "" {
		job.work.queuePaused = true
		if len(job.message["id"]) == 0 {
			return s.notify("error", map[string]any{"threadId": job.work.thread, "error": map[string]any{"message": reason, "codexErrorInfo": "other"}, "willRetry": false})
		}
	}
	return s.rpcError(job.message["id"], code, reason)
}

func (s *appSession) markStarted(work *appWork) {
	var oldest *appRPCRequest
	for _, request := range s.requests {
		if request.work == work && request.awaitStart && !request.started && (oldest == nil || request.sequence < oldest.sequence) {
			oldest = request
		}
	}
	if oldest != nil {
		oldest.started = true
	}
}
