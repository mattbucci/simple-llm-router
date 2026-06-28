// Package model holds the domain types and errors for simple-llm-router.
//
// It is the innermost layer of the architecture (ADR-0003): it imports only the
// standard library and is imported by every other internal package. Nothing in
// here knows about HTTP transport, configuration, or a specific inference
// engine.
package model

// Protocol identifies an API shape spoken on either edge of the router
// (ADR-0016).
type Protocol string

const (
	// ProtocolOpenAI is the OpenAI Chat Completions shape
	// (POST /v1/chat/completions).
	ProtocolOpenAI Protocol = "openai"
	// ProtocolAnthropic is the Anthropic Messages shape (POST /v1/messages).
	ProtocolAnthropic Protocol = "anthropic"
)
