package router

import (
	"sort"
	"sync/atomic"

	"github.com/mattbucci/simple-llm-router/internal/model"
)

// Cost weights for the pareto selector. Locally "cost" is GPU time, not dollars
// (ADR-0013): a weighted blend of measured p50 latency (seconds) and current
// in-flight load. Tune per fleet.
const (
	costLatencyWeight  = 1.0 // per second of p50 latency
	costInFlightWeight = 0.5 // per in-flight request
)

var (
	_ Selector = (*roundRobinSelector)(nil)
	_ Selector = (*paretoSelector)(nil)
)

// roundRobinSelector rotates over the healthy candidates with a lock-free atomic
// counter — no mutex, no per-request state (ADR-0006, ADR-0015).
type roundRobinSelector struct {
	counter atomic.Uint64
}

// Order returns the healthy candidates rotated by a global atomic counter so
// successive requests start at different backends. minQuality is ignored.
func (s *roundRobinSelector) Order(snap *model.Snapshot, _ *model.ChatRequest, candidates []Candidate, _ float64) []Candidate {
	healthy := filterHealthy(snap, candidates)
	n := len(healthy)
	if n == 0 {
		return nil
	}
	start := int((s.counter.Add(1) - 1) % uint64(n))
	out := make([]Candidate, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, healthy[(start+i)%n])
	}
	return out
}

// paretoSelector picks the cheapest good-enough candidate (ADR-0013). It is
// stateless and lock-free: all signals are read from the immutable snapshot.
type paretoSelector struct{}

// Order keeps candidates whose quality clears the (possibly per-request) bar and
// that have a healthy backend, ranks them by ascending cost, then appends the
// healthy below-bar survivors (also cost-ranked) so the proxy cascades through
// the tier before relaxing it (ADR-0013).
func (s *paretoSelector) Order(snap *model.Snapshot, req *model.ChatRequest, candidates []Candidate, minQuality float64) []Candidate {
	q := minQuality
	if req != nil {
		if v, ok := req.PluginParam("pareto", "min_quality"); ok {
			q = v
		}
	}

	healthy := filterHealthy(snap, candidates)
	if len(healthy) == 0 {
		return nil
	}

	inTier := make([]Candidate, 0, len(healthy))
	relaxed := make([]Candidate, 0, len(healthy))
	for _, c := range healthy {
		if c.Quality >= q {
			inTier = append(inTier, c)
		} else {
			relaxed = append(relaxed, c)
		}
	}
	rankByCost(snap, inTier)
	rankByCost(snap, relaxed)

	out := make([]Candidate, 0, len(healthy))
	out = append(out, inTier...)
	out = append(out, relaxed...)
	return out
}

// filterHealthy keeps only candidates whose backend is healthy in the snapshot
// (ADR-0006). Direct-id candidates already encode the serves-this-model check;
// alias candidates trust the operator's backend binding and filter on health
// alone.
func filterHealthy(snap *model.Snapshot, candidates []Candidate) []Candidate {
	out := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if b, ok := snap.Backends[c.Backend]; ok && b.Healthy {
			out = append(out, c)
		}
	}
	return out
}

// rankByCost stable-sorts candidates by ascending local cost (ADR-0013).
func rankByCost(snap *model.Snapshot, cands []Candidate) {
	sort.SliceStable(cands, func(i, j int) bool {
		return cost(snap, cands[i]) < cost(snap, cands[j])
	})
}

// cost is the local GPU-time estimate for a candidate's backend (ADR-0013).
func cost(snap *model.Snapshot, c Candidate) float64 {
	b := snap.Backends[c.Backend]
	return costLatencyWeight*b.P50Latency.Seconds() + costInFlightWeight*float64(b.InFlight)
}
