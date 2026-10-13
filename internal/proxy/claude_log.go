package proxy

import "encoding/json"

// Observe only errors from native stdout. Streaming/lifecycle notifications and
// successful control replies stay silent; proxy cancellation replies already
// have their own lifecycle event and never pass through this observer.
func claudeOutputErrorFields(line []byte) map[string]any {
	var header struct {
		Type           string          `json:"type"`
		Subtype        string          `json:"subtype"`
		IsError        bool            `json:"is_error"`
		Error          json.RawMessage `json:"error"`
		ErrorStatus    *int64          `json:"error_status"`
		APIErrorStatus *int64          `json:"api_error_status"`
		Response       struct {
			Subtype string          `json:"subtype"`
			Error   json.RawMessage `json:"error"`
		} `json:"response"`
	}
	// Decode only the small envelope, not prompts, results or tool payloads.
	// Logging must never change forwarding, including unknown/invalid frames.
	if json.Unmarshal(line, &header) != nil {
		return nil
	}
	errorPayload := header.Error
	failed := header.IsError || header.Type == "error"
	stage := "notification"
	if header.Type == "result" {
		switch header.Subtype {
		case "error_during_execution", "error_max_turns", "error_max_budget_usd", "error_max_structured_output_retries":
			failed = true
		}
	}
	if header.Type == "system" && header.Subtype == "mirror_error" {
		failed = true
	}
	if header.Type == "control_response" {
		failed = failed || header.Response.Subtype == "error"
		errorPayload, stage = header.Response.Error, "control_response"
	}
	if len(errorPayload) > 0 && string(errorPayload) != "null" && string(errorPayload) != `""` {
		failed = true
	}
	if !failed {
		return nil
	}
	fields := map[string]any{"stage": stage, "status": "failed", "direction": "server_to_client"}
	// Protocol strings can contain user-controlled text. Use fixed categories
	// rather than copying arbitrary type/subtype, IDs or native error messages.
	switch header.Type {
	case "assistant", "result", "system", "control_response", "auth_status", "error":
		fields["type"] = header.Type
	default:
		fields["type"] = "other"
	}
	subtype := header.Subtype
	if header.Type == "control_response" {
		subtype = header.Response.Subtype
	}
	if subtype != "" {
		switch subtype {
		case "success", "error", "api_retry", "mirror_error", "error_during_execution", "error_max_turns", "error_max_budget_usd", "error_max_structured_output_retries":
			fields["subtype"] = subtype
		default:
			fields["subtype"] = "unknown"
		}
	}
	var errorType string
	if json.Unmarshal(errorPayload, &errorType) == nil && errorType != "" {
		switch errorType {
		case "authentication_failed", "oauth_org_not_allowed", "billing_error", "rate_limit", "overloaded", "invalid_request", "model_not_found", "server_error", "unknown", "max_output_tokens":
			fields["error_type"] = errorType
		default:
			fields["error_type"] = "unknown"
		}
	}
	var errorObject struct {
		Code *int64 `json:"code"`
	}
	if json.Unmarshal(errorPayload, &errorObject) == nil && errorObject.Code != nil {
		fields["error_code"] = *errorObject.Code
	}
	status := header.ErrorStatus
	if status == nil {
		status = header.APIErrorStatus
	}
	if status != nil && *status >= 100 && *status <= 599 {
		fields["http_status"] = *status
	}
	return fields
}
