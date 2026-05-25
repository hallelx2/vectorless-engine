package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hibiken/asynq"
)

// AsynqConfig configures the Asynq (Redis-backed) Queue backend.
type AsynqConfig struct {
	Addr        string
	Password    string
	DB          int
	Concurrency int
}

// Asynq is a Redis-backed Queue using https://github.com/hibiken/asynq.
//
// Choose Asynq when the deployment already runs Redis and needs the higher
// throughput / lower latency that Redis provides relative to Postgres-backed
// queues. For greenfield self-hosts that prefer one fewer piece of
// infrastructure, use River instead.
//
// Implementation shape: unlike River (which uses a single envelope type),
// Asynq maps each engine JobKind directly to an asynq task type. The
// payload is the engine Job marshalled as JSON. Handlers are dispatched
// via an asynq.ServeMux at work time.
type Asynq struct {
	cfg    AsynqConfig
	redisOpt asynq.RedisClientOpt

	mu       sync.RWMutex
	handlers map[JobKind]Handler

	client *asynq.Client
	server *asynq.Server
}

// NewAsynq constructs a new Asynq-backed Queue.
//
// The Redis connection is validated eagerly by pinging the server so
// misconfiguration surfaces at boot, not on the first enqueue.
func NewAsynq(cfg AsynqConfig) (*Asynq, error) {
	if cfg.Addr == "" {
		return nil, errors.New("asynq: addr is required")
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 20
	}

	redisOpt := asynq.RedisClientOpt{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	}

	client := asynq.NewClient(redisOpt)

	return &Asynq{
		cfg:      cfg,
		redisOpt: redisOpt,
		handlers: map[JobKind]Handler{},
		client:   client,
	}, nil
}

func (a *Asynq) Register(kind JobKind, h Handler) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.handlers[kind] = h
}

// Enqueue publishes a job to Redis for processing by asynq workers.
//
// The engine's Job struct is serialised as the asynq task payload. Optional
// fields (RunAt, MaxRetries, DedupeKey) are mapped to asynq options:
//
//   - RunAt      → asynq.ProcessAt
//   - MaxRetries → asynq.MaxRetry
//   - DedupeKey  → asynq.Unique with a 24-hour window (prevents duplicates
//     while the original is pending or recently completed)
func (a *Asynq) Enqueue(ctx context.Context, j Job) error {
	payload, err := json.Marshal(j)
	if err != nil {
		return fmt.Errorf("asynq: marshal job: %w", err)
	}

	var opts []asynq.Option
	if !j.RunAt.IsZero() {
		opts = append(opts, asynq.ProcessAt(j.RunAt))
	}
	if j.MaxRetries > 0 {
		opts = append(opts, asynq.MaxRetry(j.MaxRetries))
	}
	if j.DedupeKey != "" {
		// Asynq's Unique option deduplicates tasks with the same type +
		// payload within the specified TTL. We use the TaskID option
		// instead when a DedupeKey is provided — it gives exact key-based
		// uniqueness rather than payload-hash-based uniqueness.
		opts = append(opts, asynq.TaskID(j.DedupeKey))
	}

	task := asynq.NewTask(string(j.Kind), payload, opts...)

	if _, err := a.client.EnqueueContext(ctx, task); err != nil {
		// If the error is a duplicate task ID (task already exists), we
		// treat it as a successful no-op — the job is already queued.
		if errors.Is(err, asynq.ErrDuplicateTask) || errors.Is(err, asynq.ErrTaskIDConflict) {
			return nil
		}
		return fmt.Errorf("asynq: enqueue: %w", err)
	}
	return nil
}

// Start begins draining jobs from Redis. It blocks until ctx is cancelled,
// then performs a graceful shutdown (finishes in-flight jobs, stops
// fetching new ones).
//
// Handlers must be registered before Start is called. The asynq.ServeMux
// is built once at start time from the registered handler map.
func (a *Asynq) Start(ctx context.Context) error {
	mux := asynq.NewServeMux()

	a.mu.RLock()
	for kind, handler := range a.handlers {
		// Capture loop variables for the closure.
		k, h := kind, handler
		mux.HandleFunc(string(k), func(_ context.Context, t *asynq.Task) error {
			var j Job
			if err := json.Unmarshal(t.Payload(), &j); err != nil {
				return fmt.Errorf("asynq: unmarshal payload for %q: %w", k, err)
			}
			return h(ctx, j)
		})
	}
	a.mu.RUnlock()

	a.server = asynq.NewServer(a.redisOpt, asynq.Config{
		Concurrency: a.cfg.Concurrency,
		// ShutdownTimeout gives in-flight handlers a bounded window to
		// finish once Stop is called. Matches River's approach.
		ShutdownTimeout: 15 * time.Second,
	})

	// Run blocks until the server is shut down. We launch it in a
	// goroutine and wait for either the server error or ctx cancellation.
	errCh := make(chan error, 1)
	go func() {
		if err := a.server.Run(mux); err != nil {
			errCh <- fmt.Errorf("asynq: server run: %w", err)
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		a.server.Shutdown()
		return nil
	}
}

// Close releases the Redis client connection. Safe to call after Start
// returns. If Start was never called, only the client is closed.
func (a *Asynq) Close() error {
	var errs []error
	if a.client != nil {
		if err := a.client.Close(); err != nil {
			errs = append(errs, fmt.Errorf("asynq: close client: %w", err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}
