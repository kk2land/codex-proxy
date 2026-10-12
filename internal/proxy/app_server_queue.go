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

func (s *appSession) queueChanged(work *appWork) error {
	s.logger.Log("queue_changed", map[string]any{"thread_id": work.thread, "queue_size": len(work.queue), "queue_paused": work.queuePaused, "queued_bytes": s.queuedSize})
	return s.notify("thread/queue/changed", map[string]any{"threadId": work.thread})
}

func queuedInput(work *appWork, id string) *appQueuedInput {
	for _, entry := range work.queue {
		if entry.ID == id {
			return entry
		}
	}
	return nil
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
	queueID := rpcString(params, "queuedSubmissionId")
	entry := queuedInput(work, queueID)
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
		if s.queuedSize-previous+proposedSize > maxAppServerMessage || (method == "thread/queue/add" && len(work.queue) >= 1024) {
			return s.rpcError(id, -32000, "proxy queue capacity exceeded")
		}
		if entry == nil {
			if clientID == "" {
				return s.rpcError(id, -32602, "clientUserMessageId is required")
			}
			entry = &appQueuedInput{ID: newRequestID(), ClientUserMessageID: clientID, enqueued: time.Now()}
			work.queue = append(work.queue, entry)
		}
		entry.Input = append(json.RawMessage(nil), params["input"]...)
		s.queuedSize += entry.size() - previous
		if err := s.reply(id, map[string]any{"queuedSubmission": entry}); err != nil {
			return err
		}
		if err := s.queueChanged(work); err != nil {
			return err
		}
		return s.startQueue(work, nil, "")
	case "thread/queue/list":
		start := 0
		if cursor := rpcString(params, "cursor"); cursor != "" {
			value, err := strconv.Atoi(cursor)
			if err != nil || value < 0 || value > len(work.queue) {
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
		end := len(work.queue)
		if limit < end-start {
			end = start + limit
		}
		var cursor any
		if end < len(work.queue) {
			cursor = strconv.Itoa(end)
		}
		data := work.queue[start:end]
		if data == nil {
			data = []*appQueuedInput{}
		}
		return s.reply(id, map[string]any{"data": data, "nextCursor": cursor})
	case "thread/queue/delete":
		if entry != nil {
			if work.pending != nil && work.pending.queueID == queueID {
				if err := s.cancelAdmission(work); err != nil {
					return err
				}
				work.queuePaused = false
			}
			s.removeQueued(work, queueID)
		}
		if err := s.reply(id, map[string]any{"deleted": entry != nil}); err != nil {
			return err
		}
		if entry != nil {
			if err := s.queueChanged(work); err != nil {
				return err
			}
		}
		return s.startQueue(work, nil, "")
	case "thread/queue/reorder":
		var ids []string
		if json.Unmarshal(params["queuedSubmissionIds"], &ids) != nil || len(ids) != len(work.queue) {
			return s.rpcError(id, -32602, "queue reorder must contain every submission exactly once")
		}
		ordered := make([]*appQueuedInput, 0, len(ids))
		seen := make(map[string]bool)
		for _, queuedID := range ids {
			item := queuedInput(work, queuedID)
			if item == nil || seen[queuedID] {
				return s.rpcError(id, -32602, "invalid queue reorder")
			}
			seen[queuedID] = true
			ordered = append(ordered, item)
		}
		// A waiting head has not executed yet: reschedule under the same quota.
		if work.pending != nil && work.pending.queueID != "" {
			if err := s.cancelAdmission(work); err != nil {
				return err
			}
			work.queuePaused = false
		}
		work.queue = ordered
		if err := s.reply(id, map[string]any{}); err != nil {
			return err
		}
		if err := s.queueChanged(work); err != nil {
			return err
		}
		return s.startQueue(work, nil, "")
	case "thread/queue/start":
		if s.reviewSources[thread] != "" || work.pending != nil || work.lease != nil || len(work.running) != 0 {
			return s.rpcError(id, -32000, "thread already has an active or waiting task")
		}
		if queueID == "" && len(work.queue) > 0 {
			queueID = work.queue[0].ID
		}
		if queuedInput(work, queueID) == nil {
			return s.rpcError(id, -32602, "queued submission does not exist")
		}
		work.queuePaused = false
		return s.startQueue(work, id, queueID)
	}
	return nil
}

func (s *appSession) removeQueued(work *appWork, id string) *appQueuedInput {
	for i, entry := range work.queue {
		if entry.ID == id {
			s.queuedSize -= entry.size()
			copy(work.queue[i:], work.queue[i+1:])
			work.queue[len(work.queue)-1] = nil
			work.queue = work.queue[:len(work.queue)-1]
			return entry
		}
	}
	return nil
}

func (s *appSession) startQueue(work *appWork, id json.RawMessage, queueID string) error {
	if s.reviewSources[work.thread] != "" || len(work.queue) == 0 || work.queuePaused || work.pending != nil || work.lease != nil || len(work.running) > 0 || work.inflight > 0 || work.awaiting > 0 {
		return nil
	}
	if queueID == "" {
		queueID = work.queue[0].ID
	}
	entry := queuedInput(work, queueID)
	message := rpcMessage{"method": rpcValue("turn/start"), "params": rpcValue(map[string]any{"threadId": work.thread, "input": entry.Input, "clientUserMessageId": entry.ClientUserMessageID})}
	if len(id) > 0 {
		message["id"] = id
	}
	return s.begin(work, message, "turn/start", queueID)
}
