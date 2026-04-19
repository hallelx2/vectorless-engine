// Package queue abstracts the background job queue used for ingest,
// tree construction, and summarization.
//
// Three backends ship with the engine:
//
//   - QStash: HTTP-based, ideal for serverless hosts (Vercel, Cloudflare
//     Workers, Lambda). No long-running workers required.
//   - River: Postgres-backed, the recommended default for self-hosters.
//     Reuses the engine's existing Postgres; no new infrastructure needed.
//   - Asynq: Redis-backed, higher throughput when Redis is already present.
//
// All three are interchangeable behind the Queue interface below.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// JobKind identifies a job's handler.
type JobKind string

// Well-known job kinds used by the engine.
const (
	KindIngestDocument JobKind = "ingest_document"
	KindBuildTree      JobKind = "build_tree"
	KindSummarize      JobKind = "summarize_section"
	KindReindex        JobKind = "reindex_document"
)

// Job is a unit of work.
type Job struct {
	Kind    JobKind         `json:"kind"`
	Payload json.RawMessage `json:"payload"`

	// Optional: scheduled time (zero value = run ASAP).
	RunAt time.Time `json:"run_at,omitempty"`

	// Optional: dedup key. If set, a queue MAY drop a later job with the
	// same key while an earlier one is pending or in progress.
	DedupeKey string `json:"dedupe_key,omitempty"`

	// Optional: max retries before dead-lettering.
	MaxRetries int `json:"max_retries,omitempty"`
}

// Handler processes a single job.
type Handler func(ctx context.Context, j Job) error

// Queue is the backend-agnostic contract.
//
// Implementations MUST be safe for concurrent use.
type Queue interface {
	// Enqueue schedules a job for execution.
	Enqueue(ctx context.Context, j Job) error

	// Register binds a handler to a job kind. Must be called before Start.
	Register(kind JobKind, h Handler)

	// Start begins processing jobs. It blocks until ctx is canceled or the
	// queue encounters an unrecoverable error.
	Start(ctx context.Context) error

	// Close releases resources. Safe to call after Start returns.
	Close() error
}

// ErrUnknownKind is returned by Queue implementations when a job with no
// registered handler is received.
var ErrUnknownKind = errors.New("queue: no handler registered for job kind")
