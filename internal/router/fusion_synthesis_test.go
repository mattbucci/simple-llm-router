package router

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/mattbucci/simple-llm-router/internal/model"
)

// TestFusionSynthesisRouterHeaders covers ADR-0020: the fusion synthesis response
// reports X-Router-Model and X-Router-Backend equal to the SYNTHESIS model/backend
// (never a panelist's or the judge's), on both the unary and streaming synthesis
// paths. The sink buffers the headers and emits them when it commits, so they must
// reflect the committing synthesis candidate.
func TestFusionSynthesisRouterHeaders(t *testing.T) {
	const panelOK = `{"choices":[{"message":{"content":"a panelist answer"}}]}`
	const judgeOK = `{"choices":[{"message":{"content":"{\"consensus\":[]}"}}]}`
	const synthUnary = `{"id":"synth","choices":[{"message":{"content":"final answer"}}]}`
	const synthSSE = "data: {\"choices\":[{\"delta\":{\"content\":\"final\"}}]}\n\ndata: [DONE]\n\n"

	cases := []struct {
		name   string
		stream bool
	}{
		{"unary synthesis sets router headers to synthesis target", false},
		{"streaming synthesis sets router headers to synthesis target", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p0 := &fakeBackend{name: "p0", protocol: model.ProtocolOpenAI, chat: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
				return upstream(200, panelOK), nil
			}}
			jb := &fakeBackend{name: "jb", protocol: model.ProtocolOpenAI, chat: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
				return upstream(200, judgeOK), nil
			}}
			sb := &fakeBackend{name: "sb", protocol: model.ProtocolOpenAI, chat: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
				if s {
					return upstream(200, synthSSE), nil
				}
				return upstream(200, synthUnary), nil
			}}
			backends := map[string]Backend{"p0": p0, "jb": jb, "sb": sb}

			alias := &Alias{
				Name:      "fuse",
				Type:      "fusion",
				Panel:     []PoolEntry{{Model: "m-p0", Backends: []string{"p0"}}},
				Judge:     Target{Model: "judge-m", Backends: []string{"jb"}},
				Synthesis: Target{Model: "synth-m", Backends: []string{"sb"}},
				MinPanel:  1,
			}
			r := newTestRouter(backends, healthySnap("p0", "jb", "sb"), map[string]*Alias{"fuse": alias}, &fakeMetrics{})

			sink := &recordingSink{}
			req := &model.ChatRequest{
				Model:    "fuse",
				Stream:   tc.stream,
				Consumer: model.ProtocolOpenAI,
				Raw:      rawMap(t, `{"model":"fuse","messages":[{"role":"user","content":"hi"}]}`),
			}
			if _, err := r.Route(context.Background(), req, sink); err != nil {
				t.Fatalf("Route: %v", err)
			}

			if got := sink.headers["X-Router-Model"]; got != "synth-m" {
				t.Fatalf("X-Router-Model = %q, want synth-m (the synthesis model)", got)
			}
			if got := sink.headers["X-Router-Backend"]; got != "sb" {
				t.Fatalf("X-Router-Backend = %q, want sb (the synthesis backend)", got)
			}
		})
	}
}

// TestFusionMinPanelFromPlan covers ADR-0014: the fusion strategy honors MinPanel
// taken directly from the resolved Plan. Driving Execute with a hand-built Plan (no
// alias resolution) isolates the threshold read from the plan: with two panelists
// answering, MinPanel=2 proceeds to synthesis, while MinPanel=3 fails 502 BEFORE the
// judge or synthesis run and commits nothing.
func TestFusionMinPanelFromPlan(t *testing.T) {
	const panelOK = `{"choices":[{"message":{"content":"a panelist answer"}}]}`
	const judgeOK = `{"choices":[{"message":{"content":"{\"consensus\":[]}"}}]}`
	const synthBody = `{"id":"synth","choices":[{"message":{"content":"final"}}]}`

	cases := []struct {
		name          string
		minPanel      int
		wantErr       bool
		wantSynthesis bool
	}{
		{"two answers meet plan MinPanel of two", 2, false, true},
		{"two answers miss plan MinPanel of three", 3, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p0 := &fakeBackend{name: "p0", protocol: model.ProtocolOpenAI, chat: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
				return upstream(200, panelOK), nil
			}}
			p1 := &fakeBackend{name: "p1", protocol: model.ProtocolOpenAI, chat: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
				return upstream(200, panelOK), nil
			}}
			jb := &fakeBackend{name: "jb", protocol: model.ProtocolOpenAI, chat: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
				return upstream(200, judgeOK), nil
			}}
			sb := &fakeBackend{name: "sb", protocol: model.ProtocolOpenAI, chat: func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
				return upstream(200, synthBody), nil
			}}

			f := &fusionStrategy{
				backends: map[string]Backend{"p0": p0, "p1": p1, "jb": jb, "sb": sb},
				logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
			}

			// Hand-built plan: the only source of MinPanel under test.
			plan := &Plan{
				Alias: "fuse",
				Type:  strategyFusion,
				Panel: []Candidate{
					{Model: "m-p0", Backend: "p0"},
					{Model: "m-p1", Backend: "p1"},
				},
				Judge:     Candidate{Model: "judge-m", Backend: "jb"},
				Synthesis: Candidate{Model: "synth-m", Backend: "sb"},
				MinPanel:  tc.minPanel,
			}

			sink := &recordingSink{}
			req := &model.ChatRequest{
				Model:    "fuse",
				Consumer: model.ProtocolOpenAI,
				Raw:      rawMap(t, `{"model":"fuse","messages":[{"role":"user","content":"hi"}]}`),
			}
			outcome, err := f.Execute(context.Background(), req, plan, sink)

			if tc.wantErr {
				var apiErr *model.APIError
				if !errors.As(err, &apiErr) {
					t.Fatalf("err = %v, want *model.APIError", err)
				}
				if apiErr.Status != 502 || apiErr.Code != "upstream_unavailable" {
					t.Fatalf("apiErr = %d/%s, want 502/upstream_unavailable", apiErr.Status, apiErr.Code)
				}
				if sink.wrote {
					t.Fatalf("sink committed bytes when the plan MinPanel was not met")
				}
			} else {
				if err != nil {
					t.Fatalf("Execute: unexpected error %v", err)
				}
				if outcome.Status != 200 || outcome.Backend != "sb" {
					t.Fatalf("outcome = %+v, want status 200 backend sb", outcome)
				}
				if string(sink.body) != synthBody {
					t.Fatalf("relayed body = %s, want synthesis body %s", sink.body, synthBody)
				}
			}

			if got := sb.calls.Load() > 0; got != tc.wantSynthesis {
				t.Fatalf("synthesis called = %v, want %v (plan MinPanel=%d)", got, tc.wantSynthesis, tc.minPanel)
			}
		})
	}
}
