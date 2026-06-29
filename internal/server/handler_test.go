package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/mattbucci/simple-llm-router/internal/backend"
	"github.com/mattbucci/simple-llm-router/internal/model"
	"github.com/mattbucci/simple-llm-router/internal/observability"
	"github.com/mattbucci/simple-llm-router/internal/router"
	"github.com/mattbucci/simple-llm-router/internal/server"
)

// fakeBackend implements router.Backend. The router always hands it an
// OpenAI-canonical body and expects an OpenAI-shaped response back, so the fake
// stands in for a same-protocol OpenAI provider while still letting us exercise
// the inbound adapters and translating sinks end to end.
type fakeBackend struct {
	name     string
	protocol model.Protocol
	fn       func(body []byte, stream bool) (*model.UpstreamResponse, error)
	lastBody []byte
}

func (f *fakeBackend) Name() string             { return f.name }
func (f *fakeBackend) Protocol() model.Protocol { return f.protocol }
func (f *fakeBackend) Chat(ctx context.Context, body []byte, stream bool) (*model.UpstreamResponse, error) {
	f.lastBody = append([]byte(nil), body...)
	return f.fn(body, stream)
}

// ChatNative is the same-protocol native relay (router.Backend). This fake stands
// in for an OpenAI provider, so it forwards to the same handler as Chat.
func (f *fakeBackend) ChatNative(ctx context.Context, body []byte, stream bool) (*model.UpstreamResponse, error) {
	f.lastBody = append([]byte(nil), body...)
	return f.fn(body, stream)
}

// capturedReq records what the httptest upstream received, handed back over a
// buffered channel to establish a happens-before edge for the race detector.
type capturedReq struct {
	path     string
	auth     string
	xAPIKey  string
	aVersion string
	body     []byte
}

type fakeHealth struct{ snap *model.Snapshot }

func (f fakeHealth) Snapshot() *model.Snapshot { return f.snap }

// Suspect is the HealthView fast-reprobe hook; tests need no re-probe, so it is a
// no-op.
func (f fakeHealth) Suspect(string) {}

func okResponse(body string) func([]byte, bool) (*model.UpstreamResponse, error) {
	return func([]byte, bool) (*model.UpstreamResponse, error) {
		return &model.UpstreamResponse{
			Status: http.StatusOK,
			Header: http.Header{"Content-Type": []string{"application/json"}},
			Body:   io.NopCloser(strings.NewReader(body)),
		}, nil
	}
}

func newServer(t *testing.T, backends map[string]router.Backend, aliases map[string]*router.Alias, snap *model.Snapshot, tokens []string, logW io.Writer) *server.Server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	metrics := observability.New(ctx)
	health := fakeHealth{snap}
	logger := slog.New(slog.NewJSONHandler(logW, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rt := router.New(backends, health, metrics, aliases, logger)
	return server.New(rt, health, metrics, server.NewStaticTokenAuth(tokens), 100<<20, server.AudioConfig{}, logger)
}

func snapshot(states ...model.BackendState) *model.Snapshot {
	bs := map[string]model.BackendState{}
	for _, s := range states {
		bs[s.Name] = s
	}
	return &model.Snapshot{Backends: bs}
}

func proxyAlias(name, upstream string, backends ...string) *router.Alias {
	return &router.Alias{Name: name, Type: "proxy", Selector: "round_robin", Model: upstream, Backends: backends}
}

// TestOpenAIPassthroughPreservesUnknownFields covers ADR-0001 end to end: an
// OpenAI consumer hitting an OpenAI backend gets the response relayed verbatim,
// including reasoning_content and other non-standard fields.
func TestOpenAIPassthroughPreservesUnknownFields(t *testing.T) {
	upstreamBody := `{"id":"chatcmpl-1","object":"chat.completion","model":"up","choices":[{"index":0,"message":{"role":"assistant","content":"hello","reasoning_content":"because"}}],"usage":{"prompt_tokens":3,"completion_tokens":1,"reasoning_tokens":5},"matched_stop":null,"metadata":{"weight_version":"default"}}`
	be := &fakeBackend{name: "b1", protocol: model.ProtocolOpenAI, fn: okResponse(upstreamBody)}

	srv := newServer(t,
		map[string]router.Backend{"b1": be},
		map[string]*router.Alias{"alias-oai": proxyAlias("alias-oai", "up", "b1")},
		snapshot(model.BackendState{Name: "b1", Healthy: true}),
		nil, io.Discard,
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias-oai","messages":[{"role":"user","content":"hi"}]}`))
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != upstreamBody {
		t.Fatalf("body not verbatim:\n got %s\nwant %s", rec.Body.String(), upstreamBody)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
}

// TestPluginsStrippedAndModelRewritten covers ADR-0001 end to end: the reserved
// plugins field is never forwarded and model is rewritten to the upstream id.
func TestPluginsStrippedAndModelRewritten(t *testing.T) {
	be := &fakeBackend{name: "b1", protocol: model.ProtocolOpenAI, fn: okResponse(`{"choices":[{"message":{"content":"ok"}}]}`)}
	srv := newServer(t,
		map[string]router.Backend{"b1": be},
		map[string]*router.Alias{"alias-oai": proxyAlias("alias-oai", "/models/Real", "b1")},
		snapshot(model.BackendState{Name: "b1", Healthy: true}),
		nil, io.Discard,
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"alias-oai","plugins":[{"id":"pareto","min_quality":0.8}],"messages":[{"role":"user","content":"hi"}]}`))
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var sent map[string]json.RawMessage
	if err := json.Unmarshal(be.lastBody, &sent); err != nil {
		t.Fatalf("forwarded body: %v", err)
	}
	if _, ok := sent["plugins"]; ok {
		t.Fatalf("plugins forwarded to backend: %s", be.lastBody)
	}
	var fwdModel string
	_ = json.Unmarshal(sent["model"], &fwdModel)
	if fwdModel != "/models/Real" {
		t.Fatalf("forwarded model = %q, want /models/Real", fwdModel)
	}
}

// TestMultimodalContentForwardedAsArray covers ADR-0008 end to end: an array of
// content parts (text, image_url, and an unknown video part) is forwarded
// verbatim — the server never flattens content to a string.
func TestMultimodalContentForwardedAsArray(t *testing.T) {
	be := &fakeBackend{name: "b1", protocol: model.ProtocolOpenAI, fn: okResponse(`{"choices":[{"message":{"content":"ok"}}]}`)}
	srv := newServer(t,
		map[string]router.Backend{"b1": be},
		map[string]*router.Alias{"alias-oai": proxyAlias("alias-oai", "up", "b1")},
		snapshot(model.BackendState{Name: "b1", Healthy: true}),
		nil, io.Discard,
	)

	content := `[{"type":"text","text":"what is this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}},{"type":"video","src":"v.mp4"}]`
	reqBody := `{"model":"alias-oai","messages":[{"role":"user","content":` + content + `}]}`

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var sent struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(be.lastBody, &sent); err != nil {
		t.Fatalf("forwarded body: %v", err)
	}
	if len(sent.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(sent.Messages))
	}
	var gotParts, wantParts any
	if err := json.Unmarshal(sent.Messages[0].Content, &gotParts); err != nil {
		t.Fatalf("content not an array/parseable: %v", err)
	}
	_ = json.Unmarshal([]byte(content), &wantParts)
	if !reflect.DeepEqual(gotParts, wantParts) {
		t.Fatalf("content parts not preserved:\n got %s", sent.Messages[0].Content)
	}
}

// TestAnthropicConsumerTranslationRoundTrip covers ADR-0016: an Anthropic
// consumer is translated inbound to the OpenAI canonical shape (system hoisted to
// a system-role message) and the OpenAI reply is translated back to Anthropic
// shape by the sink.
func TestAnthropicConsumerTranslationRoundTrip(t *testing.T) {
	openaiReply := `{"id":"chatcmpl-abc","object":"chat.completion","model":"up-oai","choices":[{"index":0,"message":{"role":"assistant","content":"Hello there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`
	be := &fakeBackend{name: "b1", protocol: model.ProtocolOpenAI, fn: okResponse(openaiReply)}
	srv := newServer(t,
		map[string]router.Backend{"b1": be},
		map[string]*router.Alias{"alias-oai": proxyAlias("alias-oai", "up-oai", "b1")},
		snapshot(model.BackendState{Name: "b1", Healthy: true}),
		nil, io.Discard,
	)

	anthropicReq := `{"model":"alias-oai","max_tokens":128,"system":"be brief","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(anthropicReq))
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	// Inbound: the backend must have received OpenAI canonical shape with the
	// system prompt hoisted into a system-role message.
	var sent struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(be.lastBody, &sent); err != nil {
		t.Fatalf("forwarded body: %v", err)
	}
	if len(sent.Messages) != 2 || sent.Messages[0].Role != "system" || sent.Messages[1].Role != "user" {
		t.Fatalf("canonical messages = %s, want [system,user]", be.lastBody)
	}

	// Outbound: the consumer must receive Anthropic Messages shape.
	var out struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response not Anthropic-shaped: %v (%s)", err, rec.Body.String())
	}
	if out.Type != "message" || out.Role != "assistant" {
		t.Fatalf("response = %s", rec.Body.String())
	}
	if len(out.Content) != 1 || out.Content[0].Type != "text" || out.Content[0].Text != "Hello there" {
		t.Fatalf("content = %+v", out.Content)
	}
	if out.StopReason != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn", out.StopReason)
	}
	if out.Usage.InputTokens != 5 || out.Usage.OutputTokens != 2 {
		t.Fatalf("usage = %+v, want 5/2", out.Usage)
	}
}

// TestAnthropicNativePassthroughOverHTTP covers ADR-0016 ("Anthropic->Anthropic =
// full passthrough") end to end through the real backend.Client: an Anthropic
// consumer routed to an Anthropic backend reaches the provider's native /messages
// endpoint with the original bytes (model rewritten, plugins stripped, but tools /
// tool_choice / top_k / metadata / cache_control intact), and the Anthropic reply
// is relayed to the consumer byte-for-byte — the fidelity a double
// Anthropic->OpenAI->Anthropic translation would silently break.
func TestAnthropicNativePassthroughOverHTTP(t *testing.T) {
	anthropicReply := `{"id":"msg_native","type":"message","role":"assistant","model":"claude-up","content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"SF"}}],"stop_reason":"tool_use","usage":{"input_tokens":11,"output_tokens":7}}`

	got := make(chan capturedReq, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- capturedReq{
			path:     r.URL.Path,
			auth:     r.Header.Get("Authorization"),
			xAPIKey:  r.Header.Get("X-Api-Key"),
			aVersion: r.Header.Get("Anthropic-Version"),
			body:     b,
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, anthropicReply)
	}))
	defer up.Close()

	be := backend.NewClient("b1", up.URL+"/v1", model.ProtocolAnthropic, "op-key", "2023-06-01", backend.ClientTimeouts{})
	srv := newServer(t,
		map[string]router.Backend{"b1": be},
		map[string]*router.Alias{"alias-ant": proxyAlias("alias-ant", "claude-up", "b1")},
		snapshot(model.BackendState{Name: "b1", Healthy: true}),
		nil, io.Discard,
	)

	reqBody := `{"model":"alias-ant","max_tokens":256,"top_k":40,"metadata":{"user_id":"u1"},"tool_choice":{"type":"auto"},"tools":[{"name":"get_weather","description":"w","input_schema":{"type":"object"}}],"plugins":[{"id":"pareto","min_quality":0.7}],"system":[{"type":"text","text":"be brief","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"weather?"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != anthropicReply {
		t.Fatalf("native reply not relayed verbatim:\n got %s\nwant %s", rec.Body.String(), anthropicReply)
	}

	g := <-got
	if g.path != "/v1/messages" {
		t.Fatalf("path = %q, want /v1/messages (native endpoint)", g.path)
	}
	if g.xAPIKey != "op-key" || g.aVersion != "2023-06-01" {
		t.Fatalf("auth = x-api-key %q anthropic-version %q, want op-key/2023-06-01", g.xAPIKey, g.aVersion)
	}
	if g.auth != "" {
		t.Fatalf("Authorization = %q, want empty for an anthropic provider", g.auth)
	}

	var sent map[string]json.RawMessage
	if err := json.Unmarshal(g.body, &sent); err != nil {
		t.Fatalf("forwarded body: %v", err)
	}
	var fwdModel string
	_ = json.Unmarshal(sent["model"], &fwdModel)
	if fwdModel != "claude-up" {
		t.Fatalf("forwarded model = %q, want claude-up", fwdModel)
	}
	if _, ok := sent["plugins"]; ok {
		t.Fatalf("plugins forwarded to backend: %s", g.body)
	}
	for _, k := range []string{"tools", "tool_choice", "top_k", "metadata", "system"} {
		if _, ok := sent[k]; !ok {
			t.Fatalf("native field %q dropped from forwarded body: %s", k, g.body)
		}
	}
	// The system array (with cache_control) must survive byte-shape verbatim — the
	// canonical path would have flattened it to a string system message.
	var origSys, sentSys any
	_ = json.Unmarshal([]byte(`[{"type":"text","text":"be brief","cache_control":{"type":"ephemeral"}}]`), &origSys)
	_ = json.Unmarshal(sent["system"], &sentSys)
	if !reflect.DeepEqual(origSys, sentSys) {
		t.Fatalf("system not preserved verbatim: %s", sent["system"])
	}
}

// TestAnthropicNativeStreamPassthroughOverHTTP covers ADR-0007/0016 end to end: a
// native Anthropic SSE stream is relayed to the consumer with its typed event:
// framing intact and no OpenAI [DONE] terminator injected.
func TestAnthropicNativeStreamPassthroughOverHTTP(t *testing.T) {
	events := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-up"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
		"",
	}, "\n")

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, events)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer up.Close()

	be := backend.NewClient("b1", up.URL+"/v1", model.ProtocolAnthropic, "op-key", "2023-06-01", backend.ClientTimeouts{})
	srv := newServer(t,
		map[string]router.Backend{"b1": be},
		map[string]*router.Alias{"alias-ant": proxyAlias("alias-ant", "claude-up", "b1")},
		snapshot(model.BackendState{Name: "b1", Healthy: true}),
		nil, io.Discard,
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"alias-ant","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"event: message_start", "event: content_block_delta", "event: message_stop", `"type":"text_delta"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("native stream missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("native Anthropic stream must not carry the OpenAI [DONE] terminator:\n%s", body)
	}
}

// TestStatusMappingOverHTTP covers ADR-0004/0006 at the HTTP edge: 404 for an
// unknown model, 503 when a known model has no healthy backend, and 502 when all
// candidates fail.
func TestStatusMappingOverHTTP(t *testing.T) {
	cases := []struct {
		name       string
		aliases    map[string]*router.Alias
		snap       *model.Snapshot
		backends   map[string]router.Backend
		model      string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "unknown model is 404",
			aliases:    map[string]*router.Alias{},
			snap:       model.EmptySnapshot(),
			backends:   map[string]router.Backend{},
			model:      "ghost",
			wantStatus: http.StatusNotFound,
			wantCode:   "model_not_found",
		},
		{
			name:       "known alias all backends down is 503",
			aliases:    map[string]*router.Alias{"a": proxyAlias("a", "up", "b1")},
			snap:       snapshot(model.BackendState{Name: "b1", Healthy: false}),
			backends:   map[string]router.Backend{"b1": &fakeBackend{name: "b1", protocol: model.ProtocolOpenAI, fn: okResponse(`{}`)}},
			model:      "a",
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "no_healthy_backend",
		},
		{
			name:    "all candidates fail is 502",
			aliases: map[string]*router.Alias{"a": proxyAlias("a", "up", "b1")},
			snap:    snapshot(model.BackendState{Name: "b1", Healthy: true}),
			backends: map[string]router.Backend{"b1": &fakeBackend{name: "b1", protocol: model.ProtocolOpenAI, fn: func([]byte, bool) (*model.UpstreamResponse, error) {
				return &model.UpstreamResponse{Status: http.StatusServiceUnavailable, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":"down"}`))}, nil
			}}},
			model:      "a",
			wantStatus: http.StatusBadGateway,
			wantCode:   "upstream_unavailable",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newServer(t, tc.backends, tc.aliases, tc.snap, nil, io.Discard)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"`+tc.model+`","messages":[]}`))
			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			var e struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
				t.Fatalf("error body: %v", err)
			}
			if e.Error.Code != tc.wantCode {
				t.Fatalf("error code = %q, want %q", e.Error.Code, tc.wantCode)
			}
		})
	}
}

// TestNoSecretsInLogs covers ADR-0009/0011: the per-request log line must never
// contain the inbound auth credential nor the prompt/response content.
func TestNoSecretsInLogs(t *testing.T) {
	const secretToken = "SUPERSECRETTOKEN12345"
	const secretPrompt = "my-confidential-prompt-text"
	const secretReply = "my-confidential-reply-text"

	be := &fakeBackend{name: "b1", protocol: model.ProtocolOpenAI, fn: okResponse(`{"choices":[{"message":{"content":"` + secretReply + `"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)}

	var logBuf bytes.Buffer
	srv := newServer(t,
		map[string]router.Backend{"b1": be},
		map[string]*router.Alias{"alias-oai": proxyAlias("alias-oai", "up", "b1")},
		snapshot(model.BackendState{Name: "b1", Healthy: true}),
		nil, &logBuf,
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"alias-oai","messages":[{"role":"user","content":"`+secretPrompt+`"}]}`))
	req.Header.Set("Authorization", "Bearer "+secretToken)
	req.Header.Set("X-Api-Key", secretToken)
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	logs := logBuf.String()
	if logs == "" {
		t.Fatalf("expected a request log line, got none")
	}
	// The log must be emitted (carries the request marker and status) ...
	if !strings.Contains(logs, `"msg":"request"`) || !strings.Contains(logs, `"status":200`) {
		t.Fatalf("request log line missing expected fields:\n%s", logs)
	}
	// ... but must contain no secrets.
	for _, secret := range []string{secretToken, secretPrompt, secretReply} {
		if strings.Contains(logs, secret) {
			t.Fatalf("secret %q leaked into logs:\n%s", secret, logs)
		}
	}
}

// TestReadyzGatedOnHealth covers ADR-0011: /readyz is 200 only when at least one
// backend is healthy.
func TestReadyzGatedOnHealth(t *testing.T) {
	t.Run("no healthy backend is 503", func(t *testing.T) {
		srv := newServer(t, map[string]router.Backend{}, nil, snapshot(model.BackendState{Name: "b1", Healthy: false}), nil, io.Discard)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
	})
	t.Run("a healthy backend is 200", func(t *testing.T) {
		srv := newServer(t, map[string]router.Backend{}, nil, snapshot(model.BackendState{Name: "b1", Healthy: true}), nil, io.Discard)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})
}
