package proxy

import (
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
func rpcErrorPayload(message rpcMessage) rpcRaw {
	raw := message["error"]
	if rpcLen(raw) == 0 || rpcNull(raw) {
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

func nativeRPCErrorText(raw rpcRaw) string {
	text := "native RPC error"
	nativeErr := rpcErrorObject(raw)
	if code := nativeErr["code"]; rpcLen(code) > 0 && !rpcNull(code) {
		text += " code=" + rpcLogText(code)
	}
	if message := rpcLogString(nativeErr["message"]); message != "" {
		text += fmt.Sprintf(" message=%q", message)
	}
	if data := nativeErr["data"]; rpcLen(data) > 0 && !rpcNull(data) {
		text += " data=" + rpcLogText(data)
	}
	if text == "native RPC error" {
		text += " payload=" + errorPayloadText(raw)
	}
	return text
}

// rpcErrorObject decodes an object-shaped payload. Non-object or empty
// payloads return an empty map; callers retain the raw value separately.
func rpcErrorObject(raw rpcRaw) rpcMessage {
	if rpcLen(raw) == 0 || rpcKind(raw) != '{' {
		return nil
	}
	var object rpcMessage
	if rpcUnmarshal(raw, &object) != nil {
		return nil
	}
	return object
}

// errorPayloadText renders arbitrary native error payloads, bounding previews
// for large values independently of the original forwarded content.
func errorPayloadText(raw rpcRaw) string {
	if rpcKind(raw) == '"' {
		return rpcLogString(raw)
	}
	return rpcLogText(raw)
}

// notificationRPCError returns the raw native error carried by a failed
// notification. Besides params.error, native uses params.message for error
// channels and turn.error for failed turn/completed events.
func notificationRPCError(method string, params rpcMessage) rpcRaw {
	if raw := rpcErrorPayload(params); raw != nil {
		return raw
	}
	if method == "turn/completed" {
		if raw := rpcErrorPayload(rpcObject(params, "turn")); raw != nil {
			return raw
		}
	}
	if method == "error" || len(method) >= len("/error") && method[len(method)-len("/error"):] == "/error" {
		if message := params["message"]; rpcKind(message) == '"' && rpcLen(message) > 2 {
			return message
		}
	}
	return nil
}
