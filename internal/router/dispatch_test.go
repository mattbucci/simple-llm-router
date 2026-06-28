package router

import (
	"context"
	"testing"

	"github.com/mattbucci/simple-llm-router/internal/model"
)

// TestStrategyForReturnsCorrectStrategy covers ADR-0006: strategyFor dispatches a
// plan to a routing Strategy polymorphically — a fusion plan to *fusionStrategy,
// every other plan (including the empty/default type) to *proxyStrategy.
func TestStrategyForReturnsCorrectStrategy(t *testing.T) {
	r := newTestRouter(nil, healthySnap(), nil, &fakeMetrics{})

	cases := []struct {
		name       string
		plan       *Plan
		wantFusion bool
	}{
		{"fusion type dispatches to fusion strategy", &Plan{Type: strategyFusion}, true},
		{"proxy type dispatches to proxy strategy", &Plan{Type: strategyProxy}, false},
		{"empty type defaults to proxy strategy", &Plan{}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			switch got := r.strategyFor(tc.plan).(type) {
			case *fusionStrategy:
				if !tc.wantFusion {
					t.Fatalf("strategyFor = *fusionStrategy, want *proxyStrategy")
				}
			case *proxyStrategy:
				if tc.wantFusion {
					t.Fatalf("strategyFor = *proxyStrategy, want *fusionStrategy")
				}
			default:
				t.Fatalf("strategyFor returned unexpected type %T", got)
			}
		})
	}
}

// TestStrategyPolymorphicDispatch covers ADR-0006/0014 end-to-end via fakes: a
// fusion alias is routed through the fusion strategy (panel fan-out + judge +
// synthesis, only the synthesis relayed) while a proxy alias is routed through the
// proxy strategy (a single transparent relay). The two strategies have disjoint
// observable behavior, so which backends are touched proves which strategy ran.
func TestStrategyPolymorphicDispatch(t *testing.T) {
	const proxyBody = `{"id":"px","choices":[{"message":{"content":"proxied"}}]}`
	const panelOK = `{"choices":[{"message":{"content":"a panelist answer"}}]}`
	const judgeOK = `{"choices":[{"message":{"content":"{\"consensus\":[]}"}}]}`
	const synthBody = `{"id":"synth","choices":[{"message":{"content":"final"}}]}`

	cases := []struct {
		name        string
		model       string
		wantBackend string
		wantModel   string
		wantBody    string
		wantFusion  bool // fusion ran (panel+judge+synthesis) vs proxy single relay
	}{
		{
			name:        "proxy alias routes through the proxy strategy",
			model:       "prox",
			wantBackend: "px",
			wantModel:   "up",
			wantBody:    proxyBody,
			wantFusion:  false,
		},
		{
			name:        "fusion alias routes through the fusion strategy",
			model:       "fuse",
			wantBackend: "sb",
			wantModel:   "synth-m",
			wantBody:    synthBody,
			wantFusion:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			px := &fakeBackend{name: "px", protocol: model.ProtocolOpenAI, chat: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
				return upstream(200, proxyBody), nil
			}}
			p0 := &fakeBackend{name: "p0", protocol: model.ProtocolOpenAI, chat: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
				return upstream(200, panelOK), nil
			}}
			jb := &fakeBackend{name: "jb", protocol: model.ProtocolOpenAI, chat: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
				return upstream(200, judgeOK), nil
			}}
			sb := &fakeBackend{name: "sb", protocol: model.ProtocolOpenAI, chat: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
				return upstream(200, synthBody), nil
			}}
			backends := map[string]Backend{"px": px, "p0": p0, "jb": jb, "sb": sb}

			aliases := map[string]*Alias{
				"prox": {Name: "prox", Type: "proxy", Selector: "round_robin", Model: "up", Backends: []string{"px"}},
				"fuse": {
					Name:      "fuse",
					Type:      "fusion",
					Panel:     []PoolEntry{{Model: "m-p0", Backends: []string{"p0"}}},
					Judge:     Target{Model: "judge-m", Backends: []string{"jb"}},
					Synthesis: Target{Model: "synth-m", Backends: []string{"sb"}},
					MinPanel:  1,
				},
			}
			r := newTestRouter(backends, healthySnap("px", "p0", "jb", "sb"), aliases, &fakeMetrics{})

			sink := &recordingSink{}
			req := &model.ChatRequest{
				Model:    tc.model,
				Consumer: model.ProtocolOpenAI,
				Raw:      rawMap(t, `{"model":"`+tc.model+`","messages":[{"role":"user","content":"hi"}]}`),
			}
			outcome, err := r.Route(context.Background(), req, sink)
			if err != nil {
				t.Fatalf("Route: %v", err)
			}

			if outcome.Backend != tc.wantBackend {
				t.Fatalf("outcome.Backend = %q, want %q", outcome.Backend, tc.wantBackend)
			}
			if string(sink.body) != tc.wantBody {
				t.Fatalf("relayed body = %s, want %s", sink.body, tc.wantBody)
			}
			if sink.headers["X-Router-Backend"] != tc.wantBackend {
				t.Fatalf("X-Router-Backend = %q, want %q", sink.headers["X-Router-Backend"], tc.wantBackend)
			}
			if sink.headers["X-Router-Model"] != tc.wantModel {
				t.Fatalf("X-Router-Model = %q, want %q", sink.headers["X-Router-Model"], tc.wantModel)
			}

			if tc.wantFusion {
				// Fusion-only behavior: the panel and judge ran and the proxy-only
				// backend was never touched.
				if p0.calls.Load() == 0 || jb.calls.Load() == 0 || sb.calls.Load() == 0 {
					t.Fatalf("fusion did not fan out: panel=%d judge=%d synth=%d", p0.calls.Load(), jb.calls.Load(), sb.calls.Load())
				}
				if px.calls.Load() != 0 {
					t.Fatalf("proxy backend was called %d times on the fusion path; must be 0", px.calls.Load())
				}
			} else {
				// Proxy-only behavior: a single transparent relay, no panel/judge.
				if px.calls.Load() != 1 {
					t.Fatalf("proxy backend calls = %d, want 1", px.calls.Load())
				}
				if p0.calls.Load() != 0 || jb.calls.Load() != 0 || sb.calls.Load() != 0 {
					t.Fatalf("fusion backends ran on the proxy path: panel=%d judge=%d synth=%d", p0.calls.Load(), jb.calls.Load(), sb.calls.Load())
				}
			}
		})
	}
}
