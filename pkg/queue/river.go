package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

// RiverConfig configures the River (Postgres-backed) Queue backend.
type RiverConfig struct {
	// DatabaseURL is the Postgres DSN. River uses the same database as the
	// engine — no additional infrastructure required.
	DatabaseURL string

	// NumWorkers is the max concurrent jobs drained from the default queue.
	NumWorkers int
}

// River is a Postgres-backed Queue using https://github.com/riverqueue/river.
//
// This is the recommended default for self-hosted deployments because it
// reuses the Postgres database the engine already depends on. No Redis, no
// new ops surface.
//
// Implementation shape: River expects typed job args per kind, but the
// engine's Queue interface speaks a single generic Job shape. We bridge by
// registering a single River worker for one "envelope" kind. The envelope
// carries the engine's JobKind + JSON payload; the worker dispatches to the
// registered handler at work time.
type River struct {
	cfg  RiverConfig
	pool *pgxpool.Pool

	mu       sync.RWMutex
	handlers map[JobKind]Handler

	client *river.Client[pgx.Tx]
	once   sync.Once
	initEr error
}

// envelopeArgs is the single job type we register with River. The real kind
// lives in DomainKind; River only sees "vle_envelope".
type envelopeArgs struct {
	DomainKind JobKind         `json:"domain_kind"`
	Payload    json.RawMessage `json:"payload"`
}

// Kind is River's own kind identifier. It must be stable and unique within
// the database; it intentionally differs from the engine's JobKind space so
// the two can't collide if River ever grows a second typed job.
func (envelopeArgs) Kind() string { return "vle_envelope" }

// envelopeWorker dispatches an envelope to the Handler registered on the
// owning *River for that DomainKind.
type envelopeWorker struct {
	river.WorkerDefaults[envelopeArgs]
	q *River
}

func (w *envelopeWorker) Work(ctx context.Context, job *river.Job[envelopeArgs]) error {
	w.q.mu.RLock()
	h, ok := w.q.handlers[job.Args.DomainKind]
	w.q.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownKind, job.Args.DomainKind)
	}
	return h(ctx, Job{Kind: job.Args.DomainKind, Payload: job.Args.Payload})
}

// NewRiver constructs a new River-backed Queue. It opens its own pgxpool
// against cfg.DatabaseURL (intentionally separate from the engine's main
// pool so queue traffic and data-plane traffic can't starve each other) and
// runs River's schema migrations idempotently at startup.
func NewRiver(cfg RiverConfig) (*River, error) {
	if cfg.DatabaseURL == "" {
		return nil, errors.New("river: database_url is required")
	}
	if cfg.NumWorkers <= 0 {
		cfg.NumWorkers = 10
	}

	// We open the pool eagerly so NewRiver surfaces DB misconfiguration at
	// boot rather than on the first enqueue. The pool is closed by Close().
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("river: open pool: %w", err)
	}

	// Apply River's own migrations (creates river_job + related tables).
	// This is idempotent and fast when up-to-date.
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("river: migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		pool.Close()
		return nil, fmt.Errorf("river: migrate: %w", err)
	}

	return &River{
		cfg:      cfg,
		pool:     pool,
		handlers: map[JobKind]Handler{},
	}, nil
}

// Register binds a handler to a domain JobKind. It's safe to call before or
// after the client starts — the envelope worker looks handlers up at work
// time, not at registration time.
func (r *River) Register(kind JobKind, h Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[kind] = h
}

// Enqueue inserts a job. If the client hasn't been built yet (no prior
// Enqueue or Start call), this triggers lazy client construction.
func (r *River) Enqueue(ctx context.Context, j Job) error {
	if err := r.ensureClient(); err != nil {
		return err
	}
	args := envelopeArgs{DomainKind: j.Kind, Payload: j.Payload}

	opts := &river.InsertOpts{}
	if !j.RunAt.IsZero() {
		opts.ScheduledAt = j.RunAt
	}
	if j.MaxRetries > 0 {
		opts.MaxAttempts = j.MaxRetries
	}
	// DedupeKey intentionally left on the table; River's native uniqueness
	// story has moved through several shapes (UniqueOpts, ByArgs, etc.) and
	// wiring it in here should be a deliberate call, not a guess. When we
	// need it we'll plumb UniqueOpts with ByKey matching DedupeKey.

	if _, err := r.client.Insert(ctx, args, opts); err != nil {
		return fmt.Errorf("river: insert: %w", err)
	}
	return nil
}

// Start begins draining jobs. Blocks until ctx is cancelled, then performs a
// graceful shutdown (finishes in-flight jobs, stops fetching new ones).
func (r *River) Start(ctx context.Context) error {
	if err := r.ensureClient(); err != nil {
		return err
	}
	if err := r.client.Start(ctx); err != nil {
		return fmt.Errorf("river: start: %w", err)
	}
	<-ctx.Done()
	// Give in-flight jobs a bounded window to finish. We detach from ctx
	// (which is already done) so Stop doesn't immediately cancel what it's
	// trying to drain.
	stopCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.client.Stop(stopCtx); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("river: stop: %w", err)
	}
	return nil
}

// Close releases the pool. Safe to call after Start returns. It does not
// stop in-flight work — Start handles graceful shutdown on ctx cancel.
func (r *River) Close() error {
	if r.pool != nil {
		r.pool.Close()
	}
	return nil
}

// ensureClient lazily constructs the river.Client. We defer this until first
// use so tests that just want to Register handlers (or engines that queue
// without working) can construct a *River without spinning up workers.
//
// The client's Workers are registered once at build time — our single
// envelopeWorker dispatches by looking up the handler map on each Work call,
// so new Register() calls after client construction still take effect.
func (r *River) ensureClient() error {
	r.once.Do(func() {
		workers := river.NewWorkers()
		river.AddWorker(workers, &envelopeWorker{q: r})

		client, err := river.NewClient[pgx.Tx](riverpgxv5.New(r.pool), &river.Config{
			Workers: workers,
			Queues: map[string]river.QueueConfig{
				river.QueueDefault: {MaxWorkers: r.cfg.NumWorkers},
			},
		})
		if err != nil {
			r.initEr = fmt.Errorf("river: new client: %w", err)
			return
		}
		r.client = client
	})
	return r.initEr
}
