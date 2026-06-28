package router

import (
	"context"
	"log/slog"

	"github.com/mattbucci/simple-llm-router/internal/model"
)

// Router is the application layer: it resolves an inbound model name to a
// routing Plan (alias-first, then direct upstream id — ADR-0004) and dispatches
// it to the proxy or fusion strategy (ADR-0006, ADR-0014). It is stateless
// across requests apart from a lock-free round-robin rotation counter
// (ADR-0006, ADR-0015) and is safe for concurrent use.
type Router struct {
	backends map[string]Backend
	health   HealthView
	metrics  MetricsRecorder
	aliases  map[string]*Alias
	logger   *slog.Logger

	proxy  *proxyStrategy
	fusion *fusionStrategy
}

// New builds a Router. backends maps a backend name to its outbound adapter;
// aliases maps a friendly model name to its resolved routing config; health and
// metrics are the lock-free health snapshot and the failover counter. cmd/router
// is the only caller (the composition root, ADR-0003). A nil logger falls back
// to slog.Default.
func New(backends map[string]Backend, health HealthView, metrics MetricsRecorder, aliases map[string]*Alias, logger *slog.Logger) *Router {
	if logger == nil {
		logger = slog.Default()
	}
	if backends == nil {
		backends = map[string]Backend{}
	}
	if aliases == nil {
		aliases = map[string]*Alias{}
	}
	r := &Router{
		backends: backends,
		health:   health,
		metrics:  metrics,
		aliases:  aliases,
		logger:   logger,
	}
	r.proxy = &proxyStrategy{
		backends: backends,
		health:   health,
		metrics:  metrics,
		selectors: map[string]Selector{
			selectorRoundRobin: &roundRobinSelector{},
			selectorPareto:     &paretoSelector{},
		},
		logger: logger,
	}
	r.fusion = &fusionStrategy{
		backends: backends,
		logger:   logger,
	}
	return r
}

// Route resolves req.Model, builds a Plan, and runs the matching strategy,
// writing the response to sink. It returns the Outcome for the per-request log
// line (ADR-0011). On a failure before any response was committed it returns a
// *model.APIError (404 model_not_found / 503 no_healthy_backend /
// 502 upstream_unavailable) and the Outcome carries the chosen status so the
// server can log and surface it (ADR-0006). The request path never panics
// (ADR-0015).
func (r *Router) Route(ctx context.Context, req *model.ChatRequest, sink ResponseSink) (*Outcome, error) {
	snap := r.health.Snapshot()
	if snap == nil {
		snap = model.EmptySnapshot()
	}

	plan, apiErr := r.resolve(snap, req)
	if apiErr != nil {
		return &Outcome{Status: apiErr.Status}, apiErr
	}

	return r.strategyFor(plan).Execute(ctx, req, plan, sink)
}

// strategyFor selects the routing strategy for a resolved plan and returns it
// through the Strategy interface (ADR-0006): a fusion plan runs the fusion
// strategy, everything else the proxy strategy. Dispatching polymorphically
// through the interface keeps the strategy boundary a real call site rather than
// a type switch at the seam.
func (r *Router) strategyFor(plan *Plan) Strategy {
	if plan.Type == strategyFusion {
		return r.fusion
	}
	return r.proxy
}
