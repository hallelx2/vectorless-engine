package reqtiming

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/storage"
)

// fakeClock hands out times the test controls.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func TestSnapshotSplitsEngineFromWaits(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	r := newRecorder(clk.Now)

	// 0–10ms engine work, then a 30ms judge call, then 5ms engine, then a
	// 20ms answer call, then 5ms engine: total 70, waiting 50, engine 20.
	clk.Advance(10 * time.Millisecond)
	start := clk.Now()
	clk.Advance(30 * time.Millisecond)
	r.add(Judge, call{interval: interval{start, clk.Now()}, attempts: 1})
	clk.Advance(5 * time.Millisecond)
	start = clk.Now()
	clk.Advance(20 * time.Millisecond)
	r.add(AnswerModel, call{interval: interval{start, clk.Now()}, attempts: 1})
	clk.Advance(5 * time.Millisecond)

	s := r.Snapshot()
	if s.TotalMS != 70 || s.WaitingMS != 50 || s.EngineMS != 20 {
		t.Fatalf("total/waiting/engine = %v/%v/%v, want 70/50/20", s.TotalMS, s.WaitingMS, s.EngineMS)
	}
	if s.Judge.WaitMS != 30 || s.AnswerModel.WaitMS != 20 || s.Judge.Calls != 1 {
		t.Errorf("judge/answer wait = %v/%v", s.Judge.WaitMS, s.AnswerModel.WaitMS)
	}
	if s.EngineMS+s.WaitingMS != s.TotalMS {
		t.Errorf("engine + waiting must equal total exactly")
	}
}

func TestParallelCallsMergeIntoOneWait(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	r := newRecorder(clk.Now)
	t0 := clk.Now()
	// Two judge calls overlapping: [0,40] and [10,50] → wall 50, busy 80.
	r.add(Judge, call{interval: interval{t0, t0.Add(40 * time.Millisecond)}})
	r.add(Judge, call{interval: interval{t0.Add(10 * time.Millisecond), t0.Add(50 * time.Millisecond)}})
	clk.Advance(60 * time.Millisecond)

	s := r.Snapshot()
	if s.Judge.WaitMS != 50 || s.Judge.BusyMS != 80 || s.Judge.Calls != 2 {
		t.Errorf("wait/busy/calls = %v/%v/%d, want 50/80/2", s.Judge.WaitMS, s.Judge.BusyMS, s.Judge.Calls)
	}
	if s.EngineMS != 10 {
		t.Errorf("engine = %v, want 10", s.EngineMS)
	}
}

func TestNilRecorderIsANoOp(t *testing.T) {
	var r *Recorder
	ctx, done := r.Span(context.Background(), Judge)
	done(nil)
	r.Observe(DB, time.Now(), nil)
	if got := r.Snapshot(); got != (Timing{}) {
		t.Errorf("nil Snapshot = %+v", got)
	}
	if From(ctx) != nil {
		t.Error("no recorder expected on a plain context")
	}
}

// A real HTTP call through Span: the wait starts when the request goes on
// the wire, so time spent before that (here, a sleep standing in for a
// limiter queue) is local, not waiting.
func TestSpanTimesTheWireNotTheQueue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(40 * time.Millisecond)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	ctx, r := Start(context.Background())
	sctx, done := r.Span(ctx, AnswerModel)
	time.Sleep(30 * time.Millisecond) // queued before sending
	req, _ := http.NewRequestWithContext(sctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	done(nil)

	s := r.Snapshot()
	a := s.AnswerModel
	if a.Calls != 1 || a.Attempts != 1 {
		t.Fatalf("calls/attempts = %d/%d", a.Calls, a.Attempts)
	}
	if a.WaitMS < 35 || a.WaitMS > 200 {
		t.Errorf("wait = %vms, want ~40 (the server's time)", a.WaitMS)
	}
	if a.LocalMS < 25 {
		t.Errorf("local = %vms, want the ~30ms spent before sending", a.LocalMS)
	}
	if a.FirstByteMS < 35 {
		t.Errorf("first byte = %vms, want ~40", a.FirstByteMS)
	}
	if math.Abs(s.EngineMS+s.WaitingMS-s.TotalMS) > 0.02 {
		t.Errorf("parts %v+%v do not sum to total %v", s.EngineMS, s.WaitingMS, s.TotalMS)
	}
}

func TestSpanWithoutNetworkRecordsNothing(t *testing.T) {
	ctx, r := Start(context.Background())
	_, done := r.Span(ctx, Judge)
	done(errors.New("rejected before sending"))
	if s := r.Snapshot(); s.Judge.Calls != 0 {
		t.Errorf("a call that never reached the network recorded %d calls", s.Judge.Calls)
	}
}

type stubClient struct{ fn func(ctx context.Context) }

func (s stubClient) Complete(ctx context.Context, _ llmgate.Request) (*llmgate.Response, error) {
	s.fn(ctx)
	return &llmgate.Response{Content: "ok"}, nil
}
func (stubClient) CountTokens(context.Context, string) (int, error) { return 1, nil }

type stubJudge struct{ fn func(ctx context.Context) }

func (s stubJudge) Judge(ctx context.Context, _ llmgate.JudgeRequest) (*llmgate.Judgment, error) {
	s.fn(ctx)
	return &llmgate.Judgment{}, nil
}

func hit(t *testing.T, url string) func(ctx context.Context) {
	return func(ctx context.Context) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		_ = resp.Body.Close()
	}
}

func TestWrappersAttributeToTheirCategory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(10 * time.Millisecond)
	}))
	defer srv.Close()

	ctx, r := Start(context.Background())
	if _, err := WrapClient(stubClient{fn: hit(t, srv.URL)}).Complete(ctx, llmgate.Request{}); err != nil {
		t.Fatal(err)
	}
	if _, err := WrapJudge(stubJudge{fn: hit(t, srv.URL)}).Judge(ctx, llmgate.JudgeRequest{}); err != nil {
		t.Fatal(err)
	}
	s := r.Snapshot()
	if s.AnswerModel.Calls != 1 || s.Judge.Calls != 1 {
		t.Errorf("answer/judge calls = %d/%d, want 1/1", s.AnswerModel.Calls, s.Judge.Calls)
	}
	if WrapClient(nil) != nil || WrapJudge(nil) != nil || WrapStorage(nil) != nil {
		t.Error("wrapping nil must stay nil so 'not configured' checks keep working")
	}
}

type memStore struct {
	storage.Storage
	data string
}

func (m memStore) Get(context.Context, string) (io.ReadCloser, storage.Metadata, error) {
	return io.NopCloser(strings.NewReader(m.data)), storage.Metadata{}, nil
}

func TestStorageReadCountsUntilClose(t *testing.T) {
	ctx, r := Start(context.Background())
	rc, _, err := WrapStorage(memStore{data: "hello"}).Get(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "hello" {
		t.Fatalf("read %q", b)
	}
	if s := r.Snapshot(); s.Storage.Calls != 1 {
		t.Errorf("storage calls = %d, want exactly 1 (EOF and Close must not double count)", s.Storage.Calls)
	}
}

func TestMiddlewareInstallsARecorder(t *testing.T) {
	var got *Recorder
	h := Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = From(r.Context()) }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/query", nil))
	if got == nil {
		t.Fatal("handler saw no recorder")
	}
}
