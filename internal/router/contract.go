// Package router is the application layer: it resolves a model name to a routing
// target, selects backends, and runs the routing strategy (proxy or fusion)
// with failover (ADR-0004, ADR-0006, ADR-0013, ADR-0014). It imports only
// internal/model plus the standard library and never imports internal/backend
// or internal/server — collaborators are reached through the interfaces defined
// here, which those packages implement structurally (ADR-0003).
package router

import (
	"context"
	"net/http"

	"github.com/mattbucci/simple-llm-router/internal/model"
)

// Backend is the outbound adapter the router needs (ADR-0002, ADR-0003).
// internal/backend implements it; any provider-protocol translation is the
// backend's concern (outbound adapter, ADR-0016), so the router always hands it
// an OpenAI-canonical body and gets an OpenAI-shaped response back.
type Backend interface {
	// Name is the configured backend name.
	Name() string
	// Protocol is the provider protocol this backend speaks.
	Protocol() model.Protocol
	// Chat sends a prepared request body to the upstream and returns the raw
	// response for relaying. The caller closes the response body. stream selects
	// the streaming vs unary response path (ADR-0007).
	Chat(ctx context.Context, body []byte, stream bool) (*model.UpstreamResponse, error)
	// ChatNative relays a request body to the provider's native endpoint with NO
	// request/response translation in either direction — the same-protocol
	// full-fidelity relay (ADR-0016 "Anthropic->Anthropic = full passthrough",
	// ADR-0001). The caller has already rewritten "model" and stripped "plugins";
	// every other field (tools, tool_choice, top_k, metadata, cache_control, ...)
	// is forwarded byte-intact and the reply is returned verbatim. The router uses
	// it only on the same-protocol native relay path; otherwise it uses Chat. The
	// caller closes the response body. stream selects streaming vs unary (ADR-0007).
	ChatNative(ctx context.Context, body []byte, stream bool) (*model.UpstreamResponse, error)
}

// HealthView exposes the current lock-free health/discovery snapshot (ADR-0005).
type HealthView interface {
	Snapshot() *model.Snapshot
	// Suspect requests a fast, single-backend re-probe of a backend that just
	// failed mid-rotation, so a dead backend leaves rotation before the next fixed
	// health tick (ADR-0005 SHOULD, ADR-0006). *backend.Monitor implements it; a
	// test fake can no-op it.
	Suspect(name string)
}

// MetricsRecorder is the slice of metrics recorded from inside routing
// (ADR-0011). The server records the rest around the request.
type MetricsRecorder interface {
	IncFailover()
}

// ResponseSink is how a strategy emits a response to the consumer. The server
// provides it. For a cross-protocol consumer the sink translates the
// OpenAI-canonical output the router produces into the consumer's shape before
// writing (ADR-0007, ADR-0016) — keeping all protocol translation in the edge
// adapters, never in the router.
type ResponseSink interface {
	// SetHeader buffers a response header to emit when the response is committed
	// (on WriteResponse or StartStream). The router uses it to report the concrete
	// routing decision back to the caller (X-Router-Model, X-Router-Backend —
	// ADR-0013) independent of any upstream echo. It does not touch the JSON body.
	// Repeated calls overwrite, so a failover sequence safely leaves only the
	// committing candidate's values.
	SetHeader(key, value string)
	// WriteResponse relays a complete, non-streamed upstream response. header is
	// the upstream response header (for Content-Type on the same-protocol path).
	WriteResponse(status int, header http.Header, body []byte) error
	// StartStream writes the streaming response headers (HTTP 200, SSE).
	StartStream() error
	// WriteEvent emits one OpenAI-canonical SSE data payload — the JSON object
	// that appeared between "data: " and the record's blank line. The sink frames
	// and, if cross-protocol, translates it, then flushes.
	WriteEvent(data []byte) error
	// EndStream writes the stream terminator and flushes.
	EndStream() error
	// WriteRawResponse relays a complete unary upstream response verbatim — no
	// protocol translation, honoring the upstream status and Content-Type — for the
	// same-protocol native relay path (ADR-0016 full passthrough, ADR-0001). A
	// verbatim sink (OpenAI) implements it as an alias for WriteResponse.
	WriteRawResponse(status int, header http.Header, body []byte) error
	// StartRawStream commits the SSE response (200 + SSE headers) for the native
	// relay path. After it returns nil, failover is impossible (ADR-0007).
	StartRawStream() error
	// WriteRawChunk relays raw upstream SSE bytes to the consumer verbatim and
	// flushes — no reframing, no translation — preserving the provider's own event
	// framing (ADR-0007, ADR-0016).
	WriteRawChunk(p []byte) error
	// Wrote reports whether any byte has been written to the client yet, so the
	// caller knows whether failover or an error response is still possible
	// (ADR-0006, ADR-0007).
	Wrote() bool
}

// Candidate is one resolved (upstream model, backend) routing option.
type Candidate struct {
	// Model is the upstream model id to send (already resolved from the alias).
	Model string
	// Backend is the backend name to send it to.
	Backend string
	// Quality is the operator-assigned quality score, used by the pareto
	// selector; 0 for round_robin (ADR-0013).
	Quality float64
}

// Selector orders candidates for a proxy strategy, best first, health-filtered
// against the snapshot (ADR-0006). Implementations are stateless apart from
// lock-free rotation counters — no mutexes, no per-request state
// (ADR-0013, ADR-0015).
type Selector interface {
	// Order returns the candidates to try, best first. round_robin rotates the
	// healthy candidates with an atomic counter; pareto filters by min_quality
	// (honoring a per-request plugins override on req) then ranks survivors by a
	// local cost score read from the snapshot, appending relaxed-tier survivors
	// last so the proxy cascades before relaxing (ADR-0013).
	Order(snap *model.Snapshot, req *model.ChatRequest, candidates []Candidate, minQuality float64) []Candidate
}

// Plan is the resolved routing target for a request.
type Plan struct {
	// Alias is the matched alias name, or "" for direct upstream-id resolution.
	Alias string
	// Type is "proxy" or "fusion".
	Type string
	// Selector is "round_robin" or "pareto" (proxy only).
	Selector string
	// MinQuality is the alias's default pareto quality bar (proxy/pareto only).
	MinQuality float64
	// Candidates are the proxy strategy's routing options (single model for a
	// round_robin alias; the pool for a pareto alias; discovered backends for a
	// direct id).
	Candidates []Candidate
	// Panel/Judge/Synthesis configure a fusion strategy (ADR-0014).
	Panel     []Candidate
	Judge     Candidate
	Synthesis Candidate
	// MinPanel is the smallest number of panelist answers fusion proceeds with
	// (ADR-0014); resolveFusion defaults it to 1.
	MinPanel    int
	Temperature float64
	MaxTokens   int
}

// Strategy turns a resolved request into a response written to the sink and
// returns the Outcome for logging (ADR-0006, ADR-0011). proxy strategies wrap a
// Selector; fusion implements its own orchestration (ADR-0014).
type Strategy interface {
	Execute(ctx context.Context, req *model.ChatRequest, plan *Plan, sink ResponseSink) (*Outcome, error)
}

// Outcome reports what actually happened, for the per-request log line
// (ADR-0011). On a failure before any backend produced a response, fields may be
// partially populated and Execute returns a *model.APIError.
type Outcome struct {
	Backend          string
	UpstreamModel    string
	ProviderProtocol model.Protocol
	Failovers        int
	Status           int
	Usage            model.Usage
}

// Alias is the router's resolved view of a configured alias. cmd/router builds
// these from config so the router never imports internal/config (ADR-0003).
type Alias struct {
	Name        string
	Type        string // proxy | fusion
	Selector    string // round_robin | pareto
	Model       string
	Backends    []string
	MinQuality  float64
	Pool        []PoolEntry
	Panel       []PoolEntry
	Judge       Target
	Synthesis   Target
	MinPanel    int
	Temperature float64
	MaxTokens   int
}

// PoolEntry is one candidate model in a pareto pool or fusion panel.
type PoolEntry struct {
	Model    string
	Backends []string
	Quality  float64
}

// Target is a single model + backend set (fusion judge/synthesis).
type Target struct {
	Model    string
	Backends []string
}
