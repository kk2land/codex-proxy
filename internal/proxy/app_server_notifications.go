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
	state.epoch = s.nextThreadEpoch()
	switch {
	case state.closed || !state.loaded:
		return s.reply(id, map[string]any{"status": "notLoaded"})
	case !state.subscribed:
		return s.reply(id, map[string]any{"status": "notSubscribed"})
	default:
		// Keep the native subscription so task completion still reaches the
		// proxy; only client-visible turn/item events are suppressed.
		state.subscribed = false
		return s.reply(id, map[string]any{"status": "unsubscribed"})
	}
}

func notificationOptOut(capabilities rpcMessage) map[string]bool {
	methods := make(map[string]bool)
	var names []string
	if json.Unmarshal(capabilities["optOutNotificationMethods"], &names) != nil {
		return methods
	}
	for _, name := range names {
		if name == "" {
			continue
		}
		methods[name] = true
	}
	return methods
}

func (s *appSession) initializeClient(message rpcMessage) error {
	params := rpcObject(message, "params")
	capabilities := rpcObject(params, "capabilities")
	preferences := notificationOptOut(capabilities)
	// Keep lifecycle events on the private connection and enable the mandatory
	// thread/settings/update policy step. Other capabilities pass through.
	capabilities["experimentalApi"] = rpcValue(true)
	delete(capabilities, "optOutNotificationMethods")
	params["capabilities"] = rpcValue(capabilities)
	message["params"] = rpcValue(params)
	if s.authPool != nil {
		s.authInitialize = params
	}
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
