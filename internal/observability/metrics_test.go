package observability

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestRenderPipeInLabels covers Linus's audit note: requests_total is keyed by a
// struct, not a delimited string, so a '|' (or any byte) inside a backend or model
// id must survive into Render() verbatim and must NOT collide with a different
// id pair. A naive "backend|model|status" string key would fold {a, b|c} and
// {a|b, c} into the same series and double-count one while losing the other; the
// struct key keeps them distinct. Both ops and the scrape travel the same channel
// to the single owner goroutine, so FIFO ordering makes this deterministic.
func TestRenderPipeInLabels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := New(ctx)

	// Two distinct (backend, model) pairs that share the same '|'-joined string.
	m.RequestDone("a", "b|c", 200, 10*time.Millisecond)
	m.RequestDone("a|b", "c", 200, 10*time.Millisecond)
	// A third series with a '|' in the model only, at a different status, to make
	// sure the status column is not where the split corrupts either.
	m.RequestDone("svc", "gpt|4", 500, 10*time.Millisecond)

	out := m.Render()

	cases := []struct {
		name string
		want string
	}{
		{
			name: "backend a, model b|c counted once and uncorrupted",
			want: `llmrouter_requests_total{backend="a",model="b|c",status="200"} 1`,
		},
		{
			name: "backend a|b, model c counted once and uncorrupted",
			want: `llmrouter_requests_total{backend="a|b",model="c",status="200"} 1`,
		},
		{
			name: "pipe in model with a distinct status counted once",
			want: `llmrouter_requests_total{backend="svc",model="gpt|4",status="500"} 1`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(out, tc.want) {
				t.Fatalf("Render() missing line %q\nfull output:\n%s", tc.want, out)
			}
		})
	}

	// Guard against the collision regression directly: if the two pipe pairs had
	// folded into one series it would carry a count of 2.
	if strings.Contains(out, `model="b|c",status="200"} 2`) ||
		strings.Contains(out, `model="c",status="200"} 2`) {
		t.Fatalf("Render() folded distinct '|' label pairs into one series:\n%s", out)
	}
}

// TestRenderEscapesLabelValues confirms the Prometheus label escaping that
// protects the exposition format: backslash, double-quote, and newline in an id
// are escaped, so a hostile/odd id cannot break out of the label string.
func TestRenderEscapesLabelValues(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := New(ctx)
	m.RequestDone(`back"end`, "line1\nline2", 200, time.Millisecond)

	out := m.Render()

	want := `llmrouter_requests_total{backend="back\"end",model="line1\nline2",status="200"} 1`
	if !strings.Contains(out, want) {
		t.Fatalf("Render() missing escaped line %q\nfull output:\n%s", want, out)
	}
}
