package router

import "github.com/mattbucci/simple-llm-router/internal/model"

// Strategy and selector identifiers used across the package.
const (
	strategyProxy  = "proxy"
	strategyFusion = "fusion"

	selectorRoundRobin = "round_robin"
	selectorPareto     = "pareto"
)

// resolve turns the inbound model name into a routing Plan, alias-first then by
// direct upstream id (ADR-0004). It never reports 404/503 for an alias: a known
// alias whose backends are all down still resolves to a Plan, and the strategy's
// selector then yields 503 no_healthy_backend (ADR-0006). 404 is reserved for a
// name that is neither an alias nor an advertised upstream id.
func (r *Router) resolve(snap *model.Snapshot, req *model.ChatRequest) (*Plan, *model.APIError) {
	if alias, ok := r.aliases[req.Model]; ok && alias != nil {
		return r.resolveAlias(snap, alias)
	}
	return r.resolveDirect(snap, req)
}

// resolveAlias builds a Plan from a configured alias (ADR-0004). Proxy aliases
// carry their full candidate set (health filtering is the selector's job);
// fusion aliases pin a healthy backend per role up front (ADR-0014).
func (r *Router) resolveAlias(snap *model.Snapshot, alias *Alias) (*Plan, *model.APIError) {
	if alias.Type == strategyFusion {
		return r.resolveFusion(snap, alias)
	}

	plan := &Plan{
		Alias:      alias.Name,
		Type:       strategyProxy,
		MinQuality: alias.MinQuality,
	}
	if alias.Selector == selectorPareto {
		plan.Selector = selectorPareto
		// Expand the pool into one candidate per (model, backend); the pareto
		// selector filters by quality and health then ranks by cost (ADR-0013).
		for _, e := range alias.Pool {
			for _, b := range e.Backends {
				plan.Candidates = append(plan.Candidates, Candidate{
					Model:   e.Model,
					Backend: b,
					Quality: e.Quality,
				})
			}
		}
	} else {
		plan.Selector = selectorRoundRobin
		for _, b := range alias.Backends {
			plan.Candidates = append(plan.Candidates, Candidate{
				Model:   alias.Model,
				Backend: b,
			})
		}
	}
	return plan, nil
}

// resolveFusion pins a healthy backend for each panelist, the judge, and the
// synthesis model (ADR-0014). Panelists with no healthy backend are dropped
// (fusion tolerates panel failures); if no panelist, judge, or synthesis backend
// is healthy the request is 503 no_healthy_backend.
func (r *Router) resolveFusion(snap *model.Snapshot, alias *Alias) (*Plan, *model.APIError) {
	// MinPanel is normalized to >= 1 at the config boundary (ADR-0014); trust it.
	plan := &Plan{
		Alias:       alias.Name,
		Type:        strategyFusion,
		MinPanel:    alias.MinPanel,
		Temperature: alias.Temperature,
		MaxTokens:   alias.MaxTokens,
	}
	for _, e := range alias.Panel {
		if b, ok := pickHealthyBackend(snap, e.Backends); ok {
			plan.Panel = append(plan.Panel, Candidate{Model: e.Model, Backend: b, Quality: e.Quality})
		}
	}
	if len(plan.Panel) == 0 {
		return nil, model.ErrNoHealthyBackend(alias.Name)
	}

	jb, ok := pickHealthyBackend(snap, alias.Judge.Backends)
	if !ok {
		return nil, model.ErrNoHealthyBackend(alias.Name)
	}
	plan.Judge = Candidate{Model: alias.Judge.Model, Backend: jb}

	sb, ok := pickHealthyBackend(snap, alias.Synthesis.Backends)
	if !ok {
		return nil, model.ErrNoHealthyBackend(alias.Name)
	}
	plan.Synthesis = Candidate{Model: alias.Synthesis.Model, Backend: sb}

	return plan, nil
}

// resolveDirect treats the model name as an upstream id (ADR-0004). It maps to
// every healthy backend advertising that id via round-robin proxy. It returns
// 503 when the id is advertised but no backend serving it is healthy, and 404
// only when no backend advertises it at all (ADR-0006).
func (r *Router) resolveDirect(snap *model.Snapshot, req *model.ChatRequest) (*Plan, *model.APIError) {
	serving := snap.BackendsServing(req.Model)
	if len(serving) > 0 {
		plan := &Plan{
			Type:     strategyProxy,
			Selector: selectorRoundRobin,
		}
		for _, b := range serving {
			plan.Candidates = append(plan.Candidates, Candidate{Model: req.Model, Backend: b})
		}
		return plan, nil
	}
	if anyAdvertises(snap, req.Model) {
		return nil, model.ErrNoHealthyBackend(req.Model)
	}
	return nil, model.ErrModelNotFound(req.Model)
}

// pickHealthyBackend chooses the least-loaded healthy backend from names, using
// the snapshot's in-flight signal as the tiebreaker (ADR-0013). It returns false
// when none of the named backends is currently healthy.
func pickHealthyBackend(snap *model.Snapshot, names []string) (string, bool) {
	best := ""
	found := false
	var bestLoad int64
	for _, n := range names {
		b, ok := snap.Backends[n]
		if !ok || !b.Healthy {
			continue
		}
		if !found || b.InFlight < bestLoad {
			best = n
			bestLoad = b.InFlight
			found = true
		}
	}
	return best, found
}

// anyAdvertises reports whether any backend (healthy or not) advertises the
// given upstream model id, distinguishing "known but down" (503) from "unknown"
// (404) for direct-id resolution (ADR-0004).
func anyAdvertises(snap *model.Snapshot, modelID string) bool {
	for _, b := range snap.Backends {
		if _, ok := b.Models[modelID]; ok {
			return true
		}
	}
	return false
}
