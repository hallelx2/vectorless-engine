// Package reqtiming records where one API request's wall-clock time went:
// the engine's own work, or waiting on the Judge, the answer model, the
// database or document storage.
//
// A round-trip number cannot tell the engine's CPU from a provider's. The
// 37–50 s queries of September were the engine rebuilding a tokenizer on
// every count, not Jev (HAL-1708), and two sessions tuned the wrong thing
// against that number. So every query and answer response carries a
// timing object built here, per request, from the request's own context,
// rather than from log lines a benchmark would have to scrape and join.
//
// How the split is measured:
//
//   - A provider call's wait starts at its first connection attempt
//     (httptrace GetConn) and ends when the call returns. Work before the
//     request goes on the wire — building the prompt, counting tokens,
//     queueing at the concurrency limiter — is the engine's.
//   - Calls run in parallel, so waits are merged as intervals: a category's
//     wait_ms is the wall-clock time at least one of its calls was on the
//     wire, and busy_ms is the plain sum.
//   - engine_ms (prepare) is the total minus the union of every wait, so
//     engine_ms + waiting_ms == total_ms exactly, and the categories'
//     wait_ms add up to waiting_ms unless two categories overlapped.
package reqtiming

import (
	"context"
	"crypto/tls"
	"math"
	"net/http/httptrace"
	"sort"
	"sync"
	"time"
)

// Category names a thing a request can wait on.
type Category string

const (
	// Judge is the System One model (TypeSafe Jev).
	Judge Category = "judge"
	// AnswerModel is the generative model: answers, and any generative
	// navigation or extraction call.
	AnswerModel Category = "answer_model"
	// DB is Postgres.
	DB Category = "db"
	// Storage is the document store (local disk, S3, GCS).
	Storage Category = "storage"
)

var categories = []Category{Judge, AnswerModel, DB, Storage}

type interval struct{ start, end time.Time }

type call struct {
	interval
	local     time.Duration // entry to first connection attempt
	firstByte time.Duration // first connection attempt to first response byte
	attempts  int
	failed    bool
}

// Recorder collects one request's waits. Safe for concurrent use.
type Recorder struct {
	start time.Time
	now   func() time.Time

	mu    sync.Mutex
	calls map[Category][]call
}

type ctxKey struct{}

// Start returns a context carrying a new Recorder, started now.
func Start(ctx context.Context) (context.Context, *Recorder) {
	r := newRecorder(time.Now)
	return context.WithValue(ctx, ctxKey{}, r), r
}

func newRecorder(now func() time.Time) *Recorder {
	return &Recorder{start: now(), now: now, calls: map[Category][]call{}}
}

// From returns the context's Recorder, or nil. Every method on a nil
// Recorder is a no-op, so callers need not check.
func From(ctx context.Context) *Recorder {
	r, _ := ctx.Value(ctxKey{}).(*Recorder)
	return r
}

// Observe records a wait that is not an HTTP call (a database query, a
// storage read) from start until now.
func (r *Recorder) Observe(cat Category, start time.Time, err error) {
	if r == nil {
		return
	}
	end := r.now()
	r.add(cat, call{interval: interval{start, end}, attempts: 1, failed: err != nil})
}

func (r *Recorder) add(cat Category, c call) {
	if c.end.Before(c.start) {
		c.end = c.start
	}
	r.mu.Lock()
	r.calls[cat] = append(r.calls[cat], c)
	r.mu.Unlock()
}

// Span measures one provider call made with the returned context. Call
// done with the call's error when it returns.
//
// The wait is timed from the first connection attempt the call makes,
// observed through net/http/httptrace on the context, so any local work
// the client does first is not counted as waiting. A call that never
// reached the network (a cache hit, an error before sending) records no
// wait at all.
func (r *Recorder) Span(ctx context.Context, cat Category) (context.Context, func(error)) {
	if r == nil {
		return ctx, func(error) {}
	}
	entered := r.now()
	var (
		mu        sync.Mutex
		wireStart time.Time
		firstByte time.Time
		attempts  int
	)
	trace := &httptrace.ClientTrace{
		GetConn: func(string) {
			mu.Lock()
			if wireStart.IsZero() {
				wireStart = r.now()
			}
			attempts++
			mu.Unlock()
		},
		GotFirstResponseByte: func() {
			mu.Lock()
			if firstByte.IsZero() {
				firstByte = r.now()
			}
			mu.Unlock()
		},
		// Present so a TLS handshake on a fresh connection is part of
		// the wait (it follows GetConn); nothing to record.
		TLSHandshakeDone: func(tls.ConnectionState, error) {},
	}
	ctx = httptrace.WithClientTrace(ctx, trace)
	return ctx, func(err error) {
		end := r.now()
		mu.Lock()
		defer mu.Unlock()
		if wireStart.IsZero() {
			return
		}
		c := call{interval: interval{wireStart, end}, local: wireStart.Sub(entered), attempts: attempts, failed: err != nil}
		if !firstByte.IsZero() {
			c.firstByte = firstByte.Sub(wireStart)
		}
		r.add(cat, c)
	}
}

// Wait is one category's share of a request.
type Wait struct {
	// Calls is the number of calls that reached the network.
	Calls int `json:"calls"`
	// Attempts counts HTTP requests, retries included.
	Attempts int `json:"attempts,omitempty"`
	// Failed is the number of calls that returned an error.
	Failed int `json:"failed,omitempty"`
	// WaitMS is wall-clock time at least one call was outstanding.
	WaitMS float64 `json:"wait_ms"`
	// BusyMS is the sum of every call's duration; above WaitMS when
	// calls overlapped.
	BusyMS float64 `json:"busy_ms"`
	// FirstByteMS is the time from a call going on the wire to the first
	// byte of its response: the provider's own latency. Sum and max.
	FirstByteMS    float64 `json:"first_byte_ms,omitempty"`
	FirstByteMaxMS float64 `json:"first_byte_max_ms,omitempty"`
	// LocalMS is the time calls spent in the engine before going on the
	// wire (limiter queue, prompt building, token counting). It is part
	// of EngineMS, reported here so a slow client is visible.
	LocalMS float64 `json:"local_ms,omitempty"`
}

// Timing is the wire shape of a request's split.
type Timing struct {
	TotalMS float64 `json:"total_ms"`
	// EngineMS is time not spent waiting on anything below: parsing the
	// request, walking the tree, ranking, assembling the response.
	EngineMS float64 `json:"engine_ms"`
	// WaitingMS is the union of every category's waits.
	WaitingMS   float64 `json:"waiting_ms"`
	Judge       Wait    `json:"judge"`
	AnswerModel Wait    `json:"answer_model"`
	DB          Wait    `json:"db"`
	Storage     Wait    `json:"storage"`
}

// Snapshot computes the split from the start until now.
func (r *Recorder) Snapshot() Timing {
	if r == nil {
		return Timing{}
	}
	end := r.now()
	total := end.Sub(r.start)

	r.mu.Lock()
	defer r.mu.Unlock()

	var all []interval
	out := Timing{TotalMS: ms(total)}
	for _, cat := range categories {
		calls := r.calls[cat]
		var w Wait
		ivs := make([]interval, 0, len(calls))
		for _, c := range calls {
			w.Calls++
			w.Attempts += c.attempts
			if c.failed {
				w.Failed++
			}
			d := c.end.Sub(c.start)
			w.BusyMS += ms(d)
			w.FirstByteMS += ms(c.firstByte)
			if fb := ms(c.firstByte); fb > w.FirstByteMaxMS {
				w.FirstByteMaxMS = fb
			}
			w.LocalMS += ms(c.local)
			ivs = append(ivs, clip(c.interval, r.start, end))
		}
		w.WaitMS = ms(unionLen(ivs))
		w.BusyMS, w.FirstByteMS, w.LocalMS = round2(w.BusyMS), round2(w.FirstByteMS), round2(w.LocalMS)
		all = append(all, ivs...)
		switch cat {
		case Judge:
			out.Judge = w
		case AnswerModel:
			out.AnswerModel = w
		case DB:
			out.DB = w
		case Storage:
			out.Storage = w
		}
	}
	waiting := unionLen(all)
	out.WaitingMS = ms(waiting)
	out.EngineMS = ms(total - waiting)
	return out
}

func clip(iv interval, lo, hi time.Time) interval {
	if iv.start.Before(lo) {
		iv.start = lo
	}
	if iv.end.After(hi) {
		iv.end = hi
	}
	if iv.end.Before(iv.start) {
		iv.end = iv.start
	}
	return iv
}

// unionLen is the length of the union of intervals.
func unionLen(ivs []interval) time.Duration {
	if len(ivs) == 0 {
		return 0
	}
	s := append([]interval(nil), ivs...)
	sort.Slice(s, func(i, j int) bool { return s[i].start.Before(s[j].start) })
	var total time.Duration
	cur := s[0]
	for _, iv := range s[1:] {
		if !iv.start.After(cur.end) {
			if iv.end.After(cur.end) {
				cur.end = iv.end
			}
			continue
		}
		total += cur.end.Sub(cur.start)
		cur = iv
	}
	return total + cur.end.Sub(cur.start)
}

// round2 rounds a sum of millisecond values to two decimals, so adding
// many per-call figures does not print float noise (4.5599999).
func round2(v float64) float64 { return math.Round(v*100) / 100 }

// ms rounds a duration to milliseconds with two decimals, so the parts of
// a fast request still add up.
func ms(d time.Duration) float64 {
	return float64(d.Round(10*time.Microsecond)) / float64(time.Millisecond)
}
