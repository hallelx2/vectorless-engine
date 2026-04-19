package queue

import (
	"context"
	"errors"
	"sync"
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
type Asynq struct {
	cfg      AsynqConfig
	mu       sync.RWMutex
	handlers map[JobKind]Handler
}

// NewAsynq constructs a new Asynq-backed Queue.
func NewAsynq(cfg AsynqConfig) (*Asynq, error) {
	if cfg.Addr == "" {
		return nil, errors.New("asynq: addr is required")
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 20
	}
	return &Asynq{
		cfg:      cfg,
		handlers: map[JobKind]Handler{},
	}, nil
}

func (a *Asynq) Register(kind JobKind, h Handler) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.handlers[kind] = h
}

func (a *Asynq) Enqueue(ctx context.Context, j Job) error {
	// TODO(phase-1): build asynq.Task(string(j.Kind), j.Payload, opts...)
	// and call client.EnqueueContext(ctx, task).
	return errors.New("asynq: Enqueue not yet implemented")
}

func (a *Asynq) Start(ctx context.Context) error {
	// TODO(phase-1): construct asynq.Server with a.cfg.Concurrency, attach
	// asynq.ServeMux with routes per registered JobKind, and Run().
	<-ctx.Done()
	return nil
}

func (a *Asynq) Close() error { return nil }
