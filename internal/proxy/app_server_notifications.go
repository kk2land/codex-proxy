package proxy

import (
	"encoding/json"
	"strings"
)

// appThreadState tracks the client-visible subscription separately from the
// native work map. Child-agent threads can share an appWork with their parent
// (s.threads[child] == s.threads[parent]), so subscription state must be keyed
// by thread id. epoch records the last explicit client control or native
// close; responses carry the epoch observed when the request was forwarded so
// a late acknowledgement cannot restore an older subscription.
type appThreadState struct {
	loaded     bool
	subscribed bool
	closed     bool
	epoch      uint64
	// Last native close, preserved when subscription state changes or reopens.
	closedEpoch uint64
	// Keep native turn events until this thread's work has finished, then
	// forward the unsubscribe instead of retaining the native subscription.
	unsubscribePending bool
}

func (s *appSession) threadState(thread string) *appThreadState {
	state := s.subs[thread]
	if state == nil {
		state = &appThreadState{}
		s.subs[thread] = state
	}
	return state
}

// threadStarted records a newly loaded thread. A thread first observed through
// its start notification has not been unsubscribed by this connection, so it
// starts subscribed like native app-server. Explicit unsubscribe/close records
// an epoch, and a later start notification must not revive that state.
func (s *appSession) threadStarted(thread string) {
	if thread == "" {
		return
	}
	state := s.threadState(thread)
	if state.closed {
		return
	}
	state.loaded = true
	if state.epoch == 0 {
		// A thread first observed through its start notification belongs to a
		// connection that never unsubscribed; keep turn/item events flowing.
		// Explicit unsubscribe or close records an epoch and is never undone
		// by a late notification.
		state.subscribed = true
	}
}

// threadCreated applies a successful thread/start, resume, or fork. The
// request epoch was captured when the client request was forwarded; any
// unsubscribe or close after that point wins.
func (s *appSession) threadCreated(thread string, epoch uint64) {
	if thread == "" {
		return
	}
	state := s.threadState(thread)
	if state.epoch > epoch {
		return
	}
	state.loaded, state.subscribed, state.closed = true, true, false
	state.unsubscribePending = false
	state.epoch = epoch
}

func (s *appSession) threadStatusChanged(thread, status string) {
	if thread == "" || status == "" {
		return
	}
	state := s.threadState(thread)
	if status == "notLoaded" {
		// closeThread performs the full task/subscription cleanup.
		return
	}
	if !state.closed {
		state.loaded = true
	}
}

func (s *appSession) threadClosed(thread string) {
	if thread == "" {
		return
	}
	state := s.threadState(thread)
	state.loaded, state.subscribed, state.closed = false, false, true
	state.unsubscribePending = false
	state.epoch = s.nextThreadEpoch()
	state.closedEpoch = state.epoch
}

func (s *appSession) nextThreadEpoch() uint64 {
	s.controlEpoch++
	return s.controlEpoch
}

func (s *appSession) unsubscribeThread(id json.RawMessage, thread string) error {
	if len(id) == 0 || string(id) == "null" {
		// A malformed unsubscribe must not change subscription state.
		return nil
	}
	if thread == "" {
		return s.rpcError(id, -32602, "threadId is required")
	}
	state := s.threadState(thread)
	switch {
	case state.closed || !state.loaded:
		return s.reply(id, map[string]any{"status": "notLoaded"})
	case !state.subscribed:
		return s.reply(id, map[string]any{"status": "notSubscribed"})
	default:
		state.epoch = s.nextThreadEpoch()
		state.subscribed = false
		if s.threadHasWork(thread) {
			// Native unsubscribe suppresses turn/completed even while a turn
			// is running. Defer it until finish has accounted for that work.
			state.unsubscribePending = true
			return s.reply(id, map[string]any{"status": "unsubscribed"})
		}
		return s.forwardUnsubscribe(id, thread)
	}
}

func (s *appSession) threadHasWork(thread string) bool {
	for work := range s.works {
		if work.hasRunning(thread) || work.inflightByThread[thread] > 0 || work.threadAwaiting(thread) > 0 || work.goalThreads[thread] || work.realtimeThreads[thread] ||
			(work.pending != nil && work.pending.thread == thread) || work.shellStarts[thread] > 0 {
			return true
		}
	}
	return false
}

func (s *appSession) forwardUnsubscribe(id json.RawMessage, thread string) error {
	state := s.threadState(thread)
	state.unsubscribePending = false
	// Leave queued input available for an explicit queue/start after resume.
	// It must not restart the thread while native is unloading it.
	s.queue(thread).paused = true
	return s.send(rpcMessage{"method": rpcValue("thread/unsubscribe"), "params": rpcValue(map[string]any{"threadId": thread})},
		&appRPCRequest{id: id, method: "thread/unsubscribe", thread: thread, threadEpoch: state.epoch})
}

func notificationOptOut(capabilities rpcMessage) (map[string]bool, bool) {
	methods := make(map[string]bool)
	var names []json.RawMessage
	if raw := capabilities["optOutNotificationMethods"]; len(raw) > 0 && json.Unmarshal(raw, &names) != nil {
		return nil, false
	}
	for _, raw := range names {
		var name string
		if string(raw) == "null" || json.Unmarshal(raw, &name) != nil {
			return nil, false
		}
		if name == "" {
			continue
		}
		methods[name] = true
	}
	return methods, true
}

func (s *appSession) initializeClient(message, params rpcMessage) error {
	var capabilities rpcMessage
	if raw := params["capabilities"]; len(raw) > 0 && json.Unmarshal(raw, &capabilities) != nil {
		return s.forward(message, "initialize", nil)
	}
	if capabilities == nil {
		capabilities = make(rpcMessage)
	}
	var experimental bool
	if raw := capabilities["experimentalApi"]; len(raw) > 0 && (string(raw) == "null" || json.Unmarshal(raw, &experimental) != nil) {
		return s.forward(message, "initialize", nil)
	}
	preferences, valid := notificationOptOut(capabilities)
	if !valid {
		// Do not erase native validation errors by stripping invalid opt-outs.
		return s.forward(message, "initialize", nil)
	}
	// Keep lifecycle events on the private connection and enable the mandatory
	// thread/settings/update policy step. Other capabilities pass through.
	capabilities["experimentalApi"] = rpcValue(true)
	delete(capabilities, "optOutNotificationMethods")
	params["capabilities"] = rpcValue(capabilities)
	message["params"] = rpcValue(params)
	var request *appRPCRequest
	if id := message["id"]; len(id) > 0 && string(id) != "null" {
		request = &appRPCRequest{id: append(json.RawMessage(nil), id...), method: "initialize", notificationPreferences: preferences}
	}
	// Preferences take effect only after native initialization succeeds.
	return s.send(message, request)
}

// notificationVisible applies the client's notification opt-out and logical
// unsubscribe filter. Requests and RPC responses/errors are never filtered.
func (s *appSession) notificationVisible(message rpcMessage) bool {
	method := rpcString(message, "method")
	if method == "" {
		return true
	}
	if id := message["id"]; len(id) > 0 && string(id) != "null" {
		return true
	}
	if method == "error" {
		// Native notification opt-out never applies to errors.
		return true
	}
	if s.optOut[method] {
		return false
	}
	thread := rpcString(rpcObject(message, "params"), "threadId")
	if thread == "" {
		return true
	}
	state := s.subs[thread]
	if state == nil || state.subscribed {
		return true
	}
	// Native unsubscribe hides turn and item events, but keeps broadcasting
	// thread status, metadata, and lifecycle notifications.
	return !strings.HasPrefix(method, "turn/") && !strings.HasPrefix(method, "item/")
}

func (s *appSession) writeNotification(message rpcMessage) error {
	if !s.notificationVisible(message) {
		return nil
	}
	return writeRPC(s.stdout, message)
}
