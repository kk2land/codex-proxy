package proxy

import (
	"encoding/json"
	"strings"
	"time"
)

// Extract operational metadata explicitly; successful protocol bodies,
// prompts, and tool data are not logged. Native error responses retain their
// code, message, and data for internal diagnosis.
type appLogger struct {
	*Logger
	user     string
	cwd      string
	childPID int
}

func (l *appLogger) Log(event string, fields map[string]any) {
	if fields == nil {
		fields = make(map[string]any)
	}
	fields["user"], fields["args"], fields["session_cwd"] = l.user, []string{"app-server"}, l.cwd
	if l.childPID != 0 {
		fields["child_pid"] = l.childPID
	}
	l.Logger.Log(event, fields)
}

func logAppMethod(method string) string {
	if len(method) > 128 {
		return "unknown"
	}
	for _, c := range method {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("/$_-.", c)) {
			return "unknown"
		}
	}
	return method
}

func logRPCID(fields map[string]any, id json.RawMessage) {
	var value any
	if json.Unmarshal(id, &value) == nil {
		switch value.(type) {
		case string, float64:
			fields["rpc_id"] = json.RawMessage(id)
		}
	}
}

func appMetadata(params rpcMessage) map[string]any {
	fields := make(map[string]any)
	for key, name := range map[string]string{"threadId": "thread_id", "turnId": "turn_id", "processHandle": "process_handle", "model": "model", "cwd": "cwd", "queuedSubmissionId": "queue_id"} {
		if value := rpcString(params, key); value != "" {
			fields[name] = value
		}
	}
	for key, name := range map[string]string{"thread": "thread_id", "turn": "turn_id", "item": "item_id"} {
		object := rpcObject(params, key)
		if id := rpcString(object, "id"); id != "" {
			fields[name] = id
		}
	}
	for key, name := range map[string]string{"exitCode": "exit_code", "signal": "signal"} {
		var value int64
		if json.Unmarshal(params[key], &value) == nil && string(params[key]) != "null" {
			fields[name] = value
		}
	}
	return fields
}

// logNativeError records the actual native cause. Error text and data are
// intentionally retained: they are the primary diagnostic for internal
// troubleshooting. Successful protocol bodies remain unlogged. Any non-null
// error payload is a failure even when it is not a standard object.
func logNativeError(fields map[string]any, raw json.RawMessage) {
	if raw == nil {
		return
	}
	nativeErr := rpcErrorObject(raw)
	var code int64
	if json.Unmarshal(nativeErr["code"], &code) == nil {
		fields["error_code"] = code
	}
	if message := rpcString(nativeErr, "message"); message != "" {
		fields["error"] = message
	}
	fields["error_payload"] = raw
	if data := nativeErr["data"]; len(data) > 0 && string(data) != "null" {
		fields["error_data"] = json.RawMessage(data)
	}
	if len(nativeErr) > 0 {
		fields["error_object"] = nativeErr
	}
	if _, set := fields["error"]; !set {
		fields["error"] = errorPayloadText(raw)
	}
}

func logRPCResult(fields map[string]any, message rpcMessage) {
	fields["status"] = "completed"
	if raw := rpcErrorPayload(message); raw != nil {
		fields["status"] = "failed"
		logNativeError(fields, raw)
	}
}

func (r *appRPCRequest) logFields() map[string]any {
	fields := make(map[string]any, len(r.metadata)+6)
	for key, value := range r.metadata {
		fields[key] = value
	}
	fields["method"], fields["rpc_sequence"] = logAppMethod(r.method), r.sequence
	fields["internal"] = r.callback != nil
	logRPCID(fields, r.id)
	if r.taskID != "" {
		fields["task_id"] = r.taskID
	}
	if r.authFile != "" {
		fields["auth_file"] = r.authFile
	}
	return fields
}

func (s *appSession) logRPCFinished(request *appRPCRequest, message rpcMessage) {
	if raw := message["error"]; request.callback != nil && (len(raw) == 0 || string(raw) == "null") {
		return
	}
	fields := request.logFields()
	for key, value := range appMetadata(rpcObject(message, "result")) {
		fields[key] = value
	}
	fields["run_ms"] = time.Since(request.sentAt).Milliseconds()
	logRPCResult(fields, message)
	s.logger.Log("rpc_finished", fields)
}

func (s *appSession) logNotificationError(message rpcMessage) {
	params := rpcObject(message, "params")
	method := rpcString(message, "method")
	// Streaming and lifecycle notifications are forwarded without logging.
	// Only notifications that carry an actual failure cause are recorded; the
	// cause text (params.error, realtime/error params.message, or
	// turn/completed turn.error) is retained for internal diagnosis.
	nativeErr := notificationRPCError(method, params)
	if len(nativeErr) == 0 {
		return
	}
	fields := appMetadata(params)
	fields["method"] = logAppMethod(method)
	fields["direction"], fields["stage"] = "server_to_client", "notification"
	for key, name := range map[string]string{"turn": "turn_status", "goal": "goal_status"} {
		if status := rpcString(rpcObject(params, key), "status"); status != "" {
			switch status {
			case "active", "paused", "completed", "failed", "interrupted", "inProgress", "pending", "budgetLimited":
				fields[name] = status
			default:
				fields[name] = "unknown"
			}
		}
	}
	logRPCResult(fields, params)
	fields["status"] = "failed"
	logNativeError(fields, nativeErr)
	if work := s.threads[rpcString(params, "threadId")]; work != nil && work.lease != nil {
		fields["task_id"] = work.taskID
	}
	s.logger.Log("error", fields)
}

func (job *appAdmission) logFields() map[string]any {
	fields := appMetadata(rpcObject(job.message, "params"))
	fields["task_id"], fields["submission_id"], fields["method"] = job.taskID, job.submissionID, job.method
	fields["wait_ms"] = time.Since(job.started).Milliseconds() + job.queueWaitMS
	fields["queue_wait_ms"], fields["slot_wait_ms"] = job.queueWaitMS, job.slotWaitMS
	if job.stage == "slot" && job.slotWaitMS == 0 {
		fields["slot_wait_ms"] = time.Since(job.started).Milliseconds()
	}
	fields["stage"] = job.stage
	fields["limited"], fields["reused_slot"] = true, !job.initial
	if job.authFile != "" {
		fields["auth_file"] = job.authFile
	}
	if job.queueID != "" {
		fields["queue_id"] = job.queueID
	}
	if !job.intervalStarted.IsZero() {
		fields["interval_wait_ms"] = time.Since(job.intervalStarted).Milliseconds()
	}
	logRPCID(fields, job.message["id"])
	return fields
}

type appTurnTiming struct {
	thread, turn string
	started      time.Time
}

func (s *appSession) logTurnStarted(work *appWork, thread, turn string, modeled bool) {
	if work.turnStarted == nil {
		work.turnStarted = make(map[string]appTurnTiming)
	}
	key := thread + ":" + turn
	if _, exists := work.turnStarted[key]; exists {
		return
	}
	work.turnStarted[key] = appTurnTiming{thread: thread, turn: turn, started: time.Now()}
	fields := map[string]any{"thread_id": thread, "turn_id": turn, "limited": modeled, "method": "thread/shellCommand", "wait_ms": 0}
	if modeled {
		fields["task_id"], fields["method"], fields["wait_ms"] = work.taskID, work.method, work.waitMS
	}
	s.logger.Log("task_started", fields)
}

func (s *appSession) logTurnFinished(work *appWork, key, status, reason string) {
	timing, ok := work.turnStarted[key]
	if !ok {
		return
	}
	delete(work.turnStarted, key)
	fields := map[string]any{"thread_id": timing.thread, "turn_id": timing.turn, "status": status, "run_ms": time.Since(timing.started).Milliseconds(), "limited": work.running[key], "method": "thread/shellCommand", "wait_ms": 0}
	if work.running[key] {
		fields["task_id"], fields["method"], fields["wait_ms"] = work.taskID, work.method, work.waitMS
	}
	if reason != "" {
		fields["reason"] = reason
	}
	s.logger.Log("task_finished", fields)
}

func (s *appSession) logProcessFinished(request *appRPCRequest, params rpcMessage, status string) {
	fields := request.logFields()
	for key, value := range appMetadata(params) {
		fields[key] = value
	}
	fields["run_ms"], fields["status"] = time.Since(request.sentAt).Milliseconds(), status
	s.logger.Log("process_finished", fields)
}
