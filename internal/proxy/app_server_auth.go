package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"
)

func authErrorCode(err error) int {
	if errors.Is(err, errNoAuthAccount) {
		return 429
	}
	return -32000
}

func (s *appSession) observeAuthError(name string, raw json.RawMessage) {
	if name == "" || !authRateLimited(raw) {
		return
	}
	limit := authErrorLimit(raw, s.stateStore.now())
	limit.Until = max(limit.Until, s.authResets[name])
	s.authPool.note(name, limit)
	s.authWrites++
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		// Persist an observed native 429 even if its client disconnects next.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := s.authPool.record(ctx, name, limit)
		if err == nil {
			s.logger.Log("auth_rate_limited", map[string]any{"auth_file": name, "cooldown_until": limit.Until})
		}
		select {
		case s.events <- appServerEvent{source: "auth_recorded", err: err}:
		case <-s.ctx.Done():
		}
	}()
}

func (s *appSession) rememberAuthThread(request *appRPCRequest, result rpcMessage) {
	if request.method != "thread/start" && request.method != "thread/resume" && request.method != "thread/fork" && request.method != "thread/settings/update" {
		return
	}
	if s.authThreads == nil {
		s.authThreads, s.authEphemeral = make(map[string]rpcMessage), make(map[string]bool)
	}
	thread := rpcString(rpcObject(result, "thread"), "id")
	if thread == "" {
		thread = request.thread
	}
	if thread == "" {
		return
	}
	params := s.authThreads[thread]
	if params == nil {
		params = make(rpcMessage)
	}
	// Keep explicit runtime configuration for restoring a persistent thread;
	// prompts, ephemeral flags and fork-only options must never be replayed.
	for _, key := range []string{"model", "modelProvider", "cwd", "config", "serviceTier", "personality", "baseInstructions", "developerInstructions"} {
		if value, ok := request.authParams[key]; ok {
			params[key] = append(json.RawMessage(nil), value...)
		}
	}
	params["threadId"] = rpcValue(thread)
	s.authThreads[thread] = params
	if rpcBoolTrue(request.authParams, "ephemeral") || rpcBoolTrue(rpcObject(result, "thread"), "ephemeral") {
		s.authEphemeral[thread] = true
	}
}

// pumpAuth never changes credentials while this native process is executing.
// Admissions wait before acquiring a slot, while queries continue to drain.
func (s *appSession) pumpAuth() error {
	if s.authPool == nil || s.authChanging {
		return nil
	}
	var jobs []*appAdmission
	for work := range s.works {
		if job := work.pending; job != nil && job.stage == "account" {
			jobs = append(jobs, job)
		}
	}
	if len(jobs) == 0 {
		if s.authHomeCancel != nil {
			s.authHomeCancel()
		}
		if s.authHome != nil && s.authInitialize != nil && !s.authExecuting() && s.authWrites == 0 {
			if err := releaseAuthHome(s.authHome); err != nil {
				return err
			}
			s.authHome = nil
		}
		return nil
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].started.Before(jobs[j].started) })
	name, err := s.authPool.choose()
	if err != nil {
		for _, job := range jobs {
			job.work.pending = nil
			job.cancel()
			if err := s.endAdmission(job); err != nil {
				return err
			}
			if err := s.admissionError(job, authErrorCode(err), err.Error()); err != nil {
				return err
			}
		}
		return nil
	}
	if s.authHome == nil {
		if !s.authHomeWaiting {
			cfg, err := s.load()
			if err != nil {
				return err
			}
			s.authHomeWaiting = true
			ctx, cancel := context.WithCancel(s.ctx)
			s.authHomeCancel = cancel
			s.workers.Add(1)
			go func() {
				defer s.workers.Done()
				lease, err := s.authPool.acquireHome(ctx, cfg.WaitTimeout)
				select {
				case s.events <- appServerEvent{source: "auth_home_ready", authHome: lease, err: err}:
				case <-s.ctx.Done():
					if lease != nil {
						lease.Close()
					}
				}
			}()
		}
		return nil
	}
	if name == s.authPool.active() && !s.authNeedsReload && s.authPool.linkedTo(name) {
		for _, job := range jobs {
			job.authFile = name
			if err := s.startAdmission(job); err != nil {
				return err
			}
		}
		return nil
	}
	if s.authExecuting() || s.authWrites != 0 {
		return nil
	}
	if s.restartNative == nil || s.authInitialize == nil {
		return errors.New("cannot restart real app-server for account switch")
	}
	if err := s.authPool.activate(name); err != nil {
		return err
	}
	if err := s.restartNative(); err != nil {
		return err
	}
	s.authNeedsReload = false
	s.authChanging = true
	s.logger.Log("auth_selected", map[string]any{"auth_file": name})
	return s.internal("initialize", s.authInitialize, func(response rpcMessage) error {
		if err := nativeRPCError(response); err != nil {
			return stageError("cannot initialize app-server after account switch", err)
		}
		if err := writeRPC(s.stdin, rpcMessage{"method": rpcValue("initialized"), "params": rpcValue(map[string]any{})}); err != nil {
			return err
		}
		var threads []string
		for id, state := range s.subs {
			if state.loaded && !state.closed {
				threads = append(threads, id)
			}
		}
		sort.Strings(threads)
		return s.restoreAuthThreads(threads)
	})
}

func (s *appSession) authExecuting() bool {
	if len(s.requests) != 0 || len(s.serverRequests) != 0 || len(s.processes) != 0 {
		return true
	}
	for work := range s.works {
		if work.lease != nil || len(work.running) != 0 || work.inflight != 0 || work.awaiting != 0 || work.goal || work.realtime || work.shell != 0 || (work.pending != nil && work.pending.stage != "account") {
			return true
		}
	}
	return false
}

func (s *appSession) restoreAuthThreads(threads []string) error {
	if len(threads) == 0 {
		s.authChanging = false
		buffer := s.authBuffered
		s.authBuffered, s.authBufferedSize = nil, 0
		for _, line := range buffer {
			if err := s.client(line); err != nil {
				return err
			}
		}
		return s.pumpAuth()
	}
	thread := threads[0]
	if s.authEphemeral[thread] {
		// Ephemeral history is not persisted by native Codex. Tell the client
		// instead of silently starting a conversation with missing context.
		if err := s.closeThread(s.work(thread), thread); err != nil {
			return err
		}
		if err := s.notify("thread/closed", map[string]any{"threadId": thread}); err != nil {
			return err
		}
		return s.restoreAuthThreads(threads[1:])
	}
	params := make(rpcMessage)
	for key, value := range s.authThreads[thread] {
		params[key] = value
	}
	params["threadId"] = rpcValue(thread)
	message := rpcMessage{"method": rpcValue("thread/resume"), "params": rpcValue(params)}
	enforceAppPolicy(message)
	return s.internal("thread/resume", rpcObject(message, "params"), func(response rpcMessage) error {
		if nativeRPCError(response) != nil {
			if err := s.closeThread(s.work(thread), thread); err != nil {
				return err
			}
			if err := s.notify("thread/closed", map[string]any{"threadId": thread}); err != nil {
				return err
			}
			return s.restoreAuthThreads(threads[1:])
		}
		return s.internal("thread/settings/update", rpcMessage{"threadId": rpcValue(thread), "sandboxPolicy": externalSandbox(), "approvalPolicy": rpcValue("never")}, func(response rpcMessage) error {
			if err := nativeRPCError(response); err != nil {
				return stageError("cannot restore thread policy after account switch", err)
			}
			s.policyReady[thread] = true
			return s.restoreAuthThreads(threads[1:])
		})
	})
}
