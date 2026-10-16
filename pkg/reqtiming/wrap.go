package reqtiming

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/hallelx2/llmgate"
	"github.com/jackc/pgx/v5"

	"github.com/hallelx2/vectorless-engine/pkg/storage"
)

// Middleware starts a Recorder for every request, so any handler below it
// can report its split with From(r.Context()).Snapshot().
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, _ := Start(r.Context())
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// WrapClient times every Complete call made with a request context as an
// AnswerModel wait. CountTokens is local work and is not wrapped.
//
// Wrap outermost — outside retry and the concurrency limiter — so a call
// queued at the limiter counts as the engine's time (LocalMS), and every
// retry attempt is part of one wait.
func WrapClient(c llmgate.Client) llmgate.Client {
	if c == nil {
		return nil
	}
	return timedClient{next: c}
}

type timedClient struct{ next llmgate.Client }

func (t timedClient) Complete(ctx context.Context, req llmgate.Request) (*llmgate.Response, error) {
	ctx, done := From(ctx).Span(ctx, AnswerModel)
	resp, err := t.next.Complete(ctx, req)
	done(err)
	return resp, err
}

func (t timedClient) CountTokens(ctx context.Context, text string) (int, error) {
	return t.next.CountTokens(ctx, text)
}

// WrapJudge times every Judge call made with a request context as a Judge
// wait. Same placement rule as WrapClient.
func WrapJudge(j llmgate.Judge) llmgate.Judge {
	if j == nil {
		return nil
	}
	return timedJudge{next: j}
}

type timedJudge struct{ next llmgate.Judge }

// Active forwards the registry proxy's Active (models.ActiveJudge): nil
// when no Judge is configured, so callers still take their no-Judge path,
// and the active Judge, timed, otherwise.
func (t timedJudge) Active() llmgate.Judge {
	a, ok := t.next.(interface{ Active() llmgate.Judge })
	if !ok {
		return t
	}
	inner := a.Active()
	if inner == nil {
		return nil
	}
	return timedJudge{next: inner}
}

func (t timedJudge) Judge(ctx context.Context, req llmgate.JudgeRequest) (*llmgate.Judgment, error) {
	ctx, done := From(ctx).Span(ctx, Judge)
	res, err := t.next.Judge(ctx, req)
	done(err)
	return res, err
}

// QueryTracer is a pgx tracer that records each query made with a request
// context as a DB wait. Install it on the pool's ConnConfig.Tracer.
type QueryTracer struct{}

type queryStartKey struct{}

// TraceQueryStart implements pgx.QueryTracer.
func (QueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	if From(ctx) == nil {
		return ctx
	}
	return context.WithValue(ctx, queryStartKey{}, time.Now())
}

// TraceQueryEnd implements pgx.QueryTracer.
func (QueryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	r := From(ctx)
	if r == nil {
		return
	}
	if start, ok := ctx.Value(queryStartKey{}).(time.Time); ok {
		r.Observe(DB, start, data.Err)
	}
}

// WrapStorage records document-store reads made with a request context as
// Storage waits. A Get's wait runs until its reader is closed (or read to
// the end), because the bytes arrive while the caller reads.
func WrapStorage(s storage.Storage) storage.Storage {
	if s == nil {
		return nil
	}
	return timedStorage{Storage: s}
}

type timedStorage struct{ storage.Storage }

func (t timedStorage) Get(ctx context.Context, key string) (io.ReadCloser, storage.Metadata, error) {
	r := From(ctx)
	start := time.Now()
	rc, meta, err := t.Storage.Get(ctx, key)
	if r == nil {
		return rc, meta, err
	}
	if err != nil {
		r.Observe(Storage, start, err)
		return rc, meta, err
	}
	return &timedReader{ReadCloser: rc, rec: r, start: start}, meta, nil
}

func (t timedStorage) Exists(ctx context.Context, key string) (bool, error) {
	r := From(ctx)
	start := time.Now()
	ok, err := t.Storage.Exists(ctx, key)
	r.Observe(Storage, start, err)
	return ok, err
}

type timedReader struct {
	io.ReadCloser
	rec   *Recorder
	start time.Time
	once  sync.Once
}

func (t *timedReader) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	if err == io.EOF {
		t.finish(nil)
	} else if err != nil {
		t.finish(err)
	}
	return n, err
}

func (t *timedReader) Close() error {
	err := t.ReadCloser.Close()
	t.finish(nil)
	return err
}

func (t *timedReader) finish(err error) {
	t.once.Do(func() { t.rec.Observe(Storage, t.start, err) })
}
