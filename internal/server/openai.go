package server

import (
	"encoding/json"

	"github.com/mattbucci/simple-llm-router/internal/model"
)

// parseOpenAIRequest is the inbound adapter for OpenAI consumers. The body is
// already in the canonical (OpenAI) shape, so it is decoded into a field map
// that preserves every unknown field for verbatim same-protocol passthrough
// (ADR-0001). Only model, stream, and the reserved plugins array are extracted;
// the message list is NOT parsed here — Raw is the single source of truth and the
// fusion/translation paths derive messages lazily via ChatRequest.CanonicalMessages
// (ADR-0014, ADR-0016), so the proxy hot path never pays for a parse it won't read.
func parseOpenAIRequest(body []byte) (*model.ChatRequest, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, model.ErrBadRequest("request body is not valid JSON")
	}

	req := &model.ChatRequest{
		Consumer: model.ProtocolOpenAI,
		Raw:      raw,
		// Keep the original bytes verbatim for the same-protocol native relay
		// (ADR-0001/ADR-0016); for OpenAI consumers this equals the inbound body.
		ConsumerBody: body,
	}
	if m, ok := raw["model"]; ok {
		_ = json.Unmarshal(m, &req.Model)
	}
	if s, ok := raw["stream"]; ok {
		_ = json.Unmarshal(s, &req.Stream)
	}
	if p, ok := raw["plugins"]; ok {
		req.Plugins = model.ParsePlugins(p)
	}
	return req, nil
}
