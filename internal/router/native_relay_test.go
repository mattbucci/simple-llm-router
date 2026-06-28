package router

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mattbucci/simple-llm-router/internal/model"
)

// TestNativeRelaySelectedPerCandidate covers ADR-0016/0006: the consumer-vs-provider
// protocol pairing is resolved per candidate during a failover sequence, not once
// per request. An Anthropic consumer whose preferred same-protocol Anthropic backend
// fails over (retryable status, before any byte is committed) still takes the native
// ChatNative relay for the Anthropic candidate, then takes the canonical translating
// Chat path for the next, OpenAI, candidate — and only the surviving candidate's
// reply is committed (failover invariant preserved).
func TestNativeRelaySelectedPerCandidate(t *testing.T) {
	const oaiReply = `{"id":"x","choices":[{"message":{"content":"hi"}}]}`

	// Anthropic backend: native relay path; returns a retryable status so it fails
	// over before committing a byte. Its translating Chat must never be used.
	beAnth := &fakeBackend{
		name:     "anth",
		protocol: model.ProtocolAnthropic,
		chatNative: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
			return upstream(503, `{"type":"error"}`), nil
		},
		chat: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
			t.Errorf("translating Chat must not be used for the Anthropic candidate")
			return upstream(200, "{}"), nil
		},
	}
	// OpenAI backend: canonical translating Chat path. Its ChatNative must never be
	// used for a cross-protocol candidate.
	beOAI := &fakeBackend{
		name:     "oai",
		protocol: model.ProtocolOpenAI,
		chat: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
			return upstream(200, oaiReply), nil
		},
		chatNative: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
			t.Errorf("ChatNative must not be used for the cross-protocol OpenAI candidate")
			return upstream(200, "{}"), nil
		},
	}
	metrics := &fakeMetrics{}
	r := newTestRouter(
		map[string]Backend{"anth": beAnth, "oai": beOAI},
		healthySnap("anth", "oai"),
		map[string]*Alias{"a": {Name: "a", Type: "proxy", Selector: "round_robin", Model: "up", Backends: []string{"anth", "oai"}}},
		metrics,
	)

	sink := &recordingSink{}
	req := &model.ChatRequest{
		Model:        "a",
		Consumer:     model.ProtocolAnthropic,
		ConsumerBody: []byte(`{"model":"a","top_k":40,"messages":[{"role":"user","content":"hi"}]}`),
		Raw:          rawMap(t, `{"model":"a","messages":[]}`),
	}
	outcome, err := r.Route(context.Background(), req, sink)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}

	// preferSameProtocol tries the Anthropic backend first (native relay), which
	// fails over to the OpenAI backend (canonical relay).
	if outcome.Backend != "oai" || outcome.Failovers != 1 {
		t.Fatalf("outcome = %+v, want backend oai, failovers 1", outcome)
	}

	// The Anthropic candidate used the native path: ChatNative saw the original
	// consumer bytes (model rewritten), and the translating Chat was not used.
	nativeSent := beAnth.nativeBody()
	if nativeSent == nil {
		t.Fatalf("Anthropic candidate did not use the native ChatNative path")
	}
	var sent map[string]json.RawMessage
	if err := json.Unmarshal(nativeSent, &sent); err != nil {
		t.Fatalf("native forwarded body: %v", err)
	}
	var sentModel string
	_ = json.Unmarshal(sent["model"], &sentModel)
	if sentModel != "up" {
		t.Fatalf("native forwarded model = %q, want up", sentModel)
	}
	if _, ok := sent["top_k"]; !ok {
		t.Fatalf("native path dropped the Anthropic-only top_k field: %s", nativeSent)
	}

	// The OpenAI candidate used the canonical translating Chat path and committed
	// via the translating WriteResponse, not the verbatim raw relay.
	if beOAI.body() == nil {
		t.Fatalf("OpenAI candidate did not use the canonical Chat path")
	}
	if string(sink.body) != oaiReply {
		t.Fatalf("committed body = %s, want canonical %s", sink.body, oaiReply)
	}
	if sink.rawBody != nil {
		t.Fatalf("raw verbatim relay used for the cross-protocol OpenAI candidate: %s", sink.rawBody)
	}
}
