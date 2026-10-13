package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	for _, arg := range os.Args[1:] {
		if arg == "--version" {
			fmt.Println("2.1.119 (Claude Code)")
			return
		}
	}
	// Prove that the guard, rather than assumptions about Bun/native FD
	// preservation, keeps the reservation alive when a proxy is killed.
	if os.Getenv("FAKE_CLAUDE_DROP_SLOT_FD") == "1" {
		_ = syscall.Close(4)
	}
	dir, mode := os.Getenv("FAKE_CLAUDE_DIR"), os.Getenv("FAKE_CLAUDE_MODE")
	pid := strconv.Itoa(os.Getpid())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		sig := <-signals
		if dir != "" {
			_ = os.WriteFile(filepath.Join(dir, pid+".terminated"), []byte(sig.String()), 0600)
		}
		os.Exit(128 + int(sig.(syscall.Signal)))
	}()
	if dir != "" {
		data, _ := json.Marshal(os.Args[1:])
		_ = os.WriteFile(filepath.Join(dir, pid+".started"), data, 0600)
	}
	if mode == "hold" {
		waitRelease(dir, pid)
		return
	}
	if mode == "echo" {
		data, _ := io.ReadAll(os.Stdin)
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"args": os.Args[1:], "stdin": string(data), "home": os.Getenv("HOME"), "config_dir": os.Getenv("CLAUDE_CONFIG_DIR")})
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 32<<20)
	index := 0
	permissionPending := false
	for scanner.Scan() {
		line := scanner.Bytes()
		var message map[string]any
		if json.Unmarshal(line, &message) != nil {
			os.Exit(2)
		}
		kind, _ := message["type"].(string)
		name := kind
		if kind == "control_request" {
			request, _ := message["request"].(map[string]any)
			name, _ = request["subtype"].(string)
			response := map[string]any{"commands": []any{}, "models": []any{}, "agents": []any{}, "account": map[string]any{}}
			if name == "cancel_async_message" {
				response = map[string]any{"cancelled": false}
			}
			send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": message["request_id"], "response": response}})
		} else if kind == "user" {
			index++
			name = fmt.Sprintf("user-%d", index)
			if path := os.Getenv("FAKE_CLAUDE_EVENT_FILE"); path != "" {
				file, err := os.Open(path)
				if err != nil {
					os.Exit(2)
				}
				_, err = io.Copy(os.Stdout, file)
				file.Close()
				if err != nil {
					os.Exit(2)
				}
			}
			if os.Getenv("FAKE_CLAUDE_PERMISSION") == "1" {
				permissionPending = true
				send(map[string]any{"type": "control_request", "request_id": "tool-permission", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "Read", "input": map[string]any{"file_path": "sample.txt"}, "tool_use_id": "tool-1"}})
			} else {
				result()
			}
		} else if kind == "control_response" && permissionPending {
			permissionPending = false
			result()
		}
		if dir != "" {
			_ = os.WriteFile(filepath.Join(dir, pid+"-"+name+".json"), append([]byte(nil), line...), 0600)
		}
	}
	if mode == "stream-hold" {
		waitRelease(dir, pid)
	}
}

func waitRelease(dir, pid string) {
	for {
		_, a := os.Stat(filepath.Join(dir, "release-all"))
		_, b := os.Stat(filepath.Join(dir, "release-"+pid))
		if a == nil || b == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func send(message any) { _ = json.NewEncoder(os.Stdout).Encode(message) }

func result() {
	text := "fixture-ok"
	if size, _ := strconv.Atoi(os.Getenv("FAKE_CLAUDE_RESULT_BYTES")); size > 0 {
		text = strings.Repeat("a", size)
	}
	send(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": text, "session_id": "11111111-1111-4111-8111-111111111111", "uuid": "22222222-2222-4222-8222-222222222222", "duration_ms": 1, "duration_api_ms": 1, "num_turns": 1, "total_cost_usd": 0, "usage": map[string]any{}, "modelUsage": map[string]any{}, "permission_denials": []any{}, "stop_reason": "end_turn"})
}
