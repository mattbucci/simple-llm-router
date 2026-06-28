package model

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Message is one canonical chat message. Only role and content are modeled
// canonically; on the same-protocol path the original bytes are forwarded
// verbatim (ADR-0001), so this parsed form is used only for cross-protocol
// translation and fusion, where loss of provider-specific message extras is
// accepted (ADR-0016).
type Message struct {
	Role    string  `json:"role"`
	Content Content `json:"content"`
}

// Plugin is one entry of the reserved top-level `plugins` routing-control array
// (ADR-0001). The router reads these to steer routing and strips them before
// forwarding so a backend never sees them.
type Plugin struct {
	ID     string
	Params map[string]json.RawMessage
}

// Float reads a numeric parameter from the plugin, e.g. min_quality for the
// pareto selector (ADR-0013).
func (p Plugin) Float(key string) (float64, bool) {
	raw, ok := p.Params[key]
	if !ok {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, false
	}
	return f, true
}

// ParsePlugins decodes the reserved `plugins` array into typed plugins. Unknown
// keys are preserved in Params. A malformed array yields nil (the field is
// advisory routing control, not part of the upstream contract).
func ParsePlugins(raw json.RawMessage) []Plugin {
	if len(raw) == 0 {
		return nil
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil
	}
	out := make([]Plugin, 0, len(entries))
	for _, e := range entries {
		p := Plugin{Params: e}
		if id, ok := e["id"]; ok {
			_ = json.Unmarshal(id, &p.ID)
		}
		out = append(out, p)
	}
	return out
}

// ChatRequest is the canonical view of an inbound request the router operates on.
//
// The request has a SINGLE source of truth: Raw (the OpenAI-canonical field map)
// plus ConsumerBody (the original inbound bytes). The parsed message list and
// system prompt are NOT stored as separate hand-synced fields; the few code paths
// that need them (fusion prompt assembly, cross-protocol translation) DERIVE them
// on demand via CanonicalMessages, which parses Raw lazily. The same-protocol
// proxy hot path never parses messages at all — it forwards Raw/ConsumerBody bytes
// with only `model` rewritten and `plugins` stripped (ADR-0001, ADR-0016).
type ChatRequest struct {
	// Consumer is the protocol the request arrived in, decided by the endpoint
	// hit (ADR-0016).
	Consumer Protocol
	// Model is the alias or upstream id the consumer asked for, before
	// resolution (ADR-0004).
	Model string
	// Stream is true when the consumer requested SSE streaming (ADR-0007).
	Stream bool
	// Plugins are the parsed routing-control directives, already stripped from
	// Raw (ADR-0001).
	Plugins []Plugin
	// Raw is the inbound body decoded into the OpenAI-canonical field map (unknown
	// fields preserved). For an OpenAI consumer it is the verbatim inbound body; for
	// an Anthropic consumer it is the inbound-adapter translation into OpenAI shape
	// (the top-level system field hoisted into a leading system-role message). It is
	// the source of truth for the OpenAI-canonical relay, for any cross-protocol
	// translation (e.g. an Anthropic consumer routed to an OpenAI backend), and for
	// the lazily-derived message list (CanonicalMessages).
	Raw map[string]json.RawMessage
	// ConsumerBody is the ORIGINAL inbound request body, byte-for-byte as the
	// consumer sent it, in the consumer's own protocol shape. It backs the
	// same-protocol native relay (ADR-0016 "Anthropic->Anthropic = full
	// passthrough", ADR-0001): when the chosen backend speaks the consumer's
	// protocol, the router forwards these bytes verbatim (rewriting only `model`
	// and stripping `plugins`) instead of the lossy canonical Raw, so
	// provider-specific fields (tools, tool_choice, top_k, metadata, cache_control,
	// ...) survive intact. May be nil/empty, in which case the canonical path is
	// used.
	ConsumerBody []byte
}

// CanonicalMessages parses the OpenAI-canonical conversation from Raw on demand
// — the single source of truth (ADR-0001). It returns the full message list and a
// best-effort plain-text system prompt gathered from any system-role messages, for
// the cross-protocol translation and fusion paths that need a parsed view; the
// same-protocol proxy hot path never calls it. Because both inbound adapters write
// the conversation (Anthropic hoisting its top-level system field into a leading
// system-role message) into Raw["messages"], that one field backs every derived
// view. A missing or empty messages field yields nil with no error; malformed JSON
// is reported wrapped.
func (r *ChatRequest) CanonicalMessages() (msgs []Message, system string, err error) {
	raw, ok := r.Raw["messages"]
	if !ok || len(raw) == 0 {
		return nil, "", nil
	}
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return nil, "", fmt.Errorf("model: parse canonical messages: %w", err)
	}
	var sb strings.Builder
	for _, m := range msgs {
		if m.Role != "system" {
			continue
		}
		if t := m.Content.Text(); t != "" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(t)
		}
	}
	return msgs, sb.String(), nil
}

// PluginParam looks up a numeric parameter from the first plugin whose id
// matches, honoring per-request overrides such as min_quality (ADR-0013).
func (r *ChatRequest) PluginParam(id, key string) (float64, bool) {
	for _, p := range r.Plugins {
		if p.ID == id {
			if v, ok := p.Float(key); ok {
				return v, true
			}
		}
	}
	return 0, false
}
