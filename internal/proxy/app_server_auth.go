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

// Only model-task responses and model-error notifications call this method.
// The protocol loop forwards the original failure and then closes the session.
func (s *appSession) observeAuthError(name string, raw json.RawMessage) {
	if name == "" || !authRateLimited(raw) {
		return
	}
	limit := authErrorLimit(raw, s.stateStore.now())
	limit.Until = max(limit.Until, s.authResets[name])
	s.authLimited = true
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
