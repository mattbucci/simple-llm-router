package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/mattbucci/simple-llm-router/internal/model"
	"github.com/mattbucci/simple-llm-router/internal/observability"
)

// AudioConfig carries the audio gateway target into Server (ADR-0022). An empty
// Gateway BaseURL means audio is not served. Connect bounds the dial; there is no
// overall request deadline because TTS of long text and transcription of long
// media are slow like streams and must not be cut off (ADR-0007).
type AudioConfig struct {
	Gateway AudioTarget
	Connect time.Duration
}

// AudioTarget is the audio gateway upstream: its base URL (typically ending in
// /v1) plus the operator-owned outbound bearer token injected on every call
// (ADR-0009).
type AudioTarget struct {
	BaseURL string
	Token   string
}

// Configured reports whether the gateway should be served.
func (t AudioTarget) Configured() bool { return t.BaseURL != "" }

// audioProxy is a transparent reverse proxy to the audio gateway. It forwards
// request and response bodies byte-for-byte and streamed (no buffering beyond
// the inbound max_body_size cap), so binary TTS/effects/music output, chunked
// audio, and large multipart uploads all pass through unmodified (ADR-0001,
// ADR-0022). The gateway itself fans each endpoint out to the local or cloud
// engine; the router neither knows nor cares which.
type audioProxy struct {
	rp *httputil.ReverseProxy
}

// newAudioProxy builds the reverse proxy for the gateway. target.BaseURL has
// already been validated as an absolute http(s) URL by config (ADR-0010), so the
// parse here cannot fail in practice; an error is still returned defensively.
func newAudioProxy(target AudioTarget, connect time.Duration, logger *slog.Logger) (*audioProxy, error) {
	base, err := url.Parse(target.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("audio gateway: parse base_url %q: %w", target.BaseURL, err)
	}
	basePath := strings.TrimRight(base.Path, "/") // e.g. "/v1"
	token := target.Token

	rp := &httputil.ReverseProxy{
		// Rewrite points the request at the upstream and injects the outbound
		// credential. The inbound consumer credential was already stripped by
		// s.authed at the trust boundary (ADR-0009), so nothing leaks upstream.
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = base.Scheme
			pr.Out.URL.Host = base.Host
			pr.Out.Host = base.Host
			// The inbound path is /v1/<resource>; map it onto the base path so a
			// base of http://h/v1 plus /v1/audio/speech yields
			// http://h/v1/audio/speech (mirrors backend.Client.url()). A non-/v1
			// inbound path is forwarded verbatim beneath the base path.
			resource := strings.TrimPrefix(pr.In.URL.Path, "/v1")
			pr.Out.URL.Path = basePath + resource
			pr.Out.URL.RawPath = ""
			if token != "" {
				pr.Out.Header.Set("Authorization", "Bearer "+token)
			}
		},
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: connect, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   32,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   connect,
			ExpectContinueTimeout: time.Second,
		},
		// Flush every write so streamed/chunked audio reaches the client promptly
		// instead of buffering (ADR-0007).
		FlushInterval: -1,
		// Surface failures as an OpenAI-shaped error (ADR-0019) instead of
		// ReverseProxy's default plain-text 502. An inbound body that exceeds the
		// cap (ADR-0008) trips the MaxBytesReader while the request streams
		// upstream and surfaces here; map it to 413, everything else to 502.
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			apiErr := model.ErrUpstreamUnavailable()
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				apiErr = &model.APIError{
					Status:  http.StatusRequestEntityTooLarge,
					Code:    "request_too_large",
					Message: "request body exceeds the configured maximum size",
				}
			}
			logger.Error("audio upstream failed",
				slog.String("request_id", observability.RequestID(r.Context())),
				slog.Int("status", apiErr.Status),
				slog.String("error", err.Error()),
			)
			writeAPIError(w, apiErr)
		},
	}
	return &audioProxy{rp: rp}, nil
}

// audioHandler wraps an audioProxy with the per-request bookkeeping the chat path
// also performs: request-id correlation, the in-flight gauge, the inbound body
// cap (ADR-0008), and exactly one log line plus one metrics record (ADR-0011). It
// does NOT decode, route, or translate — audio bypasses internal/router and the
// canonical chat model entirely (ADR-0022).
func (s *Server) audioHandler(p *audioProxy) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ctx := observability.WithRequestID(r.Context(), observability.NewRequestID())
		r = r.WithContext(ctx)

		s.metrics.AddInFlight(1)
		defer s.metrics.AddInFlight(-1)

		sw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			latency := time.Since(start)
			// The audio surface is OpenAI-shaped; there is no alias, upstream model,
			// or token accounting, so those record fields stay zero. The backend is
			// the single gateway ("audio") and the path distinguishes the endpoint
			// (speech/transcriptions/voices/isolation/sound-effects/music) in the
			// one log line and metrics series (ADR-0011).
			observability.RequestRecord{
				ConsumerProtocol: "openai",
				ModelAlias:       r.URL.Path,
				Backend:          "audio",
				Status:           sw.status,
				Latency:          latency,
			}.Emit(ctx, s.logger)
			s.metrics.RequestDone("audio", r.URL.Path, sw.status, latency)
		}()

		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, s.maxBodySize)
		}
		p.rp.ServeHTTP(sw, r)
	})
}

// statusRecorder captures the response status for the per-request log/metrics
// while forwarding writes (including streamed bodies) straight through. Unwrap
// exposes the underlying writer so the reverse proxy's ResponseController can
// still flush and set deadlines; Flush is also provided for the direct
// Flusher type-assertion path.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wrote = true
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
