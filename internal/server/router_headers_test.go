package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mattbucci/simple-llm-router/internal/model"
	"github.com/mattbucci/simple-llm-router/internal/router"
)

// TestProxySetsRouterHeaders covers ADR-0013: the proxy reports the concrete
// routing decision back to the consumer via X-Router-Model and X-Router-Backend on
// the committed response — the RESOLVED upstream model id (not the inbound alias)
// and the backend actually used — independent of any upstream echo. It must hold on
// both the verbatim OpenAI sink and the translating Anthropic sink.
func TestProxySetsRouterHeaders(t *testing.T) {
	const upstreamModel = "/models/Real-1.0"

	cases := []struct {
		name     string
		endpoint string
		reqBody  string
	}{
		{
			name:     "openai consumer",
			endpoint: "/v1/chat/completions",
			reqBody:  `{"model":"alias-x","messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name:     "anthropic consumer",
			endpoint: "/v1/messages",
			reqBody:  `{"model":"alias-x","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			be := &fakeBackend{
				name:     "b1",
				protocol: model.ProtocolOpenAI,
				fn:       okResponse(`{"id":"chatcmpl-1","model":"up","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`),
			}
			srv := newServer(t,
				map[string]router.Backend{"b1": be},
				map[string]*router.Alias{"alias-x": proxyAlias("alias-x", upstreamModel, "b1")},
				snapshot(model.BackendState{Name: "b1", Healthy: true}),
				nil, io.Discard,
			)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.endpoint, strings.NewReader(tc.reqBody))
			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("X-Router-Model"); got != upstreamModel {
				t.Fatalf("X-Router-Model = %q, want %q (the resolved upstream id, not the alias)", got, upstreamModel)
			}
			if got := rec.Header().Get("X-Router-Backend"); got != "b1" {
				t.Fatalf("X-Router-Backend = %q, want b1", got)
			}
		})
	}
}
