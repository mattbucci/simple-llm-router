package observability

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// requestKey identifies one requests_total series. Using a struct map key rather
// than a delimited string makes the label values data, not syntax: a '|' (or any
// byte) inside a backend or model id can no longer corrupt the split at render
// time.
type requestKey struct {
	backend string
	model   string
	status  int
}

// Metrics is a hand-rolled Prometheus exporter. To honor the no-locks rule
// (ADR-0015) the counters live behind a single owner goroutine; callers reach it
// only over channels, never a shared map under a mutex (ADR-0011).
type Metrics struct {
	ops chan metricOp
}

type opKind int

const (
	opRequestDone opKind = iota
	opFailover
	opInFlight
	opHealth
	opScrape
)

type metricOp struct {
	kind    opKind
	backend string
	model   string
	status  int
	latency time.Duration
	delta   int
	healthy bool
	reply   chan string
}

// latencyBounds are the histogram bucket upper bounds in seconds.
var latencyBounds = []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60}

// New starts the metrics owner goroutine, bound to ctx (ADR-0015: every
// goroutine has a context-tied lifetime).
func New(ctx context.Context) *Metrics {
	m := &Metrics{ops: make(chan metricOp, 8192)}
	go m.run(ctx)
	return m
}

func (m *Metrics) run(ctx context.Context) {
	requests := map[requestKey]int64{}
	buckets := make([]int64, len(latencyBounds)+1)
	var latencySum float64
	var latencyCount int64
	var inFlight int64
	var failovers int64
	health := map[string]float64{} // backend -> 1/0

	for {
		select {
		case <-ctx.Done():
			return
		case op := <-m.ops:
			switch op.kind {
			case opRequestDone:
				requests[requestKey{backend: op.backend, model: op.model, status: op.status}]++
				secs := op.latency.Seconds()
				latencySum += secs
				latencyCount++
				placed := false
				for i, b := range latencyBounds {
					if secs <= b {
						buckets[i]++
						placed = true
						break
					}
				}
				if !placed {
					buckets[len(buckets)-1]++
				}
			case opFailover:
				failovers++
			case opInFlight:
				inFlight += int64(op.delta)
			case opHealth:
				if op.healthy {
					health[op.backend] = 1
				} else {
					health[op.backend] = 0
				}
			case opScrape:
				op.reply <- render(requests, buckets, latencySum, latencyCount, inFlight, failovers, health)
			}
		}
	}
}

// RequestDone records a completed request and its latency (ADR-0011).
func (m *Metrics) RequestDone(backend, model string, status int, latency time.Duration) {
	m.send(metricOp{kind: opRequestDone, backend: backend, model: model, status: status, latency: latency})
}

// IncFailover records one failover/retry (ADR-0006).
func (m *Metrics) IncFailover() { m.send(metricOp{kind: opFailover}) }

// AddInFlight adjusts the in-flight gauge (+1 at request start, -1 at end).
func (m *Metrics) AddInFlight(delta int) { m.send(metricOp{kind: opInFlight, delta: delta}) }

// SetHealth sets a backend's health gauge; the server refreshes these from the
// health snapshot at scrape time so the backend layer need not import this
// package (ADR-0003).
func (m *Metrics) SetHealth(backend string, healthy bool) {
	m.send(metricOp{kind: opHealth, backend: backend, healthy: healthy})
}

// Render returns the current metrics in Prometheus text format.
func (m *Metrics) Render() string {
	reply := make(chan string, 1)
	select {
	case m.ops <- metricOp{kind: opScrape, reply: reply}:
		return <-reply
	case <-time.After(2 * time.Second):
		return "# metrics owner unavailable\n"
	}
}

// send is non-blocking: under extreme load a metric update is dropped rather
// than stalling the request path.
func (m *Metrics) send(op metricOp) {
	select {
	case m.ops <- op:
	default:
	}
}

func render(requests map[requestKey]int64, buckets []int64, latencySum float64, latencyCount, inFlight, failovers int64, health map[string]float64) string {
	var b strings.Builder

	b.WriteString("# HELP llmrouter_requests_total Total requests by backend, model, and status.\n")
	b.WriteString("# TYPE llmrouter_requests_total counter\n")
	keys := make([]requestKey, 0, len(requests))
	for k := range requests {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].backend != keys[j].backend {
			return keys[i].backend < keys[j].backend
		}
		if keys[i].model != keys[j].model {
			return keys[i].model < keys[j].model
		}
		return keys[i].status < keys[j].status
	})
	for _, k := range keys {
		fmt.Fprintf(&b, "llmrouter_requests_total{backend=\"%s\",model=\"%s\",status=\"%d\"} %d\n",
			esc(k.backend), esc(k.model), k.status, requests[k])
	}

	b.WriteString("# HELP llmrouter_request_latency_seconds Request latency.\n")
	b.WriteString("# TYPE llmrouter_request_latency_seconds histogram\n")
	var cumulative int64
	for i, bound := range latencyBounds {
		cumulative += buckets[i]
		fmt.Fprintf(&b, "llmrouter_request_latency_seconds_bucket{le=%q} %d\n",
			strconv.FormatFloat(bound, 'g', -1, 64), cumulative)
	}
	cumulative += buckets[len(buckets)-1]
	fmt.Fprintf(&b, "llmrouter_request_latency_seconds_bucket{le=\"+Inf\"} %d\n", cumulative)
	fmt.Fprintf(&b, "llmrouter_request_latency_seconds_sum %s\n", strconv.FormatFloat(latencySum, 'g', -1, 64))
	fmt.Fprintf(&b, "llmrouter_request_latency_seconds_count %d\n", latencyCount)

	b.WriteString("# HELP llmrouter_in_flight In-flight requests.\n")
	b.WriteString("# TYPE llmrouter_in_flight gauge\n")
	fmt.Fprintf(&b, "llmrouter_in_flight %d\n", inFlight)

	b.WriteString("# HELP llmrouter_failovers_total Total failovers across requests.\n")
	b.WriteString("# TYPE llmrouter_failovers_total counter\n")
	fmt.Fprintf(&b, "llmrouter_failovers_total %d\n", failovers)

	b.WriteString("# HELP llmrouter_backend_health Backend health (1 healthy, 0 down).\n")
	b.WriteString("# TYPE llmrouter_backend_health gauge\n")
	hkeys := make([]string, 0, len(health))
	for k := range health {
		hkeys = append(hkeys, k)
	}
	sort.Strings(hkeys)
	for _, k := range hkeys {
		fmt.Fprintf(&b, "llmrouter_backend_health{backend=\"%s\"} %g\n", esc(k), health[k])
	}

	return b.String()
}

// esc escapes a Prometheus label value (backslash, quote, newline).
func esc(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}
