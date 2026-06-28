package router

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/mattbucci/simple-llm-router/internal/model"
)

var _ Strategy = (*fusionStrategy)(nil)

// fusionStrategy orchestrates panel -> judge -> synthesis (ADR-0014). It is not a
// transparent proxy: it builds OpenAI-canonical sub-requests, fans them out
// through the standard backend path, and streams only the final synthesis. It is
// stateless across requests.
type fusionStrategy struct {
	backends map[string]Backend
	logger   *slog.Logger
}

// chatMessage is a constructed OpenAI chat message. Content is an interface so it
// can hold either a plain string (judge/synthesis prompts) or a model.Content
// (original messages, preserving multimodal parts).
type chatMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

// Execute runs the fusion workflow (ADR-0014): fan the prompt out to the panel
// concurrently over a buffered channel (no locks), send the surviving answers to
// the judge at temperature 0 for a structured comparison, then synthesize the
// final answer from the original conversation plus that analysis. Only the
// synthesis is written to the sink.
func (f *fusionStrategy) Execute(ctx context.Context, req *model.ChatRequest, plan *Plan, sink ResponseSink) (*Outcome, error) {
	// MinPanel is normalized to >= 1 at the config boundary (ADR-0014); trust it.
	answers := f.runPanel(ctx, req, plan)
	if len(answers) < plan.MinPanel {
		err := model.ErrUpstreamUnavailable()
		return &Outcome{Status: err.Status}, err
	}

	analysis := f.runJudge(ctx, req, plan, answers)

	return f.runSynthesis(ctx, req, plan, analysis, sink)
}

// runPanel fans the original conversation out to every panelist concurrently and
// collects the non-empty answers in panel order, dropping failures (ADR-0014).
func (f *fusionStrategy) runPanel(ctx context.Context, req *model.ChatRequest, plan *Plan) []string {
	msgs := messagesArray(req)

	type panelResult struct {
		idx  int
		text string
		ok   bool
	}
	results := make(chan panelResult, len(plan.Panel))

	for i, cand := range plan.Panel {
		go func(i int, cand Candidate) {
			body, err := buildFusionBody(cand.Model, msgs, plan.Temperature, plan.MaxTokens, false)
			if err != nil {
				results <- panelResult{idx: i}
				return
			}
			text, err := f.callUnary(ctx, cand, body)
			if err != nil {
				f.logger.WarnContext(ctx, "fusion: panelist failed",
					slog.String("model", cand.Model),
					slog.String("backend", cand.Backend),
					slog.String("error", err.Error()))
				results <- panelResult{idx: i}
				return
			}
			results <- panelResult{idx: i, text: text, ok: text != ""}
		}(i, cand)
	}

	collected := make([]string, len(plan.Panel))
	for k := 0; k < len(plan.Panel); k++ {
		r := <-results
		if r.ok {
			collected[r.idx] = r.text
		}
	}

	answers := make([]string, 0, len(collected))
	for _, t := range collected {
		if t != "" {
			answers = append(answers, t)
		}
	}
	return answers
}

// runJudge asks the judge model, at temperature 0, to produce a structured JSON
// comparison of the panel answers (ADR-0014). Judge failure is non-fatal: an
// empty analysis still lets synthesis answer from the conversation.
func (f *fusionStrategy) runJudge(ctx context.Context, req *model.ChatRequest, plan *Plan, answers []string) string {
	judgeMsgs, err := json.Marshal([]chatMessage{
		{Role: "system", Content: judgeSystemPrompt},
		{Role: "user", Content: judgeUserPrompt(req, answers)},
	})
	if err != nil {
		return ""
	}
	body, err := buildFusionBody(plan.Judge.Model, judgeMsgs, 0, plan.MaxTokens, false)
	if err != nil {
		return ""
	}
	analysis, err := f.callUnary(ctx, plan.Judge, body)
	if err != nil {
		f.logger.WarnContext(ctx, "fusion: judge failed",
			slog.String("model", plan.Judge.Model),
			slog.String("backend", plan.Judge.Backend),
			slog.String("error", err.Error()))
		return ""
	}
	return analysis
}

// runSynthesis writes the final answer to the sink (ADR-0014). It feeds the
// synthesis model the original conversation plus the judge's analysis and streams
// the result when the consumer asked for streaming, otherwise relays it unary.
func (f *fusionStrategy) runSynthesis(ctx context.Context, req *model.ChatRequest, plan *Plan, analysis string, sink ResponseSink) (*Outcome, error) {
	be, ok := f.backends[plan.Synthesis.Backend]
	if !ok {
		err := model.ErrNoHealthyBackend(plan.Alias)
		return &Outcome{Status: err.Status}, err
	}
	outcome := &Outcome{
		Backend:          plan.Synthesis.Backend,
		UpstreamModel:    plan.Synthesis.Model,
		ProviderProtocol: be.Protocol(),
	}

	// fail logs the underlying cause (preserving %w) and returns the contract
	// APIError, collapsing the repeated unavailable->status->return epilogue.
	fail := func(cause error) (*Outcome, error) {
		f.logSynthesisFailure(ctx, plan, cause)
		e := model.ErrUpstreamUnavailable()
		outcome.Status = e.Status
		return outcome, e
	}

	synthMsgs, err := appendMessage(messagesArray(req), "system", synthesisInstruction(analysis))
	if err != nil {
		return fail(fmt.Errorf("build synthesis messages: %w", err))
	}
	body, err := buildFusionBody(plan.Synthesis.Model, synthMsgs, plan.Temperature, plan.MaxTokens, req.Stream)
	if err != nil {
		return fail(fmt.Errorf("build synthesis body: %w", err))
	}

	resp, err := be.Chat(ctx, body, req.Stream)
	if err != nil {
		return fail(fmt.Errorf("chat %q: %w", plan.Synthesis.Backend, err))
	}

	// Report the concrete (model, backend) the synthesis actually used back to the
	// consumer (ADR-0020), mirroring proxy.go. The sink buffers these and emits
	// them only when it commits the response below.
	sink.SetHeader("X-Router-Model", plan.Synthesis.Model)
	sink.SetHeader("X-Router-Backend", plan.Synthesis.Backend)

	if req.Stream {
		// Fusion does not fail over (single synthesis backend), so an error status
		// is relayed inline as a unary error before the stream starts.
		if resp.Status < 200 || resp.Status >= 300 {
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			outcome.Status = resp.Status
			if werr := sink.WriteResponse(resp.Status, resp.Header, b); werr != nil {
				return outcome, fmt.Errorf("router: write error response: %w", werr)
			}
			return outcome, nil
		}
		outcome.Status = resp.Status
		if rerr := relayStream(ctx, resp, sink, &outcome.Usage); rerr != nil {
			return outcome, rerr
		}
		return outcome, nil
	}

	defer resp.Body.Close()
	b, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		return fail(fmt.Errorf("read %q body: %w", plan.Synthesis.Backend, rerr))
	}
	outcome.Status = resp.Status
	outcome.Usage = parseUsage(b)
	if werr := sink.WriteResponse(resp.Status, resp.Header, b); werr != nil {
		return outcome, fmt.Errorf("router: write response: %w", werr)
	}
	return outcome, nil
}

// logSynthesisFailure records the underlying cause of a synthesis-path failure
// (ADR-0015 %w) before the caller returns the contract APIError, mirroring the
// logging in runPanel/runJudge so the dropped cause is not lost.
func (f *fusionStrategy) logSynthesisFailure(ctx context.Context, plan *Plan, err error) {
	f.logger.WarnContext(ctx, "fusion: synthesis failed",
		slog.String("model", plan.Synthesis.Model),
		slog.String("backend", plan.Synthesis.Backend),
		slog.String("error", err.Error()))
}

// callUnary sends a prepared body to a candidate's backend and returns the
// assistant text of a successful reply (ADR-0014).
func (f *fusionStrategy) callUnary(ctx context.Context, cand Candidate, body []byte) (string, error) {
	be, ok := f.backends[cand.Backend]
	if !ok {
		return "", fmt.Errorf("unknown backend %q", cand.Backend)
	}
	resp, err := be.Chat(ctx, body, false)
	if err != nil {
		return "", fmt.Errorf("chat %q: %w", cand.Backend, err)
	}
	defer resp.Body.Close()
	if resp.Status < 200 || resp.Status >= 300 {
		return "", fmt.Errorf("backend %q status %d", cand.Backend, resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read %q body: %w", cand.Backend, err)
	}
	return extractContent(data), nil
}

// messagesArray returns the OpenAI-shaped messages array for the request straight
// from Raw — the single source of truth (ADR-0001). Forwarding the raw inbound
// array verbatim preserves multimodal/unknown fields for the panel and synthesis
// sub-requests (ADR-0014, ADR-0016). A request with no messages yields an empty
// array.
func messagesArray(req *model.ChatRequest) json.RawMessage {
	if raw, ok := req.Raw["messages"]; ok && len(raw) > 0 {
		return raw
	}
	return json.RawMessage("[]")
}

// appendMessage decodes a messages array, appends one constructed message, and
// re-encodes it.
func appendMessage(messages json.RawMessage, role, content string) (json.RawMessage, error) {
	var arr []json.RawMessage
	if len(messages) > 0 {
		if err := json.Unmarshal(messages, &arr); err != nil {
			return nil, fmt.Errorf("decode messages: %w", err)
		}
	}
	m, err := json.Marshal(chatMessage{Role: role, Content: content})
	if err != nil {
		return nil, fmt.Errorf("encode message: %w", err)
	}
	arr = append(arr, m)
	out, err := json.Marshal(arr)
	if err != nil {
		return nil, fmt.Errorf("encode messages: %w", err)
	}
	return out, nil
}

// buildFusionBody marshals a minimal OpenAI chat-completions request. messages is
// embedded verbatim; temperature is always set (the judge runs at 0); max_tokens
// is set only when positive.
func buildFusionBody(modelID string, messages json.RawMessage, temperature float64, maxTokens int, stream bool) ([]byte, error) {
	body := make(map[string]json.RawMessage, 5)

	// A string, bool, or int never fails to marshal, so those errors are dead.
	mb, _ := json.Marshal(modelID)
	body["model"] = mb
	body["messages"] = messages

	// temperature is a float64, which can fail to marshal (NaN/Inf), so it keeps
	// its guard.
	tb, err := json.Marshal(temperature)
	if err != nil {
		return nil, fmt.Errorf("marshal temperature: %w", err)
	}
	body["temperature"] = tb

	sb, _ := json.Marshal(stream)
	body["stream"] = sb

	if maxTokens > 0 {
		xb, _ := json.Marshal(maxTokens)
		body["max_tokens"] = xb
	}

	out, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal fusion body: %w", err)
	}
	return out, nil
}

// extractContent pulls the assistant text from an OpenAI-shaped response,
// best-effort. model.Content handles both string and multimodal array content.
func extractContent(body []byte) string {
	var r struct {
		Choices []struct {
			Message struct {
				Content model.Content `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return ""
	}
	if len(r.Choices) == 0 {
		return ""
	}
	return r.Choices[0].Message.Content.Text()
}

// conversationText flattens the request's messages to plain text for the judge
// prompt. The parsed message list is derived on demand from Raw — the single
// source of truth (ADR-0001) — via CanonicalMessages; a parse failure yields an
// empty conversation best-effort (the judge step is non-fatal, ADR-0014).
func conversationText(req *model.ChatRequest) string {
	msgs, _, err := req.CanonicalMessages()
	if err != nil {
		return ""
	}
	var sb strings.Builder
	for _, m := range msgs {
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(m.Role)
		sb.WriteString(": ")
		sb.WriteString(m.Content.Text())
	}
	return sb.String()
}

// judgeSystemPrompt instructs the judge to compare, not merge (ADR-0014).
const judgeSystemPrompt = "You are an impartial judge comparing multiple candidate answers to the same request. " +
	"Do not merge or rewrite the answers. Compare them and output a single JSON object with the keys " +
	"\"consensus\" (claims the answers agree on), \"disagreements\" (where they differ and why it matters), " +
	"\"coverage_gaps\" (important aspects none addressed), and \"blind_spots\" (likely errors, risks, or omissions). " +
	"Output only the JSON object, with no surrounding prose."

// judgeUserPrompt assembles the original request and the numbered panel answers
// for the judge.
func judgeUserPrompt(req *model.ChatRequest, answers []string) string {
	var sb strings.Builder
	sb.WriteString("Original request:\n")
	sb.WriteString(conversationText(req))
	sb.WriteString("\n\nCandidate answers:\n")
	for i, a := range answers {
		fmt.Fprintf(&sb, "\n[Answer %d]\n%s\n", i+1, a)
	}
	sb.WriteString("\nProduce the structured JSON comparison now.")
	return sb.String()
}

// synthesisInstruction is the context message handed to the synthesis model
// alongside the original conversation (ADR-0014).
func synthesisInstruction(analysis string) string {
	if strings.TrimSpace(analysis) == "" {
		return "Several expert models independently answered the user's request. " +
			"Write the single best final answer to the user's request. Respond directly to the user; " +
			"do not mention that multiple models were consulted."
	}
	return "Several expert models independently answered the user's request. An impartial judge produced " +
		"this structured analysis of their answers:\n\n" + analysis + "\n\nUsing this analysis and the " +
		"conversation above, write the single best final answer to the user's request. Respond directly to " +
		"the user; do not mention the panel, the judge, or this analysis process."
}
