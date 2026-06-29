package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mattbucci/simple-llm-router/internal/model"
	"github.com/mattbucci/simple-llm-router/internal/observability"
	"github.com/mattbucci/simple-llm-router/internal/router"
	"github.com/mattbucci/simple-llm-router/internal/server"
)

// stubRouter satisfies server.Router but must never be reached: audio bypasses
// internal/router entirely (ADR-0022). A call means a route was misregistered.
type stubRouter struct{ t *testing.T }

func (s stubRouter) Route(context.Context, *model.ChatRequest, router.ResponseSink) (*router.Outcome, error) {
	s.t.Fatalf("internal/router must not be invoked for audio requests")
	return nil, nil
}

// newAudioServer builds a Server wired with the given audio gateway, an empty
// health snapshot, and a router stub that fails if reached.
func newAudioServer(t *testing.T, gateway server.AudioTarget, tokens []string, maxBody int64) *server.Server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	metrics := observability.New(ctx)
	health := fakeHealth{&model.Snapshot{}}
	logger := slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	audio := server.AudioConfig{Gateway: gateway}
	return server.New(stubRouter{t}, health, metrics, server.NewStaticTokenAuth(tokens), maxBody, audio, logger)
}

// capturingUpstream records the request the gateway received and replies with the
// supplied status, content-type, and body.
type capturingUpstream struct {
	method      string
	path        string
	authHeader  string
	apiKey      string
	contentType string
	body        []byte
}

func newUpstream(t *testing.T, cap *capturingUpstream, status int, respCT string, respBody []byte) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.method = r.Method
		cap.path = r.URL.Path
		cap.authHeader = r.Header.Get("Authorization")
		cap.apiKey = r.Header.Get("X-Api-Key")
		cap.contentType = r.Header.Get("Content-Type")
		cap.body, _ = io.ReadAll(r.Body)
		if respCT != "" {
			w.Header().Set("Content-Type", respCT)
		}
		w.WriteHeader(status)
		_, _ = w.Write(respBody)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestAudioSpeechPassthrough(t *testing.T) {
	cap := &capturingUpstream{}
	wav := []byte{0x52, 0x49, 0x46, 0x46, 0x00, 0x01, 0x02, 0x03} // "RIFF" + bytes
	ts := newUpstream(t, cap, http.StatusOK, "audio/wav", wav)

	srv := newAudioServer(t, server.AudioTarget{BaseURL: ts.URL + "/v1", Token: "upstream-secret"},
		[]string{"client-token"}, 100<<20)

	reqBody := []byte(`{"input":"Hello world","voice":"narrator","engine":"auto","response_format":"wav"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/wav" {
		t.Errorf("response content-type = %q, want audio/wav", ct)
	}
	if !bytes.Equal(rec.Body.Bytes(), wav) {
		t.Errorf("response body = %v, want the upstream binary verbatim", rec.Body.Bytes())
	}
	// The gateway must see the resource path, the injected outbound token, the
	// request body verbatim (engine/voice/format included), and NONE of the
	// inbound consumer credential.
	if cap.path != "/v1/audio/speech" {
		t.Errorf("gateway path = %q, want /v1/audio/speech", cap.path)
	}
	if cap.authHeader != "Bearer upstream-secret" {
		t.Errorf("gateway Authorization = %q, want injected outbound token", cap.authHeader)
	}
	if cap.apiKey != "" {
		t.Errorf("gateway X-Api-Key = %q, want empty (inbound creds stripped)", cap.apiKey)
	}
	if !bytes.Equal(cap.body, reqBody) {
		t.Errorf("gateway body = %q, want request forwarded verbatim", cap.body)
	}
}

func TestAudioTranscriptionMultipart(t *testing.T) {
	cap := &capturingUpstream{}
	srt := []byte("1\n00:00:00,000 --> 00:00:01,000\nhello\n")
	ts := newUpstream(t, cap, http.StatusOK, "application/x-subrip", srt)

	srv := newAudioServer(t, server.AudioTarget{BaseURL: ts.URL + "/v1", Token: "tok"}, nil, 100<<20)

	// Build a multipart body with a file field, like `-F file=@meeting.mp4`.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "meeting.mp4")
	if err != nil {
		t.Fatal(err)
	}
	fileContent := []byte("fake-mp4-bytes")
	_, _ = fw.Write(fileContent)
	_ = mw.WriteField("response_format", "srt")
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), srt) {
		t.Errorf("response body = %q, want srt verbatim", rec.Body.Bytes())
	}
	if cap.path != "/v1/audio/transcriptions" {
		t.Errorf("gateway path = %q", cap.path)
	}
	// The multipart Content-Type (including the boundary) must survive untouched.
	if !strings.HasPrefix(cap.contentType, "multipart/form-data; boundary=") {
		t.Errorf("gateway content-type = %q, want multipart with boundary", cap.contentType)
	}
	if !bytes.Contains(cap.body, fileContent) {
		t.Errorf("gateway body missing the uploaded file bytes")
	}
}

// TestAudioJSONToAudioEndpoints covers the cloud JSON-in/audio-out endpoints
// (sound-effects, music) — both are plain passthrough to the gateway.
func TestAudioJSONToAudioEndpoints(t *testing.T) {
	cases := []struct {
		path string
		body string
	}{
		{"/v1/sound-effects", `{"text":"distant thunder, light rain","duration_seconds":5}`},
		{"/v1/music", `{"prompt":"calm lo-fi piano loop","music_length_ms":15000}`},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			cap := &capturingUpstream{}
			mp3 := []byte{0x49, 0x44, 0x33, 0x04} // "ID3" mp3 tag
			ts := newUpstream(t, cap, http.StatusOK, "audio/mpeg", mp3)
			srv := newAudioServer(t, server.AudioTarget{BaseURL: ts.URL + "/v1", Token: "tok"}, nil, 100<<20)

			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			if cap.path != tc.path || cap.method != http.MethodPost {
				t.Errorf("gateway got %s %s, want POST %s", cap.method, cap.path, tc.path)
			}
			if cap.body == nil || string(cap.body) != tc.body {
				t.Errorf("gateway body = %q, want %q", cap.body, tc.body)
			}
			if !bytes.Equal(rec.Body.Bytes(), mp3) {
				t.Errorf("audio body not forwarded verbatim")
			}
		})
	}
}

// TestAudioIsolationMultipart covers the cloud multipart isolation endpoint.
func TestAudioIsolationMultipart(t *testing.T) {
	cap := &capturingUpstream{}
	ts := newUpstream(t, cap, http.StatusOK, "audio/mpeg", []byte("clean-audio"))
	srv := newAudioServer(t, server.AudioTarget{BaseURL: ts.URL + "/v1", Token: "tok"}, nil, 100<<20)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "episode.mp3")
	_, _ = fw.Write([]byte("noisy-audio-bytes"))
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/isolation", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if cap.path != "/v1/audio/isolation" {
		t.Errorf("gateway path = %q, want /v1/audio/isolation", cap.path)
	}
	if !strings.HasPrefix(cap.contentType, "multipart/form-data; boundary=") {
		t.Errorf("gateway content-type = %q, want multipart", cap.contentType)
	}
}

// TestAudioVoicesSubtree covers list (GET), register (POST), and delete-by-id
// (DELETE /v1/voices/{id}) — the method-less subtree must pass them all through.
func TestAudioVoicesSubtree(t *testing.T) {
	cases := []struct {
		method string
		path   string
		body   string
		status int
	}{
		{http.MethodGet, "/v1/voices", "", http.StatusOK},
		{http.MethodPost, "/v1/voices", `{"name":"anna","description":"bright host"}`, http.StatusCreated},
		{http.MethodDelete, "/v1/voices/v_123", "", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			cap := &capturingUpstream{}
			ts := newUpstream(t, cap, tc.status, "application/json", []byte(`{"ok":true}`))
			srv := newAudioServer(t, server.AudioTarget{BaseURL: ts.URL + "/v1", Token: "tok"}, nil, 100<<20)

			var body io.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			}
			req := httptest.NewRequest(tc.method, tc.path, body)
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			if cap.method != tc.method || cap.path != tc.path {
				t.Errorf("gateway got %s %s, want %s %s", cap.method, cap.path, tc.method, tc.path)
			}
		})
	}
}

func TestAudioAuthRequired(t *testing.T) {
	hit := false
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	t.Cleanup(ts.Close)

	srv := newAudioServer(t, server.AudioTarget{BaseURL: ts.URL + "/v1", Token: "tok"},
		[]string{"client-token"}, 100<<20)

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	// No inbound credential presented.
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if hit {
		t.Error("gateway was contacted despite failed inbound auth")
	}
	var doc struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("error body not JSON: %v", err)
	}
	if doc.Error.Code != "unauthorized" {
		t.Errorf("error code = %q, want unauthorized", doc.Error.Code)
	}
}

func TestAudioUnconfiguredIs404(t *testing.T) {
	// No gateway configured: the audio paths must not be registered at all.
	srv := newAudioServer(t, server.AudioTarget{}, nil, 100<<20)
	for _, path := range []string{
		"/v1/audio/speech", "/v1/audio/transcriptions", "/v1/audio/isolation",
		"/v1/sound-effects", "/v1/music", "/v1/voices",
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404 when unconfigured", path, rec.Code)
		}
	}
}

func TestAudioUpstreamDownIsOpenAIError(t *testing.T) {
	// Point at a closed listener so the dial fails.
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := ts.URL
	ts.Close() // now nothing listens on closedURL

	srv := newAudioServer(t, server.AudioTarget{BaseURL: closedURL + "/v1", Token: "tok"}, nil, 100<<20)

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var doc struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("error body not OpenAI-shaped JSON: %v (%s)", err, rec.Body.String())
	}
	if doc.Error.Code != "upstream_unavailable" {
		t.Errorf("error code = %q, want upstream_unavailable", doc.Error.Code)
	}
}

func TestAudioBodyCapIs413(t *testing.T) {
	cap := &capturingUpstream{}
	ts := newUpstream(t, cap, http.StatusOK, "application/json", []byte(`{}`))

	const limit = 16
	srv := newAudioServer(t, server.AudioTarget{BaseURL: ts.URL + "/v1", Token: "tok"}, nil, limit)

	big := bytes.Repeat([]byte("A"), limit*8)
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(big))
	req.Header.Set("Content-Type", "application/octet-stream")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}
