package router

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/mattbucci/simple-llm-router/internal/model"
)

// TestBuildOutboundBody covers ADR-0001: the outbound body rewrites only "model"
// to the resolved upstream id, strips the reserved "plugins" field, and preserves
// every other field verbatim — including unknown fields and array-shaped
// (multimodal) content (ADR-0008).
func TestBuildOutboundBody(t *testing.T) {
	cases := []struct {
		name          string
		raw           string
		upstream      string
		wantModel     string
		wantNoPlugins bool
		// preservedKeys must be present and byte-identical to their input value.
		preservedKeys []string
	}{
		{
			name:          "rewrite model and strip plugins, keep unknown fields",
			raw:           `{"model":"north","plugins":[{"id":"pareto","min_quality":0.9}],"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high","metadata":{"k":"v"}}`,
			upstream:      "/models/North-Mini-Code-1.0-fp8",
			wantModel:     "/models/North-Mini-Code-1.0-fp8",
			wantNoPlugins: true,
			preservedKeys: []string{"messages", "reasoning_effort", "metadata"},
		},
		{
			name:          "no plugins present is a no-op besides model rewrite",
			raw:           `{"model":"gemma","stream":true,"temperature":0.2,"messages":[]}`,
			upstream:      "gemma4-31b",
			wantModel:     "gemma4-31b",
			wantNoPlugins: true,
			preservedKeys: []string{"stream", "temperature", "messages"},
		},
		{
			name:          "multimodal array content survives verbatim",
			raw:           `{"model":"gemma","messages":[{"role":"user","content":[{"type":"text","text":"what is this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}},{"type":"video","src":"v.mp4"}]}]}`,
			upstream:      "gemma4-31b",
			wantModel:     "gemma4-31b",
			wantNoPlugins: true,
			preservedKeys: []string{"messages"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := rawMap(t, tc.raw)
			req := &model.ChatRequest{Raw: in}

			out, err := buildOutboundBody(req, tc.upstream)
			if err != nil {
				t.Fatalf("buildOutboundBody: %v", err)
			}

			var got map[string]json.RawMessage
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("unmarshal outbound: %v", err)
			}

			var gotModel string
			if err := json.Unmarshal(got["model"], &gotModel); err != nil {
				t.Fatalf("model field: %v", err)
			}
			if gotModel != tc.wantModel {
				t.Fatalf("model = %q, want %q", gotModel, tc.wantModel)
			}

			if tc.wantNoPlugins {
				if _, ok := got["plugins"]; ok {
					t.Fatalf("plugins must never be forwarded, found: %s", got["plugins"])
				}
			}

			for _, k := range tc.preservedKeys {
				if !bytesEqualJSON(t, got[k], in[k]) {
					t.Fatalf("key %q not preserved verbatim: got %s want %s", k, got[k], in[k])
				}
			}
		})
	}
}

// TestUnknownResponseFieldSurvivesUnary covers ADR-0001: a non-standard response
// field (reasoning_content / reasoning_tokens / metadata / matched_stop) must
// pass through the router untouched on the unary same-protocol path.
func TestUnknownResponseFieldSurvivesUnary(t *testing.T) {
	body := `{"id":"x","choices":[{"message":{"content":"","reasoning_content":"the user asks..."}}],"usage":{"prompt_tokens":3,"completion_tokens":30,"reasoning_tokens":30},"metadata":{"weight_version":"default"},"matched_stop":null}`

	be := &fakeBackend{
		name:     "be1",
		protocol: model.ProtocolOpenAI,
		chat: func(ctx context.Context, b []byte, stream bool) (*model.UpstreamResponse, error) {
			return upstream(200, body), nil
		},
	}
	r := newTestRouter(
		map[string]Backend{"be1": be},
		healthySnap("be1"),
		map[string]*Alias{"a": {Name: "a", Type: "proxy", Selector: "round_robin", Model: "up", Backends: []string{"be1"}}},
		&fakeMetrics{},
	)

	sink := &recordingSink{}
	req := &model.ChatRequest{Model: "a", Consumer: model.ProtocolOpenAI, Raw: rawMap(t, `{"model":"a","messages":[]}`)}
	outcome, err := r.Route(context.Background(), req, sink)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}

	if string(sink.body) != body {
		t.Fatalf("relayed body not verbatim:\n got %s\nwant %s", sink.body, body)
	}
	for _, field := range []string{"reasoning_content", "reasoning_tokens", "metadata", "matched_stop"} {
		if !strings.Contains(string(sink.body), field) {
			t.Fatalf("field %q dropped from relayed body", field)
		}
	}
	if outcome.Usage.ReasoningTokens != 30 {
		t.Fatalf("reasoning_tokens usage = %d, want 30", outcome.Usage.ReasoningTokens)
	}
	if outcome.Status != 200 {
		t.Fatalf("status = %d, want 200", outcome.Status)
	}
}

// TestUnknownResponseFieldSurvivesStream covers ADR-0001/0007: a non-standard
// field inside a streamed chunk reaches the sink verbatim.
func TestUnknownResponseFieldSurvivesStream(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"because\"}}]}\n\ndata: [DONE]\n\n"
	be := &fakeBackend{
		name:     "be1",
		protocol: model.ProtocolOpenAI,
		chat: func(ctx context.Context, b []byte, stream bool) (*model.UpstreamResponse, error) {
			if !stream {
				t.Errorf("expected stream=true")
			}
			return upstream(200, sse), nil
		},
	}
	r := newTestRouter(
		map[string]Backend{"be1": be},
		healthySnap("be1"),
		map[string]*Alias{"a": {Name: "a", Type: "proxy", Selector: "round_robin", Model: "up", Backends: []string{"be1"}}},
		&fakeMetrics{},
	)

	sink := &recordingSink{}
	req := &model.ChatRequest{Model: "a", Stream: true, Consumer: model.ProtocolOpenAI, Raw: rawMap(t, `{"model":"a","stream":true,"messages":[]}`)}
	if _, err := r.Route(context.Background(), req, sink); err != nil {
		t.Fatalf("Route: %v", err)
	}

	if !sink.started || !sink.ended {
		t.Fatalf("stream lifecycle: started=%v ended=%v", sink.started, sink.ended)
	}
	if len(sink.events) != 1 {
		t.Fatalf("events = %d, want 1", len(sink.events))
	}
	if !strings.Contains(string(sink.events[0]), "reasoning_content") {
		t.Fatalf("reasoning_content dropped from event: %s", sink.events[0])
	}
}

// TestAnthropicNativePassthrough covers ADR-0016 ("Anthropic->Anthropic = full
// passthrough") / ADR-0001 at the router layer: an Anthropic consumer routed to an
// Anthropic backend forwards the ORIGINAL consumer bytes via ChatNative (model
// rewritten, plugins stripped, but tools/top_k/metadata kept byte-intact) and
// relays the Anthropic reply verbatim — never the lossy translating Chat/sink path.
func TestAnthropicNativePassthrough(t *testing.T) {
	reply := `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"f","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":3}}`
	be := &fakeBackend{
		name:     "be1",
		protocol: model.ProtocolAnthropic,
		chatNative: func(ctx context.Context, b []byte, stream bool) (*model.UpstreamResponse, error) {
			return upstream(200, reply), nil
		},
		chat: func(ctx context.Context, b []byte, stream bool) (*model.UpstreamResponse, error) {
			t.Errorf("translating Chat must not be used on the native same-protocol path")
			return upstream(200, "{}"), nil
		},
	}
	r := newTestRouter(
		map[string]Backend{"be1": be},
		healthySnap("be1"),
		map[string]*Alias{"a": {Name: "a", Type: "proxy", Selector: "round_robin", Model: "claude-up", Backends: []string{"be1"}}},
		&fakeMetrics{},
	)

	consumerBody := `{"model":"a","top_k":40,"metadata":{"u":"1"},"tool_choice":{"type":"auto"},"tools":[{"name":"f"}],"plugins":[{"id":"pareto"}],"messages":[{"role":"user","content":"hi"}]}`
	sink := &recordingSink{}
	req := &model.ChatRequest{
		Model:        "a",
		Consumer:     model.ProtocolAnthropic,
		ConsumerBody: []byte(consumerBody),
		// Raw is the lossy canonical map; the native path must NOT source from it.
		Raw: rawMap(t, `{"model":"a","messages":[]}`),
	}
	outcome, err := r.Route(context.Background(), req, sink)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}

	if string(sink.rawBody) != reply {
		t.Fatalf("reply not relayed verbatim:\n got %s\nwant %s", sink.rawBody, reply)
	}
	if sink.body != nil {
		t.Fatalf("translating WriteResponse used on the native path: %s", sink.body)
	}

	fwd := be.nativeBody()
	if fwd == nil {
		t.Fatalf("ChatNative was not used on the native path")
	}
	var sent map[string]json.RawMessage
	if err := json.Unmarshal(fwd, &sent); err != nil {
		t.Fatalf("forwarded body: %v", err)
	}
	var m string
	_ = json.Unmarshal(sent["model"], &m)
	if m != "claude-up" {
		t.Fatalf("forwarded model = %q, want claude-up", m)
	}
	if _, ok := sent["plugins"]; ok {
		t.Fatalf("plugins must be stripped on the native path: %s", fwd)
	}
	for _, k := range []string{"tools", "tool_choice", "top_k", "metadata"} {
		if _, ok := sent[k]; !ok {
			t.Fatalf("native field %q dropped from forwarded body: %s", k, fwd)
		}
	}
	if outcome.Usage.PromptTokens != 5 || outcome.Usage.CompletionTokens != 3 {
		t.Fatalf("usage = %+v, want 5/3", outcome.Usage)
	}
	if outcome.Status != 200 {
		t.Fatalf("status = %d, want 200", outcome.Status)
	}
}

// TestAnthropicNativeStreamPassthrough covers ADR-0007/0016: a native Anthropic SSE
// stream is copied through verbatim (typed event: framing preserved, no OpenAI
// [DONE] terminator added).
func TestAnthropicNativeStreamPassthrough(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	be := &fakeBackend{
		name:     "be1",
		protocol: model.ProtocolAnthropic,
		chatNative: func(ctx context.Context, b []byte, stream bool) (*model.UpstreamResponse, error) {
			if !stream {
				t.Errorf("expected stream=true")
			}
			return upstream(200, sse), nil
		},
		chat: func(ctx context.Context, b []byte, stream bool) (*model.UpstreamResponse, error) {
			t.Errorf("translating Chat must not be used on the native stream path")
			return upstream(200, "{}"), nil
		},
	}
	r := newTestRouter(
		map[string]Backend{"be1": be},
		healthySnap("be1"),
		map[string]*Alias{"a": {Name: "a", Type: "proxy", Selector: "round_robin", Model: "claude-up", Backends: []string{"be1"}}},
		&fakeMetrics{},
	)

	sink := &recordingSink{}
	req := &model.ChatRequest{
		Model:        "a",
		Stream:       true,
		Consumer:     model.ProtocolAnthropic,
		ConsumerBody: []byte(`{"model":"a","stream":true,"messages":[]}`),
		Raw:          rawMap(t, `{"model":"a","stream":true,"messages":[]}`),
	}
	if _, err := r.Route(context.Background(), req, sink); err != nil {
		t.Fatalf("Route: %v", err)
	}

	if !sink.rawStarted {
		t.Fatalf("raw stream not started")
	}
	var got []byte
	for _, c := range sink.rawChunks {
		got = append(got, c...)
	}
	if string(got) != sse {
		t.Fatalf("native stream not relayed verbatim:\n got %s\nwant %s", got, sse)
	}
	if strings.Contains(string(got), "[DONE]") {
		t.Fatalf("native Anthropic stream must not carry the OpenAI [DONE] terminator")
	}
}

// TestAnthropicNativeFailover covers ADR-0006 on the native path: a retryable
// status before any byte is committed still fails over to the next candidate.
func TestAnthropicNativeFailover(t *testing.T) {
	reply := `{"id":"m","type":"message","role":"assistant","content":[]}`
	be1 := &fakeBackend{name: "be1", protocol: model.ProtocolAnthropic, chatNative: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
		return upstream(503, `{"error":"down"}`), nil
	}}
	be2 := &fakeBackend{name: "be2", protocol: model.ProtocolAnthropic, chatNative: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
		return upstream(200, reply), nil
	}}
	metrics := &fakeMetrics{}
	r := newTestRouter(
		map[string]Backend{"be1": be1, "be2": be2},
		healthySnap("be1", "be2"),
		map[string]*Alias{"a": {Name: "a", Type: "proxy", Selector: "round_robin", Model: "up", Backends: []string{"be1", "be2"}}},
		metrics,
	)

	sink := &recordingSink{}
	req := &model.ChatRequest{
		Model:        "a",
		Consumer:     model.ProtocolAnthropic,
		ConsumerBody: []byte(`{"model":"a","messages":[]}`),
		Raw:          rawMap(t, `{"model":"a","messages":[]}`),
	}
	outcome, err := r.Route(context.Background(), req, sink)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if outcome.Backend != "be2" || outcome.Failovers != 1 {
		t.Fatalf("outcome = %+v, want backend be2, failovers 1", outcome)
	}
	if string(sink.rawBody) != reply {
		t.Fatalf("reply = %s, want %s", sink.rawBody, reply)
	}
}

// TestFailover covers the ADR-0006 failover table for unary requests: connect
// errors and 502/503/504 fail over to the next candidate; 4xx never does;
// exhausting all candidates yields 502 upstream_unavailable.
func TestFailover(t *testing.T) {
	cases := []struct {
		name          string
		be1           func(context.Context, []byte, bool) (*model.UpstreamResponse, error)
		wantBackend   string // expected winning backend ("" => error)
		wantStatus    int
		wantFailovers int
		wantBe2Called bool
		wantErrCode   string
	}{
		{
			name: "connect error fails over",
			be1: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
				return nil, errors.New("connection refused")
			},
			wantBackend:   "be2",
			wantStatus:    200,
			wantFailovers: 1,
			wantBe2Called: true,
		},
		{
			name: "503 fails over",
			be1: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
				return upstream(503, `{"error":"unavailable"}`), nil
			},
			wantBackend:   "be2",
			wantStatus:    200,
			wantFailovers: 1,
			wantBe2Called: true,
		},
		{
			name: "4xx does not fail over",
			be1: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
				return upstream(400, `{"error":"bad request"}`), nil
			},
			wantBackend:   "be1",
			wantStatus:    400,
			wantFailovers: 0,
			wantBe2Called: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			be1 := &fakeBackend{name: "be1", protocol: model.ProtocolOpenAI, chat: tc.be1}
			be2 := &fakeBackend{name: "be2", protocol: model.ProtocolOpenAI, chat: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
				return upstream(200, `{"ok":true}`), nil
			}}
			metrics := &fakeMetrics{}
			r := newTestRouter(
				map[string]Backend{"be1": be1, "be2": be2},
				healthySnap("be1", "be2"),
				map[string]*Alias{"a": {Name: "a", Type: "proxy", Selector: "round_robin", Model: "up", Backends: []string{"be1", "be2"}}},
				metrics,
			)

			sink := &recordingSink{}
			req := &model.ChatRequest{Model: "a", Consumer: model.ProtocolOpenAI, Raw: rawMap(t, `{"model":"a","messages":[]}`)}
			outcome, _ := r.Route(context.Background(), req, sink)

			if outcome.Backend != tc.wantBackend {
				t.Fatalf("backend = %q, want %q", outcome.Backend, tc.wantBackend)
			}
			if sink.status != tc.wantStatus {
				t.Fatalf("status = %d, want %d", sink.status, tc.wantStatus)
			}
			if outcome.Failovers != tc.wantFailovers {
				t.Fatalf("failovers = %d, want %d", outcome.Failovers, tc.wantFailovers)
			}
			if got := metrics.failovers.Load(); int(got) != tc.wantFailovers {
				t.Fatalf("metrics failovers = %d, want %d", got, tc.wantFailovers)
			}
			if (be2.calls.Load() > 0) != tc.wantBe2Called {
				t.Fatalf("be2 called = %v, want %v", be2.calls.Load() > 0, tc.wantBe2Called)
			}
		})
	}
}

// TestFailoverExhaustionIs502 covers ADR-0006: when every candidate fails with a
// retryable status, the router returns 502 upstream_unavailable.
func TestFailoverExhaustionIs502(t *testing.T) {
	down := func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
		return upstream(503, `{"error":"down"}`), nil
	}
	be1 := &fakeBackend{name: "be1", protocol: model.ProtocolOpenAI, chat: down}
	be2 := &fakeBackend{name: "be2", protocol: model.ProtocolOpenAI, chat: down}
	r := newTestRouter(
		map[string]Backend{"be1": be1, "be2": be2},
		healthySnap("be1", "be2"),
		map[string]*Alias{"a": {Name: "a", Type: "proxy", Selector: "round_robin", Model: "up", Backends: []string{"be1", "be2"}}},
		&fakeMetrics{},
	)

	sink := &recordingSink{}
	req := &model.ChatRequest{Model: "a", Consumer: model.ProtocolOpenAI, Raw: rawMap(t, `{"model":"a","messages":[]}`)}
	outcome, err := r.Route(context.Background(), req, sink)

	var apiErr *model.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *model.APIError", err)
	}
	if apiErr.Status != 502 || apiErr.Code != "upstream_unavailable" {
		t.Fatalf("apiErr = %d/%s, want 502/upstream_unavailable", apiErr.Status, apiErr.Code)
	}
	if outcome.Status != 502 {
		t.Fatalf("outcome.Status = %d, want 502", outcome.Status)
	}
	if sink.wrote {
		t.Fatalf("sink should not have committed any bytes on a pre-first-byte failure")
	}
}

// TestStreamFailoverBeforeBytes covers ADR-0006/0007: a retryable status on the
// streaming path (no byte committed yet) still fails over to the next candidate.
func TestStreamFailoverBeforeBytes(t *testing.T) {
	be1 := &fakeBackend{name: "be1", protocol: model.ProtocolOpenAI, chat: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
		return upstream(503, `{"error":"down"}`), nil
	}}
	be2 := &fakeBackend{name: "be2", protocol: model.ProtocolOpenAI, chat: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
		return upstream(200, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"), nil
	}}
	metrics := &fakeMetrics{}
	r := newTestRouter(
		map[string]Backend{"be1": be1, "be2": be2},
		healthySnap("be1", "be2"),
		map[string]*Alias{"a": {Name: "a", Type: "proxy", Selector: "round_robin", Model: "up", Backends: []string{"be1", "be2"}}},
		metrics,
	)

	sink := &recordingSink{}
	req := &model.ChatRequest{Model: "a", Stream: true, Consumer: model.ProtocolOpenAI, Raw: rawMap(t, `{"model":"a","stream":true,"messages":[]}`)}
	outcome, err := r.Route(context.Background(), req, sink)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if outcome.Backend != "be2" || outcome.Failovers != 1 {
		t.Fatalf("outcome = %+v, want backend be2, failovers 1", outcome)
	}
	if len(sink.events) != 1 || !sink.started || !sink.ended {
		t.Fatalf("stream not relayed from be2: events=%d started=%v ended=%v", len(sink.events), sink.started, sink.ended)
	}
}

// TestNoRetryAfterStreamBytes covers the critical ADR-0006/0007 invariant: once a
// streamed response has emitted bytes to the client, a mid-stream upstream failure
// must NOT fail over to another backend — the response is committed.
func TestNoRetryAfterStreamBytes(t *testing.T) {
	be1 := &fakeBackend{name: "be1", protocol: model.ProtocolOpenAI, chat: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
		// One full SSE record, then a hard read error before the stream ends.
		return &model.UpstreamResponse{
			Status: 200,
			Body: &errReadCloser{
				data: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"),
				err:  errors.New("upstream connection reset mid-stream"),
			},
		}, nil
	}}
	be2 := &fakeBackend{name: "be2", protocol: model.ProtocolOpenAI, chat: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
		t.Errorf("be2 must NOT be tried after stream bytes were emitted")
		return upstream(200, "data: [DONE]\n\n"), nil
	}}
	metrics := &fakeMetrics{}
	r := newTestRouter(
		map[string]Backend{"be1": be1, "be2": be2},
		healthySnap("be1", "be2"),
		map[string]*Alias{"a": {Name: "a", Type: "proxy", Selector: "round_robin", Model: "up", Backends: []string{"be1", "be2"}}},
		metrics,
	)

	sink := &recordingSink{}
	req := &model.ChatRequest{Model: "a", Stream: true, Consumer: model.ProtocolOpenAI, Raw: rawMap(t, `{"model":"a","stream":true,"messages":[]}`)}
	_, err := r.Route(context.Background(), req, sink)

	if err == nil {
		t.Fatalf("expected the mid-stream error to surface, got nil")
	}
	if be2.calls.Load() != 0 {
		t.Fatalf("be2 was tried %d times after stream commit; must be 0", be2.calls.Load())
	}
	if metrics.failovers.Load() != 0 {
		t.Fatalf("failovers = %d after a committed stream, want 0", metrics.failovers.Load())
	}
	if len(sink.events) != 1 || !sink.started {
		t.Fatalf("expected one committed event before the failure: events=%d started=%v", len(sink.events), sink.started)
	}
}

func bytesEqualJSON(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}
