// Package backend is the outbound adapter layer: it speaks the provider's HTTP
// protocol to a configured upstream and exposes a small surface the router
// consumes through interfaces it owns (ADR-0002, ADR-0003). It imports only
// internal/model plus the standard library and never imports internal/router or
// internal/server; *Client structurally satisfies router.Backend and *Monitor
// structurally satisfies router.HealthView.
//
// A backend is defined solely by its OpenAI-compatible /v1 base URL (ADR-0002):
// the client appends /chat/completions (or, for an Anthropic provider, /messages)
// and /models. Provider-protocol translation is entirely this layer's concern
// (the outbound adapter, ADR-0016) so the router only ever hands the client an
// OpenAI-canonical body and receives an OpenAI-shaped response back.
package backend

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mattbucci/simple-llm-router/internal/model"
)

// ClientTimeouts bounds the upstream call. Connect is the dial timeout, Request
// is the overall deadline for a non-streaming call, and Idle bounds the gap
// between reads on a streaming response. Streaming calls are exempt from an
// overall deadline (ADR-0007).
type ClientTimeouts struct {
	Connect time.Duration
	Request time.Duration
	Idle    time.Duration
}

// Client is a single upstream backend reached over its OpenAI-compatible (or
// Anthropic) HTTP surface (ADR-0002). It is safe for concurrent use: it holds no
// per-request mutable state, only an atomic in-flight counter and an immutable
// configuration plus a shared *http.Client (ADR-0015).
type Client struct {
	// inFlight is the lock-free request gauge read as a pareto cost signal
	// (ADR-0013). It is declared first so it is also fine on 32-bit builds.
	inFlight atomic.Int64

	name             string
	baseURL          string
	apiKey           string
	anthropicVersion string
	protocol         model.Protocol
	timeouts         ClientTimeouts
	http             *http.Client
}

// NewClient builds a backend client. baseURL is the provider's versioned base
// (e.g. http://host:port/v1); credentials are operator-owned and injected per
// provider protocol on every outbound call (ADR-0009). The returned *Client
// structurally satisfies router.Backend.
func NewClient(name, baseURL string, protocol model.Protocol, apiKey, anthropicVersion string, t ClientTimeouts) *Client {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   t.Connect,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   t.Connect,
		ExpectContinueTimeout: time.Second,
	}
	return &Client{
		name:             name,
		baseURL:          baseURL,
		apiKey:           apiKey,
		anthropicVersion: anthropicVersion,
		protocol:         protocol,
		timeouts:         t,
		// No http.Client.Timeout: it would also kill long streams (ADR-0007);
		// deadlines are imposed per call via context instead.
		http: &http.Client{Transport: transport},
	}
}

// Name returns the configured backend name.
func (c *Client) Name() string { return c.name }

// Protocol returns the provider protocol this backend speaks.
func (c *Client) Protocol() model.Protocol { return c.protocol }

// BaseURL returns the configured upstream base URL.
func (c *Client) BaseURL() string { return c.baseURL }

// InFlight returns the current number of in-flight upstream requests, a pareto
// load signal (ADR-0013). It is incremented when a Chat call is dispatched and
// decremented when the returned response body is closed.
func (c *Client) InFlight() int64 { return c.inFlight.Load() }

// Chat sends a prepared OpenAI-canonical body to the upstream and returns the
// raw response for the router to relay (ADR-0001, ADR-0007). For an Anthropic
// provider the body is translated to /v1/messages on the way out and the reply
// is translated back to OpenAI shape so the router and sink only ever see OpenAI
// (ADR-0016). stream selects the streaming vs unary response path. The caller
// owns and must close the returned body.
func (c *Client) Chat(ctx context.Context, body []byte, stream bool) (*model.UpstreamResponse, error) {
	if c.protocol == model.ProtocolAnthropic {
		return c.chatAnthropic(ctx, body, stream)
	}
	return c.chatOpenAI(ctx, body, stream)
}

// ChatNative relays a request body to the Anthropic provider's native /messages
// endpoint with NO request or response translation in either direction — the
// same-protocol full-fidelity relay (ADR-0016 "Anthropic->Anthropic = full
// passthrough", ADR-0001). The body is POSTed to {baseURL}/messages with
// x-api-key + anthropic-version. The caller (router) has already rewritten `model`
// and stripped `plugins`; everything else (tools, tool_choice, top_k, metadata,
// cache_control, ...) is forwarded byte-intact and the reply is returned verbatim.
//
// Its only caller (the router's native-relay path) reaches it solely when both the
// consumer and the chosen provider speak Anthropic, so the native target is
// unconditionally /messages — an OpenAI provider already gets full byte fidelity
// from the ordinary Chat verbatim relay and never takes this path. In-flight
// accounting, per-call timeouts, and the streaming idle guard match Chat exactly.
// The caller owns and must close the returned body.
func (c *Client) ChatNative(ctx context.Context, body []byte, stream bool) (*model.UpstreamResponse, error) {
	reqCtx, cancel := c.requestContext(ctx, stream)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.url("/messages"), bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("backend %q: build native request: %w", c.name, err)
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
		return nil, fmt.Errorf("backend %q: native request: %w", c.name, err)
	}
	disarm() // ownership of the in-flight slot + cancel transfers to the guard
	return &model.UpstreamResponse{
		Status: resp.StatusCode,
		Header: resp.Header,
		Body:   c.guard(resp.Body, stream, cancel),
	}, nil
}

// chatOpenAI proxies an OpenAI-shaped body to {baseURL}/chat/completions and
// returns the raw response verbatim (ADR-0001).
func (c *Client) chatOpenAI(ctx context.Context, body []byte, stream bool) (*model.UpstreamResponse, error) {
	reqCtx, cancel := c.requestContext(ctx, stream)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.url("/chat/completions"), bytes.NewReader(body))
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
	disarm() // ownership of the in-flight slot + cancel transfers to the guard
	return &model.UpstreamResponse{
		Status: resp.StatusCode,
		Header: resp.Header,
		Body:   c.guard(resp.Body, stream, cancel),
	}, nil
}

// acquire bumps the in-flight gauge and returns a one-shot cleanup that releases
// it and cancels the call context, plus a disarm. Every outbound path defers
// cleanup so no early return can leak the gauge or the context (ADR-0013); the
// success path calls disarm once ownership of that cleanup transfers to the
// response body's guard (which performs the same release on Close).
func (c *Client) acquire(cancel context.CancelFunc) (cleanup, disarm func()) {
	c.inFlight.Add(1)
	armed := true
	cleanup = func() {
		if armed {
			armed = false
			c.inFlight.Add(-1)
			cancel()
		}
	}
	return cleanup, func() { armed = false }
}

// requestContext derives the call context. Unary calls get the overall Request
// deadline; streaming calls get only cancellation (no overall deadline) and rely
// on the idle timeout applied to the body (ADR-0007).
func (c *Client) requestContext(ctx context.Context, stream bool) (context.Context, context.CancelFunc) {
	if stream || c.timeouts.Request <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, c.timeouts.Request)
}

// injectAuth adds the operator-owned provider credential in the form the
// provider protocol expects (ADR-0009). The inbound consumer credential is
// stripped at the server edge and never reaches here.
func (c *Client) injectAuth(req *http.Request) {
	switch c.protocol {
	case model.ProtocolAnthropic:
		if c.apiKey != "" {
			req.Header.Set("x-api-key", c.apiKey)
		}
		if c.anthropicVersion != "" {
			req.Header.Set("anthropic-version", c.anthropicVersion)
		}
	default:
		if c.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
	}
}

// url joins the configured base URL with a resource path, tolerating a trailing
// slash on the base.
func (c *Client) url(path string) string {
	if n := len(c.baseURL); n > 0 && c.baseURL[n-1] == '/' {
		return c.baseURL[:n-1] + path
	}
	return c.baseURL + path
}

// guard wraps an upstream body so that closing it decrements the in-flight
// counter and cancels the call context exactly once. For streaming bodies it
// also enforces the idle read timeout (ADR-0007).
func (c *Client) guard(rc io.ReadCloser, applyIdle bool, cancel context.CancelFunc) io.ReadCloser {
	g := &guardedBody{
		rc:     rc,
		cancel: cancel,
		done:   func() { c.inFlight.Add(-1) },
	}
	if applyIdle && c.timeouts.Idle > 0 {
		g.timer = newIdleTimer(c.timeouts.Idle, cancel)
	}
	return g
}

// guardedBody wraps an upstream response body to enforce an idle read timeout
// and to run cleanup (cancel the call context, decrement the in-flight gauge)
// exactly once when the consumer closes it (ADR-0007, ADR-0013).
type guardedBody struct {
	rc     io.ReadCloser
	timer  *idleTimer
	cancel context.CancelFunc
	done   func()
	once   sync.Once
}

// Read relays from the upstream body. When an idle timeout is configured, a read
// that stalls longer than idle cancels the call context, aborting the upstream
// request so the stalled Read returns promptly.
func (g *guardedBody) Read(p []byte) (int, error) {
	if g.timer == nil {
		return g.rc.Read(p)
	}
	g.timer.arm()
	n, err := g.rc.Read(p)
	g.timer.disarm()
	return n, err
}

// idleTimer enforces an idle gap between reads on a streaming body by canceling
// the call context when a read stalls past idle (ADR-0007). One timer is reused
// per body across reads via Reset rather than allocating a fresh time.AfterFunc
// on every Read; it is driven from the single reader goroutine so arm/disarm
// never race each other (only the fired cancel runs elsewhere, and cancel is
// safe to call concurrently). It is shared by guardedBody and the Anthropic
// stream reader.
type idleTimer struct {
	idle   time.Duration
	cancel context.CancelFunc
	timer  *time.Timer
}

// newIdleTimer returns an idle guard for a positive idle duration; the caller
// only constructs one when idle > 0.
func newIdleTimer(idle time.Duration, cancel context.CancelFunc) *idleTimer {
	return &idleTimer{idle: idle, cancel: cancel}
}

// arm (re)starts the idle countdown immediately before a blocking read.
func (t *idleTimer) arm() {
	if t.timer == nil {
		t.timer = time.AfterFunc(t.idle, t.cancel)
		return
	}
	t.timer.Reset(t.idle)
}

// disarm stops the countdown once a read has returned.
func (t *idleTimer) disarm() {
	if t.timer != nil {
		t.timer.Stop()
	}
}

// Close closes the upstream body and runs the one-shot cleanup.
func (g *guardedBody) Close() error {
	err := g.rc.Close()
	g.finish()
	return err
}

func (g *guardedBody) finish() {
	g.once.Do(func() {
		if g.cancel != nil {
			g.cancel()
		}
		if g.done != nil {
			g.done()
		}
	})
}
