package router

import (
	"context"
	"errors"
	"testing"

	"github.com/mattbucci/simple-llm-router/internal/model"
)

// TestFusionMinPanelResponses covers ADR-0014: fusion honors the configured
// min_panel_responses threshold. When fewer panelists than the threshold return a
// non-empty answer, the request fails 502 upstream_unavailable BEFORE the judge or
// synthesis run (no half-baked answer); when the threshold is met, fusion proceeds
// to synthesis and relays its reply. The threshold is enforced at runtime against
// the surviving panel answers, independent of backend health.
func TestFusionMinPanelResponses(t *testing.T) {
	const panelOK = `{"choices":[{"message":{"content":"a panelist answer"}}]}`
	const judgeOK = `{"choices":[{"message":{"content":"{\"consensus\":[]}"}}]}`
	const synthBody = `{"id":"synth","choices":[{"message":{"content":"final answer"}}],"usage":{"prompt_tokens":7,"completion_tokens":4}}`

	cases := []struct {
		name          string
		panelSucceeds []bool // per-panelist runtime success
		minPanel      int
		wantErr       bool
		wantStatus    int
		wantSynthesis bool
	}{
		{
			name:          "below configured threshold fails before synthesis",
			panelSucceeds: []bool{true, false, false},
			minPanel:      2,
			wantErr:       true,
			wantStatus:    502,
			wantSynthesis: false,
		},
		{
			name:          "configured threshold met proceeds to synthesis",
			panelSucceeds: []bool{true, true, false},
			minPanel:      2,
			wantErr:       false,
			wantStatus:    200,
			wantSynthesis: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backends := map[string]Backend{}
			names := make([]string, 0, len(tc.panelSucceeds)+2)
			panel := make([]PoolEntry, 0, len(tc.panelSucceeds))
			for i, ok := range tc.panelSucceeds {
				name := "p" + string(rune('0'+i))
				status, body := 200, panelOK
				if !ok {
					status, body = 500, `{"error":"panelist down"}`
				}
				st, bd := status, body
				backends[name] = &fakeBackend{
					name:     name,
					protocol: model.ProtocolOpenAI,
					chat: func(ctx context.Context, b []byte, stream bool) (*model.UpstreamResponse, error) {
						return upstream(st, bd), nil
					},
				}
				panel = append(panel, PoolEntry{Model: "m-" + name, Backends: []string{name}})
				names = append(names, name)
			}

			jb := &fakeBackend{name: "jb", protocol: model.ProtocolOpenAI, chat: func(ctx context.Context, b []byte, stream bool) (*model.UpstreamResponse, error) {
				return upstream(200, judgeOK), nil
			}}
			sb := &fakeBackend{name: "sb", protocol: model.ProtocolOpenAI, chat: func(ctx context.Context, b []byte, stream bool) (*model.UpstreamResponse, error) {
				return upstream(200, synthBody), nil
			}}
			backends["jb"], backends["sb"] = jb, sb
			names = append(names, "jb", "sb")

			alias := &Alias{
				Name:      "fuse",
				Type:      "fusion",
				Panel:     panel,
				Judge:     Target{Model: "judge-m", Backends: []string{"jb"}},
				Synthesis: Target{Model: "synth-m", Backends: []string{"sb"}},
				MinPanel:  tc.minPanel,
			}
			r := newTestRouter(backends, healthySnap(names...), map[string]*Alias{"fuse": alias}, &fakeMetrics{})

			sink := &recordingSink{}
			req := &model.ChatRequest{
				Model:    "fuse",
				Consumer: model.ProtocolOpenAI,
				Raw:      rawMap(t, `{"model":"fuse","messages":[{"role":"user","content":"hi"}]}`),
			}
			outcome, err := r.Route(context.Background(), req, sink)

			if tc.wantErr {
				var apiErr *model.APIError
				if !errors.As(err, &apiErr) {
					t.Fatalf("err = %v, want *model.APIError", err)
				}
				if apiErr.Status != tc.wantStatus || apiErr.Code != "upstream_unavailable" {
					t.Fatalf("apiErr = %d/%s, want %d/upstream_unavailable", apiErr.Status, apiErr.Code, tc.wantStatus)
				}
				if sink.wrote {
					t.Fatalf("sink committed bytes when the panel threshold was not met")
				}
			} else {
				if err != nil {
					t.Fatalf("Route: unexpected error %v", err)
				}
				if outcome.Status != tc.wantStatus {
					t.Fatalf("status = %d, want %d", outcome.Status, tc.wantStatus)
				}
				if outcome.Backend != "sb" {
					t.Fatalf("outcome.Backend = %q, want sb", outcome.Backend)
				}
				if string(sink.body) != synthBody {
					t.Fatalf("relayed body = %s, want synthesis body %s", sink.body, synthBody)
				}
			}

			if got := sb.calls.Load() > 0; got != tc.wantSynthesis {
				t.Fatalf("synthesis called = %v, want %v (min_panel_responses=%d)", got, tc.wantSynthesis, tc.minPanel)
			}
		})
	}
}
