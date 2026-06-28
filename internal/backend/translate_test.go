package backend

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mattbucci/simple-llm-router/internal/model"
)

// TestOpenAIToAnthropicRequest covers the outbound request transforms (ADR-0016):
// system hoisting, OpenAI stop -> Anthropic stop_sequences, the required
// max_tokens default, and multimodal content-block mapping (ADR-0008). It is
// table-driven per ADR-0012: each case feeds one OpenAI-canonical body through
// openAIToAnthropicRequest and asserts the Anthropic /v1/messages shape.
func TestOpenAIToAnthropicRequest(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// wantSystem is the expected top-level system value; nil means the system
		// key must be absent.
		wantSystem any
		// wantMaxTokens is asserted on every case (Anthropic always requires it).
		wantMaxTokens float64
		// wantStop is the expected stop_sequences; nil means the key must be absent.
		wantStop []string
		// wantMsgCount is the expected number of (non-system) messages.
		wantMsgCount int
		// check runs extra structural assertions (used for multimodal blocks).
		check func(t *testing.T, got map[string]any)
	}{
		{
			name: "system hoisted, stop string, multimodal blocks mapped",
			in: `{
				"model":"claude-3",
				"temperature":0.5,
				"stop":"END",
				"max_tokens":256,
				"messages":[
					{"role":"system","content":"be brief"},
					{"role":"user","content":[
						{"type":"text","text":"describe"},
						{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}
					]}
				]
			}`,
			wantSystem:    "be brief",
			wantMaxTokens: 256,
			wantStop:      []string{"END"},
			wantMsgCount:  1,
			check: func(t *testing.T, got map[string]any) {
				blocks := userContentBlocks(t, got)
				if len(blocks) != 2 {
					t.Fatalf("content blocks = %v, want 2", blocks)
				}
				text := blocks[0].(map[string]any)
				if text["type"] != "text" || text["text"] != "describe" {
					t.Fatalf("text block = %v", text)
				}
				img := blocks[1].(map[string]any)
				if img["type"] != "image" {
					t.Fatalf("image block type = %v, want image", img["type"])
				}
				src := img["source"].(map[string]any)
				if src["type"] != "base64" || src["media_type"] != "image/png" || src["data"] != "QUJD" {
					t.Fatalf("image source = %v", src)
				}
			},
		},
		{
			name:          "max_tokens defaulted when absent",
			in:            `{"model":"c","messages":[{"role":"user","content":"hi"}]}`,
			wantSystem:    nil,
			wantMaxTokens: 4096,
			wantStop:      nil,
			wantMsgCount:  1,
		},
		{
			name:          "max_completion_tokens preferred over max_tokens",
			in:            `{"model":"c","max_completion_tokens":42,"max_tokens":7,"messages":[{"role":"user","content":"hi"}]}`,
			wantMaxTokens: 42,
			wantMsgCount:  1,
		},
		{
			name:          "stop array maps to stop_sequences",
			in:            `{"model":"c","max_tokens":10,"stop":["A","B"],"messages":[{"role":"user","content":"hi"}]}`,
			wantMaxTokens: 10,
			wantStop:      []string{"A", "B"},
			wantMsgCount:  1,
		},
		{
			name:          "multiple system messages joined and hoisted",
			in:            `{"model":"c","max_tokens":5,"messages":[{"role":"system","content":"a"},{"role":"system","content":"b"},{"role":"user","content":"hi"}]}`,
			wantSystem:    "a\n\nb",
			wantMaxTokens: 5,
			wantMsgCount:  1,
		},
		{
			name:          "http image url maps to a url source",
			in:            `{"model":"c","max_tokens":5,"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.test/y.png"}}]}]}`,
			wantMaxTokens: 5,
			wantMsgCount:  1,
			check: func(t *testing.T, got map[string]any) {
				blocks := userContentBlocks(t, got)
				if len(blocks) != 1 {
					t.Fatalf("content blocks = %v, want 1", blocks)
				}
				img := blocks[0].(map[string]any)
				src := img["source"].(map[string]any)
				if src["type"] != "url" || src["url"] != "https://example.test/y.png" {
					t.Fatalf("image source = %v, want url source", src)
				}
			},
		},
		{
			name:          "unknown multimodal part is dropped, text survives",
			in:            `{"model":"c","max_tokens":5,"messages":[{"role":"user","content":[{"type":"text","text":"keep"},{"type":"video","video":{"url":"x"}}]}]}`,
			wantMaxTokens: 5,
			wantMsgCount:  1,
			check: func(t *testing.T, got map[string]any) {
				blocks := userContentBlocks(t, got)
				if len(blocks) != 1 {
					t.Fatalf("content blocks = %v, want 1 (unknown part dropped)", blocks)
				}
				text := blocks[0].(map[string]any)
				if text["type"] != "text" || text["text"] != "keep" {
					t.Fatalf("text block = %v", text)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := openAIToAnthropicRequest([]byte(tc.in))
			if err != nil {
				t.Fatalf("openAIToAnthropicRequest: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			if tc.wantSystem == nil {
				if v, ok := got["system"]; ok {
					t.Fatalf("system = %v, want absent", v)
				}
			} else if got["system"] != tc.wantSystem {
				t.Fatalf("system = %v, want %v", got["system"], tc.wantSystem)
			}

			if mt, _ := got["max_tokens"].(float64); mt != tc.wantMaxTokens {
				t.Fatalf("max_tokens = %v, want %v", got["max_tokens"], tc.wantMaxTokens)
			}

			if tc.wantStop == nil {
				if v, ok := got["stop_sequences"]; ok {
					t.Fatalf("stop_sequences = %v, want absent", v)
				}
			} else {
				ss, ok := got["stop_sequences"].([]any)
				if !ok || len(ss) != len(tc.wantStop) {
					t.Fatalf("stop_sequences = %v, want %v", got["stop_sequences"], tc.wantStop)
				}
				for i, want := range tc.wantStop {
					if ss[i] != want {
						t.Fatalf("stop_sequences[%d] = %v, want %q", i, ss[i], want)
					}
				}
			}

			msgs, _ := got["messages"].([]any)
			if len(msgs) != tc.wantMsgCount {
				t.Fatalf("messages = %v, want %d (system hoisted out)", got["messages"], tc.wantMsgCount)
			}

			if tc.check != nil {
				tc.check(t, got)
			}
		})
	}
}

// userContentBlocks returns the content blocks of the first message in an
// Anthropic-shaped body, failing the test if the content is not an array.
func userContentBlocks(t *testing.T, got map[string]any) []any {
	t.Helper()
	msgs, ok := got["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("messages = %v, want at least one", got["messages"])
	}
	msg := msgs[0].(map[string]any)
	blocks, ok := msg["content"].([]any)
	if !ok {
		t.Fatalf("content = %v, want an array of blocks", msg["content"])
	}
	return blocks
}

// TestAnthropicToOpenAIResponse covers the unary response transforms (ADR-0016):
// thinking blocks mapped to the non-standard reasoning_content field (ADR-0001),
// text concatenation, stop_reason -> finish_reason mapping, and usage accounting.
func TestAnthropicToOpenAIResponse(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// wantReasoning is "" when reasoning_content must be absent.
		wantContent    string
		wantReasoning  string
		wantFinish     any
		wantPrompt     int
		wantCompletion int
	}{
		{
			name:           "thinking -> reasoning_content, end_turn -> stop",
			in:             `{"id":"msg_1","model":"claude-3","content":[{"type":"thinking","thinking":"let me reason"},{"type":"text","text":"the answer"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":4}}`,
			wantContent:    "the answer",
			wantReasoning:  "let me reason",
			wantFinish:     "stop",
			wantPrompt:     7,
			wantCompletion: 4,
		},
		{
			name:           "max_tokens -> length, no thinking leaves reasoning absent",
			in:             `{"id":"m","model":"c","content":[{"type":"text","text":"hi"}],"stop_reason":"max_tokens","usage":{"input_tokens":1,"output_tokens":2}}`,
			wantContent:    "hi",
			wantReasoning:  "",
			wantFinish:     "length",
			wantPrompt:     1,
			wantCompletion: 2,
		},
		{
			name:        "tool_use -> tool_calls",
			in:          `{"id":"m","model":"c","content":[{"type":"text","text":"x"}],"stop_reason":"tool_use","usage":{"input_tokens":0,"output_tokens":0}}`,
			wantContent: "x",
			wantFinish:  "tool_calls",
		},
		{
			name:        "empty stop_reason -> null finish_reason",
			in:          `{"id":"m","model":"c","content":[{"type":"text","text":"x"}],"usage":{"input_tokens":0,"output_tokens":0}}`,
			wantContent: "x",
			wantFinish:  nil,
		},
		{
			name:        "multiple text blocks concatenated, stop_sequence -> stop",
			in:          `{"id":"m","model":"c","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}],"stop_reason":"stop_sequence","usage":{"input_tokens":0,"output_tokens":0}}`,
			wantContent: "ab",
			wantFinish:  "stop",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := anthropicToOpenAIResponse([]byte(tc.in))
			if err != nil {
				t.Fatalf("anthropicToOpenAIResponse: %v", err)
			}
			var got struct {
				Object  string `json:"object"`
				Choices []struct {
					Message struct {
						Content          string  `json:"content"`
						ReasoningContent *string `json:"reasoning_content"`
					} `json:"message"`
					FinishReason any `json:"finish_reason"`
				} `json:"choices"`
				Usage struct {
					PromptTokens     int `json:"prompt_tokens"`
					CompletionTokens int `json:"completion_tokens"`
					TotalTokens      int `json:"total_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			if got.Object != "chat.completion" {
				t.Fatalf("object = %q, want chat.completion", got.Object)
			}
			if len(got.Choices) != 1 {
				t.Fatalf("choices = %d, want 1", len(got.Choices))
			}
			ch := got.Choices[0]
			if ch.Message.Content != tc.wantContent {
				t.Fatalf("content = %q, want %q", ch.Message.Content, tc.wantContent)
			}
			if tc.wantReasoning == "" {
				if ch.Message.ReasoningContent != nil {
					t.Fatalf("reasoning_content = %q, want absent", *ch.Message.ReasoningContent)
				}
			} else if ch.Message.ReasoningContent == nil || *ch.Message.ReasoningContent != tc.wantReasoning {
				t.Fatalf("reasoning_content = %v, want %q", ch.Message.ReasoningContent, tc.wantReasoning)
			}
			if ch.FinishReason != tc.wantFinish {
				t.Fatalf("finish_reason = %v, want %v", ch.FinishReason, tc.wantFinish)
			}
			if got.Usage.PromptTokens != tc.wantPrompt || got.Usage.CompletionTokens != tc.wantCompletion {
				t.Fatalf("usage = %+v, want prompt=%d completion=%d", got.Usage, tc.wantPrompt, tc.wantCompletion)
			}
			if want := tc.wantPrompt + tc.wantCompletion; got.Usage.TotalTokens != want {
				t.Fatalf("total_tokens = %d, want %d", got.Usage.TotalTokens, want)
			}
		})
	}
}

// TestAnthropicStopToOpenAIFinish exhaustively covers the finish_reason <->
// stop_reason mapping cells (ADR-0016) used by both the unary and streaming
// translators.
func TestAnthropicStopToOpenAIFinish(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want any
	}{
		{"empty -> null", "", nil},
		{"max_tokens -> length", "max_tokens", "length"},
		{"tool_use -> tool_calls", "tool_use", "tool_calls"},
		{"end_turn -> stop", "end_turn", "stop"},
		{"stop_sequence -> stop", "stop_sequence", "stop"},
		{"unknown -> stop", "something_else", "stop"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := anthropicStopToOpenAIFinish(tc.in); got != tc.want {
				t.Fatalf("anthropicStopToOpenAIFinish(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// capturedReq records what an httptest upstream received, handed back over a
// buffered channel to establish a happens-before edge for the race detector.
type capturedReq struct {
	path     string
	auth     string
	xAPIKey  string
	aVersion string
	body     []byte
}

// TestClientChatOpenAIPassthrough covers ADR-0001/0009: an OpenAI provider gets
// the body forwarded verbatim (reasoning_content survives), the operator's bearer
// credential is injected, and no Anthropic header is sent.
func TestClientChatOpenAIPassthrough(t *testing.T) {
	respBody := `{"id":"x","choices":[{"message":{"content":"","reasoning_content":"because"}}],"metadata":{"weight_version":"default"}}`
	got := make(chan capturedReq, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- capturedReq{
			path:    r.URL.Path,
			auth:    r.Header.Get("Authorization"),
			xAPIKey: r.Header.Get("X-Api-Key"),
			body:    b,
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, respBody)
	}))
	defer srv.Close()

	c := NewClient("be", srv.URL+"/v1", model.ProtocolOpenAI, "operator-secret", "", ClientTimeouts{})
	reqBody := `{"model":"gpt","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`
	resp, err := c.Chat(context.Background(), []byte(reqBody), false)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	if string(out) != respBody {
		t.Fatalf("response not verbatim:\n got %s\nwant %s", out, respBody)
	}

	g := <-got
	if g.path != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions", g.path)
	}
	if g.auth != "Bearer operator-secret" {
		t.Fatalf("Authorization = %q, want Bearer operator-secret", g.auth)
	}
	if g.xAPIKey != "" {
		t.Fatalf("x-api-key = %q, want empty for an openai provider", g.xAPIKey)
	}
	if string(g.body) != reqBody {
		t.Fatalf("forwarded body not verbatim:\n got %s\nwant %s", g.body, reqBody)
	}
}

// TestClientChatAnthropicUnary covers ADR-0016/0009: an Anthropic provider
// receives a translated /v1/messages request with x-api-key + anthropic-version,
// and the reply is translated back to OpenAI shape for the router.
func TestClientChatAnthropicUnary(t *testing.T) {
	anthropicResp := `{"id":"msg_9","model":"claude-3","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
	got := make(chan capturedReq, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- capturedReq{
			path:     r.URL.Path,
			auth:     r.Header.Get("Authorization"),
			xAPIKey:  r.Header.Get("X-Api-Key"),
			aVersion: r.Header.Get("Anthropic-Version"),
			body:     b,
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, anthropicResp)
	}))
	defer srv.Close()

	c := NewClient("be", srv.URL+"/v1", model.ProtocolAnthropic, "op-key", "2023-06-01", ClientTimeouts{})
	reqBody := `{"model":"claude-3","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}],"max_tokens":100}`
	resp, err := c.Chat(context.Background(), []byte(reqBody), false)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// The router/sink only ever see OpenAI shape.
	var oai struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &oai); err != nil {
		t.Fatalf("response not OpenAI-shaped: %v (%s)", err, out)
	}
	if oai.Object != "chat.completion" || len(oai.Choices) != 1 || oai.Choices[0].Message.Content != "hello" {
		t.Fatalf("translated response = %s", out)
	}

	g := <-got
	if g.path != "/v1/messages" {
		t.Fatalf("path = %q, want /v1/messages", g.path)
	}
	if g.xAPIKey != "op-key" {
		t.Fatalf("x-api-key = %q, want op-key", g.xAPIKey)
	}
	if g.aVersion != "2023-06-01" {
		t.Fatalf("anthropic-version = %q, want 2023-06-01", g.aVersion)
	}
	if g.auth != "" {
		t.Fatalf("Authorization = %q, want empty for an anthropic provider", g.auth)
	}
	// The upstream must have received Anthropic shape: system hoisted out.
	var sent map[string]any
	if err := json.Unmarshal(g.body, &sent); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if sent["system"] != "be brief" {
		t.Fatalf("upstream system = %v, want %q", sent["system"], "be brief")
	}
	if _, ok := sent["max_tokens"]; !ok {
		t.Fatalf("upstream missing max_tokens")
	}
}

// TestClientChatAnthropicStream covers ADR-0007/0016: an Anthropic SSE stream is
// translated on the fly into an OpenAI chunk stream, preserving reasoning via
// thinking_delta -> reasoning_content and terminating with data: [DONE].
func TestClientChatAnthropicStream(t *testing.T) {
	events := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-3"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"ponder"}}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
		"",
	}, "\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, events)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	c := NewClient("be", srv.URL+"/v1", model.ProtocolAnthropic, "k", "2023-06-01", ClientTimeouts{})
	resp, err := c.Chat(context.Background(), []byte(`{"model":"claude-3","stream":true,"messages":[{"role":"user","content":"hi"}]}`), true)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	s := string(out)
	if !strings.Contains(s, `"content":"hi"`) {
		t.Fatalf("translated stream missing text delta:\n%s", s)
	}
	if !strings.Contains(s, `"reasoning_content":"ponder"`) {
		t.Fatalf("translated stream missing reasoning delta:\n%s", s)
	}
	if !strings.Contains(s, "data: [DONE]") {
		t.Fatalf("translated stream missing terminator:\n%s", s)
	}
}

// TestClientChatNative covers ADR-0018/0009: the native same-protocol relay POSTs
// to /messages (NOT /chat/completions) with x-api-key + anthropic-version, forwards
// the request body byte-for-byte (no translation, so provider-specific fields like
// tools/tool_choice/top_k/metadata survive), and relays the provider's unary or SSE
// reply verbatim with its status and Content-Type preserved.
func TestClientChatNative(t *testing.T) {
	cases := []struct {
		name     string
		stream   bool
		reqBody  string
		respBody string
		respCT   string
	}{
		{
			name:     "unary verbatim preserves provider-specific fields",
			stream:   false,
			reqBody:  `{"model":"claude-3","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"t"}],"tool_choice":{"type":"auto"},"top_k":5,"metadata":{"user_id":"u"}}`,
			respBody: `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn"}`,
			respCT:   "application/json",
		},
		{
			name:     "stream verbatim preserves SSE framing",
			stream:   true,
			reqBody:  `{"model":"claude-3","stream":true,"max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`,
			respBody: "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			respCT:   "text/event-stream",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := make(chan capturedReq, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				got <- capturedReq{
					path:     r.URL.Path,
					auth:     r.Header.Get("Authorization"),
					xAPIKey:  r.Header.Get("X-Api-Key"),
					aVersion: r.Header.Get("Anthropic-Version"),
					body:     b,
				}
				w.Header().Set("Content-Type", tc.respCT)
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tc.respBody)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}))
			defer srv.Close()

			c := NewClient("be", srv.URL+"/v1", model.ProtocolAnthropic, "op-key", "2023-06-01", ClientTimeouts{})
			resp, err := c.ChatNative(context.Background(), []byte(tc.reqBody), tc.stream)
			if err != nil {
				t.Fatalf("ChatNative: %v", err)
			}
			out, _ := io.ReadAll(resp.Body)
			if err := resp.Body.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}

			if resp.Status != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.Status)
			}
			if string(out) != tc.respBody {
				t.Fatalf("response not verbatim:\n got %q\nwant %q", out, tc.respBody)
			}
			if ct := resp.Header.Get("Content-Type"); ct != tc.respCT {
				t.Fatalf("content-type = %q, want %q", ct, tc.respCT)
			}

			g := <-got
			if g.path != "/v1/messages" {
				t.Fatalf("path = %q, want /v1/messages (not /v1/chat/completions)", g.path)
			}
			if g.xAPIKey != "op-key" {
				t.Fatalf("x-api-key = %q, want op-key", g.xAPIKey)
			}
			if g.aVersion != "2023-06-01" {
				t.Fatalf("anthropic-version = %q, want 2023-06-01", g.aVersion)
			}
			if g.auth != "" {
				t.Fatalf("Authorization = %q, want empty for an anthropic provider", g.auth)
			}
			if string(g.body) != tc.reqBody {
				t.Fatalf("forwarded body not verbatim (no translation):\n got %q\nwant %q", g.body, tc.reqBody)
			}
			if n := c.InFlight(); n != 0 {
				t.Fatalf("in-flight after close = %d, want 0", n)
			}
		})
	}
}

// TestClientChatStatusRelayed covers ADR-0006/0016: a non-2xx upstream status is
// relayed with its code and body intact (the backend never translates an error
// document) so the proxy can decide failover. It also asserts the in-flight gauge
// returns to zero once the relayed body is closed.
func TestClientChatStatusRelayed(t *testing.T) {
	cases := []struct {
		name     string
		protocol model.Protocol
		status   int
	}{
		{"openai 502", model.ProtocolOpenAI, http.StatusBadGateway},
		{"openai 503", model.ProtocolOpenAI, http.StatusServiceUnavailable},
		{"openai 504", model.ProtocolOpenAI, http.StatusGatewayTimeout},
		{"openai 429", model.ProtocolOpenAI, http.StatusTooManyRequests},
		{"openai 400", model.ProtocolOpenAI, http.StatusBadRequest},
		{"anthropic 502", model.ProtocolAnthropic, http.StatusBadGateway},
		{"anthropic 503", model.ProtocolAnthropic, http.StatusServiceUnavailable},
		{"anthropic 504", model.ProtocolAnthropic, http.StatusGatewayTimeout},
		{"anthropic 429", model.ProtocolAnthropic, http.StatusTooManyRequests},
		{"anthropic 400", model.ProtocolAnthropic, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errBody := `{"error":"upstream"}`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, errBody)
			}))
			defer srv.Close()

			c := NewClient("be", srv.URL+"/v1", tc.protocol, "k", "2023-06-01", ClientTimeouts{})
			resp, err := c.Chat(context.Background(), []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), false)
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			if err := resp.Body.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			if resp.Status != tc.status {
				t.Fatalf("status = %d, want %d", resp.Status, tc.status)
			}
			if string(body) != errBody {
				t.Fatalf("relayed body = %q, want %q (verbatim, no translation)", body, errBody)
			}
			if n := c.InFlight(); n != 0 {
				t.Fatalf("in-flight after close = %d, want 0", n)
			}
		})
	}
}

// captureRT is a fake http.RoundTripper that records the outbound request (so the
// test can inspect its call context) and either returns a canned 200 response or a
// transport error. It enables deterministic, network-free coverage of the
// in-flight-gauge / call-context lifecycle (ADR-0012, ADR-0013, ADR-0015).
type captureRT struct {
	captured chan *http.Request
	body     string
	err      error
}

func (f *captureRT) RoundTrip(r *http.Request) (*http.Response, error) {
	f.captured <- r
	if f.err != nil {
		return nil, f.err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(f.body)),
		Request:    r,
	}, nil
}

// TestClientInFlightNoLeak verifies the in-flight gauge is released and the call
// context is cancelled exactly once on BOTH the success path (when the relayed
// body is closed) and the pre-guard error path (when the upstream Do fails before
// ownership transfers to the body guard) — covering Chat (OpenAI + Anthropic) and
// the native relay, unary and streaming (ADR-0013, ADR-0015). It is white-box
// (package backend) so it can inject a fake RoundTripper and observe the actual
// client-side request context, with no live network.
func TestClientInFlightNoLeak(t *testing.T) {
	const okAnthropic = `{"id":"m","model":"c","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	reqBody := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	cases := []struct {
		name     string
		protocol model.Protocol
		native   bool
		stream   bool
	}{
		{"openai unary", model.ProtocolOpenAI, false, false},
		{"openai stream", model.ProtocolOpenAI, false, true},
		{"anthropic unary", model.ProtocolAnthropic, false, false},
		{"anthropic stream", model.ProtocolAnthropic, false, true},
		{"native unary", model.ProtocolAnthropic, true, false},
		{"native stream", model.ProtocolAnthropic, true, true},
	}

	dispatch := func(c *Client, ctx context.Context, native, stream bool) (*model.UpstreamResponse, error) {
		if native {
			return c.ChatNative(ctx, reqBody, stream)
		}
		return c.Chat(ctx, reqBody, stream)
	}

	for _, tc := range cases {
		t.Run("success/"+tc.name, func(t *testing.T) {
			rt := &captureRT{captured: make(chan *http.Request, 1), body: okAnthropic}
			c := NewClient("be", "http://fake.invalid/v1", tc.protocol, "k", "2023-06-01", ClientTimeouts{})
			c.http = &http.Client{Transport: rt}

			resp, err := dispatch(c, context.Background(), tc.native, tc.stream)
			if err != nil {
				t.Fatalf("dispatch: %v", err)
			}
			if n := c.InFlight(); n != 1 {
				t.Fatalf("in-flight before close = %d, want 1", n)
			}
			req := <-rt.captured
			if e := req.Context().Err(); e != nil {
				t.Fatalf("call context cancelled before close: %v", e)
			}

			if _, err := io.ReadAll(resp.Body); err != nil {
				t.Fatalf("read body: %v", err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			if n := c.InFlight(); n != 0 {
				t.Fatalf("in-flight after close = %d, want 0 (leak)", n)
			}
			if e := req.Context().Err(); e != context.Canceled {
				t.Fatalf("call context after close = %v, want canceled", e)
			}
			// Closing twice must stay a no-op (the gauge must not go negative).
			_ = resp.Body.Close()
			if n := c.InFlight(); n != 0 {
				t.Fatalf("in-flight after double close = %d, want 0", n)
			}
		})

		t.Run("pre-guard-error/"+tc.name, func(t *testing.T) {
			rt := &captureRT{captured: make(chan *http.Request, 1), err: errors.New("dial boom")}
			c := NewClient("be", "http://fake.invalid/v1", tc.protocol, "k", "2023-06-01", ClientTimeouts{})
			c.http = &http.Client{Transport: rt}

			resp, err := dispatch(c, context.Background(), tc.native, tc.stream)
			if err == nil {
				if resp != nil {
					resp.Body.Close()
				}
				t.Fatalf("dispatch: want error, got nil")
			}
			if n := c.InFlight(); n != 0 {
				t.Fatalf("in-flight after error = %d, want 0 (leak)", n)
			}
			req := <-rt.captured
			if e := req.Context().Err(); e != context.Canceled {
				t.Fatalf("call context after error = %v, want canceled", e)
			}
		})
	}
}

// TestChatAnthropicTranslateErrorNoLeak covers the other pre-guard error path: a
// request body that cannot be translated to Anthropic shape must fail before the
// in-flight gauge is ever incremented (ADR-0013, ADR-0016).
func TestChatAnthropicTranslateErrorNoLeak(t *testing.T) {
	rt := &captureRT{captured: make(chan *http.Request, 1), body: ""}
	c := NewClient("be", "http://fake.invalid/v1", model.ProtocolAnthropic, "k", "2023-06-01", ClientTimeouts{})
	c.http = &http.Client{Transport: rt}

	resp, err := c.Chat(context.Background(), []byte(`{not-json`), false)
	if err == nil {
		if resp != nil {
			resp.Body.Close()
		}
		t.Fatalf("Chat: want translate error, got nil")
	}
	if n := c.InFlight(); n != 0 {
		t.Fatalf("in-flight = %d, want 0 (never incremented)", n)
	}
	select {
	case <-rt.captured:
		t.Fatalf("upstream was contacted despite a translate error")
	default:
	}
}
