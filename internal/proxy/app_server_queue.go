package proxy

import (
	"encoding/json"
	"strconv"
	"time"
)

type appQueuedInput struct {
	ID                  string          `json:"id"`
	ClientUserMessageID string          `json:"clientUserMessageId"`
	Input               json.RawMessage `json:"input"`
	enqueued            time.Time
}

func (entry *appQueuedInput) size() int {
	return len(entry.ID) + len(entry.ClientUserMessageID) + len(entry.Input)
}

// appQueueState owns the queue of exactly one thread id. A child thread can
// share its parent's appWork, so queue data must never hang off appWork.
type appQueueState struct {
	thread string
	items  []*appQueuedInput
	paused bool
}

// queue returns the state for thread, creating it lazily. Emptiness is
// decided by items, never by the presence of the state entry.
func (s *appSession) queue(thread string) *appQueueState {
	if state := s.queues[thread]; state != nil {
		return state
	}
	state := &appQueueState{thread: thread}
	s.queues[thread] = state
	return state
}

func (s *appSession) queueState(thread string) *appQueueState {
	return s.queues[thread]
}

func (s *appSession) queueChanged(thread string) error {
	state := s.queueState(thread)
	size, paused := 0, false
	if state != nil {
		size, paused = len(state.items), state.paused
	}
	s.logger.Log("queue_changed", map[string]any{"thread_id": thread, "queue_size": size, "queue_paused": paused, "queued_bytes": s.queuedSize})
	return s.notify("thread/queue/changed", map[string]any{"threadId": thread})
}

func queuedInput(state *appQueueState, id string) *appQueuedInput {
	if state == nil {
		return nil
	}
	for _, entry := range state.items {
		if entry.ID == id {
			return entry
		}
	}
	return nil
}

// A dispatched entry stays queued until ACK so a rejection can preserve it.
// During that window it is immutable: native already has its input. Closed
// requests no longer own the entry and cannot block an edit after resume.
func (s *appSession) queuedInputInFlight(thread, id string) bool {
	for _, request := range s.requests {
		if request.thread == thread && request.queueID != "" && (id == "" || request.queueID == id) && s.taskGenerationCurrent(request.thread, request.lifeGen) {
			return true
		}
	}
	return false
}

func validQueueInput(input json.RawMessage) bool {
	var items []json.RawMessage
	return json.Unmarshal(input, &items) == nil && len(items) > 0
}

func (s *appSession) queueRequest(message, params rpcMessage, method, thread string) error {
	switch method {
	case "thread/queue/add", "thread/queue/list", "thread/queue/update", "thread/queue/delete", "thread/queue/reorder", "thread/queue/start":
	default:
		return s.forward(message, method, nil)
	}
	id := message["id"]
	if len(id) == 0 || string(id) == "null" {
		// Native request methods require IDs. In particular, a malformed queue
		// notification must never become an admitted automatic task.
		return nil
	}
	work := s.threads[thread]
	if work == nil || thread == "" {
		return s.rpcError(id, -32602, "thread must be loaded before accessing its queue")
	}
	state := s.queue(thread)
	queueID := rpcString(params, "queuedSubmissionId")
	entry := queuedInput(state, queueID)
	if entry != nil && (method == "thread/queue/add" || method == "thread/queue/update" || method == "thread/queue/delete") && s.queuedInputInFlight(thread, queueID) {
		return s.rpcError(id, -32000, "queued submission is awaiting app-server acknowledgement")
	}
	switch method {
	case "thread/queue/add", "thread/queue/update":
		if !validQueueInput(params["input"]) {
			return s.rpcError(id, -32602, "queued submission requires a nonempty input array")
		}
		if method == "thread/queue/update" && entry == nil {
			return s.rpcError(id, -32602, "queued submission does not exist")
		}
		previous := 0
		clientID := rpcString(params, "clientUserMessageId")
		proposedSize := len(params["input"]) + len(clientID) + 32
		if entry != nil {
			previous = entry.size()
			proposedSize = len(params["input"]) + len(entry.ID) + len(entry.ClientUserMessageID)
		}
		if s.queuedSize-previous+proposedSize > maxAppServerMessage || (method == "thread/queue/add" && len(state.items) >= 1024) {
			return s.rpcError(id, -32000, "proxy queue capacity exceeded")
		}
		if entry == nil {
			if clientID == "" {
				return s.rpcError(id, -32602, "clientUserMessageId is required")
			}
			entry = &appQueuedInput{ID: newRequestID(), ClientUserMessageID: clientID, enqueued: time.Now()}
			state.items = append(state.items, entry)
		}
		entry.Input = append(json.RawMessage(nil), params["input"]...)
		s.queuedSize += entry.size() - previous
		if err := s.reply(id, map[string]any{"queuedSubmission": entry}); err != nil {
			return err
		}
		if err := s.queueChanged(thread); err != nil {
			return err
		}
		return s.startQueue(work, thread, nil, "")
	case "thread/queue/list":
		start := 0
		if cursor := rpcString(params, "cursor"); cursor != "" {
			value, err := strconv.Atoi(cursor)
			if err != nil || value < 0 || value > len(state.items) {
				return s.rpcError(id, -32602, "invalid queue cursor")
			}
			start = value
		}
		limit := 100
		if raw := params["limit"]; len(raw) > 0 && string(raw) != "null" {
			if json.Unmarshal(raw, &limit) != nil || limit <= 0 {
				return s.rpcError(id, -32602, "invalid queue limit")
			}
		}
		end := len(state.items)
		if limit < end-start {
			end = start + limit
		}
		var cursor any
		if end < len(state.items) {
			cursor = strconv.Itoa(end)
		}
		data := state.items[start:end]
		if data == nil {
			data = []*appQueuedInput{}
		}
		return s.reply(id, map[string]any{"data": data, "nextCursor": cursor})
	case "thread/queue/delete":
		if entry != nil {
			if work.pending != nil && work.pending.queueID == queueID && work.pending.thread == thread {
				if err := s.cancelAdmission(work); err != nil {
					return err
				}
				state.paused = false
			}
			s.removeQueued(thread, queueID)
		}
		if err := s.reply(id, map[string]any{"deleted": entry != nil}); err != nil {
			return err
		}
		if entry != nil {
			if err := s.queueChanged(thread); err != nil {
				return err
			}
		}
		return s.startQueue(work, thread, nil, "")
	case "thread/queue/reorder":
		var ids []string
		if json.Unmarshal(params["queuedSubmissionIds"], &ids) != nil || len(ids) != len(state.items) {
			return s.rpcError(id, -32602, "queue reorder must contain every submission exactly once")
		}
		if s.queuedInputInFlight(thread, "") {
			return s.rpcError(id, -32000, "queued submission is awaiting app-server acknowledgement")
		}
		ordered := make([]*appQueuedInput, 0, len(ids))
		seen := make(map[string]bool)
		for _, queuedID := range ids {
			item := queuedInput(state, queuedID)
			if item == nil || seen[queuedID] {
				return s.rpcError(id, -32602, "invalid queue reorder")
			}
			seen[queuedID] = true
			ordered = append(ordered, item)
		}
		// A waiting head has not executed yet: reschedule under the same quota.
		if work.pending != nil && work.pending.queueID != "" && work.pending.thread == thread {
			if err := s.cancelAdmission(work); err != nil {
				return err
			}
			state.paused = false
		}
		state.items = ordered
		if err := s.reply(id, map[string]any{}); err != nil {
			return err
		}
		if err := s.queueChanged(thread); err != nil {
			return err
		}
		return s.startQueue(work, thread, nil, "")
	case "thread/queue/start":
		if s.reviewSources[thread] != "" || work.pending != nil || work.lease != nil || len(work.running) != 0 || work.inflight > 0 || work.awaiting > 0 {
			return s.rpcError(id, -32000, "thread already has an active or waiting task")
		}
		if queueID == "" && len(state.items) > 0 {
			queueID = state.items[0].ID
		}
		if queuedInput(state, queueID) == nil {
			return s.rpcError(id, -32602, "queued submission does not exist")
		}
		state.paused = false
		return s.startQueue(work, thread, id, queueID)
	}
	return nil
}

func (s *appSession) removeQueued(thread, id string) *appQueuedInput {
	state := s.queueState(thread)
	if state == nil {
		return nil
	}
	for i, entry := range state.items {
		if entry.ID == id {
			s.queuedSize -= entry.size()
			copy(state.items[i:], state.items[i+1:])
			state.items[len(state.items)-1] = nil
			state.items = state.items[:len(state.items)-1]
			return entry
		}
	}
	return nil
}

// startQueue dispatches one entry from the given thread's queue when the
// shared appWork is entirely idle. Busy work simply keeps queued input waiting
// for finish to choose the next thread.
func (s *appSession) startQueue(work *appWork, thread string, id json.RawMessage, queueID string) error {
	if s.reviewSources[thread] != "" || work.pending != nil || work.lease != nil || len(work.running) > 0 || work.inflight > 0 || work.awaiting > 0 {
		return nil
	}
	state := s.queueState(thread)
	if state == nil || len(state.items) == 0 || state.paused {
		return nil
	}
	if queueID == "" {
		queueID = state.items[0].ID
	}
	entry := queuedInput(state, queueID)
	if entry == nil {
		return nil
	}
	message := rpcMessage{"method": rpcValue("turn/start"), "params": rpcValue(map[string]any{"threadId": thread, "input": entry.Input, "clientUserMessageId": entry.ClientUserMessageID})}
	if len(id) > 0 {
		message["id"] = id
	}
	return s.begin(work, message, "turn/start", queueID, 0)
}

// nextQueue picks the queue to dispatch once a shared appWork becomes idle.
// Every thread sharing the work is eligible, not only work.thread. Ties on
// the head's enqueue time break on thread id for deterministic scheduling.
func (s *appSession) nextQueue(work *appWork) (string, string) {
	var bestThread, bestID string
	var best time.Time
	for thread := range s.threads {
		if s.threads[thread] != work {
			continue
		}
		state := s.queueState(thread)
		if state == nil || state.paused || len(state.items) == 0 || s.reviewSources[thread] != "" || s.closedTasks[thread] {
			continue
		}
		head := state.items[0]
		if bestThread == "" || head.enqueued.Before(best) || (head.enqueued.Equal(best) && thread < bestThread) {
			bestThread, bestID, best = thread, head.ID, head.enqueued
		}
	}
	return bestThread, bestID
}
