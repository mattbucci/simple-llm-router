package router

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	"github.com/mattbucci/simple-llm-router/internal/model"
)

const (
	// maxAttempts bounds failover to a primary plus two fallbacks, mirroring
	// OpenRouter (ADR-0006, ADR-0013). Combined with the healthy-candidate count
	// it caps the work a single request can trigger.
	maxAttempts = 3
	// maxSSEToken bounds a single spliced SSE record so a hostile or oversized
	// stream cannot exhaust memory.
	maxSSEToken = 16 << 20
)

var _ Strategy = (*proxyStrategy)(nil)

// proxyStrategy is the shared selector-driven path: it orders healthy candidates
// (ADR-0006), prepares the outbound body (rewrite model, strip plugins — ADR-0001),
// dispatches to a backend, and fails over before any byte is committed.
type proxyStrategy struct {
	backends  map[string]Backend
	health    HealthView
	metrics   MetricsRecorder
	selectors map[string]Selector
	logger    *slog.Logger
}

// Execute selects backends and proxies the request with failover (ADR-0006). For
// a non-stream request it buffers the upstream body, parses usage best-effort,
// and relays it; for a stream it splices SSE records to the sink. Pre-first-byte
// connect errors and upstream 502/503/504 fail over to the next candidate; 4xx,
// post-first-byte errors, and context cancellation never do.
func (p *proxyStrategy) Execute(ctx context.Context, req *model.ChatRequest, plan *Plan, sink ResponseSink) (*Outcome, error) {
	snap := p.health.Snapshot()
	if snap == nil {
		snap = model.EmptySnapshot()
	}

	sel := p.selectors[plan.Selector]
	if sel == nil {
		sel = p.selectors[selectorRoundRobin]
	}
	ordered := sel.Order(snap, req, plan.Candidates, plan.MinQuality)
	// Prefer a backend that speaks the consumer's protocol so the same-protocol
	// near-passthrough path is taken when available (ADR-0016); this only reorders
	// the failover sequence, it performs no translation. The pareto selector's
	// cost order is authoritative (ADR-0013): re-partitioning it by protocol could
	// promote a costlier same-protocol candidate ahead of the cheapest one, so we
	// never apply this to a pareto plan. round_robin is unaffected.
	if plan.Selector != selectorPareto {
		ordered = preferSameProtocol(ordered, p.backends, req.Consumer)
	}

	if len(ordered) == 0 {
		err := model.ErrNoHealthyBackend(req.Model)
		return &Outcome{Status: err.Status}, err
	}

	outcome := &Outcome{}
	var lastErr error
	sent := 0
	for _, cand := range ordered {
		be, ok := p.backends[cand.Backend]
		if !ok {
			continue
		}
		if sent >= maxAttempts {
			break
		}
		if err := ctx.Err(); err != nil {
			return outcome, fmt.Errorf("router: request canceled before dispatch: %w", err)
		}
		if sent > 0 {
			p.metrics.IncFailover()
			outcome.Failovers++
		}
		sent++

		outcome.Backend = cand.Backend
		outcome.UpstreamModel = cand.Model
		outcome.ProviderProtocol = be.Protocol()

		// Per-candidate protocol decision (ADR-0016): the consumer-vs-provider
		// pairing is resolved here, per candidate (not once per request), because
		// failover may cross backends speaking different protocols. On the
		// same-protocol native relay ("Anthropic->Anthropic = full passthrough",
		// ADR-0001) the ORIGINAL consumer bytes (model rewritten, plugins stripped)
		// go to the provider's native endpoint with NO translation and the reply is
		// relayed verbatim — so tools, tool_choice, top_k, metadata, and
		// cache_control survive instead of being dropped by a double
		// Anthropic->OpenAI->Anthropic translation. Every other pairing keeps the
		// canonical translate/relay path. Selecting the body builder, dispatch fn,
		// and handler pair once here keeps that choice off the stream/unary split
		// (and the failover invariant intact).
		buildBody := func() ([]byte, error) { return buildOutboundBody(req, cand.Model) }
		chat := be.Chat
		streamHandler := handleStreamResponse
		unaryHandler := handleUnaryResponse
		if nativeRelayFor(req, be) {
			buildBody = func() ([]byte, error) { return buildNativeBody(req.ConsumerBody, cand.Model) }
			chat = be.ChatNative
			streamHandler = handleNativeStreamResponse
			unaryHandler = handleNativeUnaryResponse
		}

		body, err := buildBody()
		if err != nil {
			// A marshal failure is deterministic — the same input fails for every
			// candidate — so surface it rather than burning the failover budget.
			apiErr := model.ErrBadRequest("could not encode upstream request body")
			outcome.Status = apiErr.Status
			return outcome, apiErr
		}

		resp, err := chat(ctx, body, req.Stream)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return outcome, fmt.Errorf("router: upstream call canceled: %w", ctx.Err())
			}
			// Connect/transport error before any byte was sent: fail over, and ask
			// health to fast re-probe this backend so a dead one leaves rotation
			// before the next fixed tick (ADR-0005, ADR-0006).
			p.suspectBackend(cand.Backend)
			continue
		}

		// Report the concrete (model, backend) actually selected back to the caller
		// (ADR-0013), independent of any upstream echo. The sink buffers these and
		// emits them only when it commits the response, so a failover sequence
		// harmlessly overwrites them until one candidate's response is written.
		sink.SetHeader("X-Router-Model", cand.Model)
		sink.SetHeader("X-Router-Backend", cand.Backend)

		var done, retry bool
		var herr error
		if req.Stream {
			done, retry, herr = streamHandler(ctx, resp, sink, outcome)
		} else {
			done, retry, herr = unaryHandler(resp, sink, outcome)
		}
		if retry {
			lastErr = herr
			if ctx.Err() != nil {
				return outcome, fmt.Errorf("router: upstream call canceled: %w", ctx.Err())
			}
			// Retryable upstream status (502/503/504): fail over and fast re-probe
			// the failed backend so it leaves rotation promptly (ADR-0005, ADR-0006).
			p.suspectBackend(cand.Backend)
			continue
		}
		if done {
			return outcome, herr
		}
	}

	if lastErr != nil {
		// Debug-level only: the server already emits exactly one per-request
		// RequestRecord summary (ADR-0011), which carries the resulting status. This
		// is extra failover detail for diagnosis, not a second normal-operation line.
		p.logger.DebugContext(ctx, "router: all candidates failed",
			slog.String("model", req.Model),
			slog.Int("attempts", sent),
			slog.String("last_error", lastErr.Error()))
	}
	err := model.ErrUpstreamUnavailable()
	outcome.Status = err.Status
	return outcome, err
}

// suspectBackend asks the health collaborator to fast re-probe a backend that
// just failed mid-rotation, so it drops out of rotation before the next fixed
// health tick (ADR-0005 SHOULD, ADR-0006). Suspect is part of the HealthView
// interface, so this is a direct polymorphic call.
func (p *proxyStrategy) suspectBackend(name string) {
	p.health.Suspect(name)
}

// handleUnaryResponse buffers and relays a non-stream upstream reply. It signals
// failover on a retryable upstream status or a truncated read (nothing has been
// written to the client yet); otherwise it writes the response and is done.
func handleUnaryResponse(resp *model.UpstreamResponse, sink ResponseSink, outcome *Outcome) (done, retry bool, err error) {
	if isRetryableStatus(resp.Status) {
		resp.Body.Close()
		return false, true, fmt.Errorf("router: upstream status %d", resp.Status)
	}
	defer resp.Body.Close()

	b, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		// The body was truncated before we relayed anything: safe to fail over.
		return false, true, fmt.Errorf("router: read upstream body: %w", readErr)
	}

	outcome.Status = resp.Status
	outcome.Usage = parseUsage(b)
	if werr := sink.WriteResponse(resp.Status, resp.Header, b); werr != nil {
		return true, false, fmt.Errorf("router: write response: %w", werr)
	}
	return true, false, nil
}

// handleStreamResponse drives the streaming path. A retryable status fails over
// (no byte committed); a non-2xx, non-retryable status is relayed as a unary
// error; a 2xx status starts the stream and splices SSE records until the
// upstream ends. Once StartStream writes headers, failover is impossible
// (ADR-0007).
func handleStreamResponse(ctx context.Context, resp *model.UpstreamResponse, sink ResponseSink, outcome *Outcome) (done, retry bool, err error) {
	if isRetryableStatus(resp.Status) {
		resp.Body.Close()
		return false, true, fmt.Errorf("router: upstream stream status %d", resp.Status)
	}
	if resp.Status < 200 || resp.Status >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		outcome.Status = resp.Status
		if werr := sink.WriteResponse(resp.Status, resp.Header, b); werr != nil {
			return true, false, fmt.Errorf("router: write error response: %w", werr)
		}
		return true, false, nil
	}

	outcome.Status = resp.Status
	if rerr := relayStream(ctx, resp, sink, &outcome.Usage); rerr != nil {
		return true, false, rerr
	}
	return true, false, nil
}

// relayStream relays an upstream SSE body to the sink without buffering the whole
// response: it splits on the blank-line record boundary, extracts each record's
// data payload, and emits it via the sink, which frames (and, cross-protocol,
// translates) each event (ADR-0007). The upstream body is always closed.
func relayStream(ctx context.Context, resp *model.UpstreamResponse, sink ResponseSink, usage *model.Usage) error {
	defer resp.Body.Close()

	if err := sink.StartStream(); err != nil {
		return fmt.Errorf("router: start stream: %w", err)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), maxSSEToken)
	sc.Split(splitSSE)

	for sc.Scan() {
		if ctx.Err() != nil {
			break
		}
		payload, isDone := extractData(sc.Bytes())
		if isDone {
			break
		}
		if payload == nil {
			continue
		}
		parseUsageInto(payload, usage)
		if err := sink.WriteEvent(payload); err != nil {
			return fmt.Errorf("router: write stream event: %w", err)
		}
	}
	if scErr := sc.Err(); scErr != nil && ctx.Err() == nil {
		// Best-effort terminate the committed stream on a mid-stream read error.
		_ = sink.EndStream()
		return fmt.Errorf("router: read upstream stream: %w", scErr)
	}

	if err := sink.EndStream(); err != nil {
		return fmt.Errorf("router: end stream: %w", err)
	}
	return nil
}

// handleNativeUnaryResponse relays a non-stream upstream reply verbatim on the
// same-protocol native path (ADR-0016 full passthrough): no translation, the
// upstream status/Content-Type/body pass through unchanged. Failover rules mirror
// handleUnaryResponse — a retryable status or a truncated read (before any byte is
// committed) fails over; everything else writes and is done.
func handleNativeUnaryResponse(resp *model.UpstreamResponse, sink ResponseSink, outcome *Outcome) (done, retry bool, err error) {
	if isRetryableStatus(resp.Status) {
		resp.Body.Close()
		return false, true, fmt.Errorf("router: upstream status %d", resp.Status)
	}
	defer resp.Body.Close()

	b, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		// The body was truncated before we relayed anything: safe to fail over.
		return false, true, fmt.Errorf("router: read upstream body: %w", readErr)
	}

	outcome.Status = resp.Status
	outcome.Usage = parseAnthropicUsage(b)
	if werr := sink.WriteRawResponse(resp.Status, resp.Header, b); werr != nil {
		return true, false, fmt.Errorf("router: write response: %w", werr)
	}
	return true, false, nil
}

// handleNativeStreamResponse drives the streaming same-protocol native path: a
// retryable status fails over (no byte committed); any other non-2xx is relayed
// verbatim as a unary error; a 2xx status starts the stream and copies the
// upstream's own SSE bytes through unchanged (preserving Anthropic event framing).
// Once StartRawStream commits the headers, failover is impossible (ADR-0007).
func handleNativeStreamResponse(ctx context.Context, resp *model.UpstreamResponse, sink ResponseSink, outcome *Outcome) (done, retry bool, err error) {
	if isRetryableStatus(resp.Status) {
		resp.Body.Close()
		return false, true, fmt.Errorf("router: upstream stream status %d", resp.Status)
	}
	if resp.Status < 200 || resp.Status >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		outcome.Status = resp.Status
		if werr := sink.WriteRawResponse(resp.Status, resp.Header, b); werr != nil {
			return true, false, fmt.Errorf("router: write error response: %w", werr)
		}
		return true, false, nil
	}

	outcome.Status = resp.Status
	if rerr := relayRawStream(ctx, resp, sink); rerr != nil {
		return true, false, rerr
	}
	return true, false, nil
}

// relayRawStream copies an upstream SSE body to the sink verbatim without buffering
// the whole response and without reframing or translating — the bytes (including
// each Anthropic "event:" line) reach the consumer exactly as the provider sent
// them (ADR-0016 full passthrough, ADR-0007). The upstream body is always closed.
// Once StartRawStream commits, a mid-stream read error surfaces inline rather than
// failing over.
func relayRawStream(ctx context.Context, resp *model.UpstreamResponse, sink ResponseSink) error {
	defer resp.Body.Close()

	if err := sink.StartRawStream(); err != nil {
		return fmt.Errorf("router: start stream: %w", err)
	}

	buf := make([]byte, 32*1024)
	for {
		if ctx.Err() != nil {
			break
		}
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if werr := sink.WriteRawChunk(buf[:n]); werr != nil {
				return fmt.Errorf("router: write stream chunk: %w", werr)
			}
		}
		if rerr != nil {
			if rerr == io.EOF || ctx.Err() != nil {
				break
			}
			return fmt.Errorf("router: read upstream stream: %w", rerr)
		}
	}
	return nil
}

// buildOutboundBody prepares the upstream request body from the inbound canonical
// raw body (ADR-0001): it copies every field verbatim, rewrites only "model" to the
// resolved upstream id, and removes the reserved "plugins" routing-control field
// so the backend never sees it. No other field is added, removed, or
// semantically altered; unknown fields survive.
func buildOutboundBody(req *model.ChatRequest, upstreamModel string) ([]byte, error) {
	return patchOutboundBody(req.Raw, upstreamModel)
}

// buildNativeBody prepares the same-protocol native relay's outbound body from the
// ORIGINAL consumer bytes (ADR-0016 full passthrough, ADR-0001). An Anthropic body
// also carries a top-level "model", so the same surgical map-patch applies:
// rewrite only "model", strip "plugins", leave every provider-specific field
// (tools, tool_choice, top_k, metadata, cache_control, ...) byte-intact — unlike
// the canonical Raw, which is lossy for those fields.
func buildNativeBody(consumerBody []byte, upstreamModel string) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(consumerBody, &raw); err != nil {
		return nil, fmt.Errorf("decode consumer body: %w", err)
	}
	return patchOutboundBody(raw, upstreamModel)
}

// patchOutboundBody applies ADR-0001's surgical map-patch to a parsed body: copy
// every field verbatim, rewrite only "model" to the resolved upstream id, and drop
// the reserved "plugins" routing-control field. Shared by the canonical and native
// relay paths.
func patchOutboundBody(raw map[string]json.RawMessage, upstreamModel string) ([]byte, error) {
	out := make(map[string]json.RawMessage, len(raw)+1)
	for k, v := range raw {
		if k == "plugins" {
			continue
		}
		out[k] = v
	}
	mb, err := json.Marshal(upstreamModel)
	if err != nil {
		return nil, fmt.Errorf("marshal model: %w", err)
	}
	out["model"] = mb

	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal outbound body: %w", err)
	}
	return b, nil
}

// nativeRelayFor reports whether this candidate takes the same-protocol native
// relay path (ADR-0016): the consumer is Anthropic, the chosen provider also
// speaks Anthropic, and the original consumer bytes are available. On that path
// the backend's ChatNative and the sink's raw-relay methods (both part of the
// Backend/ResponseSink interfaces) forward bytes verbatim; otherwise the canonical
// translate/relay path is used. (OpenAI->OpenAI already gets full byte fidelity
// through the ordinary Chat verbatim relay, so it needs no native branch.)
func nativeRelayFor(req *model.ChatRequest, be Backend) bool {
	return req.Consumer == model.ProtocolAnthropic && be.Protocol() == req.Consumer && len(req.ConsumerBody) > 0
}

// preferSameProtocol stable-partitions candidates so backends speaking the
// consumer's protocol are tried first, preserving the selector's relative order
// within each group (ADR-0016). It only reorders failover; it never translates.
func preferSameProtocol(cands []Candidate, backends map[string]Backend, consumer model.Protocol) []Candidate {
	if consumer == "" || len(cands) < 2 {
		return cands
	}
	same := make([]Candidate, 0, len(cands))
	other := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if be, ok := backends[c.Backend]; ok && be.Protocol() == consumer {
			same = append(same, c)
		} else {
			other = append(other, c)
		}
	}
	return append(same, other...)
}

// isRetryableStatus reports whether an upstream status permits failover before
// any byte is committed (ADR-0006): only 502/503/504.
func isRetryableStatus(s int) bool {
	return s == 502 || s == 503 || s == 504
}

// parseUsage reads token accounting from an OpenAI-shaped response body
// best-effort (ADR-0011); a missing or malformed usage object yields zero.
func parseUsage(body []byte) model.Usage {
	var wrap struct {
		Usage model.Usage `json:"usage"`
	}
	_ = json.Unmarshal(body, &wrap)
	return wrap.Usage
}

// parseAnthropicUsage reads token accounting from an Anthropic Messages response
// body best-effort (ADR-0011) for the native relay path; a missing or malformed
// usage object yields zero. Anthropic's input_tokens/output_tokens map to the
// canonical prompt/completion token fields.
func parseAnthropicUsage(body []byte) model.Usage {
	var wrap struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(body, &wrap)
	return model.Usage{
		PromptTokens:     wrap.Usage.InputTokens,
		CompletionTokens: wrap.Usage.OutputTokens,
	}
}

// parseUsageInto updates usage from a streamed chunk's payload when that chunk
// carries non-zero usage (the final chunk, when usage streaming is enabled), so
// the last reported usage wins.
func parseUsageInto(payload []byte, usage *model.Usage) {
	got := parseUsage(payload)
	if got.PromptTokens != 0 || got.CompletionTokens != 0 || got.ReasoningTokens != 0 {
		*usage = got
	}
}

// splitSSE is a bufio.SplitFunc that yields one SSE record per call, splitting on
// the blank-line ("\n\n") boundary (ADR-0007).
func splitSSE(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
		return i + 2, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// extractData returns the concatenated "data:" payload of one SSE record and
// whether it is the OpenAI [DONE] terminator. A record with no data line yields
// a nil payload. The returned slice aliases the scanner buffer and must be used
// before the next Scan (relayStream does).
func extractData(record []byte) (payload []byte, isDone bool) {
	var data []byte
	for _, line := range bytes.Split(record, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		v := bytes.TrimSpace(line[len("data:"):])
		if data == nil {
			data = v
		} else {
			data = append(append(data, '\n'), v...)
		}
	}
	if data == nil {
		return nil, false
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return nil, true
	}
	return data, false
}
