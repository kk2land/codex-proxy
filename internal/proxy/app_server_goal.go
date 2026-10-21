package proxy

import "encoding/json"

// appGoalState caches the native goal for a thread id. Goal metadata must be
// keyed by id: child-agent threads can share an appWork with their parent.
// seq orders reads against newer client intent. observedSeq orders confirmed
// native mutations: an unacknowledged control has not changed the native goal.
type appGoalState struct {
	known       bool
	status      string
	objective   string
	seq         uint64
	observedSeq uint64
}

type goalSetMode int

const (
	goalSetPatch goalSetMode = iota
	goalSetRun
)

func (s *appSession) goalState(thread string) *appGoalState {
	state := s.goals[thread]
	if state == nil {
		state = &appGoalState{}
		s.goals[thread] = state
	}
	return state
}

func (s *appSession) goalStateSeq(thread string) uint64 {
	if state := s.goals[thread]; state != nil {
		return state.seq
	}
	return 0
}

func (s *appSession) nextGoalSeq() uint64 {
	s.goalSequence++
	return s.goalSequence
}

// reserveGoalControl marks the newest client intent before admission or
// forwarding, preventing an earlier goal/get from overwriting that intent.
func (s *appSession) reserveGoalControl(thread string) uint64 {
	state := s.goalState(thread)
	state.seq = s.nextGoalSeq()
	return state.seq
}

func (s *appSession) goalEpochCurrent(thread string, epoch uint64) bool {
	if state := s.goals[thread]; state != nil {
		return state.seq == epoch
	}
	return epoch == 0
}

func (s *appSession) storeGoal(thread string, goal rpcMessage) {
	state := s.goalState(thread)
	state.known = true
	state.status = rpcString(goal, "status")
	state.objective = rpcString(goal, "objective")
}

func (s *appSession) applyGoalNotification(thread string, goal rpcMessage) {
	state := s.goalState(thread)
	s.storeGoal(thread, goal)
	state.seq = s.nextGoalSeq()
	state.observedSeq = state.seq
}

func (s *appSession) applyGoalCleared(thread string) {
	state := s.goalState(thread)
	state.known = true
	state.status, state.objective = "", ""
	state.seq = s.nextGoalSeq()
	state.observedSeq = state.seq
}

// applyGoalGet accepts a successful goal/get only if no newer goal control or
// notification has been observed since the query was sent.
func (s *appSession) applyGoalGet(thread string, epoch uint64, response rpcMessage) bool {
	if !s.goalEpochCurrent(thread, epoch) || response["error"] != nil {
		return false
	}
	state := s.goalState(thread)
	s.storeGoal(thread, rpcObject(rpcObject(response, "result"), "goal"))
	state.seq = epoch
	return true
}

// goalMutationCurrent rejects replies superseded by a confirmed mutation or
// notification. Merely submitting a later control cannot suppress a successful
// activation: the native goal remains active until that control takes effect.
func (s *appSession) goalMutationCurrent(thread string, epoch uint64) bool {
	state := s.goals[thread]
	return state == nil || epoch >= state.observedSeq
}

func (s *appSession) applyGoalSetResult(thread string, epoch uint64, response rpcMessage) bool {
	if !s.goalMutationCurrent(thread, epoch) || response["error"] != nil {
		return false
	}
	state := s.goalState(thread)
	s.storeGoal(thread, rpcObject(rpcObject(response, "result"), "goal"))
	state.seq = s.nextGoalSeq()
	state.observedSeq = epoch
	return true
}

func (s *appSession) applyGoalClearResult(thread string, epoch uint64, response rpcMessage) bool {
	if !s.goalMutationCurrent(thread, epoch) || response["error"] != nil {
		return false
	}
	state := s.goalState(thread)
	state.known = true
	state.status, state.objective = "", ""
	state.seq = s.nextGoalSeq()
	state.observedSeq = epoch
	return true
}

// goalStatus reports whether the request explicitly carries a non-null status.
func goalStatus(params rpcMessage) (string, bool) {
	raw, exists := params["status"]
	if !exists || len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var status string
	if json.Unmarshal(raw, &status) != nil {
		return "", true
	}
	return status, true
}

// nullableString returns a non-null string field and whether it was supplied.
func nullableString(params rpcMessage, field string) (string, bool) {
	raw, exists := params[field]
	if !exists || len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", true
	}
	return value, true
}

// goalStopsWork reports whether a goal mutation explicitly pauses or ends the
// goal. Only these controls may cancel a waiting activation; budget or
// objective patches must not.
func goalStopsWork(params rpcMessage) bool {
	status, explicit := goalStatus(params)
	if !explicit {
		return false
	}
	switch status {
	case "paused", "blocked", "usageLimited", "budgetLimited", "complete":
		return true
	}
	return false
}

// goalSetMode classifies an accepted thread/goal/set request without mutating
// state. Explicit activation and new objectives can start execution. Patches
// that preserve a known paused/blocked goal are control operations. Unknown
// state is admitted conservatively unless an existing chain already owns the
// work chain's slot. A pending initial admission owns no slot, so it must not
// make a bare patch look safe.
func (s *appSession) goalSetMode(thread string, params rpcMessage, work *appWork) goalSetMode {
	if status, explicit := goalStatus(params); explicit {
		if status == "active" {
			return goalSetRun
		}
		return goalSetPatch
	}
	objective, hasObjective := nullableString(params, "objective")
	state := s.goals[thread]
	held := work != nil && work.lease != nil
	if hasObjective && objective != "" {
		if state == nil || !state.known || state.status == "" {
			return goalSetRun
		}
		if objective != state.objective {
			return goalSetRun
		}
		if state.status == "active" && !held {
			return goalSetRun
		}
		return goalSetPatch
	}
	if state == nil || !state.known {
		if held {
			return goalSetPatch
		}
		// Unknown state with a bare patch: hold a slot before forwarding so a
		// native activation cannot run unadmitted.
		return goalSetRun
	}
	if state.status == "active" && !held {
		return goalSetRun
	}
	return goalSetPatch
}
