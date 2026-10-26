package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

func authErrorCode(err error) int {
	if errors.Is(err, errNoAuthAccount) {
		return 429
	}
	return -32000
}

// observeAuthMessage inspects one decoded native message for evidence that the
// pinned Codex account is rate limited. Only model-task responses and model
// error notifications count; other notifications may carry MCP/OAuth/tool
// failures and never describe the GPT account used by this native task.
// The normal protocol loop and the shutdown drain share this helper, so the
// drain keeps observing native output without running request handling.
func (s *appSession) observeAuthMessage(message rpcMessage) {
	if s.authPool == nil {
		return
	}
	method := rpcString(message, "method")
	if method == "" {
		if len(message["id"]) == 0 || string(message["id"]) == "null" {
			return
		}
		request := s.requests[string(message["id"])]
		if request == nil || !request.task {
			return
		}
		s.observeAuthError(request.authFile, rpcErrorPayload(message))
		return
	}
	name := s.authPool.active()
	if method == "account/rateLimits/updated" {
		if s.authResets == nil {
			s.authResets = make(map[string]int64)
		}
		hint := authResetHint(message["params"], s.stateStore.now().Unix())
		s.authResets[name] = hint
		// A hint never starts a cooldown on its own, but a longer exhausted
		// window may extend a cooldown that a real 429 already recorded.
		s.mergeAuthHint(name, hint)
		return
	}
	if method == "error" || method == "turn/completed" || method == "thread/realtime/error" {
		s.observeAuthError(name, notificationRPCError(method, rpcObject(message, "params")))
	}
}

// mergeAuthHint extends an already observed 429 with a later, longer hint.
// A pure hint leaves state untouched, including the last 429 timestamp.
func (s *appSession) mergeAuthHint(name string, hint int64) {
	if s.authPool == nil || name == "" || hint <= 0 {
		return
	}
	s.authPool.mu.Lock()
	old, observed := s.authPool.local[name]
	s.authPool.mu.Unlock()
	if !observed || old.At == 0 || hint <= old.Until {
		return
	}
	s.recordAuthLimit(name, authLimit{At: old.At, Until: hint})
}

// Only model-task responses and model-error notifications call this method.
// The protocol loop forwards the original failure and then closes the session.
func (s *appSession) observeAuthError(name string, raw json.RawMessage) {
	if name == "" || !authRateLimited(raw) {
		return
	}
	limit := authErrorLimit(raw, s.stateStore.now())
	limit.Until = max(limit.Until, s.authResets[name])
	s.authLimited = true
	s.recordAuthLimit(name, limit)
}

// recordAuthLimit applies a limit locally before persisting it asynchronously.
// Callers run on the protocol goroutine, so every authRecords.Add happens
// before shutdown starts waiting, and the write never holds state.lock while
// the protocol goroutine still needs it.
func (s *appSession) recordAuthLimit(name string, limit authLimit) {
	if s.authPool == nil || name == "" {
		return
	}
	s.authPool.note(name, limit)
	s.authRecords.Add(1)
	go func() {
		defer s.authRecords.Done()
		// Independent of client/session cancellation, and of the auth writer.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.authPool.record(ctx, name, limit); err != nil {
			s.logger.Log("error", map[string]any{"stage": "account_state", "auth_file": name, "error": err.Error()})
		} else {
			s.logger.Log("auth_rate_limited", map[string]any{"auth_file": name, "cooldown_until": limit.Until})
		}
	}()
}

func (s *appSession) checkAuth() error {
	state, err := s.stateStore.readState()
	if err == nil {
		err = s.authPool.checkCurrent(state)
	}
	if errors.Is(err, errNoAuthAccount) {
		s.authLimited = true
	}
	return err
}
