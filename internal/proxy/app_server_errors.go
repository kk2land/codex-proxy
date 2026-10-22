package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
)

// stageError pairs an operational stage with its actual cause. Callers keep
// the stable stage text for clients and logs while the wrapped cause remains
// available through the error chain. A nil cause must never reach %w.
func stageError(stage string, cause error) error {
	if cause == nil {
		return errors.New(stage)
	}
	return fmt.Errorf("%s: %w", stage, cause)
}

func stageErrorText(stage string, cause error) string {
	return stageError(stage, cause).Error()
}

// rpcErrorPayload reports a failed response by the presence of a non-null
// "error" field. Native error shapes are not guaranteed to be objects, so the
// raw payload is returned unchanged and callers must never infer success from
// an empty or non-object value.
func rpcErrorPayload(message rpcMessage) json.RawMessage {
	raw := message["error"]
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return raw
}

func rpcErrorPresent(message rpcMessage) bool {
	return rpcErrorPayload(message) != nil
}

// nativeRPCError describes a native error response while preserving its raw
// code, message, and data (or the entire non-object payload).
func nativeRPCError(message rpcMessage) error {
	raw := rpcErrorPayload(message)
	if raw == nil {
		return nil
	}
	return errors.New(nativeRPCErrorText(raw))
}

func nativeRPCErrorText(raw json.RawMessage) string {
	text := "native RPC error"
	nativeErr := rpcErrorObject(raw)
	if code := nativeErr["code"]; len(code) > 0 && string(code) != "null" {
		text += " code=" + string(code)
	}
	if message := rpcString(nativeErr, "message"); message != "" {
		text += fmt.Sprintf(" message=%q", message)
	}
	if data := nativeErr["data"]; len(data) > 0 && string(data) != "null" {
		text += " data=" + string(data)
	}
	if text == "native RPC error" {
		text += " payload=" + errorPayloadText(raw)
	}
	return text
}

// rpcErrorObject decodes an object-shaped payload. Non-object or empty
// payloads return an empty map; callers retain the raw value separately.
func rpcErrorObject(raw json.RawMessage) rpcMessage {
	if len(raw) == 0 || raw[0] != '{' {
		return nil
	}
	var object rpcMessage
	if json.Unmarshal(raw, &object) != nil {
		return nil
	}
	return object
}

// errorPayloadText renders arbitrary native error payloads without losing the
// original content.
func errorPayloadText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return string(raw)
}

// notificationRPCError returns the raw native error carried by a failed
// notification. Besides params.error, native uses params.message for error
// channels and turn.error for failed turn/completed events.
func notificationRPCError(method string, params rpcMessage) json.RawMessage {
	if raw := rpcErrorPayload(params); raw != nil {
		return raw
	}
	if method == "turn/completed" {
		if raw := rpcErrorPayload(rpcObject(params, "turn")); raw != nil {
			return raw
		}
	}
	if method == "error" || len(method) >= len("/error") && method[len(method)-len("/error"):] == "/error" {
		if message := rpcString(params, "message"); message != "" {
			return rpcValue(message)
		}
	}
	return nil
}
