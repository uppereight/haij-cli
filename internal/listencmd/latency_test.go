package listencmd

// Spec 30 test #86 — `haij listen` end-to-end tail latency benchmark.
//
// The spec calls for typical latency of 1.0-2.0s and a 95th-percentile
// ceiling of 3.0s under the default 1s poll interval, with zero
// dropped events. The full operator-facing test runs 100 events over
// 5 minutes; the regression test we ship in-tree is a 60-event stress
// run that completes in under 3 minutes (per the spec acceptance
// criterion: "A regression test in cli/internal/listen/latency_test.go
// runs a 60-event stress in under 3 minutes and asserts the same
// bounds").
//
// The test wires a stub /api/v1/events server that emits one event per
// second on a deterministic schedule, runs the real listencmd polling
// loop with --poll-interval=1s, and measures (received_at -
// occurred_at) for each forwarded delivery. We assert:
//
//   - Zero dropped events: count(received) == count(emitted)
//   - Median latency in [0, 2.0s] (relaxed from the spec's [1.0, 2.0s]
//     because the regression run uses simulated timestamps — production
//     events spend ~0.5s in the broker before they show up in
//     /api/v1/events; we don't simulate that broker latency here, so
//     the floor is allowed to dip below 1s).
//   - p95 latency <= 3.0s (the ceiling that operators care about).
//
// To keep CI runtime tight we use 60 events at 0.25s spacing instead
// of 60 events at 1s spacing — the per-tick math is identical and the
// regression remains meaningful.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/uppereight/haij-cli/internal/client"
)

// emittedEvent pairs an event id with the simulated wall-clock time it
// was "emitted" by the API. The stub server only returns events whose
// emittedAt is <= now.
type emittedEvent struct {
	id        string
	name      string
	emittedAt time.Time
}

func TestListenLatency_60EventStressMeetsBounds(t *testing.T) {
	if testing.Short() {
		t.Skip("listen latency stress: skip under -short")
	}

	const (
		eventCount   = 60
		emitInterval = 250 * time.Millisecond // total emit window: 15s
		// We measure latency in fractions of seconds. The bounds in
		// the spec are typical 1-2s, p95 <= 3s, but the regression
		// runs against a stub that emits at the same wall-clock as
		// the poll, so the floor relaxes (see test docstring).
		p95Ceiling = 3 * time.Second
	)

	// Generate a deterministic schedule of 60 emissions.
	start := time.Now()
	emissions := make([]emittedEvent, eventCount)
	for i := range emissions {
		emissions[i] = emittedEvent{
			id:        idForIndex(i),
			name:      "entry.updated",
			emittedAt: start.Add(time.Duration(i) * emitInterval),
		}
	}

	// Stub /api/v1/events: returns the slice of events whose
	// emittedAt <= now AND whose id sorts after since_cursor.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/events", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		cursor := r.URL.Query().Get("since_cursor")
		out := make([]map[string]any, 0)
		for _, e := range emissions {
			if e.emittedAt.After(now) {
				continue
			}
			if cursor != "" && e.id <= cursor {
				continue
			}
			out = append(out, map[string]any{
				"id":          e.id,
				"name":        e.name,
				"occurred_at": e.emittedAt.UTC().Format(time.RFC3339Nano),
			})
		}
		nextCursor := ""
		if len(out) > 0 {
			nextCursor = out[len(out)-1]["id"].(string)
		}
		body, _ := json.Marshal(map[string]any{
			"data":        out,
			"has_more":    false,
			"next_cursor": nextCursor,
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	api := httptest.NewServer(mux)
	defer api.Close()

	// Local receiver records receive-time per event.
	type recv struct {
		id         string
		occurredAt time.Time
		receivedAt time.Time
	}
	var (
		mu       sync.Mutex
		received []recv
	)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var wire struct {
			ID         string `json:"id"`
			OccurredAt string `json:"occurred_at"`
		}
		_ = json.NewDecoder(r.Body).Decode(&wire)
		t, _ := time.Parse(time.RFC3339Nano, wire.OccurredAt)
		mu.Lock()
		received = append(received, recv{
			id:         wire.ID,
			occurredAt: t,
			receivedAt: time.Now(),
		})
		mu.Unlock()
		w.WriteHeader(204)
	}))
	defer receiver.Close()

	// Drive a real pollLoop directly — bypasses RunWith so the test
	// doesn't need a profile / preflight machinery.
	flags := &flagSet{
		forwardTo:    receiver.URL,
		events:       []string{},
		pollInterval: 1 * time.Second,
	}
	c := client.New(client.Config{BaseURL: api.URL, Token: "tok"})

	// Resume from a cursor before the first emission: a listener with
	// no cursor would take the events already out as its start
	// (haij-cli#4) and forward none of them.
	cwd := t.TempDir()
	loop := newPollLoop(c, flags, "01G", cwd, &nullWriter{}, &nullWriter{}, true, RuntimeOptions{})

	// Run the loop until every event has been received OR the
	// 3-minute spec ceiling fires.
	deadline := start.Add(3 * time.Minute)
	tick := time.NewTicker(flags.pollInterval)
	defer tick.Stop()
	ctx := context.Background()

	// One immediate tick to seed.
	loop.tickOnce(ctx)

	for {
		mu.Lock()
		got := len(received)
		mu.Unlock()
		if got >= eventCount {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("listen latency: only %d/%d events received within 3-minute deadline", got, eventCount)
		}
		<-tick.C
		loop.tickOnce(ctx)
	}

	// Compute latency stats.
	mu.Lock()
	defer mu.Unlock()
	if len(received) != eventCount {
		t.Fatalf("expected %d events, got %d", eventCount, len(received))
	}
	latencies := make([]time.Duration, len(received))
	for i, r := range received {
		latencies[i] = r.receivedAt.Sub(r.occurredAt)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	median := latencies[len(latencies)/2]
	p95 := latencies[(len(latencies)*95)/100]

	t.Logf("listen latency: median=%s p95=%s (n=%d)", median, p95, len(latencies))

	if p95 > p95Ceiling {
		t.Errorf("p95 latency %s exceeds spec ceiling %s — listener is not keeping up at the default 1s poll interval", p95, p95Ceiling)
	}
	// Median in [0, 2*pollInterval]. Wider than the spec's [1s, 2s]
	// because the in-tree stub doesn't simulate broker latency.
	if median > 2*flags.pollInterval {
		t.Errorf("median latency %s exceeds 2x poll interval (%s) — typical-case path is regressed", median, 2*flags.pollInterval)
	}
}

func idForIndex(i int) string {
	// Lexicographically sortable so since_cursor comparisons work.
	return "01H" + zeroPad(i, 6)
}

func zeroPad(i, width int) string {
	s := []byte("000000")
	x := i
	for k := width - 1; k >= 0; k-- {
		s[k] = byte('0' + (x % 10))
		x /= 10
	}
	return string(s)
}

type nullWriter struct{}

func (*nullWriter) Write(p []byte) (int, error) { return len(p), nil }
