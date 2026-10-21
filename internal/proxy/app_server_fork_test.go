package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func forkBoolTrue(message rpcMessage, field string) bool {
	var value bool
	return json.Unmarshal(message[field], &value) == nil && value
}

func forkMessage(fields map[string]any) rpcMessage {
	params := map[string]any{"threadId": "thread-parent"}
	for key, value := range fields {
		params[key] = value
	}
	return rpcMessage{"method": rpcValue("thread/fork"), "params": rpcValue(params)}
}

func TestEnforceAppPolicyForkVariants(t *testing.T) {
	tests := []struct {
		name       string
		fields     map[string]any
		wantDefer  bool
		hasDefer   bool
		hasSandbox bool
	}{
		{
			name:   "prediction",
			fields: map[string]any{"ephemeral": true, "excludeTurns": true, "experimentalPredictionMode": true},
		},
		{
			name:   "prediction keeps client overrides for native validation",
			fields: map[string]any{"ephemeral": true, "experimentalPredictionMode": true, "config": map[string]any{"model": "client-model"}},
		},
		{
			name:       "ephemeral",
			fields:     map[string]any{"ephemeral": true},
			hasSandbox: true,
		},
		{
			name:       "ephemeral keeps client defer flag",
			fields:     map[string]any{"ephemeral": true, "deferGoalContinuation": true},
			wantDefer:  true,
			hasDefer:   true,
			hasSandbox: true,
		},
		{
			name:       "persistent",
			fields:     map[string]any{},
			wantDefer:  true,
			hasDefer:   true,
			hasSandbox: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message := forkMessage(test.fields)
			enforceAppPolicy(message)
			params := rpcObject(message, "params")
			_, deferExists := params["deferGoalContinuation"]
			if deferExists != test.hasDefer {
				t.Fatalf("deferGoalContinuation presence = %v, want %v: %s", deferExists, test.hasDefer, params["deferGoalContinuation"])
			}
			if deferExists && forkBoolTrue(params, "deferGoalContinuation") != test.wantDefer {
				t.Fatalf("deferGoalContinuation = %s, want %v", params["deferGoalContinuation"], test.wantDefer)
			}
			_, sandboxExists := params["sandbox"]
			_, approvalExists := params["approvalPolicy"]
			_, configExists := params["config"]
			if sandboxExists != test.hasSandbox || approvalExists != test.hasSandbox {
				t.Fatalf("policy injection = sandbox:%v approval:%v, want %v", sandboxExists, approvalExists, test.hasSandbox)
			}
			if test.hasSandbox {
				if rpcString(params, "sandbox") != "danger-full-access" || rpcString(params, "approvalPolicy") != "never" {
					t.Fatalf("unexpected policy values: %s", rpcValue(params))
				}
				config := rpcObject(params, "config")
				if rpcString(config, "sandbox_mode") != "danger-full-access" || rpcString(config, "approval_policy") != "never" {
					t.Fatalf("config policy not applied: %s", params["config"])
				}
				return
			}
			if test.name == "prediction keeps client overrides for native validation" {
				if !configExists || rpcString(rpcObject(params, "config"), "model") != "client-model" {
					t.Fatalf("client config override was rewritten: %s", params["config"])
				}
			} else if configExists {
				t.Fatalf("prediction fork gained a config override: %s", params["config"])
			}
		})
	}
	// Ordinary thread creation keeps its full policy and gains no fork fields.
	for _, method := range []string{"thread/start", "thread/resume"} {
		message := rpcMessage{"method": rpcValue(method), "params": rpcValue(map[string]any{"sandbox": "read-only"})}
		enforceAppPolicy(message)
		params := rpcObject(message, "params")
		if rpcString(params, "sandbox") != "danger-full-access" || rpcString(params, "approvalPolicy") != "never" {
			t.Fatalf("%s policy changed: %s", method, rpcValue(params))
		}
		if _, exists := params["deferGoalContinuation"]; exists {
			t.Fatalf("%s gained a fork-only field", method)
		}
	}
}

func forkCaptures(t *testing.T, dir string) []rpcMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "rpc.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var forks []rpcMessage
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		message, err := decodeRPC([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		if rpcString(message, "method") == "thread/fork" {
			forks = append(forks, message)
		}
	}
	return forks
}

func assertNoField(t *testing.T, params rpcMessage, fields ...string) {
	t.Helper()
	for _, field := range fields {
		if _, exists := params[field]; exists {
			t.Fatalf("captured fork unexpectedly contains %s: %s", field, rpcValue(params))
		}
	}
}

func settingsForThread(t *testing.T, dir, thread string) []rpcMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "rpc.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var updates []rpcMessage
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		message, err := decodeRPC([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		if rpcString(message, "method") != "thread/settings/update" {
			continue
		}
		params := rpcObject(message, "params")
		if rpcString(params, "threadId") == thread {
			updates = append(updates, params)
		}
	}
	return updates
}

func TestAppServerForkPolicyCompatibility(t *testing.T) {
	home := t.TempDir()
	configureAppProxy(t, home, fakeCodex(t), 1, "5s")
	client := startApp(t, home)
	source := client.thread()

	prediction := client.call("thread/fork", map[string]any{
		"threadId": source, "ephemeral": true, "excludeTurns": true, "experimentalPredictionMode": true,
	})
	predictionThread := rpcString(rpcObject(prediction, "thread"), "id")
	if predictionThread == "" || rpcString(rpcObject(prediction, "thread"), "forkedFromId") != source {
		t.Fatalf("prediction fork lost parent: %v", prediction)
	}
	if rpcString(rpcObject(prediction, "sandbox"), "type") != "externalSandbox" {
		t.Fatalf("prediction fork did not apply externalSandbox: %v", prediction)
	}
	awaitSlots(t, home, 0)

	ordinary := client.call("thread/fork", map[string]any{"threadId": source, "ephemeral": true, "excludeTurns": true})
	ordinaryThread := rpcString(rpcObject(ordinary, "thread"), "id")
	persistent := client.call("thread/fork", map[string]any{"threadId": source})
	persistentThread := rpcString(rpcObject(persistent, "thread"), "id")

	forks := forkCaptures(t, client.dir)
	if len(forks) != 3 {
		t.Fatalf("captured %d forks, want 3", len(forks))
	}
	predictionParams := rpcObject(forks[0], "params")
	assertNoField(t, predictionParams, "sandbox", "approvalPolicy", "config", "deferGoalContinuation")
	if !forkBoolTrue(predictionParams, "experimentalPredictionMode") {
		t.Fatal("prediction flag was dropped")
	}
	ephemeralParams := rpcObject(forks[1], "params")
	assertNoField(t, ephemeralParams, "deferGoalContinuation")
	if rpcString(ephemeralParams, "sandbox") != "danger-full-access" || rpcString(ephemeralParams, "approvalPolicy") != "never" {
		t.Fatalf("ordinary ephemeral fork policy changed: %s", rpcValue(ephemeralParams))
	}
	persistentParams := rpcObject(forks[2], "params")
	if !forkBoolTrue(persistentParams, "deferGoalContinuation") {
		t.Fatalf("persistent fork lost forced goal deferral: %s", rpcValue(persistentParams))
	}

	// Every successful fork applies externalSandbox/never before its reply.
	for _, thread := range []string{predictionThread, ordinaryThread, persistentThread} {
		updates := settingsForThread(t, client.dir, thread)
		if len(updates) == 0 || rpcString(rpcObject(updates[0], "sandboxPolicy"), "type") != "externalSandbox" || rpcString(updates[0], "approvalPolicy") != "never" {
			t.Fatalf("fork %s did not apply external sandbox: %v", thread, updates)
		}
	}

	// A prediction child's model task is admitted independently of its parent,
	// and no parent reservation may be reused.
	configureAppProxy(t, home, fakeCodex(t), 2, "5s")
	client.call("turn/start", taskParams(source, "parent task"))
	awaitSlots(t, home, 1)
	client.call("turn/start", taskParams(predictionThread, "prediction child task"))
	awaitSlots(t, home, 2)
	client.finish(predictionThread)
	awaitSlots(t, home, 1)
	client.finish(source)
	awaitSlots(t, home, 0)

	// Client-supplied invalid combinations reach native validation unchanged.
	client.send("prediction-overrides", "thread/fork", map[string]any{
		"threadId": source, "ephemeral": true, "experimentalPredictionMode": true,
		"config": map[string]any{"model": "client-model"},
	})
	if response := client.response("prediction-overrides"); rpcObject(response, "error")["code"] == nil {
		t.Fatalf("prediction override was accepted: %v", response)
	}
	client.send("ephemeral-defer", "thread/fork", map[string]any{
		"threadId": source, "ephemeral": true, "deferGoalContinuation": true,
	})
	if response := client.response("ephemeral-defer"); rpcObject(response, "error")["code"] == nil {
		t.Fatalf("ephemeral defer was accepted: %v", response)
	}
	client.send("prediction-non-ephemeral", "thread/fork", map[string]any{
		"threadId": source, "experimentalPredictionMode": true,
	})
	if response := client.response("prediction-non-ephemeral"); rpcObject(response, "error")["code"] == nil {
		t.Fatalf("non-ephemeral prediction was accepted: %v", response)
	}
	if code := client.stop(); code != 0 {
		t.Fatalf("exit=%d", code)
	}
}
