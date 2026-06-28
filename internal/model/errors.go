package model

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// APIError carries the HTTP status and OpenAI-style error code the router should
// return to the consumer. The request path returns these instead of panicking
// (ADR-0015).
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s (%d): %s", e.Code, e.Status, e.Message)
}

// errorBody is the OpenAI-compatible error envelope APIError.Body renders.
type errorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// Body renders the error as an OpenAI-compatible error JSON document.
func (e *APIError) Body() []byte {
	var doc errorBody
	doc.Error.Message = e.Message
	doc.Error.Type = "router_error"
	doc.Error.Code = e.Code
	b, err := json.Marshal(doc)
	if err != nil { // unreachable for these plain-string fields
		return []byte(`{"error":{"message":"internal error","type":"router_error","code":"internal"}}`)
	}
	return b
}

// ErrModelNotFound is returned when a model name resolves to no backend at all —
// neither a configured alias nor an upstream id any backend advertises
// (ADR-0004, ADR-0006): HTTP 404.
func ErrModelNotFound(name string) *APIError {
	return &APIError{
		Status:  http.StatusNotFound,
		Code:    "model_not_found",
		Message: fmt.Sprintf("model %q is not a known alias or upstream id", name),
	}
}

// ErrNoHealthyBackend is returned when a model is known but no backend serving
// it is currently healthy (ADR-0006): HTTP 503.
func ErrNoHealthyBackend(name string) *APIError {
	return &APIError{
		Status:  http.StatusServiceUnavailable,
		Code:    "no_healthy_backend",
		Message: fmt.Sprintf("no healthy backend currently serves %q", name),
	}
}

// ErrUpstreamUnavailable is returned when every candidate backend failed after
// the failover budget was exhausted (ADR-0006): HTTP 502.
func ErrUpstreamUnavailable() *APIError {
	return &APIError{
		Status:  http.StatusBadGateway,
		Code:    "upstream_unavailable",
		Message: "all candidate backends failed",
	}
}

// ErrBadRequest is returned for a malformed inbound request the router itself
// rejects (e.g. unparseable JSON on a translated path): HTTP 400.
func ErrBadRequest(msg string) *APIError {
	return &APIError{Status: http.StatusBadRequest, Code: "invalid_request_error", Message: msg}
}
