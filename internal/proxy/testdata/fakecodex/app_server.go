package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func appServer(dir string) {
	encoder := json.NewEncoder(os.Stdout)
	notify := func(method string, params any) {
		_ = encoder.Encode(map[string]any{"method": method, "params": params})
	}
	turns := make(map[string]string)
	goals := make(map[string]string)
	processes := make(map[string]bool)
	sequence := 0
	experimental := false
	delayReviews := false
	var reviewReplies []map[string]any
	var deferredReply json.RawMessage
	var processReplies []json.RawMessage
	start := func(thread string) map[string]any {
		if turns[thread] == "" {
			sequence++
			turns[thread] = fmt.Sprintf("turn-%d", sequence)
			notify("turn/started", map[string]any{"threadId": thread, "turn": map[string]any{"id": turns[thread], "status": "inProgress"}})
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
		thread := str("threadId")
		result := map[string]any{}
		switch message.Method {
		case "initialize":
			var capabilities map[string]bool
			_ = json.Unmarshal(message.Params["capabilities"], &capabilities)
			experimental = capabilities["experimentalApi"]
			result["userAgent"] = "fixture"
		case "thread/start", "thread/resume", "thread/fork":
			source := thread
			if thread == "" || message.Method == "thread/fork" {
				sequence++
				thread = fmt.Sprintf("thread-%d", sequence)
			}
			info := map[string]any{"id": thread}
			if message.Method == "thread/fork" {
				goals[thread] = goals[source]
				info["forkedFromId"] = source
			}
			notify("thread/started", map[string]any{"thread": info})
			result["thread"], result["sandbox"] = info, map[string]any{"type": "dangerFullAccess"}
		case "thread/settings/update":
			if !experimental {
				_ = encoder.Encode(map[string]any{"id": message.ID, "error": map[string]any{"code": -32600, "message": "requires experimentalApi"}})
				continue
			}
			result["sandboxPolicy"] = message.Params["sandboxPolicy"]
		case "turn/start":
			result["turn"] = start(thread)
		case "review/start":
			if str("delivery") == "detached" {
				sequence++
				child := fmt.Sprintf("review-%d", sequence)
				notify("thread/started", map[string]any{"thread": map[string]any{"id": child, "forkedFromId": thread}})
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
			notify("thread/prediction/updated", map[string]any{"threadId": thread})
		case "thread/realtime/start":
			notify("thread/realtime/started", map[string]any{"threadId": thread})
		case "thread/realtime/stop":
			notify("thread/realtime/closed", map[string]any{"threadId": thread, "reason": "requested"})
		case "thread/goal/set":
			status := str("status")
			if status == "" {
				status = "active"
			}
			goals[thread] = status
			goal := map[string]any{"threadId": thread, "status": status, "objective": "private goal"}
			notify("thread/goal/updated", map[string]any{"threadId": thread, "goal": goal})
			result["goal"] = goal
			if status == "active" {
				start(thread)
			}
		case "thread/goal/clear":
			goals[thread] = ""
			notify("thread/goal/cleared", map[string]any{"threadId": thread})
		case "thread/goal/get":
			if status := goals[thread]; status != "" {
				result["goal"] = map[string]any{"threadId": thread, "status": status}
			} else {
				result["goal"] = nil
			}
		case "thread/shellCommand":
			sequence++
			turn := map[string]any{"id": fmt.Sprintf("shell-%d", sequence), "status": "inProgress"}
			notify("turn/started", map[string]any{"threadId": thread, "turn": turn})
			turn["status"] = "completed"
			notify("turn/completed", map[string]any{"threadId": thread, "turn": turn})
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
				notify("process/exited", map[string]any{"processHandle": handle, "exitCode": 0})
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
			notify("process/exited", map[string]any{"processHandle": handle, "exitCode": 0})
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
				goals[thread] = "paused"
			}
			turn := map[string]any{"id": turns[thread], "status": status}
			delete(turns, thread)
			notify("turn/completed", map[string]any{"threadId": thread, "turn": turn})
			if goals[thread] == "active" {
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
			goals[thread] = "active"
		case "test/delayReviews":
			delayReviews = true
		case "test/exit":
			os.Exit(39)
		case "test/error":
			_ = encoder.Encode(map[string]any{"id": message.ID, "error": map[string]any{"code": -32000, "message": "private prompt in error"}})
			continue
		case "":
			notify("test/toolReply", map[string]any{"id": message.ID})
			continue
		}
		if len(message.ID) > 0 {
			_ = encoder.Encode(map[string]any{"id": message.ID, "result": result})
		}
	}
}
