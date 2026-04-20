package queue

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/riverqueue/river"
)

// TestRiverEnvelopeDispatch exercises the envelopeWorker.Work dispatch table
// directly, without touching Postgres. It proves the indirection between
// River's single envelope kind and the engine's JobKind space behaves — a
// registered handler is invoked on the right kind, an unregistered kind
// produces ErrUnknownKind.
func TestRiverEnvelopeDispatch(t *testing.T) {
	t.Parallel()

	q := &River{
		handlers: map[JobKind]Handler{},
	}
	w := &envelopeWorker{q: q}

	var got Job
	q.Register(KindIngestDocument, func(_ context.Context, j Job) error {
		got = j
		return nil
	})

	payload, _ := json.Marshal(map[string]string{"doc": "abc"})
	err := w.Work(context.Background(), &river.Job[envelopeArgs]{
		Args: envelopeArgs{DomainKind: KindIngestDocument, Payload: payload},
	})
	if err != nil {
		t.Fatalf("work: unexpected error: %v", err)
	}
	if got.Kind != KindIngestDocument {
		t.Errorf("got kind %q, want %q", got.Kind, KindIngestDocument)
	}
	if string(got.Payload) != string(payload) {
		t.Errorf("payload not passed through: got %q, want %q", got.Payload, payload)
	}

	err = w.Work(context.Background(), &river.Job[envelopeArgs]{
		Args: envelopeArgs{DomainKind: "never_registered"},
	})
	if err == nil {
		t.Fatal("work: expected error for unregistered kind, got nil")
	}
}

// TestRiverIntegration is a full round-trip against a real Postgres. Gated
// on TEST_DATABASE_URL so it stays out of the default `go test ./...` path
// and only runs when someone has a DB ready (docker-compose up postgres).
func TestRiverIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping River integration test")
	}

	q, err := NewRiver(RiverConfig{DatabaseURL: dsn, NumWorkers: 2})
	if err != nil {
		t.Fatalf("new river: %v", err)
	}
	defer q.Close()

	done := make(chan Job, 1)
	q.Register(KindIngestDocument, func(_ context.Context, j Job) error {
		done <- j
		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Start workers in the background.
	workerDone := make(chan error, 1)
	go func() { workerDone <- q.Start(ctx) }()

	payload, _ := json.Marshal(map[string]string{"doc": "integration"})
	if err := q.Enqueue(ctx, Job{Kind: KindIngestDocument, Payload: payload}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	select {
	case j := <-done:
		if j.Kind != KindIngestDocument {
			t.Errorf("handler saw kind %q, want %q", j.Kind, KindIngestDocument)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("handler never fired within 20s")
	}

	cancel() // triggers graceful shutdown in Start
	if err := <-workerDone; err != nil {
		t.Fatalf("start returned: %v", err)
	}
}
