package router

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mattbucci/simple-llm-router/internal/model"
)

// TestResolutionStatusMapping covers ADR-0004/0006: alias-first then direct-id
// resolution, with 404 model_not_found for an entirely unknown name, 503
// no_healthy_backend for a known-but-down name (alias or direct), and a
// successful resolution for healthy candidates.
func TestResolutionStatusMapping(t *testing.T) {
	const directModel = "gemma4-31b"
	const aliasUpstream = "/models/North-Mini-Code-1.0-fp8"

	aliases := map[string]*Alias{
		"north": {Name: "north", Type: "proxy", Selector: "round_robin", Model: aliasUpstream, Backends: []string{"gpu0"}},
	}

	okChat := func(ctx context.Context, b []byte, s bool) (*model.UpstreamResponse, error) {
		return upstream(200, `{"id":"x","choices":[{"message":{"content":"hello"}}]}`), nil
	}

	cases := []struct {
		name          string
		model         string
		snap          *model.Snapshot
		wantErrStatus int // 0 => success
		wantErrCode   string
		wantBackend   string
		wantUpstream  string
	}{
		{
			name:          "unknown name is 404",
			model:         "ghost",
			snap:          model.EmptySnapshot(),
			wantErrStatus: 404,
			wantErrCode:   "model_not_found",
		},
		{
			name:  "direct id known but unhealthy is 503",
			model: directModel,
			snap: &model.Snapshot{Backends: map[string]model.BackendState{
				"g": {Name: "g", Healthy: false, Models: map[string]struct{}{directModel: {}}},
			}},
			wantErrStatus: 503,
			wantErrCode:   "no_healthy_backend",
		},
		{
			name:  "alias with all backends down is 503",
			model: "north",
			snap: &model.Snapshot{Backends: map[string]model.BackendState{
				"gpu0": {Name: "gpu0", Healthy: false},
			}},
			wantErrStatus: 503,
			wantErrCode:   "no_healthy_backend",
		},
		{
			name:  "alias resolves to its upstream id and backend",
			model: "north",
			snap: &model.Snapshot{Backends: map[string]model.BackendState{
				"gpu0": {Name: "gpu0", Healthy: true},
			}},
			wantBackend:  "gpu0",
			wantUpstream: aliasUpstream,
		},
		{
			name:  "direct id resolves against advertising healthy backend",
			model: directModel,
			snap: &model.Snapshot{Backends: map[string]model.BackendState{
				"g": {Name: "g", Healthy: true, Models: map[string]struct{}{directModel: {}}},
			}},
			wantBackend:  "g",
			wantUpstream: directModel,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backends := map[string]Backend{
				"gpu0": &fakeBackend{name: "gpu0", protocol: model.ProtocolOpenAI, chat: okChat},
				"g":    &fakeBackend{name: "g", protocol: model.ProtocolOpenAI, chat: okChat},
			}
			r := newTestRouter(backends, tc.snap, aliases, &fakeMetrics{})

			sink := &recordingSink{}
			req := &model.ChatRequest{
				Model:    tc.model,
				Consumer: model.ProtocolOpenAI,
				Raw:      map[string]json.RawMessage{"model": json.RawMessage(`"` + tc.model + `"`), "messages": json.RawMessage(`[]`)},
			}
			outcome, err := r.Route(context.Background(), req, sink)

			if tc.wantErrStatus != 0 {
				var apiErr *model.APIError
				if !errors.As(err, &apiErr) {
					t.Fatalf("err = %v, want *model.APIError", err)
				}
				if apiErr.Status != tc.wantErrStatus || apiErr.Code != tc.wantErrCode {
					t.Fatalf("apiErr = %d/%s, want %d/%s", apiErr.Status, apiErr.Code, tc.wantErrStatus, tc.wantErrCode)
				}
				if sink.wrote {
					t.Fatalf("sink committed bytes on an error resolution")
				}
				return
			}

			if err != nil {
				t.Fatalf("Route: unexpected error %v", err)
			}
			if outcome.Backend != tc.wantBackend {
				t.Fatalf("backend = %q, want %q", outcome.Backend, tc.wantBackend)
			}
			if outcome.UpstreamModel != tc.wantUpstream {
				t.Fatalf("upstream model = %q, want %q", outcome.UpstreamModel, tc.wantUpstream)
			}

			// The outbound body must carry the resolved upstream id, not the alias.
			be := backends[tc.wantBackend].(*fakeBackend)
			var sent map[string]json.RawMessage
			if err := json.Unmarshal(be.body(), &sent); err != nil {
				t.Fatalf("forwarded body: %v", err)
			}
			var sentModel string
			_ = json.Unmarshal(sent["model"], &sentModel)
			if sentModel != tc.wantUpstream {
				t.Fatalf("forwarded model = %q, want %q", sentModel, tc.wantUpstream)
			}
		})
	}
}
