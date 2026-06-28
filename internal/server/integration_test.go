//go:build integration

// Package server_test's integration tier (ADR-0012) proves the router works
// end-to-end against a REAL, already-running router process — not the in-memory
// fakes the unit tests drive. Nothing here spins up a server or contacts a
// backend directly: every case speaks plain HTTP to ROUTER_URL exactly the way a
// consumer SDK would, mirroring the uv eval battery (evals/run_evals.py) so the
// two harnesses assert the same ADR guarantees from two languages.
//
// Public-safe by construction: no hostnames, model ids, or filesystem paths are
// baked in. The router base URL comes from ROUTER_URL (default
// http://localhost:8080) and the three routing aliases exercised come from
// NORTH_ALIAS / GEMMA_ALIAS / SMART_ALIAS (defaults "north" / "gemma" / "smart").
// If ROUTER_URL is unreachable every case t.Skips, so `go test -tags integration`
// is a no-op on a machine with no fleet rather than a failure.
//
// Only net/http + encoding/json carry the protocol; the remaining imports are
// plumbing (env, timeouts, SSE line scanning).
package server_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// ----- environment-derived configuration (no hardcoded fleet specifics) ------

// itEnv returns the trimmed value of key, or def when unset/blank.
func itEnv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// itRouterURL is the base URL of the running router under test.
func itRouterURL() string {
	return strings.TrimRight(itEnv("ROUTER_URL", "http://localhost:8080"), "/")
}

// Routing aliases the cases drive. These are config names, not upstream ids, so
// they stay stable and public even as the fleet behind them changes.
func itNorthAlias() string { return itEnv("NORTH_ALIAS", "north") }
func itGemmaAlias() string { return itEnv("GEMMA_ALIAS", "gemma") }
func itSmartAlias() string { return itEnv("SMART_ALIAS", "smart") }

// itTimeout bounds a single request. Reasoning models can be slow, so the
// default is generous and overridable via ROUTER_TEST_TIMEOUT (a Go duration).
func itTimeout() time.Duration {
	if d, err := time.ParseDuration(itEnv("ROUTER_TEST_TIMEOUT", "")); err == nil && d > 0 {
		return d
	}
	return 120 * time.Second
}

// ----- HTTP plumbing ---------------------------------------------------------

// itClient relies on per-request context deadlines rather than Client.Timeout so
// the streaming case can read a long-lived SSE body without a blanket cutoff.
func itClient() *http.Client { return &http.Client{} }

// itNewRequest builds a request to path under ROUTER_URL, attaching JSON headers
// when a body is present and an optional credential from ROUTER_TOKEN (set both
// header forms the router accepts, ADR-0009) so the suite works against an
// auth-enabled router too.
func itNewRequest(ctx context.Context, t *testing.T, method, path string, body []byte) *http.Request {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, itRouterURL()+path, r)
	if err != nil {
		t.Fatalf("build request %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok := itEnv("ROUTER_TOKEN", ""); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("x-api-key", tok)
	}
	return req
}

// itRequireRouter probes /healthz with a short timeout; a connection failure
// skips the calling test so the integration tier is inert without a live router.
func itRequireRouter(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req := itNewRequest(ctx, t, http.MethodGet, "/healthz", nil)
	resp, err := itClient().Do(req)
	if err != nil {
		t.Skipf("router at %s unreachable (%v); set ROUTER_URL to a running router", itRouterURL(), err)
	}
	resp.Body.Close()
}

// itDo sends a unary request, returning the response (body already drained and
// closed; status and headers remain readable) and the body bytes.
func itDo(t *testing.T, method, path string, payload any) (*http.Response, []byte) {
	t.Helper()
	var body []byte
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		body = b
	}
	ctx, cancel := context.WithTimeout(context.Background(), itTimeout())
	defer cancel()
	resp, err := itClient().Do(itNewRequest(ctx, t, method, path, body))
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s body: %v", method, path, err)
	}
	return resp, data
}

// itChatBody is a canonical OpenAI chat request. temperature is pinned to 0 for
// determinism; max_tokens is omitted when non-positive.
func itChatBody(modelAlias, content string, maxTokens int, stream bool) map[string]any {
	body := map[string]any{
		"model":       modelAlias,
		"temperature": 0,
		"messages":    []map[string]any{{"role": "user", "content": content}},
	}
	if maxTokens > 0 {
		body["max_tokens"] = maxTokens
	}
	if stream {
		body["stream"] = true
	}
	return body
}

// ----- response shapes the cases read ---------------------------------------

type itChatCompletion struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
			// Pointer so an absent key (nil) is distinguishable from an empty
			// string: ADR-0001 reasoning passthrough is satisfied by the key
			// merely surviving, exactly as the uv eval checks it.
			ReasoningContent *string `json:"reasoning_content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"usage"`
}

type itAPIError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

type itAnthropicMessage struct {
	Type    string `json:"type"`
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// ----- cases (mirroring evals/run_evals.py) ----------------------------------

// TestIntegrationHealthz: liveness endpoint is 200 whenever the process is up.
func TestIntegrationHealthz(t *testing.T) {
	itRequireRouter(t)
	resp, body := itDo(t, http.MethodGet, "/healthz", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200 (body=%q)", resp.StatusCode, body)
	}
}

// TestIntegrationReadyz: readiness flips to 200 once a backend is healthy; poll
// briefly so a just-started router's first health probe can land (ADR-0011).
func TestIntegrationReadyz(t *testing.T) {
	itRequireRouter(t)
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, body := itDo(t, http.MethodGet, "/readyz", nil)
		if resp.StatusCode == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("/readyz never returned 200; last status %d (body=%q)", resp.StatusCode, body)
		}
		time.Sleep(time.Second)
	}
}

// TestIntegrationModels: /v1/models lists at least one upstream id, none blank
// (ADR-0016). The concrete ids are fleet-specific, so only their presence and
// non-emptiness are asserted.
func TestIntegrationModels(t *testing.T) {
	itRequireRouter(t)
	resp, body := itDo(t, http.MethodGet, "/v1/models", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/models status = %d, want 200 (body=%q)", resp.StatusCode, body)
	}
	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode model list: %v (body=%q)", err, body)
	}
	if len(list.Data) == 0 {
		t.Fatalf("no models advertised (body=%q)", body)
	}
	for i, m := range list.Data {
		if strings.TrimSpace(m.ID) == "" {
			t.Fatalf("model[%d] has empty id (body=%q)", i, body)
		}
	}
}

// TestIntegrationChatCompletion: a non-streaming completion returns non-empty
// assistant content.
func TestIntegrationChatCompletion(t *testing.T) {
	itRequireRouter(t)
	resp, body := itDo(t, http.MethodPost, "/v1/chat/completions",
		itChatBody(itNorthAlias(), "Reply with exactly: OK", 256, false))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200 (body=%q)", resp.StatusCode, body)
	}
	var comp itChatCompletion
	if err := json.Unmarshal(body, &comp); err != nil {
		t.Fatalf("decode completion: %v (body=%q)", err, body)
	}
	if len(comp.Choices) == 0 {
		t.Fatalf("no choices in completion (body=%q)", body)
	}
	if strings.TrimSpace(comp.Choices[0].Message.Content) == "" {
		t.Fatalf("empty assistant content (body=%q)", body)
	}
}

// TestIntegrationReasoningSurvives: a reasoning model's non-standard fields are
// passed through (ADR-0001) — either message.reasoning_content is present or
// usage.reasoning_tokens is positive.
func TestIntegrationReasoningSurvives(t *testing.T) {
	itRequireRouter(t)
	resp, body := itDo(t, http.MethodPost, "/v1/chat/completions",
		itChatBody(itNorthAlias(), "Briefly: what is 2+2?", 256, false))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200 (body=%q)", resp.StatusCode, body)
	}
	var comp itChatCompletion
	if err := json.Unmarshal(body, &comp); err != nil {
		t.Fatalf("decode completion: %v (body=%q)", err, body)
	}
	if len(comp.Choices) == 0 {
		t.Fatalf("no choices in completion (body=%q)", body)
	}
	hasReasoning := comp.Choices[0].Message.ReasoningContent != nil || comp.Usage.ReasoningTokens > 0
	if !hasReasoning {
		t.Fatalf("neither message.reasoning_content nor usage.reasoning_tokens present — "+
			"router may be dropping non-standard fields (body=%q)", body)
	}
}

// TestIntegrationModelRewrite: the response reports the resolved upstream id, not
// the inbound alias (ADR-0004).
func TestIntegrationModelRewrite(t *testing.T) {
	itRequireRouter(t)
	alias := itNorthAlias()
	resp, body := itDo(t, http.MethodPost, "/v1/chat/completions",
		itChatBody(alias, "hi", 16, false))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200 (body=%q)", resp.StatusCode, body)
	}
	var comp itChatCompletion
	if err := json.Unmarshal(body, &comp); err != nil {
		t.Fatalf("decode completion: %v (body=%q)", err, body)
	}
	if comp.Model == "" {
		t.Fatalf("response model is empty (body=%q)", body)
	}
	if comp.Model == alias {
		t.Fatalf("response model is still the alias %q; expected the resolved upstream id", alias)
	}
}

// TestIntegrationStreaming: an SSE completion yields >=1 data chunk and the
// [DONE] terminator with a text/event-stream content type (ADR-0007).
func TestIntegrationStreaming(t *testing.T) {
	itRequireRouter(t)
	payload, err := json.Marshal(itChatBody(itNorthAlias(), "Count: 1 2 3", 64, true))
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), itTimeout())
	defer cancel()
	resp, err := itClient().Do(itNewRequest(ctx, t, http.MethodPost, "/v1/chat/completions", payload))
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("stream status = %d, want 200 (body=%q)", resp.StatusCode, data)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("stream content-type = %q, want text/event-stream", ct)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	chunks := 0
	sawDone := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			sawDone = true
			break
		}
		chunks++
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan SSE stream: %v", err)
	}
	if chunks == 0 {
		t.Fatalf("no data chunks received before terminator")
	}
	if !sawDone {
		t.Fatalf("stream ended without a [DONE] terminator")
	}
}

// TestIntegrationParetoSelectsConcreteModel: the pareto/smart alias resolves to a
// concrete model, never echoing an alias back (ADR-0013). The exact upstream id
// is fleet-specific, so the assertion is "non-empty and not any routing alias".
func TestIntegrationParetoSelectsConcreteModel(t *testing.T) {
	itRequireRouter(t)
	smart := itSmartAlias()
	resp, body := itDo(t, http.MethodPost, "/v1/chat/completions",
		itChatBody(smart, "Reply with exactly: OK", 32, false))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pareto chat status = %d, want 200 (body=%q)", resp.StatusCode, body)
	}
	var comp itChatCompletion
	if err := json.Unmarshal(body, &comp); err != nil {
		t.Fatalf("decode completion: %v (body=%q)", err, body)
	}
	if comp.Model == "" {
		t.Fatalf("pareto returned an empty model (body=%q)", body)
	}
	for _, alias := range []string{smart, itNorthAlias(), itGemmaAlias()} {
		if comp.Model == alias {
			t.Fatalf("pareto returned alias %q, expected a concrete upstream id", comp.Model)
		}
	}
}

// TestIntegrationUnknownModel: an unresolvable model is 404 model_not_found in the
// OpenAI error envelope (ADR-0006).
func TestIntegrationUnknownModel(t *testing.T) {
	itRequireRouter(t)
	resp, body := itDo(t, http.MethodPost, "/v1/chat/completions",
		itChatBody("no-such-model-xyz-router-integration", "hi", 0, false))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown model status = %d, want 404 (body=%q)", resp.StatusCode, body)
	}
	var apiErr itAPIError
	if err := json.Unmarshal(body, &apiErr); err != nil {
		t.Fatalf("decode error envelope: %v (body=%q)", err, body)
	}
	if apiErr.Error.Code != "model_not_found" {
		t.Fatalf("error code = %q, want model_not_found (body=%q)", apiErr.Error.Code, body)
	}
}

// TestIntegrationAnthropicMessages: the Anthropic /v1/messages endpoint returns a
// well-formed Anthropic message with a non-empty text content block, proving the
// inbound+outbound translation round-trip (ADR-0016).
func TestIntegrationAnthropicMessages(t *testing.T) {
	itRequireRouter(t)
	payload := map[string]any{
		"model":       itNorthAlias(),
		"max_tokens":  512,
		"temperature": 0,
		"messages":    []map[string]any{{"role": "user", "content": "Reply with exactly: OK"}},
	}
	resp, body := itDo(t, http.MethodPost, "/v1/messages", payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/messages status = %d, want 200 (body=%q)", resp.StatusCode, body)
	}
	var msg itAnthropicMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("decode anthropic message: %v (body=%q)", err, body)
	}
	if msg.Type != "message" {
		t.Fatalf("response type = %q, want \"message\" (body=%q)", msg.Type, body)
	}
	var text strings.Builder
	for _, b := range msg.Content {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	if strings.TrimSpace(text.String()) == "" {
		t.Fatalf("no non-empty text content block (body=%q)", body)
	}
}

// TestIntegrationRouterModelHeader: a proxied response carries X-Router-Model,
// reporting the concrete routing decision back to the consumer (ADR-0013).
func TestIntegrationRouterModelHeader(t *testing.T) {
	itRequireRouter(t)
	resp, body := itDo(t, http.MethodPost, "/v1/chat/completions",
		itChatBody(itNorthAlias(), "hi", 64, false))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200 (body=%q)", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Router-Model"); strings.TrimSpace(got) == "" {
		t.Fatalf("X-Router-Model header missing on proxy response")
	}
}
