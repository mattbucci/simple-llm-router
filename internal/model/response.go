package model

import (
	"io"
	"net/http"
)

// UpstreamResponse is a backend's HTTP reply, returned by the Backend adapter
// for the router to relay (ADR-0001, ADR-0007). The caller owns Body and MUST
// close it.
type UpstreamResponse struct {
	Status int
	Header http.Header
	Body   io.ReadCloser
}

// Usage captures token accounting for the per-request log line (ADR-0011).
// Non-standard reasoning_tokens is included; fields absent on a response stay
// zero. Usage is read from a copy of the body and never alters what is relayed.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	ReasoningTokens  int `json:"reasoning_tokens"`
}
