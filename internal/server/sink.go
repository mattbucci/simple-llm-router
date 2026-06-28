package server

import (
	"bytes"
	"io"
	"net/http"
)

// openaiSink relays an OpenAI-canonical response to an OpenAI consumer verbatim
// (ADR-0001, ADR-0007). It performs no translation: unknown response fields and
// SSE chunks pass through unchanged, preserving reasoning_content and friends.
type openaiSink struct {
	w     http.ResponseWriter
	rc    *http.ResponseController
	wrote bool
	extra map[string]string
}

// newOpenAISink builds a verbatim relay sink over w.
func newOpenAISink(w http.ResponseWriter) *openaiSink {
	return &openaiSink{w: w, rc: http.NewResponseController(w)}
}

// Wrote reports whether any byte has reached the client yet.
func (s *openaiSink) Wrote() bool { return s.wrote }

// SetHeader buffers a router-set response header (e.g. X-Router-Model) to be
// emitted when the response is committed. Single-goroutine per request, so no
// synchronization is needed (ADR-0015).
func (s *openaiSink) SetHeader(key, value string) {
	if s.extra == nil {
		s.extra = make(map[string]string, 2)
	}
	s.extra[key] = value
}

// WriteResponse relays a complete unary upstream response, faithfully carrying
// the upstream status and Content-Type (ADR-0001).
func (s *openaiSink) WriteResponse(status int, header http.Header, body []byte) error {
	ct := ""
	if header != nil {
		ct = header.Get("Content-Type")
	}
	if ct == "" {
		ct = "application/json"
	}
	s.w.Header().Set("Content-Type", ct)
	applyExtraHeaders(s.w.Header(), s.extra)
	s.w.WriteHeader(status)
	s.wrote = true
	_, err := s.w.Write(body)
	return err
}

// StartStream writes the SSE response headers and flushes them so the client
// sees the stream open immediately (ADR-0007).
func (s *openaiSink) StartStream() error {
	setStreamHeaders(s.w.Header())
	applyExtraHeaders(s.w.Header(), s.extra)
	s.w.WriteHeader(http.StatusOK)
	s.wrote = true
	_ = s.rc.Flush()
	return nil
}

// WriteEvent re-frames one OpenAI SSE data payload as "data: <payload>\n\n" and
// flushes (ADR-0007). The upstream [DONE] sentinel is skipped here; EndStream
// emits the terminator so it is written exactly once.
func (s *openaiSink) WriteEvent(data []byte) error {
	if isDone(data) {
		return nil
	}
	s.wrote = true
	if _, err := s.w.Write([]byte("data: ")); err != nil {
		return err
	}
	if _, err := s.w.Write(data); err != nil {
		return err
	}
	if _, err := s.w.Write([]byte("\n\n")); err != nil {
		return err
	}
	_ = s.rc.Flush()
	return nil
}

// EndStream writes the OpenAI stream terminator and flushes (ADR-0007).
func (s *openaiSink) EndStream() error {
	s.wrote = true
	if _, err := io.WriteString(s.w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	_ = s.rc.Flush()
	return nil
}

// WriteRawResponse relays a complete unary upstream response verbatim for the
// same-protocol native relay path (ADR-0016). The OpenAI sink's normal
// WriteResponse is already a byte-for-byte relay, so the raw path delegates to it.
// (An OpenAI consumer never actually takes the native relay path — the gate
// requires an Anthropic consumer — but the sink satisfies router.ResponseSink.)
func (s *openaiSink) WriteRawResponse(status int, header http.Header, body []byte) error {
	return s.WriteResponse(status, header, body)
}

// StartRawStream commits the SSE response; identical to StartStream for the
// verbatim OpenAI relay (ADR-0007).
func (s *openaiSink) StartRawStream() error { return s.StartStream() }

// WriteRawChunk relays raw upstream SSE bytes to the consumer verbatim and flushes
// (no reframing, no translation), preserving the provider's own event framing
// (ADR-0007, ADR-0016).
func (s *openaiSink) WriteRawChunk(p []byte) error {
	s.wrote = true
	if _, err := s.w.Write(p); err != nil {
		return err
	}
	_ = s.rc.Flush()
	return nil
}

// setStreamHeaders sets the SSE content type and disables downstream buffering
// (ADR-0007).
func setStreamHeaders(h http.Header) {
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
}

// isDone reports whether an SSE data payload is the OpenAI [DONE] sentinel.
func isDone(data []byte) bool {
	return bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]"))
}

// applyExtraHeaders writes the buffered router-set headers onto h. Shared by both
// sinks so X-Router-Model / X-Router-Backend land before WriteHeader is called.
func applyExtraHeaders(h http.Header, extra map[string]string) {
	for k, v := range extra {
		h.Set(k, v)
	}
}
