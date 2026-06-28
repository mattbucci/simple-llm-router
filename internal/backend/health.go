package backend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mattbucci/simple-llm-router/internal/model"
)

// latencyWindow is the number of recent successful probe latencies kept per
// backend to estimate P50 (ADR-0013).
const latencyWindow = 20

// suspectBuffer bounds the suspect re-probe channel. Sends are non-blocking
// (Suspect drops on a full buffer), so the cap only sets how many distinct
// fast-re-probe requests can queue between owner-loop iterations (ADR-0006).
const suspectBuffer = 64

// Monitor is the single background health/discovery loop (ADR-0005). It polls
// GET {baseURL}/models on every backend each interval, publishing an immutable
// *model.Snapshot via atomic.Value that readers consume lock-free (ADR-0015).
// *Monitor structurally satisfies router.HealthView.
type Monitor struct {
	clients  []*Client
	byName   map[string]*Client // name -> client, for O(1) single-backend re-probe
	interval time.Duration
	timeout  time.Duration
	onHealth func(name string, healthy bool)
	snap     atomic.Value // holds *model.Snapshot
	suspect  chan string  // names to fast re-probe; drained only by loop (ADR-0006)
}

// NewMonitor builds the health loop over the given clients. onHealth is invoked
// each time a backend's health is (re)evaluated (it may be nil). The initial
// snapshot is empty so no backend is in rotation before the first probe.
func NewMonitor(clients []*Client, interval, timeout time.Duration, onHealth func(name string, healthy bool)) *Monitor {
	byName := make(map[string]*Client, len(clients))
	for _, cl := range clients {
		byName[cl.Name()] = cl
	}
	m := &Monitor{
		clients:  clients,
		byName:   byName,
		interval: interval,
		timeout:  timeout,
		onHealth: onHealth,
		suspect:  make(chan string, suspectBuffer),
	}
	m.snap.Store(model.EmptySnapshot())
	return m
}

// Suspect requests a fast, single-backend re-probe of name (ADR-0006): the proxy
// calls this when a backend fails mid-rotation so it leaves the rotation before
// the next fixed health tick. The send is non-blocking — if the buffer is full
// the request is dropped, since a pending re-probe already covers it — so the
// request path never blocks and there is no lock (ADR-0015).
func (m *Monitor) Suspect(name string) {
	select {
	case m.suspect <- name:
	default:
	}
}

// Snapshot returns the current immutable health/discovery snapshot, read without
// locks (ADR-0005).
func (m *Monitor) Snapshot() *model.Snapshot {
	if s, ok := m.snap.Load().(*model.Snapshot); ok {
		return s
	}
	return model.EmptySnapshot()
}

// Start launches the single background health goroutine bound to ctx; it returns
// immediately and the loop runs until ctx is canceled (ADR-0005, ADR-0015).
func (m *Monitor) Start(ctx context.Context) {
	go m.loop(ctx)
}

// loop owns all health state (latency history) and probes on the interval. State
// is owned by this one goroutine and published only as immutable snapshots, so
// no lock is needed (ADR-0015).
func (m *Monitor) loop(ctx context.Context) {
	history := make(map[string][]time.Duration, len(m.clients))

	m.probeAll(ctx, history) // probe once up front so rotation can fill promptly
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.probeAll(ctx, history)
		case name := <-m.suspect:
			// A backend just failed mid-rotation: re-probe only it and republish so
			// it drops out of rotation without waiting for the next tick (ADR-0006).
			m.probeOne(ctx, history, name)
		}
	}
}

// probeResult is one backend's probe outcome, handed back over a buffered channel
// so the loop goroutine remains the sole owner of shared state (ADR-0015).
type probeResult struct {
	name     string
	healthy  bool
	models   map[string]struct{}
	latency  time.Duration
	inFlight int64
}

// probeAll fans out a probe to every backend concurrently, then folds the
// results into a fresh snapshot and publishes it.
func (m *Monitor) probeAll(ctx context.Context, history map[string][]time.Duration) {
	results := make(chan probeResult, len(m.clients))
	var wg sync.WaitGroup
	for _, cl := range m.clients {
		wg.Add(1)
		go func(cl *Client) {
			defer wg.Done()
			healthy, models, latency := m.probe(ctx, cl)
			results <- probeResult{
				name:     cl.Name(),
				healthy:  healthy,
				models:   models,
				latency:  latency,
				inFlight: cl.InFlight(),
			}
		}(cl)
	}
	wg.Wait()
	close(results)

	prev := m.Snapshot()
	backends := make(map[string]model.BackendState, len(m.clients))
	for r := range results {
		backends[r.name] = m.fold(history, prev, r)
	}

	m.snap.Store(&model.Snapshot{Backends: backends})
}

// probeOne re-probes a single backend by name and republishes a snapshot that
// carries forward every other backend's last-known state, replacing only this
// one (ADR-0006). It runs on the loop goroutine, so it is the sole writer of
// history and the snapshot — no lock (ADR-0015). An unknown name is a no-op.
func (m *Monitor) probeOne(ctx context.Context, history map[string][]time.Duration, name string) {
	cl, ok := m.byName[name]
	if !ok {
		return
	}

	healthy, models, latency := m.probe(ctx, cl)
	r := probeResult{
		name:     name,
		healthy:  healthy,
		models:   models,
		latency:  latency,
		inFlight: cl.InFlight(),
	}

	prev := m.Snapshot()
	backends := make(map[string]model.BackendState, len(prev.Backends)+1)
	for k, v := range prev.Backends {
		backends[k] = v
	}
	backends[name] = m.fold(history, prev, r)

	m.snap.Store(&model.Snapshot{Backends: backends})
}

// fold turns one probeResult into a BackendState, updating the latency history
// and invoking onHealth. It is called only from the loop goroutine, which owns
// both history and the snapshot (ADR-0015).
func (m *Monitor) fold(history map[string][]time.Duration, prev *model.Snapshot, r probeResult) model.BackendState {
	// Track P50 from recent successful probes only.
	if r.healthy {
		h := append(history[r.name], r.latency)
		if len(h) > latencyWindow {
			h = h[len(h)-latencyWindow:]
		}
		history[r.name] = h
	}

	// Keep the last-known model catalog when a probe fails so discovery info
	// survives a transient blip; Healthy=false keeps it out of rotation anyway
	// (ADR-0005).
	models := r.models
	if !r.healthy {
		if ps, ok := prev.Backends[r.name]; ok {
			models = ps.Models
		}
	}

	if m.onHealth != nil {
		m.onHealth(r.name, r.healthy)
	}
	return model.BackendState{
		Name:       r.name,
		Healthy:    r.healthy,
		Models:     models,
		P50Latency: median(history[r.name]),
		InFlight:   r.inFlight,
	}
}

// probe performs one bounded GET {baseURL}/models, returning health, the
// advertised model ids, and the round-trip latency.
func (m *Monitor) probe(ctx context.Context, cl *Client) (bool, map[string]struct{}, time.Duration) {
	pctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	start := time.Now()
	models, ok := cl.probeModels(pctx)
	return ok, models, time.Since(start)
}

// probeModels calls GET {baseURL}/models and parses the advertised model ids.
// A 200 with a parseable body means healthy (ADR-0005). The same {data:[{id}]}
// shape is returned by every OpenAI-compatible engine and by Anthropic, so one
// parser serves both protocols (ADR-0002).
func (c *Client) probeModels(ctx context.Context) (map[string]struct{}, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/models"), nil)
	if err != nil {
		return nil, false
	}
	c.injectAuth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, false
	}

	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, false
	}
	models := make(map[string]struct{}, len(parsed.Data))
	for _, d := range parsed.Data {
		if d.ID != "" {
			models[d.ID] = struct{}{}
		}
	}
	return models, true
}

// median returns the P50 of the samples (the upper-middle for an even count). It
// copies before sorting so it never mutates the caller's slice.
func median(samples []time.Duration) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	cp := append([]time.Duration(nil), samples...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	return cp[len(cp)/2]
}
