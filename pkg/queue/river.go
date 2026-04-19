package queue

import (
	"context"
	"errors"
	"sync"
)

// RiverConfig configures the River (Postgres-backed) Queue backend.
type RiverConfig struct {
	// DatabaseURL is the Postgres DSN. River uses the same database as the
	// engine — no additional infrastructure required.
	DatabaseURL string

	// NumWorkers is the size of the worker pool that drains jobs.
	NumWorkers int
}

// River is a Postgres-backed Queue using https://github.com/riverqueue/river.
//
// This is the recommended default for self-hosted deployments because it
// reuses the Postgres database the engine already depends on. No Redis, no
// new ops surface.
//
// The actual River client wiring will be added when we integrate the
// `river` + `riverpgxv5` packages. For scaffolding we keep the shape of the
// implementation so the rest of the engine can depend on it.
type River struct {
	cfg      RiverConfig
	mu       sync.RWMutex
	handlers map[JobKind]Handler
}

// NewRiver constructs a new River-backed Queue.
func NewRiver(cfg RiverConfig) (*River, error) {
	if cfg.DatabaseURL == "" {
		return nil, errors.New("river: database_url is required")
	}
	if cfg.NumWorkers <= 0 {
		cfg.NumWorkers = 10
	}
	return &River{
		cfg:      cfg,
		handlers: map[JobKind]Handler{},
	}, nil
}

func (r *River) Register(kind JobKind, h Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[kind] = h
}

func (r *River) Enqueue(ctx context.Context, j Job) error {
	// TODO(phase-1): insert into river_job via riverpgxv5.Client.Insert.
	return errors.New("river: Enqueue not yet implemented")
}

func (r *River) Start(ctx context.Context) error {
	// TODO(phase-1): start river.Client workers. Each registered JobKind
	// maps to a river.Worker[T] that calls the handler.
	<-ctx.Done()
	return nil
}

func (r *River) Close() error { return nil }
