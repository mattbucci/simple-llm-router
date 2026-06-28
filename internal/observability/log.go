// Package observability provides structured logging, request correlation, and
// hand-rolled Prometheus metrics for the router, all on the standard library
// (ADR-0011): log/slog for logs and a single-owner goroutine for metrics, with
// no third-party logging or metrics client.
package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
	"time"
)

type ctxKey int

const requestIDKey ctxKey = iota

// NewRequestID returns a random 128-bit request id as hex.
func NewRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is not fatal for correlation; fall back to time.
		return "req-" + time.Now().Format("150405.000000")
	}
	return hex.EncodeToString(b[:])
}

// WithRequestID attaches a request id to the context so every layer's log lines
// correlate (ADR-0003, ADR-0011).
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID reads the request id from the context, or "" if absent.
func RequestID(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// NewLogger builds a JSON slog logger at the given level.
func NewLogger(level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}

// RequestRecord is the one structured log line emitted per request (ADR-0011).
// It deliberately omits prompts, completions, secrets, and auth headers.
type RequestRecord struct {
	ConsumerProtocol string
	ModelAlias       string
	UpstreamModel    string
	Backend          string
	ProviderProtocol string
	Status           int
	Latency          time.Duration
	Failovers        int
	PromptTokens     int
	CompletionTokens int
	ReasoningTokens  int
}

// Emit writes the record as a single structured log line, tagged with the
// request id from the context.
func (r RequestRecord) Emit(ctx context.Context, l *slog.Logger) {
	l.LogAttrs(ctx, slog.LevelInfo, "request",
		slog.String("request_id", RequestID(ctx)),
		slog.String("consumer_protocol", r.ConsumerProtocol),
		slog.String("model_alias", r.ModelAlias),
		slog.String("upstream_model", r.UpstreamModel),
		slog.String("backend", r.Backend),
		slog.String("provider_protocol", r.ProviderProtocol),
		slog.Int("status", r.Status),
		slog.Int64("latency_ms", r.Latency.Milliseconds()),
		slog.Int("failovers", r.Failovers),
		slog.Int("prompt_tokens", r.PromptTokens),
		slog.Int("completion_tokens", r.CompletionTokens),
		slog.Int("reasoning_tokens", r.ReasoningTokens),
	)
}
