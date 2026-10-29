package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type appRPCRequest struct {
	authFile                string
	id                      json.RawMessage
	method                  string
	work                    *appWork
	cancelJob               *appAdmission
	callback                func(rpcMessage) error
	started                 bool
	task                    bool
	awaitStart              bool
	sequence                uint64
	previousRealtime        bool
	goalEpoch               uint64
	initial                 bool
	processHandle           string
	processExited           bool
	sentAt                  time.Time
	metadata                map[string]any
	taskID                  string
	threadEpoch             uint64
	thread                  string
	queueID                 string
	clientUserMessageID     string
	prediction              bool
	independent             bool
	sourceTurnID            string
	lifeGen                 uint64
	notificationPreferences map[string]bool
}

type appAdmission struct {
	authFile         string
	work             *appWork
	message          rpcMessage
	method           string
	queueID          string
	ctx              context.Context
	initial          bool
	cancel           context.CancelFunc
	started          time.Time
	taskID           string
	submissionID     string
	goalEpoch        uint64
	thread           string
	lifeGen          uint64
	predictionSource string
	independent      bool
	stage            string
	slotWaitMS       int64
	queueWaitMS      int64
	intervalStarted  time.Time
}

type appWork struct {
	thread           string
	lease            *Lease
	guard            *appTaskGuard
	taskID           string
	started          time.Time
	submitted        time.Time
	method           string
	waitMS           int64
	turnStarted      map[string]appTurnTiming
	pending          *appAdmission
	running          map[string]bool
	completed        map[string]bool
	inflight         int
	awaiting         int
	goal             bool
	realtime         bool
	goalThreads      map[string]bool
	realtimeThreads  map[string]bool
	inflightByThread map[string]int
	awaitingByThread map[string]int
	waits            map[*appRPCRequest]bool
	shellStarts      map[string]int
}

type appSession struct {
	ctx             context.Context
	dir             string
	load            ConfigLoader
	poll            time.Duration
	logger          *appLogger
	stdin           io.Writer
	stdout          io.Writer
	events          chan<- appServerEvent
	workers         sync.WaitGroup
	threads         map[string]*appWork
	queues          map[string]*appQueueState
	subs            map[string]*appThreadState
	goals           map[string]*appGoalState
	goalSequence    uint64
	policyReady     map[string]bool
	taskGen         map[string]uint64
	closedTasks     map[string]bool
	optOut          map[string]bool
	controlEpoch    uint64
	works           map[*appWork]bool
	requests        map[string]*appRPCRequest
	serverRequests  map[string]*appRPCRequest
	processes       map[string]*appRPCRequest
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
	authPool        *authPool
	authResets      map[string]int64
	authHome        *os.File
	authLimited     bool
	authRecords     sync.WaitGroup
}

func newAppSession(ctx context.Context, dir string, load ConfigLoader, poll time.Duration, logger *appLogger, stdin, stdout io.Writer, events chan<- appServerEvent) *appSession {
	return &appSession{ctx: ctx, dir: dir, load: load, poll: poll, logger: logger, stdin: stdin, stdout: stdout, events: events, stateStore: newStateStore(dir, Config{}, poll, nil),
		threads: make(map[string]*appWork), queues: make(map[string]*appQueueState), subs: make(map[string]*appThreadState), goals: make(map[string]*appGoalState), policyReady: make(map[string]bool), taskGen: make(map[string]uint64), closedTasks: make(map[string]bool), optOut: make(map[string]bool), works: make(map[*appWork]bool), requests: make(map[string]*appRPCRequest), serverRequests: make(map[string]*appRPCRequest), processes: make(map[string]*appRPCRequest), detached: make(map[string][]*appWork), reviewSources: make(map[string]string), reviewEvents: make(map[string][][]byte), nonce: newRequestID()}
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

// reopenThread clears the closed marker for an explicit client start/resume/
// fork while keeping any stale generation invalid for old in-flight replies.
func (s *appSession) reopenThread(thread string) {
	delete(s.closedTasks, thread)
	state := s.threadState(thread)
	state.loaded, state.closed = true, false
}

func (s *appSession) handle(event appServerEvent) error {
	switch event.source {
	case "admitted":
		return s.admitted(event)
	case "state_ready":
		return s.stateReady(event)
	case "guard":
		if event.err != nil {
			return event.err
		}
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
		if s.authPool != nil && request.authFile == "" {
			request.authFile = s.authPool.active()
		}
		s.sequence++
		request.sequence = s.sequence
		request.sentAt = time.Now()
		request.metadata = appMetadata(rpcObject(message, "params"))
		request.metadata["direction"] = "client_to_server"
		if request.work != nil && request.work.lease != nil {
			request.taskID = request.work.taskID
		}
		id := "codex-proxy:" + s.nonce + ":" + strconv.FormatUint(s.sequence, 10)
		message["id"] = rpcValue(id)
		s.requests[string(message["id"])] = request
	}
	return writeRPC(s.stdin, message)
}

func (s *appSession) forward(message rpcMessage, method string, work *appWork) error {
	var request *appRPCRequest
	if id := message["id"]; len(id) > 0 && string(id) != "null" {
		request = &appRPCRequest{id: append(json.RawMessage(nil), id...), method: method, work: work, thread: rpcString(rpcObject(message, "params"), "threadId")}
		if method == "process/spawn" {
			request.processHandle = rpcString(rpcObject(message, "params"), "processHandle")
		}
		if method == "thread/start" || method == "thread/resume" || method == "thread/fork" {
			// Capture the client intent order so a later unsubscribe or close
			// cannot be overwritten by this request's delayed success reply.
			request.threadEpoch = s.nextThreadEpoch()
			request.lifeGen = s.taskLifeGeneration(rpcString(rpcObject(message, "params"), "threadId"))
		}
		if method == "thread/goal/get" {
			request.goalEpoch = s.goalStateSeq(rpcString(rpcObject(message, "params"), "threadId"))
		}
	}
	return s.send(message, request)
}

// Controls bypass admission. Capture the waiting job, but only cancel it once
// native accepts the control; a rejected or stale control must not stop work.
func (s *appSession) forwardControl(message rpcMessage, method, thread string, epoch uint64) error {
	var request *appRPCRequest
	if id := message["id"]; len(id) > 0 && string(id) != "null" {
		work := s.work(thread)
		request = &appRPCRequest{id: append(json.RawMessage(nil), id...), method: method, work: work, goalEpoch: epoch, thread: thread, lifeGen: s.taskLifeGeneration(thread)}
		if job := work.pending; job != nil && job.thread == thread {
			if method == "turn/interrupt" ||
				(method == "thread/realtime/stop" && job.method == "thread/realtime/start") ||
				(job.method == "thread/goal/set" && (method == "thread/goal/clear" || (method == "thread/goal/set" && goalStopsWork(rpcObject(message, "params"))))) {
				request.cancelJob = job
			}
		}
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
	fields := map[string]any{"status": "completed", "local": true}
	logRPCID(fields, id)
	s.logger.Log("rpc_finished", fields)
	return writeRPC(s.stdout, rpcMessage{"id": id, "result": rpcValue(result)})
}

func (s *appSession) rpcError(id json.RawMessage, code int, message string) error {
	fields := map[string]any{"error_code": code, "error": message, "local": true}
	logRPCID(fields, id)
	s.logger.Log("rejected", fields)
	if len(id) == 0 || string(id) == "null" {
		return nil
	}
	return writeRPC(s.stdout, rpcMessage{"id": id, "error": rpcValue(map[string]any{"code": code, "message": message})})
}

func (s *appSession) notify(method string, params any) error {
	message := rpcMessage{"method": rpcValue(method), "params": rpcValue(params)}
	s.logNotificationError(message)
	return s.writeNotification(message)
}

func externalSandbox() json.RawMessage {
	return rpcValue(map[string]string{"type": "externalSandbox", "networkAccess": "enabled"})
}

func rpcBoolTrue(message rpcMessage, field string) bool {
	var value bool
	return json.Unmarshal(message[field], &value) == nil && value
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
		// Prediction forks start from the loaded parent's effective config and
		// reject every configuration override, including the proxy's sandbox
		// and approval settings. The post-response thread/settings/update path
		// still applies externalSandbox/never, so leave the request untouched
		// and let native validation handle any client-supplied overrides.
		prediction := method == "thread/fork" && rpcBoolTrue(params, "experimentalPredictionMode")
		if !prediction {
			var config rpcMessage
			if raw := params["config"]; len(raw) > 0 && json.Unmarshal(raw, &config) != nil {
				// Preserve malformed client input for native validation instead
				// of turning it into a valid thread creation request.
				return
			}
			if config == nil {
				config = make(rpcMessage)
			}
			delete(params, "permissions")
			params["sandbox"] = rpcValue("danger-full-access")
			params["approvalPolicy"] = rpcValue("never")
			config["sandbox_mode"] = rpcValue("danger-full-access")
			config["approval_policy"] = rpcValue("never")
			params["config"] = rpcValue(config)
		}
		// Native rejects deferGoalContinuation together with ephemeral. Only
		// persistent forks need the forced deferral; ephemeral forks keep the
		// client's value (or absence) for native validation.
		if method == "thread/fork" && !rpcBoolTrue(params, "ephemeral") {
			// A fork must not start an automatic goal before task admission.
			params["deferGoalContinuation"] = rpcValue(true)
		}
	default:
		return
	}
	message["params"] = rpcValue(params)
}

func modelTask(method string, params rpcMessage) bool {
	switch method {
	case "turn/start", "review/start", "thread/compact/start", "thread/queue/start", "thread/approveGuardianDeniedAction", "thread/prediction/request", "thread/realtime/start":
		return true
	}
	return false
}

func (s *appSession) client(line []byte) error {
	message, decodeErr := decodeRPC(line)
	if decodeErr != nil {
		// Codex owns protocol validation: forward the original line unchanged
		// and keep the actual decoding cause in the diagnostic. Only the write
		// failure affects protocol forwarding, as before.
		s.logger.Log("error", map[string]any{"stage": "client_protocol", "error": decodeErr.Error(), "message_bytes": len(line)})
		_, err := s.stdin.Write(append(line, '\n'))
		return err
	}
	method := rpcString(message, "method")
	if method == "" {
		// Responses to server-initiated approvals/tools retain the server's id.
		if request := s.serverRequests[string(message["id"])]; request != nil {
			s.logRPCFinished(request, message)
		}
		delete(s.serverRequests, string(message["id"]))
		return writeRPC(s.stdin, message)
	}
	if s.authPool != nil && (method == "account/login/start" || method == "account/logout") {
		return s.rpcError(message["id"], -32602, "account authentication is managed by CODEX_PROXY_AUTH_DIR")
	}
	if method == "remoteControl/enable" || method == "remoteControl/pairing/start" {
		return s.rpcError(message["id"], -32602, "remote task ingress is unavailable through the stdio proxy")
	}
	var params rpcMessage
	if json.Unmarshal(message["params"], &params) != nil || params == nil {
		// Native owns parameter validation. In particular, policy overlays
		// must not replace a scalar/array/null params value with an object.
		return s.forward(message, method, nil)
	}
	thread := rpcString(params, "threadId")
	if method == "initialize" {
		return s.initializeClient(message, params)
	}
	if method == "thread/unsubscribe" {
		return s.unsubscribeThread(message["id"], thread)
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
	if thread != "" && (method == "turn/interrupt" || method == "thread/realtime/stop") {
		return s.forwardControl(message, method, thread, 0)
	}
	if thread != "" && (method == "thread/goal/set" || method == "thread/goal/clear") {
		work := s.work(thread)
		if method == "thread/goal/clear" || goalStopsWork(params) {
			epoch := s.reserveGoalControl(thread)
			return s.forwardControl(message, method, thread, epoch)
		} else if s.goalSetMode(thread, params, work) == goalSetRun {
			if s.reviewSources[thread] != "" {
				return s.rpcError(message["id"], -32000, "detached review thread is still starting")
			}
			if len(message["id"]) == 0 || string(message["id"]) == "null" {
				return s.rpcError(rpcValue(nil), -32600, "task methods require a request id")
			}
			if work.pending != nil {
				return s.rpcError(message["id"], -32000, "thread already has a task waiting for a slot")
			}
			epoch := s.reserveGoalControl(thread)
			return s.begin(work, message, method, "", epoch)
		} else {
			// A safe control patch is forwarded even while an activation is
			// waiting; it must not cancel that activation.
			epoch := s.reserveGoalControl(thread)
			enforceAppPolicy(message)
			return s.forwardControl(message, method, thread, epoch)
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
			return s.begin(work, message, method, "", 0)
		}
		if work.pending != nil {
			return s.rpcError(message["id"], -32000, "thread already has a task waiting for a slot")
		}
		return s.begin(work, message, method, "", 0)
	}
	enforceAppPolicy(message)
	var work *appWork
	if thread != "" {
		work = s.work(thread)
	}
	if method == "thread/shellCommand" && work != nil && !work.hasRunning(thread) {
		if work.shellStarts == nil {
			work.shellStarts = make(map[string]int)
		}
		work.shellStarts[thread]++
	}
	return s.forward(message, method, work)
}

func (s *appSession) begin(work *appWork, message rpcMessage, method, queueID string, goalEpoch uint64) error {
	if s.authPool != nil {
		if err := s.checkAuth(); err != nil {
			if queueID != "" {
				s.queue(work.thread).paused = true
			}
			return s.admissionError(&appAdmission{message: message, queueID: queueID, thread: work.thread}, authErrorCode(err), err.Error())
		}
	}
	waiting := 0
	for existing := range s.works {
		if existing.pending != nil {
			waiting++
		}
	}
	if waiting >= 128 {
		return s.rpcError(message["id"], -32000, "too many tasks waiting for slots")
	}
	thread := rpcString(rpcObject(message, "params"), "threadId")
	if thread == "" {
		thread = work.thread
	}
	if s.closedTasks[thread] {
		return s.rpcError(message["id"], -32800, "thread is closed; resume it before starting a task")
	}
	ctx, cancel := context.WithCancel(s.ctx)
	job := &appAdmission{work: work, message: message, method: method, queueID: queueID, ctx: ctx, initial: work.lease == nil, cancel: cancel, started: time.Now(), goalEpoch: goalEpoch, thread: thread, lifeGen: s.taskLifeGeneration(thread)}
	job.independent = method == "review/start" && rpcString(rpcObject(message, "params"), "delivery") == "detached"
	job.taskID, job.submissionID, job.stage = work.taskID, newRequestID(), "slot"
	if job.initial {
		job.taskID = newRequestID()
	}
	if entry := queuedInput(s.queueState(job.thread), queueID); entry != nil {
		job.queueWaitMS = time.Since(entry.enqueued).Milliseconds()
	}
	work.pending = job
	s.logger.Log("task_received", job.logFields())
	if s.authPool != nil {
		job.authFile = s.authPool.active()
	}
	return s.startAdmission(job)
}

func (s *appSession) startAdmission(job *appAdmission) error {
	ctx := job.ctx
	job.stage = "slot"
	if !job.initial {
		return s.prepareAdmission(job)
	}
	// Snapshot metadata before the worker starts; cancellation may mutate work.
	waitFields := job.logFields()
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		limiter := Limiter{Dir: s.dir, Load: s.load, PollInterval: s.poll, OnWait: func(cfg Config, elapsed time.Duration) {
			waitFields["limit"], waitFields["wait_timeout_ms"] = cfg.MaxConcurrency, cfg.WaitTimeout.Milliseconds()
			waitFields["wait_ms"], waitFields["slot_wait_ms"] = elapsed.Milliseconds()+job.queueWaitMS, elapsed.Milliseconds()
			s.logger.Log("waiting", waitFields)
		}}
		lease, cfg, err := limiter.Acquire(ctx)
		select {
		case s.events <- appServerEvent{source: "admitted", job: job, lease: lease, cfg: cfg, err: err}:
		case <-s.ctx.Done():
			if lease != nil {
				if err := lease.Close(); err != nil {
					s.logCleanupJobError("close undelivered task slot", job, err)
				}
			}
		}
	}()
	return nil
}

func (s *appSession) admitted(event appServerEvent) error {
	job, work := event.job, event.job.work
	if work.pending != job || s.ctx.Err() != nil || !s.taskGenerationCurrent(job.thread, job.lifeGen) {
		if event.lease != nil {
			if err := event.lease.Close(); err != nil {
				s.logCleanupError("close stale task slot", work, err)
			}
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
		// Keep the stable stage text and code while returning and logging the
		// actual admission/configuration cause.
		reason = stageErrorText(reason, event.err)
		work.pending = nil
		job.cancel()
		job.slotWaitMS = time.Since(job.started).Milliseconds()
		fields := job.logFields()
		fields["error"], fields["error_code"], fields["limit"] = reason, code, event.cfg.MaxConcurrency
		s.logger.Log(eventName, fields)
		if err := s.endAdmission(job); err != nil {
			return err
		}
		return s.admissionError(job, code, reason)
	}
	work.lease = event.lease
	work.taskID, work.started = job.taskID, time.Now()
	work.method, work.submitted = job.method, time.Time{}
	work.waitMS = time.Since(job.started).Milliseconds() + job.queueWaitMS
	job.slotWaitMS = time.Since(job.started).Milliseconds()
	guard, err := s.startGuard(event.lease)
	if err != nil {
		reason := stageErrorText("cannot start task lock guardian", err)
		work.pending = nil
		job.cancel()
		fields := job.logFields()
		fields["stage"], fields["error"] = "guard", reason
		s.logger.Log("error", fields)
		if err := s.endAdmission(job); err != nil {
			return err
		}
		return s.admissionError(job, -32000, reason)
	}
	work.guard = guard
	fields := job.logFields()
	fields["limit"], fields["wait_timeout_ms"] = event.cfg.MaxConcurrency, event.cfg.WaitTimeout.Milliseconds()
	s.logger.Log("admitted", fields)
	job.stage = "policy"
	if s.policyReady[job.thread] {
		return s.prepareAdmission(job)
	}
	return s.internal("thread/settings/update", rpcMessage{"threadId": rpcValue(job.thread), "sandboxPolicy": externalSandbox(), "approvalPolicy": rpcValue("never")}, func(response rpcMessage) error {
		if work.pending != job || !s.taskGenerationCurrent(job.thread, job.lifeGen) {
			return nil
		}
		if nativeErr := nativeRPCError(response); nativeErr != nil {
			reason := stageErrorText("real app-server cannot apply externalSandbox policy", nativeErr)
			work.pending = nil
			job.cancel()
			fields := job.logFields()
			fields["error"] = reason
			s.logger.Log("error", fields)
			if err := s.endAdmission(job); err != nil {
				return err
			}
			return s.admissionError(job, -32000, reason)
		}
		s.policyReady[job.thread] = true
		return s.prepareAdmission(job)
	})
}

func (s *appSession) prepareAdmission(job *appAdmission) error {
	if job.method != "turn/start" {
		return s.dispatchAdmission(job)
	}
	// A resumed/forked thread can carry an active persisted goal. Its next
	// explicit turn also owns all automatic continuations, even when this
	// connection has not received a goal update yet. Use the actual target id:
	// an appWork can be shared with a parent or subagent.
	thread := job.thread
	epoch := s.goalStateSeq(thread)
	return s.internal("thread/goal/get", rpcMessage{"threadId": rpcValue(thread)}, func(response rpcMessage) error {
		if job.work.pending != job || !s.taskGenerationCurrent(thread, job.lifeGen) {
			return nil
		}
		s.applyGoalGet(thread, epoch, response)
		if state := s.goals[thread]; state != nil && state.known {
			job.work.setGoal(thread, state.status == "active" && job.work.lease != nil)
		}
		return s.dispatchAdmission(job)
	})
}

func (s *appSession) dispatchAdmission(job *appAdmission) error {
	// Policy/goal preparation has completed and the work already owns a slot.
	// Only this worker sleeps; the protocol loop stays responsive to queries,
	// approvals, interruption, and cancellation while holding the reservation.
	job.stage, job.intervalStarted = "task_interval", time.Now()
	s.logger.Log("waiting", job.logFields())
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		state, err := s.stateStore.Acquire(job.ctx)
		select {
		case s.events <- appServerEvent{source: "state_ready", job: job, state: state, err: err}:
		case <-s.ctx.Done():
			if state != nil {
				if err := state.Close(); err != nil {
					s.logCleanupJobError("close undelivered state lease", job, err)
				}
			}
		}
	}()
	return nil
}

func (s *appSession) stateReady(event appServerEvent) error {
	job, work := event.job, event.job.work
	if event.state != nil {
		defer func() {
			if err := event.state.Close(); err != nil {
				s.logCleanupError("close state lease", job.work, err)
			}
		}()
	}
	if work.pending != job || s.ctx.Err() != nil || !s.taskGenerationCurrent(job.thread, job.lifeGen) {
		return nil
	}
	err := event.err
	if err == nil && s.authPool != nil {
		err = s.authPool.checkCurrent(event.state.state)
		if errors.Is(err, errNoAuthAccount) {
			s.authLimited = true
		}
	}
	if err == nil {
		err = event.state.Record(job.ctx)
	}
	if err != nil {
		work.pending = nil
		job.cancel()
		if job.initial {
			work.setGoal(job.thread, false)
		}
		if err := s.endAdmission(job); err != nil {
			return err
		}
		code, reason := -32000, "cannot apply Codex task submission interval"
		if errors.Is(err, context.Canceled) {
			code, reason = -32800, "Codex task canceled before execution"
		} else if errors.Is(err, errNoAuthAccount) {
			code, reason = 429, "Codex account unavailable before task submission"
		}
		reason = stageErrorText(reason, err)
		fields := job.logFields()
		fields["error"], fields["error_code"] = reason, code
		eventName := "error"
		if code == -32800 {
			eventName = "canceled"
		}
		s.logger.Log(eventName, fields)
		return s.admissionError(job, code, reason)
	}
	work.pending = nil
	job.cancel()
	if job.queueID != "" {
		entry := queuedInput(s.queueState(job.thread), job.queueID)
		if entry == nil {
			s.release(work)
			return s.rpcError(job.message["id"], -32800, "queued submission was removed")
		}
		params := rpcObject(job.message, "params")
		params["input"], params["clientUserMessageId"] = entry.Input, rpcValue(entry.ClientUserMessageID)
		job.message["params"] = rpcValue(params)
	}
	if work.submitted.IsZero() {
		work.submitted = time.Now()
		work.waitMS = time.Since(job.started).Milliseconds() + job.queueWaitMS
	}
	fields := job.logFields()
	fields["stage"] = "submitted"
	s.logger.Log("submitted", fields)
	return s.dispatch(work, job)
}

func (s *appSession) dispatch(work *appWork, job *appAdmission) error {
	message, method := job.message, job.method
	enforceAppPolicy(message)
	thread := job.thread
	if method == "thread/goal/set" {
		// Safe patches may finish while activation waits for admission or the
		// submission interval. Order this mutation when it is actually sent.
		job.goalEpoch = s.reserveGoalControl(thread)
	}
	work.addInflight(thread, 1)
	previousRealtime := work.realtimeThreads[thread]
	awaitStart := false
	// inflight retains a goal activation's reservation until its ACK. Only a
	// successful native result/notification may mark an automatic goal chain.
	if method == "thread/realtime/start" {
		work.setRealtime(thread, true)
	} else if method != "thread/goal/set" && !(method == "turn/start" && work.hasRunning(thread)) {
		awaitStart = true
	}
	if method == "review/start" && rpcString(rpcObject(message, "params"), "delivery") == "detached" {
		s.detached[work.thread] = append(s.detached[work.thread], work)
	}
	request := &appRPCRequest{id: append(json.RawMessage(nil), message["id"]...), method: method, work: work, thread: thread, task: true, awaitStart: awaitStart, previousRealtime: previousRealtime, initial: job.initial, goalEpoch: job.goalEpoch, queueID: job.queueID, lifeGen: job.lifeGen, prediction: method == "thread/prediction/request", independent: job.independent}
	if job.queueID != "" {
		request.clientUserMessageID = rpcString(rpcObject(message, "params"), "clientUserMessageId")
	}
	if method == "thread/prediction/request" {
		request.sourceTurnID = rpcString(rpcObject(message, "params"), "sourceTurnId")
	}
	if awaitStart {
		if err := s.send(message, request); err != nil {
			return err
		}
		work.registerWait(request)
		return nil
	}
	return s.send(message, request)
}

func (s *appSession) cancelAdmission(work *appWork) error {
	job := work.pending
	if job == nil {
		return nil
	}
	work.pending = nil
	job.cancel()
	s.queue(job.thread).paused = true
	if job.initial && work.lease != nil && !work.hasRunning(job.thread) {
		work.setGoal(job.thread, false)
	}
	if err := s.endAdmission(job); err != nil {
		return err
	}
	fields := job.logFields()
	fields["reason"] = "client_request"
	s.logger.Log("canceled", fields)
	return s.rpcError(job.message["id"], -32800, "Codex task canceled before execution")
}

func (s *appSession) server(line []byte) error {
	message, err := decodeRPC(line)
	if err != nil {
		return fmt.Errorf("real app-server emitted invalid JSON: %w", err)
	}
	method := rpcString(message, "method")
	// Observe the account signal before a response deletes its request. The
	// shutdown drain calls the same helper, so a 429 that arrives while native
	// is stopping cannot be lost with the request mapping.
	s.observeAuthMessage(message)
	if id := message["id"]; method != "" && len(id) > 0 && string(id) != "null" {
		request := &appRPCRequest{id: id, method: method, sentAt: time.Now(), metadata: appMetadata(rpcObject(message, "params"))}
		request.metadata["direction"] = "server_to_client"
		s.serverRequests[string(id)] = request
	}
	if method == "" && len(message["id"]) > 0 {
		request := s.requests[string(message["id"])]
		if request != nil {
			delete(s.requests, string(message["id"]))
			s.logRPCFinished(request, message)
			if request.callback != nil {
				return request.callback(message)
			}
			message["id"] = request.id
			return s.response(request, message)
		}
	}
	params := rpcObject(message, "params")
	if s.authPool != nil && (method == "error" || method == "turn/completed" || method == "thread/realtime/error") && s.authLimited {
		// Other notifications can carry MCP/OAuth/tool failures. They do not
		// describe the GPT account used by this native model task.
		s.logNotificationError(message)
		return s.writeNotification(message)
	}
	if method == "serverRequest/resolved" {
		if request := s.serverRequests[string(params["requestId"])]; request != nil {
			fields := request.logFields()
			fields["run_ms"], fields["status"] = time.Since(request.sentAt).Milliseconds(), "resolved"
			s.logger.Log("rpc_finished", fields)
		}
		delete(s.serverRequests, string(params["requestId"]))
	}
	if method == "process/exited" {
		handle := rpcString(params, "processHandle")
		process := s.processes[handle]
		delete(s.processes, handle)
		// A fast process can exit before its spawn response arrives. Keep
		// that response from resurrecting the completed process; requests
		// submitted later may still reuse the same handle.
		for _, pending := range s.requests {
			if pending.method == "process/spawn" && pending.processHandle == handle {
				if !pending.processExited && (process == nil || pending.sequence < process.sequence) {
					process = pending
				}
				pending.processExited = true
			}
		}
		if process != nil {
			s.logProcessFinished(process, params, "exited")
		}
	}
	thread := rpcString(params, "threadId")
	if method == "thread/started" {
		info := rpcObject(params, "thread")
		id := rpcString(info, "id")
		if id != "" && s.closedTasks[id] {
			// A closed id can still receive a late start frame. Binding it to a
			// live parent's shared work would revive the closed execution chain.
			s.logNotificationError(message)
			return s.writeNotification(message)
		}
		parent := rpcString(info, "forkedFromId")
		if pending := s.detached[parent]; len(pending) > 0 {
			// Multiple reviews or an ordinary fork can emit indistinguishable
			// thread/started notifications. Bind the reservation using the
			// review response's authoritative reviewThreadId, not arrival order.
			s.reviewSources[id] = parent
			s.threadStarted(id)
			s.work(id)
		} else if parent := rpcString(info, "parentThreadId"); parent != "" && s.threads[parent] != nil && s.threads[parent].lease != nil {
			// Native subagents belong to their submitting task, like codex exec.
			s.threadStarted(id)
			s.threads[id] = s.threads[parent]
		} else if id != "" {
			s.threadStarted(id)
			s.work(id)
		}
	}
	if method == "thread/status/changed" {
		status := rpcString(rpcObject(params, "status"), "type")
		s.threadStatusChanged(thread, status)
		if status == "notLoaded" && thread != "" {
			if err := s.closeThread(s.work(thread), thread); err != nil {
				return err
			}
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
		if s.closedTasks[thread] {
			// A closed thread's slot and task state are already gone. Forward
			// late lifecycle events but do not let them revive running turns,
			// waits, or goal caches; an explicit reopen clears this marker.
			switch method {
			case "turn/started", "turn/completed", "thread/goal/updated", "thread/goal/cleared", "thread/realtime/closed", "thread/prediction/updated":
				s.logNotificationError(message)
				return s.writeNotification(message)
			}
		}
		switch method {
		case "turn/started":
			turnID := rpcString(rpcObject(params, "turn"), "id")
			key := thread + ":" + turnID
			if _, exists := work.running[key]; exists || work.completed[key] {
				// ACKs and repeated notifications may identify an existing turn;
				// they must not consume a later operation's start wait.
				break
			}
			if work.shellStarts[thread] > 0 && !work.hasRunning(thread) {
				work.shellStarts[thread]--
				if work.shellStarts[thread] == 0 {
					delete(work.shellStarts, thread)
				}
				work.running[thread+":"+turnID] = false
			} else {
				if work.lease == nil {
					return errors.New("unadmitted autonomous Codex turn")
				}
				work.running[thread+":"+turnID] = true
				work.completeTurnWait(thread)
				s.queue(thread).paused = false
			}
			s.logTurnStarted(work, thread, turnID, work.running[thread+":"+turnID])
		case "turn/completed":
			turn := rpcObject(params, "turn")
			key := thread + ":" + rpcString(turn, "id")
			s.logTurnFinished(work, key, logTurnStatus(rpcString(turn, "status")), "")
			delete(work.running, key)
			work.completed[key] = true
			if len(work.completed) > 256 {
				// Completed ids only suppress an acknowledgement arriving after
				// its completion notification; do not retain conversation history.
				work.completed = map[string]bool{key: true}
			}
			status := rpcString(turn, "status")
			if status == "interrupted" || status == "failed" {
				work.setGoal(thread, false)
				s.queue(thread).paused = true
			}
		case "thread/goal/updated":
			goal := rpcObject(params, "goal")
			s.applyGoalNotification(thread, goal)
			// An active persisted goal on an idle loaded thread does not own a
			// slot until an explicit request has admitted its execution chain.
			work.setGoal(thread, rpcString(goal, "status") == "active" && work.lease != nil)
		case "thread/goal/cleared":
			s.applyGoalCleared(thread)
			work.setGoal(thread, false)
		case "thread/closed", "thread/deleted", "thread/archived":
			// Native shutdown has ended this thread's work. Shared child-agent
			// reservations remain until the other threads in that task end.
			if err := s.closeThread(work, thread); err != nil {
				return err
			}
		case "thread/realtime/closed":
			work.setRealtime(thread, false)
		case "thread/prediction/updated":
			result := rpcString(rpcObject(params, "result"), "type")
			if result == "completed" || result == "failed" {
				work.completePredictionWait(thread, rpcString(params, "sourceTurnId"))
			}
		}
		s.logNotificationError(message)
		if err := s.writeNotification(message); err != nil {
			return err
		}
		return s.finish(work)
	}
	s.logNotificationError(message)
	return s.writeNotification(message)
}

func (s *appSession) response(request *appRPCRequest, message rpcMessage) error {
	method, work := request.method, request.work
	result := rpcObject(message, "result")
	if message["error"] == nil && work != nil && s.taskGenerationCurrent(request.thread, request.lifeGen) {
		if method == "turn/interrupt" {
			s.queue(request.thread).paused = true
		}
		if request.cancelJob != nil && work.pending == request.cancelJob {
			if err := s.cancelAdmission(work); err != nil {
				return err
			}
		}
	}
	if method == "thread/unsubscribe" {
		state := s.threadState(request.thread)
		if state.epoch == request.threadEpoch {
			if message["error"] != nil {
				// A failed native unsubscribe may be retried. A newer resume or
				// close owns its own subscription state and must not be undone.
				state.subscribed = true
			} else if rpcString(result, "status") == "notLoaded" {
				work = s.threads[request.thread]
				if err := s.closeThread(work, request.thread); err != nil {
					return err
				}
			}
		}
		if len(request.id) == 0 && message["error"] != nil {
			return s.notify("error", map[string]any{"threadId": request.thread, "turnId": "", "willRetry": false, "error": rpcObject(message, "error")})
		}
	}
	if method == "initialize" && message["error"] == nil {
		s.optOut = request.notificationPreferences
	}
	if method == "process/spawn" && message["error"] == nil && !request.processExited {
		// Only a successful spawn owns a live handle. A rejected duplicate
		// must not clear the existing process, and process/kill's reply does
		// not end its lifetime: wait for process/exited.
		s.processes[request.processHandle] = request
		s.logger.Log("process_started", request.logFields())
	}
	if method == "thread/start" || method == "thread/resume" || method == "thread/fork" {
		id := rpcString(rpcObject(result, "thread"), "id")
		if message["error"] == nil && id != "" {
			// Resume reopens a known destination, provided no close occurred
			// after submission. Start/fork create a fresh destination whose
			// generation is independent of the source's generation.
			stale := s.taskLifeGeneration(id) != 0 || s.closedTasks[id]
			if method == "thread/resume" {
				// With path/history, native may ignore the requested threadId.
				// Compare the actual destination's last close with submission,
				// retaining that close marker across subsequent reopen/unsubscribe.
				stale = s.threadState(id).closedEpoch > request.threadEpoch
			}
			if stale {
				// The destination was closed after this request was sent. The
				// reply must not reopen it or restore a subscription.
				return s.rpcError(request.id, -32800, "thread closed before initialization completed")
			}
			// The client's own start/resume/fork restores its subscription even
			// after an earlier unsubscribe, unless a newer control intervened.
			s.reopenThread(id)
			s.threadCreated(id, request.threadEpoch)
			request.lifeGen = s.taskLifeGeneration(id)
			work := s.work(id)
			// An ordinary fork can share the review's source. Its response
			// disambiguates it before any explicit task is allowed to start.
			delete(s.reviewSources, id)
			if err := s.replayReviewEvents(id); err != nil {
				return err
			}
			return s.internal("thread/settings/update", rpcMessage{"threadId": rpcValue(id), "sandboxPolicy": externalSandbox(), "approvalPolicy": rpcValue("never")}, func(response rpcMessage) error {
				if !s.taskGenerationCurrent(id, request.lifeGen) {
					return s.rpcError(request.id, -32800, "thread closed before initialization completed")
				}
				if nativeErr := nativeRPCError(response); nativeErr != nil {
					reason := stageErrorText("real app-server cannot apply externalSandbox policy", nativeErr)
					fields := request.logFields()
					fields["stage"], fields["error"] = "policy", reason
					s.logger.Log("error", fields)
					return s.rpcError(request.id, -32000, reason)
				}
				s.policyReady[id] = true
				result["sandbox"], result["approvalPolicy"], result["activePermissionProfile"] = externalSandbox(), rpcValue("never"), rpcValue(nil)
				message["result"] = rpcValue(result)
				epoch := s.goalStateSeq(id)
				// Best-effort goal cache initialization. Ephemeral threads do not
				// support goals (native returns an error); that must not fail the
				// successful thread creation.
				return s.internal("thread/goal/get", rpcMessage{"threadId": rpcValue(id)}, func(goalResponse rpcMessage) error {
					if !s.taskGenerationCurrent(id, request.lifeGen) {
						return s.rpcError(request.id, -32800, "thread closed before initialization completed")
					}
					s.applyGoalGet(id, epoch, goalResponse)
					if err := writeRPC(s.stdout, message); err != nil {
						return err
					}
					return s.finish(work)
				})
			})
		}
	}
	// Reads must match the newest intent; mutations are ordered against the
	// latest confirmed native state. Failed controls never create or roll back
	// an automatic chain, including when a newer budget patch is pending.
	if method == "thread/goal/get" {
		s.applyGoalGet(request.thread, request.goalEpoch, message)
	}
	if method == "thread/goal/clear" && s.applyGoalClearResult(request.thread, request.goalEpoch, message) && work != nil {
		work.setGoal(request.thread, false)
	}
	if method == "thread/goal/set" && s.applyGoalSetResult(request.thread, request.goalEpoch, message) && work != nil {
		work.setGoal(request.thread, s.goalState(request.thread).status == "active" && work.lease != nil)
	}
	// An independent detached review is its own task; source close must not
	// make its ACK stale. After rebinding, the review thread's generation is
	// the one that matters.
	target := request.thread
	// Close invalidates this request's generation, except for an independent
	// detached review: its source close must not cancel the review task. After
	// binding, the authoritative review destination is checked instead.
	closed := work != nil && !request.independent && !s.taskGenerationCurrent(target, request.lifeGen)
	if work != nil {
		if method == "thread/shellCommand" && message["error"] != nil && work.shellStarts[request.thread] > 0 {
			work.shellStarts[request.thread]--
			if work.shellStarts[request.thread] == 0 {
				delete(work.shellStarts, request.thread)
			}
		}
		if request.task && !closed {
			work.addInflight(request.thread, -1)
			if method == "review/start" {
				s.dropDetached(work)
				if id := rpcString(result, "reviewThreadId"); id != "" {
					// The wait was registered against the source. Resolve it
					// before binding/replaying the independent destination.
					work.completeWait(request)
					if placeholder := s.threads[id]; placeholder != nil && placeholder != work {
						// Queue state is keyed by thread id, not by work, so the
						// destination's queue already survives the rebinding.
						delete(s.works, placeholder)
					}
					work.thread = id
					s.threads[id] = work
					delete(s.reviewSources, id)
					if err := s.replayReviewEvents(id); err != nil {
						return err
					}
					target = id
					// Re-check the authoritative destination. The review ACK
					// itself is valid independent of the source generation.
					if s.closedTasks[id] {
						closed = true
					}
				}
			}
			turn := rpcObject(result, "turn")
			if turnID := rpcString(turn, "id"); turnID != "" && !closed {
				// Use the actual target thread: child-agent ACKs can share the
				// parent's appWork.
				key := target + ":" + turnID
				if !work.completed[key] {
					work.running[key] = true
					s.logTurnStarted(work, target, turnID, true)
				}
				// An ACK containing the turn also resolves its start wait.
				work.completeWait(request)
			} else if message["error"] != nil {
				started := request.started
				work.completeWait(request)
				if request.initial && method == "turn/start" && !started && !work.hasRunning(request.thread) {
					// A rejected explicit turn did not begin the persisted goal's
					// continuation chain, so it must not retain a fresh reservation.
					work.setGoal(request.thread, false)
				}
				if method == "thread/realtime/start" {
					work.setRealtime(request.thread, request.previousRealtime)
				}
			}
		}
	}
	queuedFailure := !closed && request.queueID != "" && message["error"] != nil
	if queuedFailure {
		// Keep the failed input at the head of the queue; only pause it.
		s.queue(target).paused = true
	}
	if !closed && request.queueID != "" && message["error"] == nil {
		s.removeQueued(target, request.queueID)
	}
	if len(request.id) > 0 {
		if err := writeRPC(s.stdout, message); err != nil {
			return err
		}
	}
	if work != nil {
		if queuedFailure && len(request.id) == 0 {
			// Automatic submissions have no client id. Report a correlated error
			// and pause the remaining queue without dropping the failed input.
			if err := s.notifyQueueFailure(request, message); err != nil {
				return err
			}
		}
		if request.queueID != "" {
			if err := s.queueChanged(target); err != nil {
				return err
			}
		}
		return s.finish(work)
	}
	return nil
}

// notifyQueueFailure reports an automatic queued submission rejection. The
// client keeps its correlation ids and native error details so the failure can
// be diagnosed without dropping the failed entry or resuming the queue.
func (s *appSession) notifyQueueFailure(request *appRPCRequest, response rpcMessage) error {
	stage := "queued submission was rejected by app-server"
	errorInfo := map[string]any{"message": stage, "codexErrorInfo": "other"}
	if raw := rpcErrorPayload(response); raw != nil {
		nativeErr := rpcErrorObject(raw)
		errorInfo["message"] = stageErrorText(stage, errors.New(nativeRPCErrorText(raw)))
		errorInfo["error"] = json.RawMessage(raw)
		if code := nativeErr["code"]; len(code) > 0 && string(code) != "null" {
			errorInfo["code"] = json.RawMessage(code)
		}
		if data := nativeErr["data"]; len(data) > 0 && string(data) != "null" {
			errorInfo["data"] = json.RawMessage(data)
		}
	}
	// The generated error notification is recorded by logNotificationError,
	// which also includes the queued submission metadata.
	return s.notify("error", map[string]any{
		"threadId":            request.thread,
		"turnId":              "",
		"willRetry":           false,
		"queuedSubmissionId":  request.queueID,
		"clientUserMessageId": request.clientUserMessageID,
		"error":               errorInfo,
	})
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
	for thread, state := range s.subs {
		if state.unsubscribePending && !s.threadHasWork(thread) {
			if err := s.forwardUnsubscribe(nil, thread); err != nil {
				return err
			}
		}
	}
	if work.pending == nil && len(work.running) == 0 && work.inflight == 0 && work.awaiting == 0 && !work.goal && !work.realtime {
		s.release(work)
		if thread, queueID := s.nextQueue(work); thread != "" {
			return s.startQueue(work, thread, nil, queueID)
		}
	}
	return nil
}

func (s *appSession) release(work *appWork) {
	if work.guard != nil {
		if err := work.guard.release(); err != nil {
			s.logCleanupError("release task lock guardian", work, err)
		}
		work.guard = nil
	}
	if work.lease != nil {
		if err := work.lease.Close(); err != nil {
			s.logCleanupError("close task slot", work, err)
		}
		work.lease = nil
		runMS := int64(0)
		if !work.submitted.IsZero() {
			runMS = time.Since(work.submitted).Milliseconds()
		}
		s.logger.Log("released", map[string]any{"task_id": work.taskID, "thread_id": work.thread, "method": work.method, "wait_ms": work.waitMS, "run_ms": runMS, "held_ms": time.Since(work.started).Milliseconds()})
	}
}

// logCleanupError reports a real cleanup failure without changing the
// non-fatal release order or the task's lifetime.
func (s *appSession) logCleanupError(stage string, work *appWork, err error) {
	fields := map[string]any{"stage": stage, "error": err.Error()}
	if work != nil {
		fields["task_id"], fields["thread_id"], fields["method"] = work.taskID, work.thread, work.method
	}
	s.logger.Log("error", fields)
}

// logCleanupJobError is the worker-safe variant: an admission's identity
// fields are fixed before its goroutine starts, while appWork fields may be
// updated by the protocol loop concurrently.
func (s *appSession) logCleanupJobError(stage string, job *appAdmission, err error) {
	fields := map[string]any{"stage": stage, "error": err.Error()}
	if job != nil {
		fields["task_id"], fields["thread_id"], fields["method"], fields["submission_id"] = job.taskID, job.thread, job.method, job.submissionID
	}
	s.logger.Log("error", fields)
}

func (s *appSession) close() {
	defer func() {
		if s.authHome != nil {
			if err := releaseAuthHome(s.authHome); err != nil {
				s.logCleanupError("close authentication lease", nil, err)
			}
			s.authHome = nil
		}
	}()
	for work := range s.works {
		if work.pending != nil {
			work.pending.cancel()
			fields := work.pending.logFields()
			fields["reason"] = "session_closed"
			s.logger.Log("canceled", fields)
		}
		for key := range work.turnStarted {
			s.logTurnFinished(work, key, "canceled", "session_closed")
		}
		s.release(work)
	}
	for _, requests := range []map[string]*appRPCRequest{s.requests, s.serverRequests} {
		for _, request := range requests {
			fields := request.logFields()
			fields["status"], fields["reason"], fields["run_ms"] = "canceled", "session_closed", time.Since(request.sentAt).Milliseconds()
			s.logger.Log("rpc_finished", fields)
		}
	}
	for _, process := range s.processes {
		s.logProcessFinished(process, nil, "session_closed")
	}
}

func logTurnStatus(status string) string {
	switch status {
	case "completed", "failed", "interrupted":
		return status
	}
	return "unknown"
}

// endAdmission finishes a canceled or failed admission that will never run.
// It pauses only the job's own thread queue (a child may share the work with
// its parent), releases the shared slot once no other thread still owns
// running turns, waits, goals, or realtime, and then lets finish schedule any
// other eligible queue. Call it after clearing work.pending and before the
// error reply so a rescheduled entry can never be the failed one.
func (s *appSession) endAdmission(job *appAdmission) error {
	if job == nil || job.work == nil {
		return nil
	}
	s.queue(job.thread).paused = true
	work := job.work
	if job.initial && work.lease != nil && !work.hasRunning(job.thread) {
		work.setGoal(job.thread, false)
	}
	return s.finish(work)
}

func (s *appSession) admissionError(job *appAdmission, code int, reason string) error {
	if job.queueID != "" {
		s.queue(job.thread).paused = true
		if len(job.message["id"]) == 0 {
			info := "other"
			if code == 429 {
				info = "usageLimitExceeded"
			}
			return s.notify("error", map[string]any{"threadId": job.thread, "error": map[string]any{"code": code, "message": reason, "codexErrorInfo": info}, "willRetry": false})
		}
	}
	return s.rpcError(job.message["id"], code, reason)
}
