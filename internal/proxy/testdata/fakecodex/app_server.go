package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func appServer(dir string) {
	encoder := json.NewEncoder(os.Stdout)
	optOut := make(map[string]bool)
	unsubscribed := make(map[string]bool)
	notify := func(method string, params any) {
		_ = encoder.Encode(map[string]any{"method": method, "params": params})
	}
	// notifyClient mirrors native per-connection filtering. Thread subscription
	// only hides turn/item events; thread status and other metadata stay
	// broadcast. Requests, responses, and errors bypass this filter.
	notifyClient := func(method string, params any) {
		if method != "error" && optOut[method] {
			return
		}
		if values, ok := params.(map[string]any); ok {
			if thread, ok := values["threadId"].(string); ok && unsubscribed[thread] &&
				(strings.HasPrefix(method, "turn/") || strings.HasPrefix(method, "item/")) {
				return
			}
		}
		notify(method, params)
	}
	turns := make(map[string]string)
	type fixtureGoal struct {
		objective   string
		status      string
		tokenBudget *int64
	}
	goals := make(map[string]*fixtureGoal)
	ephemeralThreads := make(map[string]bool)
	loaded := make(map[string]bool)
	processes := make(map[string]bool)
	sequence := 0
	experimental := false
	initialized := false
	deferSettings := false
	var settingsReplies []map[string]any
	delayReviews := false
	rejectTurns := false
	delayTurnReplies := false
	var turnReplies []map[string]any
	delayPrediction := false
	ackBeforePrediction := false
	failPrediction := false
	var pendingPredictions []map[string]any
	var reviewReplies []map[string]any
	var deferredReply json.RawMessage
	var processReplies []json.RawMessage
	start := func(thread string) map[string]any {
		if turns[thread] == "" {
			sequence++
			turns[thread] = fmt.Sprintf("turn-%d", sequence)
			notifyClient("turn/started", map[string]any{"threadId": thread, "turn": map[string]any{"id": turns[thread], "status": "inProgress"}})
		}
		return map[string]any{"id": turns[thread], "status": "inProgress"}
	}
	capture, err := os.OpenFile(filepath.Join(dir, "rpc.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		panic(err)
	}
	defer capture.Close()
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 32<<20)
	for scanner.Scan() {
		_, _ = capture.Write(append(append([]byte(nil), scanner.Bytes()...), '\n'))
		var message struct {
			ID     json.RawMessage            `json:"id"`
			Method string                     `json:"method"`
			Params map[string]json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &message) != nil {
			continue
		}
		str := func(key string) string { var v string; _ = json.Unmarshal(message.Params[key], &v); return v }
		boolean := func(key string) bool { var v bool; _ = json.Unmarshal(message.Params[key], &v); return v }
		thread := str("threadId")
		result := map[string]any{}
		switch message.Method {
		case "initialize":
			if initialized {
				_ = encoder.Encode(map[string]any{"id": message.ID, "error": map[string]any{"code": -32600, "message": "Already initialized"}})
				continue
			}
			initialized = true
			var capabilities map[string]json.RawMessage
			_ = json.Unmarshal(message.Params["capabilities"], &capabilities)
			var experimentalAPI bool
			_ = json.Unmarshal(capabilities["experimentalApi"], &experimentalAPI)
			experimental = experimentalAPI
			var methods []string
			_ = json.Unmarshal(capabilities["optOutNotificationMethods"], &methods)
			optOut = make(map[string]bool, len(methods))
			for _, method := range methods {
				optOut[method] = true
			}
			result["userAgent"] = "fixture"
		case "thread/start", "thread/resume", "thread/fork":
			source := thread
			if message.Method == "thread/fork" {
				// Mirror native fork validation for the constraints the proxy
				// must not violate when it rewrites parameters.
				ephemeral := boolean("ephemeral")
				prediction := boolean("experimentalPredictionMode")
				if prediction && rpcForkConfigurationOverrides(message.Params) {
					_ = encoder.Encode(map[string]any{"id": message.ID, "error": map[string]any{"code": -32600, "message": "`experimentalPredictionMode` cannot be combined with configuration overrides"}})
					continue
				}
				if prediction && !ephemeral {
					_ = encoder.Encode(map[string]any{"id": message.ID, "error": map[string]any{"code": -32600, "message": "`experimentalPredictionMode` requires `ephemeral: true`"}})
					continue
				}
				if ephemeral && boolean("deferGoalContinuation") {
					_ = encoder.Encode(map[string]any{"id": message.ID, "error": map[string]any{"code": -32600, "message": "`deferGoalContinuation` cannot be combined with `ephemeral`"}})
					continue
				}
			}
			if thread == "" || message.Method == "thread/fork" {
				sequence++
				thread = fmt.Sprintf("thread-%d", sequence)
			}
			info := map[string]any{"id": thread}
			loaded[thread] = true
			unsubscribed[thread] = false
			if message.Method == "thread/fork" {
				if boolean("ephemeral") {
					ephemeralThreads[thread] = true
				}
				if goal := goals[source]; goal != nil {
					copied := *goal
					goals[thread] = &copied
				}
				info["forkedFromId"] = source
				info["ephemeral"] = boolean("ephemeral")
			}
			notifyClient("thread/started", map[string]any{"thread": info})
			result["thread"], result["sandbox"] = info, map[string]any{"type": "dangerFullAccess"}
		case "thread/settings/update":
			if !experimental {
				_ = encoder.Encode(map[string]any{"id": message.ID, "error": map[string]any{"code": -32600, "message": "requires experimentalApi"}})
				continue
			}
			result["sandboxPolicy"] = message.Params["sandboxPolicy"]
			if deferSettings {
				deferSettings = false
				settingsReplies = append(settingsReplies, map[string]any{"id": message.ID, "result": result})
				continue
			}
		case "test/deferNextSettings":
			deferSettings = true
		case "test/flushSettings":
			for _, reply := range settingsReplies {
				_ = encoder.Encode(reply)
			}
			settingsReplies = nil
		case "turn/start":
			if rejectTurns {
				_ = encoder.Encode(map[string]any{"id": message.ID, "error": map[string]any{"code": -32602, "message": "rejected by fixture"}})
				continue
			}
			result["turn"] = start(thread)
			if delayTurnReplies {
				turnReplies = append(turnReplies, map[string]any{"id": message.ID, "result": result})
				continue
			}
		case "review/start":
			if str("delivery") == "detached" {
				sequence++
				child := fmt.Sprintf("review-%d", sequence)
				notifyClient("thread/started", map[string]any{"thread": map[string]any{"id": child, "forkedFromId": thread}})
				thread = child
			}
			result["turn"], result["reviewThreadId"] = start(thread), thread
			if delayReviews {
				reviewReplies = append(reviewReplies, map[string]any{"id": message.ID, "result": result})
				if len(reviewReplies) == 2 {
					_ = encoder.Encode(reviewReplies[1])
					_ = encoder.Encode(reviewReplies[0])
					reviewReplies = nil
				}
				continue
			}
		case "thread/compact/start", "thread/approveGuardianDeniedAction":
			start(thread)
		case "thread/prediction/request":
			sourceTurnID := str("sourceTurnId")
			if sourceTurnID == "" {
				_ = encoder.Encode(map[string]any{"id": message.ID, "error": map[string]any{"code": -32602, "message": "sourceTurnId is required"}})
				continue
			}
			if delayPrediction {
				pendingPredictions = append(pendingPredictions, map[string]any{"threadId": thread, "sourceTurnId": sourceTurnID, "id": append(json.RawMessage(nil), message.ID...)})
				continue
			}
			if ackBeforePrediction {
				// Emit only the ACK first; the notification is sent later by
				// test/emitPrediction so ACK-before-update is real.
				pendingPredictions = append(pendingPredictions, map[string]any{"threadId": thread, "sourceTurnId": sourceTurnID, "id": nil})
				_ = encoder.Encode(map[string]any{"id": message.ID, "result": map[string]any{}})
				continue
			}
			notifyClient("thread/prediction/updated", map[string]any{
				"threadId":     thread,
				"sourceTurnId": sourceTurnID,
				"result":       map[string]any{"type": "completed", "text": "prediction"},
			})
		case "thread/realtime/start":
			modality := str("outputModality")
			if modality != "text" && modality != "audio" {
				_ = encoder.Encode(map[string]any{"id": message.ID, "error": map[string]any{"code": -32602, "message": "outputModality is required"}})
				continue
			}
			if boolean("flushTranscriptTailOnSessionEnd") && str("realtimeEndInstructions") == "" {
				_ = encoder.Encode(map[string]any{"id": message.ID, "error": map[string]any{"code": -32602, "message": "realtimeEndInstructions is required for transcript flush"}})
				continue
			}
			notifyClient("thread/realtime/started", map[string]any{"threadId": thread, "version": "v2"})
		case "thread/realtime/stop":
			notifyClient("thread/realtime/closed", map[string]any{"threadId": thread, "reason": "requested"})
		case "thread/goal/set":
			// Mirror native partial-update semantics: omitted/null status keeps
			// an existing goal's status; omitted objective keeps the current
			// objective; a new objective on a complete goal re-activates it.
			objective, hasObjective := nullableString(message.Params, "objective")
			status, hasStatus := nullableString(message.Params, "status")
			current := goals[thread]
			if current == nil && objective == "" && !hasStatus {
				_ = encoder.Encode(map[string]any{"id": message.ID, "error": map[string]any{"code": -32602, "message": "goal has no objective"}})
				continue
			}
			goal := &fixtureGoal{}
			if current != nil {
				*goal = *current
			}
			if hasObjective {
				if objective == "" {
					_ = encoder.Encode(map[string]any{"id": message.ID, "error": map[string]any{"code": -32602, "message": "goal objective must not be empty"}})
					continue
				}
				if objective != goal.objective {
					goal.objective = objective
					// A replaced objective starts a fresh active goal unless
					// this request supplies an explicit status.
					if !hasStatus {
						goal.status = "active"
					}
				}
			}
			if hasStatus {
				goal.status = status
			}
			if goal.status == "" {
				goal.status = "active"
			}
			if raw, ok := message.Params["tokenBudget"]; ok && string(raw) != "null" {
				var budget int64
				if json.Unmarshal(raw, &budget) != nil || budget <= 0 {
					_ = encoder.Encode(map[string]any{"id": message.ID, "error": map[string]any{"code": -32602, "message": "invalid token budget"}})
					continue
				}
				goal.tokenBudget = &budget
			}
			goals[thread] = goal
			payload := map[string]any{"threadId": thread, "status": goal.status, "objective": goal.objective}
			if goal.tokenBudget != nil {
				payload["tokenBudget"] = *goal.tokenBudget
			}
			if origin, ok := nullableString(message.Params, "origin"); ok {
				payload["origin"] = origin
			}
			notifyClient("thread/goal/updated", map[string]any{"threadId": thread, "goal": payload})
			result["goal"] = payload
			if goal.status == "active" && (current == nil || current.status != "active" || (hasObjective && objective != current.objective)) {
				start(thread)
			}
		case "thread/goal/clear":
			delete(goals, thread)
			notifyClient("thread/goal/cleared", map[string]any{"threadId": thread})
		case "thread/unsubscribe":
			var status string
			var notice map[string]any
			switch {
			case !loaded[thread]:
				status = "notLoaded"
			case unsubscribed[thread]:
				status = "notSubscribed"
			default:
				status = "unsubscribed"
				unsubscribed[thread] = true
				notice = map[string]any{"threadId": thread, "status": map[string]any{"type": "idle"}}
			}
			result["status"] = status
			if notice != nil {
				// A subscription change is broadcast metadata, not a
				// per-connection turn/item event.
				notifyClient("thread/status/changed", notice)
			}
		case "test/threadStatus":
			status := str("status")
			if status == "" {
				status = "idle"
			}
			notifyClient("thread/status/changed", map[string]any{"threadId": thread, "status": map[string]any{"type": status}})
			if status == "notLoaded" {
				loaded[thread] = false
				unsubscribed[thread] = false
			}
		case "thread/goal/get":
			if ephemeralThreads[thread] {
				_ = encoder.Encode(map[string]any{"id": message.ID, "error": map[string]any{"code": -32600, "message": "goals are unavailable for ephemeral threads"}})
				continue
			}
			if goal := goals[thread]; goal != nil {
				payload := map[string]any{"threadId": thread, "status": goal.status, "objective": goal.objective}
				if goal.tokenBudget != nil {
					payload["tokenBudget"] = *goal.tokenBudget
				}
				result["goal"] = payload
			} else {
				result["goal"] = nil
			}
		case "thread/shellCommand":
			sequence++
			turn := map[string]any{"id": fmt.Sprintf("shell-%d", sequence), "status": "inProgress"}
			notifyClient("turn/started", map[string]any{"threadId": thread, "turn": turn})
			turn["status"] = "completed"
			notifyClient("turn/completed", map[string]any{"threadId": thread, "turn": turn})
		case "process/spawn":
			handle := str("processHandle")
			var command []string
			_ = json.Unmarshal(message.Params["command"], &command)
			if processes[handle] || len(command) == 0 || command[0] == "fail" {
				_ = encoder.Encode(map[string]any{"id": message.ID, "error": map[string]any{"code": -32602, "message": "cannot spawn fixture process"}})
				continue
			}
			processes[handle] = true
			if command[0] == "exit-before-reply" || command[0] == "exit-before-delayed-reply" {
				delete(processes, handle)
				notifyClient("process/exited", map[string]any{"processHandle": handle, "exitCode": 0})
				if command[0] == "exit-before-delayed-reply" {
					processReplies = append(processReplies, message.ID)
					continue
				}
			}
		case "process/kill":
			// Acknowledge first; the test controls the later exit notification.
		case "test/processExit":
			handle := str("processHandle")
			delete(processes, handle)
			notifyClient("process/exited", map[string]any{"processHandle": handle, "exitCode": 0})
		case "test/flushProcessReplies":
			for _, id := range processReplies {
				_ = encoder.Encode(map[string]any{"id": id, "result": map[string]any{}})
			}
			processReplies = nil
		case "turn/interrupt", "test/finish":
			status := str("status")
			if status == "" {
				status = "completed"
			}
			if message.Method == "turn/interrupt" {
				status = "interrupted"
				if goal := goals[thread]; goal != nil {
					goal.status = "paused"
				}
			}
			turn := map[string]any{"id": turns[thread], "status": status}
			delete(turns, thread)
			notifyClient("turn/completed", map[string]any{"threadId": thread, "turn": turn})
			if goal := goals[thread]; goal != nil && goal.status == "active" {
				start(thread)
			}
		case "test/toolRequest":
			_ = encoder.Encode(map[string]any{"id": "tool-server-id", "method": "item/tool/call", "params": map[string]any{"threadId": thread, "arguments": "private tool data"}})
		case "test/launch":
			cwd, _ := os.Getwd()
			result = map[string]any{"args": os.Args[1:], "cwd": cwd, "home": os.Getenv("HOME"), "user": os.Getenv("USER"), "parentPid": os.Getppid()}
		case "test/deferReply":
			deferredReply = message.ID
			continue
		case "test/flushReply":
			_ = encoder.Encode(map[string]any{"id": deferredReply, "result": map[string]any{}})
			deferredReply = nil
		case "test/stderr":
			fmt.Fprintln(os.Stderr, "private prompt in native stderr")
		case "test/persistGoal":
			goals[thread] = &fixtureGoal{objective: "private goal", status: "active"}
		case "test/delayReviews":
			delayReviews = true
		case "test/delayTurnReplies":
			delayTurnReplies = true
		case "test/flushTurnReplies":
			delayTurnReplies = false
			for _, reply := range turnReplies {
				_ = encoder.Encode(reply)
			}
			turnReplies = nil
		case "test/rejectTurns":
			rejectTurns = true
		case "test/acceptTurns":
			rejectTurns = false
		case "test/ackBeforePrediction":
			ackBeforePrediction = true
		case "test/ackBeforePredictionOff":
			ackBeforePrediction = false
		case "test/delayPredictions":
			delayPrediction = true
		case "test/failPredictions":
			failPrediction = true
		case "test/flushPredictions":
			for _, pending := range pendingPredictions {
				result := map[string]any{"type": "completed", "text": "prediction"}
				if failPrediction {
					result = map[string]any{"type": "failed"}
				}
				notifyClient("thread/prediction/updated", map[string]any{
					"threadId":     pending["threadId"],
					"sourceTurnId": pending["sourceTurnId"],
					"result":       result,
				})
				if id, ok := pending["id"]; ok && id != nil {
					_ = encoder.Encode(map[string]any{"id": id, "result": map[string]any{}})
				}
			}
			pendingPredictions = nil
		case "test/exit":
			os.Exit(39)
		case "test/error":
			_ = encoder.Encode(map[string]any{"id": message.ID, "error": map[string]any{"code": -32000, "message": "private prompt in error"}})
			continue
		case "test/notify":
			notifyClient(str("method"), message.Params["params"])
		case "":
			notifyClient("test/toolReply", map[string]any{"id": message.ID})
			continue
		}
		if len(message.ID) > 0 {
			_ = encoder.Encode(map[string]any{"id": message.ID, "result": result})
		}
	}
}

// rpcForkConfigurationOverrides reports whether a prediction fork carries any
// override native rejects. Null and empty values are treated as absent.
func rpcForkConfigurationOverrides(params map[string]json.RawMessage) bool {
	for _, key := range []string{"approvalPolicy", "approvalsReviewer", "baseInstructions", "beforeTurnId", "config", "cwd", "developerInstructions", "lastTurnId", "model", "modelProvider", "path", "permissions", "runtimeWorkspaceRoots", "sandbox", "serviceTier"} {
		raw, exists := params[key]
		if !exists {
			continue
		}
		var value any
		if json.Unmarshal(raw, &value) != nil || value == nil {
			continue
		}
		if text, ok := value.(string); ok && text == "" {
			continue
		}
		return true
	}
	return false
}

// nullableString returns a non-null string field and whether it was supplied.
func nullableString(params map[string]json.RawMessage, field string) (string, bool) {
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
