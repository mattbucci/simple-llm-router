package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/mattbucci/simple-llm-router/internal/model"
	"github.com/mattbucci/simple-llm-router/internal/observability"
)

// ----- inbound adapter: Anthropic Messages -> canonical OpenAI shape ---------

// anthropicInMessage is the part of an Anthropic message the adapter reads.
type anthropicInMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// parseAnthropicRequest is the inbound adapter for Anthropic consumers. It
// translates the Anthropic Messages body into the canonical OpenAI-shaped Raw map
// the router operates on (ADR-0016): the top-level system field becomes a
// system-role message, stop_sequences becomes stop, and content blocks map to
// OpenAI content parts (ADR-0008). The response-side translation back to
// Anthropic shape is handled by anthropicSink.
func parseAnthropicRequest(body []byte) (*model.ChatRequest, error) {
	var in map[string]json.RawMessage
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, model.ErrBadRequest("request body is not valid JSON")
	}

	out := map[string]json.RawMessage{}
	// ConsumerBody keeps the original Anthropic bytes verbatim so an Anthropic
	// consumer routed to an Anthropic backend takes the same-protocol native relay
	// (ADR-0016 full passthrough) instead of the lossy canonical translation; the
	// OpenAI-canonical out/Raw below is still populated for the cross-protocol case
	// (Anthropic consumer -> OpenAI backend).
	req := &model.ChatRequest{Consumer: model.ProtocolAnthropic, ConsumerBody: body}

	if m, ok := in["model"]; ok {
		out["model"] = m
		_ = json.Unmarshal(m, &req.Model)
	}
	if s, ok := in["stream"]; ok {
		out["stream"] = s
		_ = json.Unmarshal(s, &req.Stream)
	}
	if mt, ok := in["max_tokens"]; ok {
		out["max_tokens"] = mt
	}
	// Common sampling parameters carry over by the same name.
	for _, k := range []string{"temperature", "top_p"} {
		if v, ok := in[k]; ok {
			out[k] = v
		}
	}
	// Anthropic stop_sequences -> OpenAI stop.
	if ss, ok := in["stop_sequences"]; ok {
		out["stop"] = ss
	}
	// Routing-control plugins are honored on either endpoint and stripped before
	// forwarding (ADR-0001).
	if p, ok := in["plugins"]; ok {
		req.Plugins = model.ParsePlugins(p)
	}

	var inMsgs []anthropicInMessage
	if m, ok := in["messages"]; ok {
		_ = json.Unmarshal(m, &inMsgs)
	}

	var systemText string
	if sys, ok := in["system"]; ok {
		systemText = anthropicSystemText(sys)
	}

	// Build only the OpenAI-canonical messages array (the single source of truth in
	// Raw). The Anthropic top-level system field is hoisted into a leading
	// system-role message; the parsed model.Message view, when a downstream path
	// needs it, is derived lazily from this via ChatRequest.CanonicalMessages — so
	// there is no per-message marshal->unmarshal round-trip here (ADR-0014, ADR-0016).
	oaiMsgs := make([]json.RawMessage, 0, len(inMsgs)+1)
	if systemText != "" {
		raw, _ := json.Marshal(map[string]any{"role": "system", "content": systemText})
		oaiMsgs = append(oaiMsgs, raw)
	}
	for _, m := range inMsgs {
		content := translateAnthropicContent(m.Content)
		raw, _ := json.Marshal(map[string]any{"role": m.Role, "content": content})
		oaiMsgs = append(oaiMsgs, raw)
	}

	msgsRaw, _ := json.Marshal(oaiMsgs)
	out["messages"] = msgsRaw
	req.Raw = out
	return req, nil
}

// anthropicSystemText flattens an Anthropic system field (a string or an array of
// text blocks) to plain text for use as an OpenAI system message.
func anthropicSystemText(raw json.RawMessage) string {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return ""
	}
	switch t[0] {
	case '"':
		var s string
		_ = json.Unmarshal(t, &s)
		return s
	case '[':
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(t, &blocks); err == nil {
			var sb strings.Builder
			for _, b := range blocks {
				if b.Text == "" {
					continue
				}
				if sb.Len() > 0 {
					sb.WriteByte('\n')
				}
				sb.WriteString(b.Text)
			}
			return sb.String()
		}
	}
	return ""
}

// translateAnthropicContent maps Anthropic message content (string or content
// blocks) to the OpenAI content form (string or content parts). Unknown block
// types are passed through best-effort rather than dropped (ADR-0008, ADR-0016).
func translateAnthropicContent(raw json.RawMessage) any {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return ""
	}
	switch t[0] {
	case '"':
		var s string
		if err := json.Unmarshal(t, &s); err == nil {
			return s
		}
		return ""
	case '[':
		var blocks []map[string]json.RawMessage
		if err := json.Unmarshal(t, &blocks); err != nil {
			return ""
		}
		parts := make([]any, 0, len(blocks))
		for _, b := range blocks {
			parts = append(parts, translateAnthropicBlock(b))
		}
		return parts
	default:
		var s string
		if err := json.Unmarshal(t, &s); err == nil {
			return s
		}
		return ""
	}
}

// translateAnthropicBlock maps one Anthropic content block to an OpenAI content
// part. text -> text; image (base64/url source) -> image_url; anything else is
// reproduced as a generic object so nothing is silently dropped.
func translateAnthropicBlock(b map[string]json.RawMessage) any {
	var typ string
	if t, ok := b["type"]; ok {
		_ = json.Unmarshal(t, &typ)
	}
	switch typ {
	case "text":
		var text string
		if t, ok := b["text"]; ok {
			_ = json.Unmarshal(t, &text)
		}
		return map[string]any{"type": "text", "text": text}
	case "image":
		if src, ok := b["source"]; ok {
			var s struct {
				Type      string `json:"type"`
				MediaType string `json:"media_type"`
				Data      string `json:"data"`
				URL       string `json:"url"`
			}
			if err := json.Unmarshal(src, &s); err == nil {
				url := s.URL
				if s.Type == "base64" && s.Data != "" {
					url = "data:" + s.MediaType + ";base64," + s.Data
				}
				return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
			}
		}
		return map[string]any{"type": "text", "text": ""}
	default:
		generic := make(map[string]any, len(b))
		for k, v := range b {
			var anyv any
			_ = json.Unmarshal(v, &anyv)
			generic[k] = anyv
		}
		return generic
	}
}

// ----- response sink: OpenAI-canonical output -> Anthropic Messages shape ----

// anthropicSink translates the OpenAI-canonical output the router produces into
// the Anthropic Messages response shape (ADR-0016), for both unary and SSE.
// Streaming translation is stateful within a single stream (ADR-0007): it tracks
// whether the message has been opened and what stop reason/usage to report.
type anthropicSink struct {
	w     http.ResponseWriter
	rc    *http.ResponseController
	wrote bool
	extra map[string]string

	started bool
	msgID   string
	model   string
	stop    string
	out     int
	in      int
}

// newAnthropicSink builds a translating sink over w.
func newAnthropicSink(w http.ResponseWriter) *anthropicSink {
	return &anthropicSink{w: w, rc: http.NewResponseController(w)}
}

// Wrote reports whether any byte has reached the client yet.
func (s *anthropicSink) Wrote() bool { return s.wrote }

// SetHeader buffers a router-set response header (e.g. X-Router-Model) to be
// emitted when the response is committed. Single-goroutine per request, so no
// synchronization is needed (ADR-0015).
func (s *anthropicSink) SetHeader(key, value string) {
	if s.extra == nil {
		s.extra = make(map[string]string, 2)
	}
	s.extra[key] = value
}

// WriteResponse translates a complete OpenAI chat completion (or error) into the
// Anthropic unary response shape (ADR-0016).
func (s *anthropicSink) WriteResponse(status int, header http.Header, body []byte) error {
	s.w.Header().Set("Content-Type", "application/json")
	applyExtraHeaders(s.w.Header(), s.extra)
	s.w.WriteHeader(status)
	s.wrote = true
	if status >= 400 {
		_, err := s.w.Write(anthropicErrorBody(body))
		return err
	}
	_, err := s.w.Write(translateCompletionToAnthropic(body))
	return err
}

// WriteRawResponse relays a complete upstream response verbatim — no translation —
// for the same-protocol native relay path (Anthropic->Anthropic full passthrough,
// ADR-0016/ADR-0001). The reply is already Anthropic-shaped, so the router's
// double-translation is bypassed and the upstream status/Content-Type/body are
// carried through unchanged, preserving tools, tool_choice, top_k, metadata, and
// cache_control. It is part of the router.ResponseSink raw-relay surface.
func (s *anthropicSink) WriteRawResponse(status int, header http.Header, body []byte) error {
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

// StartRawStream commits the SSE response for the native relay path (ADR-0007).
// Unlike StartStream it does not arm the translating stream machinery: the
// upstream's own Anthropic event stream is copied through verbatim by WriteRawChunk.
func (s *anthropicSink) StartRawStream() error {
	setStreamHeaders(s.w.Header())
	applyExtraHeaders(s.w.Header(), s.extra)
	s.w.WriteHeader(http.StatusOK)
	s.wrote = true
	_ = s.rc.Flush()
	return nil
}

// WriteRawChunk relays raw upstream SSE bytes to the consumer verbatim and flushes
// (no reframing, no translation), preserving the Anthropic "event:"/"data:" framing
// the consumer's SDK expects on the same-protocol native path (ADR-0007/ADR-0016).
func (s *anthropicSink) WriteRawChunk(p []byte) error {
	s.wrote = true
	if _, err := s.w.Write(p); err != nil {
		return err
	}
	_ = s.rc.Flush()
	return nil
}

// StartStream writes the SSE response headers and flushes (ADR-0007). Anthropic
// stream events are emitted lazily once the first chunk arrives, since the model
// id is only known then.
func (s *anthropicSink) StartStream() error {
	setStreamHeaders(s.w.Header())
	applyExtraHeaders(s.w.Header(), s.extra)
	s.w.WriteHeader(http.StatusOK)
	s.wrote = true
	_ = s.rc.Flush()
	return nil
}

// WriteEvent translates one OpenAI streaming chunk into Anthropic stream events
// (ADR-0007, ADR-0016). The OpenAI [DONE] sentinel is skipped; EndStream emits
// the Anthropic terminators.
func (s *anthropicSink) WriteEvent(data []byte) error {
	if isDone(data) {
		return nil
	}
	var chunk oaiChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil // ignore an unparseable chunk best-effort
	}
	if chunk.Model != "" {
		s.model = chunk.Model
	}
	if !s.started {
		if err := s.emitStart(); err != nil {
			return err
		}
	}
	if len(chunk.Choices) > 0 {
		ch := chunk.Choices[0]
		if text := contentText(ch.Delta.Content); text != "" {
			if err := s.emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]any{"type": "text_delta", "text": text},
			}); err != nil {
				return err
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			s.stop = openAIFinishToAnthropicStop(*ch.FinishReason)
		}
	}
	if chunk.Usage != nil {
		if chunk.Usage.CompletionTokens > 0 {
			s.out = chunk.Usage.CompletionTokens
		}
		if chunk.Usage.PromptTokens > 0 {
			s.in = chunk.Usage.PromptTokens
		}
	}
	return nil
}

// EndStream emits the Anthropic stream terminators (ADR-0007).
func (s *anthropicSink) EndStream() error {
	if !s.started {
		if err := s.emitStart(); err != nil {
			return err
		}
	}
	if err := s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}); err != nil {
		return err
	}
	stop := s.stop
	if stop == "" {
		stop = "end_turn"
	}
	if err := s.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": s.out},
	}); err != nil {
		return err
	}
	return s.emit("message_stop", map[string]any{"type": "message_stop"})
}

// emitStart opens the Anthropic message: message_start + content_block_start.
func (s *anthropicSink) emitStart() error {
	s.started = true
	if s.msgID == "" {
		s.msgID = "msg_" + observability.NewRequestID()
	}
	if err := s.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            s.msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         s.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": s.in, "output_tokens": 0},
		},
	}); err != nil {
		return err
	}
	return s.emit("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
}

// emit writes one Anthropic SSE event ("event:" + "data:") and flushes.
func (s *anthropicSink) emit(event string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	s.wrote = true
	if _, err := io.WriteString(s.w, "event: "+event+"\n"); err != nil {
		return err
	}
	if _, err := s.w.Write([]byte("data: ")); err != nil {
		return err
	}
	if _, err := s.w.Write(b); err != nil {
		return err
	}
	if _, err := s.w.Write([]byte("\n\n")); err != nil {
		return err
	}
	_ = s.rc.Flush()
	return nil
}

// ----- OpenAI response shapes the translation reads -------------------------

type oaiCompletion struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

type oaiChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content json.RawMessage `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// translateCompletionToAnthropic converts a unary OpenAI completion into the
// Anthropic Messages response shape (best-effort, ADR-0016).
func translateCompletionToAnthropic(body []byte) []byte {
	var comp oaiCompletion
	_ = json.Unmarshal(body, &comp)

	id := comp.ID
	if id == "" {
		id = "msg_" + observability.NewRequestID()
	} else {
		id = "msg_" + strings.TrimPrefix(id, "chatcmpl-")
	}

	text := ""
	stop := "end_turn"
	if len(comp.Choices) > 0 {
		text = contentText(comp.Choices[0].Message.Content)
		stop = openAIFinishToAnthropicStop(comp.Choices[0].FinishReason)
	}

	out, err := json.Marshal(map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         comp.Model,
		"content":       []any{map[string]any{"type": "text", "text": text}},
		"stop_reason":   stop,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  comp.Usage.PromptTokens,
			"output_tokens": comp.Usage.CompletionTokens,
		},
	})
	if err != nil {
		return body
	}
	return out
}

// anthropicErrorBody wraps an upstream OpenAI-style error into the Anthropic
// error envelope (best-effort, ADR-0016).
func anthropicErrorBody(body []byte) []byte {
	msg := "request failed"
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err == nil && e.Error.Message != "" {
		msg = e.Error.Message
	}
	out, err := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "api_error", "message": msg},
	})
	if err != nil {
		return body
	}
	return out
}

// contentText extracts a plain-text view of an OpenAI content value (string or
// parts), never assuming it is a string (ADR-0008).
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var c model.Content
	if err := json.Unmarshal(raw, &c); err != nil {
		return ""
	}
	return c.Text()
}

// openAIFinishToAnthropicStop maps an OpenAI finish_reason to the Anthropic
// stop_reason.
func openAIFinishToAnthropicStop(finish string) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		// "stop", "content_filter", "", and anything unknown collapse to the
		// natural end (best-effort, ADR-0016).
		return "end_turn"
	}
}
