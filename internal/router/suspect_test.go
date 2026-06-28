package router

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/mattbucci/simple-llm-router/internal/model"
)

// TestSuspectOnFailover covers ADR-0005/0006: when a candidate fails in a way that
// triggers failover — a pre-first-byte connect/transport error or a retryable
// upstream status (502/503/504) — the proxy strategy asks the health view to fast
// re-probe (Suspect) exactly that backend, so a dead backend leaves rotation before
// the next fixed health tick. A non-retryable 4xx or a successful response must NOT
// Suspect anything (no spurious re-probes, no failover). Asserted with a recording
// HealthView via the router's interfaces — no live network.
func TestSuspectOnFailover(t *testing.T) {
	cases := []struct {
		name          string
		be1           func(context.Context, []byte, bool) (*model.UpstreamResponse, error)
		wantSuspected []string
		wantBackend   string
		wantFailovers int
	}{
		{
			name: "connect error suspects the failed backend then fails over",
			be1: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
				return nil, errors.New("connection refused")
			},
			wantSuspected: []string{"be1"},
			wantBackend:   "be2",
			wantFailovers: 1,
		},
		{
			name: "retryable 503 suspects the failed backend then fails over",
			be1: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
				return upstream(503, `{"error":"unavailable"}`), nil
			},
			wantSuspected: []string{"be1"},
			wantBackend:   "be2",
			wantFailovers: 1,
		},
		{
			name: "non-retryable 4xx never suspects and never fails over",
			be1: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
				return upstream(400, `{"error":"bad request"}`), nil
			},
			wantSuspected: nil,
			wantBackend:   "be1",
			wantFailovers: 0,
		},
		{
			name: "success never suspects",
			be1: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
				return upstream(200, `{"ok":true}`), nil
			},
			wantSuspected: nil,
			wantBackend:   "be1",
			wantFailovers: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			be1 := &fakeBackend{name: "be1", protocol: model.ProtocolOpenAI, chat: tc.be1}
			be2 := &fakeBackend{name: "be2", protocol: model.ProtocolOpenAI, chat: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
				return upstream(200, `{"ok":true}`), nil
			}}
			health := &recordingHealth{snap: healthySnap("be1", "be2")}
			r := newTestRouterWithHealth(
				map[string]Backend{"be1": be1, "be2": be2},
				health,
				map[string]*Alias{"a": {Name: "a", Type: "proxy", Selector: "round_robin", Model: "up", Backends: []string{"be1", "be2"}}},
				&fakeMetrics{},
			)

			sink := &recordingSink{}
			req := &model.ChatRequest{Model: "a", Consumer: model.ProtocolOpenAI, Raw: rawMap(t, `{"model":"a","messages":[]}`)}
			outcome, _ := r.Route(context.Background(), req, sink)

			if !reflect.DeepEqual(health.suspected, tc.wantSuspected) {
				t.Fatalf("suspected = %v, want %v", health.suspected, tc.wantSuspected)
			}
			if outcome.Backend != tc.wantBackend {
				t.Fatalf("outcome.Backend = %q, want %q", outcome.Backend, tc.wantBackend)
			}
			if outcome.Failovers != tc.wantFailovers {
				t.Fatalf("outcome.Failovers = %d, want %d", outcome.Failovers, tc.wantFailovers)
			}
		})
	}
}
