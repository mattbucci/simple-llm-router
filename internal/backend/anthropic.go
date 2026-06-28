package backend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mattbucci/simple-llm-router/internal/model"
)

// chatAnthropic is the Anthropic provider outbound adapter (ADR-0016). It
// translates the OpenAI-canonical body into an Anthropic /v1/messages request,
// POSTs it to {baseURL}/messages with the x-api-key + anthropic-version headers
// (ADR-0009), and translates the Anthropic reply — unary or streaming SSE — back
// into OpenAI shape so the router and sink only ever see OpenAI. Translation is
// best-effort; provider-specific extras may be dropped (ADR-0016).
func (c *Client) chatAnthropic(ctx context.Context, body []byte, stream bool) (*model.UpstreamResponse, error) {
	outBody, err := openAIToAnthropicRequest(body)
	if err != nil {
		return nil, fmt.Errorf("backend %q: translate request: %w", c.name, err)
	}

	reqCtx, cancel := c.requestContext(ctx, stream)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.url("/messages"), bytes.NewReader(outBody))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("backend %q: build request: %w", c.name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	c.injectAuth(req)

	cleanup, disarm := c.acquire(cancel)
	defer cleanup()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("backend %q: request: %w", c.name, err)
	}

	// On a non-2xx status the response carries the upstream's status (which the
	// proxy inspects for pre-first-byte failover, ADR-0006); relay its body as-is
	// rather than attempt to translate an error document (best-effort, ADR-0016).
	if resp.StatusCode != http.StatusOK {
		disarm() // ownership of the in-flight slot + cancel transfers to the guard
		return &model.UpstreamResponse{
			Status: resp.StatusCode,
			Header: resp.Header,
			Body:   c.guard(resp.Body, false, cancel),
		}, nil
	}

	if stream {
		header := http.Header{}
		header.Set("Content-Type", "text/event-stream")
		header.Set("Cache-Control", "no-cache")
		sr := &anthropicStreamReader{
			src:      bufio.NewReader(resp.Body),
			upstream: resp.Body,
			cancel:   cancel,
			done:     func() { c.inFlight.Add(-1) },
			created:  time.Now().Unix(),
			model:    "anthropic",
		}
		if c.timeouts.Idle > 0 {
			sr.timer = newIdleTimer(c.timeouts.Idle, cancel)
		}
		disarm() // ownership of the in-flight slot + cancel transfers to the reader
		return &model.UpstreamResponse{Status: http.StatusOK, Header: header, Body: sr}, nil
	}

	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("backend %q: read response: %w", c.name, err)
	}
	oai, err := anthropicToOpenAIResponse(raw)
	if err != nil {
		return nil, fmt.Errorf("backend %q: translate response: %w", c.name, err)
	}
	disarm() // ownership of the in-flight slot + cancel transfers to the guard
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	return &model.UpstreamResponse{
		Status: http.StatusOK,
		Header: header,
		Body:   c.guard(io.NopCloser(bytes.NewReader(oai)), false, cancel),
	}, nil
}

// openAIToAnthropicRequest translates an OpenAI Chat Completions body into an
// Anthropic /v1/messages body. The model field has already been rewritten to the
// upstream id by the router (ADR-0001) and is passed through verbatim. system
// messages are hoisted into Anthropic's top-level system field; max_tokens is
// required by Anthropic so a default is supplied when absent.
func openAIToAnthropicRequest(body []byte) ([]byte, error) {
	var in map[string]json.RawMessage
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("decode openai body: %w", err)
	}

	out := map[string]any{}
	// Pass numeric/boolean/string knobs through as raw JSON so nothing is lost.
	if v, ok := in["model"]; ok {
		out["model"] = json.RawMessage(v)
	}
	if v, ok := in["stream"]; ok {
		out["stream"] = json.RawMessage(v)
	}
	for _, k := range []string{"temperature", "top_p"} {
		if v, ok := in[k]; ok {
			out[k] = json.RawMessage(v)
		}
	}

	// Anthropic requires max_tokens. Honor max_completion_tokens then max_tokens.
	maxTok := 0
	if v, ok := in["max_completion_tokens"]; ok {
		_ = json.Unmarshal(v, &maxTok)
	}
	if maxTok == 0 {
		if v, ok := in["max_tokens"]; ok {
			_ = json.Unmarshal(v, &maxTok)
		}
	}
	if maxTok <= 0 {
		maxTok = 4096
	}
	out["max_tokens"] = maxTok

	// OpenAI stop (string or []string) -> Anthropic stop_sequences ([]string).
	if v, ok := in["stop"]; ok {
		var one string
		var many []string
		if json.Unmarshal(v, &one) == nil && one != "" {
			out["stop_sequences"] = []string{one}
		} else if json.Unmarshal(v, &many) == nil && len(many) > 0 {
			out["stop_sequences"] = many
		}
	}

	var msgs []model.Message
	if v, ok := in["messages"]; ok {
		_ = json.Unmarshal(v, &msgs)
	}
	var system strings.Builder
	amsgs := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "system" {
			if system.Len() > 0 {
				system.WriteString("\n\n")
			}
			system.WriteString(m.Content.Text())
			continue
		}
		amsgs = append(amsgs, map[string]any{
			"role":    m.Role,
			"content": anthropicContent(m.Content),
		})
	}
	if system.Len() > 0 {
		out["system"] = system.String()
	}
	out["messages"] = amsgs

	return json.Marshal(out)
}

// anthropicContent maps canonical message content to an Anthropic content value:
// a plain string passes through; an array becomes Anthropic content blocks. Text
// and image_url parts are translated; unknown parts are dropped (best-effort,
// ADR-0008, ADR-0016).
func anthropicContent(c model.Content) any {
	if c.Str != nil {
		return *c.Str
	}
	blocks := make([]any, 0, len(c.Parts))
	for _, raw := range c.Parts {
		var part struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL struct {
				URL string `json:"url"`
			} `json:"image_url"`
		}
		if json.Unmarshal(raw, &part) != nil {
			continue
		}
		switch part.Type {
		case "text", "":
			if part.Text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": part.Text})
			}
		case "image_url":
			url := part.ImageURL.URL
			if strings.HasPrefix(url, "data:") {
				if mediaType, data, ok := parseDataURL(url); ok {
					blocks = append(blocks, map[string]any{
						"type":   "image",
						"source": map[string]any{"type": "base64", "media_type": mediaType, "data": data},
					})
				}
			} else if url != "" {
				blocks = append(blocks, map[string]any{
					"type":   "image",
					"source": map[string]any{"type": "url", "url": url},
				})
			}
		}
	}
	if len(blocks) == 0 {
		return ""
	}
	return blocks
}

// parseDataURL splits a data: URL into its media type and (base64) payload.
func parseDataURL(s string) (mediaType, data string, ok bool) {
	if !strings.HasPrefix(s, "data:") {
		return "", "", false
	}
	rest := s[len("data:"):]
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return "", "", false
	}
	meta := strings.TrimSuffix(rest[:comma], ";base64")
	if meta == "" {
		meta = "application/octet-stream"
	}
	return meta, rest[comma+1:], true
}

// anthropicToOpenAIResponse translates a unary Anthropic Messages response into
// an OpenAI Chat Completions object (best-effort, ADR-0016).
func anthropicToOpenAIResponse(body []byte) ([]byte, error) {
	var ar struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &ar); err != nil {
		return nil, fmt.Errorf("decode anthropic response: %w", err)
	}

	var text, reasoning strings.Builder
	for _, blk := range ar.Content {
		switch blk.Type {
		case "text":
			text.WriteString(blk.Text)
		case "thinking":
			reasoning.WriteString(blk.Thinking)
		}
	}

	message := map[string]any{"role": "assistant", "content": text.String()}
	if reasoning.Len() > 0 {
		// Preserve reasoning under the non-standard field the fleet uses (ADR-0001).
		message["reasoning_content"] = reasoning.String()
	}

	out := map[string]any{
		"id":      ar.ID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   ar.Model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": anthropicStopToOpenAIFinish(ar.StopReason),
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     ar.Usage.InputTokens,
			"completion_tokens": ar.Usage.OutputTokens,
			"total_tokens":      ar.Usage.InputTokens + ar.Usage.OutputTokens,
		},
	}
	return json.Marshal(out)
}

// anthropicStopToOpenAIFinish maps an Anthropic stop_reason to an OpenAI
// finish_reason. An empty reason yields JSON null.
func anthropicStopToOpenAIFinish(reason string) any {
	switch reason {
	case "":
		return nil
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default: // end_turn, stop_sequence, and anything else
		return "stop"
	}
}

// anthropicStreamReader translates an Anthropic SSE event stream into an
// OpenAI Chat Completions chunk stream on the fly, exposing the result as an
// io.ReadCloser. It pulls one Anthropic event at a time and emits the equivalent
// OpenAI "data: {chunk}\n\n" records, finishing with "data: [DONE]\n\n" so the
// proxy treats it identically to a native OpenAI stream (ADR-0007, ADR-0016).
// It uses no goroutine and no lock: translation happens synchronously inside
// Read (ADR-0015).
type anthropicStreamReader struct {
	src      *bufio.Reader
	upstream io.ReadCloser
	timer    *idleTimer
	cancel   context.CancelFunc
	done     func()
	once     sync.Once

	out      bytes.Buffer
	id       string
	model    string
	created  int64
	finished bool // the [DONE] terminator has been queued
}

// Read drains queued OpenAI bytes, pumping and translating further Anthropic
// events as needed until the stream terminates.
func (r *anthropicStreamReader) Read(p []byte) (int, error) {
	for r.out.Len() == 0 && !r.finished {
		if err := r.pump(); err != nil {
			// Upstream ended or aborted: queue the terminator once.
			r.emitDone()
		}
	}
	if r.out.Len() == 0 {
		return 0, io.EOF
	}
	return r.out.Read(p)
}

// pump reads one Anthropic SSE event (lines up to the blank separator) and
// translates it into queued OpenAI bytes. It returns a non-nil error when the
// upstream stream ends or errors.
func (r *anthropicStreamReader) pump() error {
	var data []byte
	for {
		line, err := r.readLine()
		if err != nil {
			return err
		}
		if line == "" { // end of one SSE event
			if data != nil {
				r.handleEvent(data)
			}
			return nil
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(line[len("data:"):])...)
		}
		// event:, id:, retry:, comments and blank-padding are ignored; the event
		// type is taken from the data payload's own "type" field below.
	}
}

// readLine reads a single line from the upstream stream with the configured idle
// timeout (a stalled read past idle cancels the call context, ADR-0007).
func (r *anthropicStreamReader) readLine() (string, error) {
	if r.timer != nil {
		r.timer.arm()
	}
	line, err := r.src.ReadString('\n')
	if r.timer != nil {
		r.timer.disarm()
	}
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// handleEvent dispatches one Anthropic event payload, queuing the equivalent
// OpenAI chunk(s).
func (r *anthropicStreamReader) handleEvent(data []byte) {
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			ID    string `json:"id"`
			Model string `json:"model"`
		} `json:"message"`
		Delta struct {
			Type       string `json:"type"`
			Text       string `json:"text"`
			Thinking   string `json:"thinking"`
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	switch ev.Type {
	case "message_start":
		if ev.Message.ID != "" {
			r.id = ev.Message.ID
		}
		if ev.Message.Model != "" {
			r.model = ev.Message.Model
		}
		r.chunk(map[string]any{"role": "assistant"}, nil)
	case "content_block_delta":
		switch ev.Delta.Type {
		case "text_delta":
			if ev.Delta.Text != "" {
				r.chunk(map[string]any{"content": ev.Delta.Text}, nil)
			}
		case "thinking_delta":
			if ev.Delta.Thinking != "" {
				r.chunk(map[string]any{"reasoning_content": ev.Delta.Thinking}, nil)
			}
		}
	case "message_delta":
		if ev.Delta.StopReason != "" {
			r.chunk(map[string]any{}, anthropicStopToOpenAIFinish(ev.Delta.StopReason))
		}
	case "message_stop":
		r.emitDone()
	}
}

// chunk queues one OpenAI chat.completion.chunk SSE record. finish is nil for an
// in-progress chunk or the mapped finish_reason string for the terminal one.
func (r *anthropicStreamReader) chunk(delta map[string]any, finish any) {
	obj := map[string]any{
		"id":      r.id,
		"object":  "chat.completion.chunk",
		"created": r.created,
		"model":   r.model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"delta":         delta,
				"finish_reason": finish,
			},
		},
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return
	}
	r.out.WriteString("data: ")
	r.out.Write(b)
	r.out.WriteString("\n\n")
}

// emitDone queues the OpenAI stream terminator exactly once.
func (r *anthropicStreamReader) emitDone() {
	if r.finished {
		return
	}
	r.out.WriteString("data: [DONE]\n\n")
	r.finished = true
}

// Close closes the upstream body and runs one-shot cleanup (cancel the call
// context, decrement the in-flight gauge).
func (r *anthropicStreamReader) Close() error {
	err := r.upstream.Close()
	r.once.Do(func() {
		if r.cancel != nil {
			r.cancel()
		}
		if r.done != nil {
			r.done()
		}
	})
	return err
}
