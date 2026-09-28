// Package judgestats collects typesafe.RequestTrace records from a
// benchmark run and summarises where Judge request time went — local
// preparation against the provider's own time — so a latency number is
// never again reported without saying whose latency it is (HAL-1708).
package judgestats

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/hallelx2/llmgate/judge/typesafe"
)

// Recorder accumulates traces; safe for concurrent use.
type Recorder struct {
	mu     sync.Mutex
	traces []typesafe.RequestTrace
}

// Observe is a typesafe.Config.OnRequest hook.
func (r *Recorder) Observe(tr typesafe.RequestTrace) {
	r.mu.Lock()
	r.traces = append(r.traces, tr)
	r.mu.Unlock()
}

// Summary writes one block of percentiles to w.
func (r *Recorder) Summary(w io.Writer) {
	r.mu.Lock()
	ts := append([]typesafe.RequestTrace(nil), r.traces...)
	r.mu.Unlock()
	if len(ts) == 0 {
		return
	}
	pick := func(f func(typesafe.RequestTrace) time.Duration) []time.Duration {
		out := make([]time.Duration, len(ts))
		for i, t := range ts {
			out[i] = f(t)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	pct := func(ds []time.Duration, p float64) time.Duration {
		return ds[min(len(ds)-1, int(p*float64(len(ds))))]
	}
	counted, reused, failed := 0, 0, 0
	for _, t := range ts {
		if t.GuardCounted {
			counted++
		}
		if t.ConnReused {
			reused++
		}
		if t.Err != nil {
			failed++
		}
	}
	fmt.Fprintf(w, "\njudge requests %d (failed %d, tokenised by the guard %d, on a reused connection %d)\n", len(ts), failed, counted, reused)
	for _, row := range []struct {
		name string
		ds   []time.Duration
	}{
		{"prepare (local)", pick(func(t typesafe.RequestTrace) time.Duration { return t.Prepare })},
		{"first byte (provider)", pick(func(t typesafe.RequestTrace) time.Duration { return t.FirstByte })},
		{"total", pick(func(t typesafe.RequestTrace) time.Duration { return t.Total })},
	} {
		fmt.Fprintf(w, "  %-22s p50 %7.0fms  p95 %7.0fms  max %7.0fms\n", row.name,
			ms(pct(row.ds, 0.5)), ms(pct(row.ds, 0.95)), ms(row.ds[len(row.ds)-1]))
	}
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
