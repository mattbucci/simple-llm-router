package model

import "time"

// Snapshot is an immutable view of fleet health and discovery, published by the
// health loop and read lock-free by the router (ADR-0005, ADR-0015). A Snapshot
// is never mutated after publication; the loop swaps in a new one each interval.
type Snapshot struct {
	Backends map[string]BackendState
}

// BackendState is one backend's health, discovered models, and load signals.
type BackendState struct {
	Name string
	// Healthy is true once the backend has had at least one successful probe and
	// its most recent probe succeeded (ADR-0005).
	Healthy bool
	// Models is the set of upstream model ids the backend advertises via
	// GET /v1/models.
	Models map[string]struct{}
	// P50Latency is a recent latency estimate used as a pareto cost signal
	// (ADR-0013).
	P50Latency time.Duration
	// InFlight is the backend's current in-flight request count, another pareto
	// cost signal (ADR-0013).
	InFlight int64
}

// Serves reports whether the backend is healthy and advertises the model id.
func (s BackendState) Serves(model string) bool {
	if !s.Healthy {
		return false
	}
	_, ok := s.Models[model]
	return ok
}

// EmptySnapshot is the initial state before the first probe: no backend is in
// rotation (ADR-0005).
func EmptySnapshot() *Snapshot {
	return &Snapshot{Backends: map[string]BackendState{}}
}

// Healthy reports whether at least one backend is healthy, backing /readyz
// (ADR-0011).
func (s *Snapshot) Healthy() bool {
	for _, b := range s.Backends {
		if b.Healthy {
			return true
		}
	}
	return false
}

// BackendsServing returns the names of all healthy backends advertising the
// given upstream model id, for direct-id resolution (ADR-0004).
func (s *Snapshot) BackendsServing(model string) []string {
	var out []string
	for name, b := range s.Backends {
		if b.Serves(model) {
			out = append(out, name)
		}
	}
	return out
}
