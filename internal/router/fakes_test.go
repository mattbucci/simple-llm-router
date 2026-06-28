package router

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mattbucci/simple-llm-router/internal/model"
)

// fakeBackend is a router.Backend whose Chat behavior is driven by a closure. It
// records the number of calls and the last forwarded body for assertions. Its
// ChatNative method (part of router.Backend) lets tests exercise the
// same-protocol native relay path; when chatNative is nil it falls back to chat.
type fakeBackend struct {
	name       string
	protocol   model.Protocol
	chat       func(ctx context.Context, body []byte, stream bool) (*model.UpstreamResponse, error)
	chatNative func(ctx context.Context, body []byte, stream bool) (*model.UpstreamResponse, error)
	calls      atomic.Int64
	lastBody   atomic.Value // []byte
	lastNative atomic.Value // []byte
}

func (f *fakeBackend) Name() string             { return f.name }
func (f *fakeBackend) Protocol() model.Protocol { return f.protocol }

func (f *fakeBackend) Chat(ctx context.Context, body []byte, stream bool) (*model.UpstreamResponse, error) {
	f.calls.Add(1)
	f.lastBody.Store(append([]byte(nil), body...))
	return f.chat(ctx, body, stream)
}

// ChatNative is the same-protocol native relay (router.Backend).
func (f *fakeBackend) ChatNative(ctx context.Context, body []byte, stream bool) (*model.UpstreamResponse, error) {
	f.calls.Add(1)
	f.lastNative.Store(append([]byte(nil), body...))
	if f.chatNative != nil {
		return f.chatNative(ctx, body, stream)
	}
	return f.chat(ctx, body, stream)
}

func (f *fakeBackend) body() []byte {
	if v, ok := f.lastBody.Load().([]byte); ok {
		return v
	}
	return nil
}

func (f *fakeBackend) nativeBody() []byte {
	if v, ok := f.lastNative.Load().([]byte); ok {
		return v
	}
	return nil
}

// fakeHealth is a router.HealthView returning a fixed snapshot.
type fakeHealth struct{ snap *model.Snapshot }

func (f fakeHealth) Snapshot() *model.Snapshot { return f.snap }

// Suspect is the HealthView fast-reprobe hook; tests need no re-probe, so it is a
// no-op.
func (f fakeHealth) Suspect(string) {}

// recordingHealth is a router.HealthView returning a fixed snapshot and recording
// the backend names passed to Suspect, so tests can assert the fast-reprobe hook
// fires on the right failover paths (ADR-0005/0006). Suspect is invoked only from
// the sequential proxy failover loop in the calling goroutine, so the unguarded
// slice is race-free (no mutex needed — ADR-0015).
type recordingHealth struct {
	snap      *model.Snapshot
	suspected []string
}

func (h *recordingHealth) Snapshot() *model.Snapshot { return h.snap }
func (h *recordingHealth) Suspect(name string)       { h.suspected = append(h.suspected, name) }

// fakeMetrics is a router.MetricsRecorder counting failovers.
type fakeMetrics struct{ failovers atomic.Int64 }

func (f *fakeMetrics) IncFailover() { f.failovers.Add(1) }

// recordingSink is a router.ResponseSink recording everything written, including
// the raw-relay methods that exercise the same-protocol native relay path
// (verbatim, no translation).
type recordingSink struct {
	status     int
	header     http.Header
	body       []byte
	events     [][]byte
	headers    map[string]string
	started    bool
	ended      bool
	wrote      bool
	rawBody    []byte
	rawChunks  [][]byte
	rawStarted bool
}

func (s *recordingSink) SetHeader(key, value string) {
	if s.headers == nil {
		s.headers = make(map[string]string, 2)
	}
	s.headers[key] = value
}

func (s *recordingSink) WriteResponse(status int, header http.Header, body []byte) error {
	s.status = status
	s.header = header
	s.body = append([]byte(nil), body...)
	s.wrote = true
	return nil
}

func (s *recordingSink) StartStream() error { s.started = true; s.wrote = true; return nil }

func (s *recordingSink) WriteEvent(data []byte) error {
	s.events = append(s.events, append([]byte(nil), data...))
	s.wrote = true
	return nil
}

func (s *recordingSink) EndStream() error { s.ended = true; s.wrote = true; return nil }
func (s *recordingSink) Wrote() bool      { return s.wrote }

// Raw-relay methods (router.ResponseSink): verbatim, no translation.
func (s *recordingSink) WriteRawResponse(status int, header http.Header, body []byte) error {
	s.status = status
	s.header = header
	s.rawBody = append([]byte(nil), body...)
	s.wrote = true
	return nil
}

func (s *recordingSink) StartRawStream() error { s.rawStarted = true; s.wrote = true; return nil }

func (s *recordingSink) WriteRawChunk(p []byte) error {
	s.rawChunks = append(s.rawChunks, append([]byte(nil), p...))
	s.wrote = true
	return nil
}

// upstream builds a *model.UpstreamResponse with a string body.
func upstream(status int, body string) *model.UpstreamResponse {
	return &model.UpstreamResponse{
		Status: status,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   io.NopCloser(strings.NewReader(body)),
	}
}

// errReadCloser yields data once then returns a non-EOF error, simulating a
// stream that fails mid-flight after bytes have already been spliced.
type errReadCloser struct {
	data []byte
	pos  int
	err  error
}

func (r *errReadCloser) Read(p []byte) (int, error) {
	if r.pos < len(r.data) {
		n := copy(p, r.data[r.pos:])
		r.pos += n
		return n, nil
	}
	return 0, r.err
}

func (r *errReadCloser) Close() error { return nil }

// newTestRouter builds a Router wired with the given collaborators and a silent
// logger, using a fixed (no-op-Suspect) health view over snap.
func newTestRouter(backends map[string]Backend, snap *model.Snapshot, aliases map[string]*Alias, m MetricsRecorder) *Router {
	return newTestRouterWithHealth(backends, fakeHealth{snap}, aliases, m)
}

// newTestRouterWithHealth builds a Router with an explicit HealthView so tests can
// observe Suspect calls (recordingHealth) or other health behavior.
func newTestRouterWithHealth(backends map[string]Backend, health HealthView, aliases map[string]*Alias, m MetricsRecorder) *Router {
	return New(backends, health, m, aliases, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func rawMap(t *testing.T, s string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("rawMap: %v", err)
	}
	return m
}

func healthySnap(names ...string) *model.Snapshot {
	bs := map[string]model.BackendState{}
	for _, n := range names {
		bs[n] = model.BackendState{Name: n, Healthy: true}
	}
	return &model.Snapshot{Backends: bs}
}
