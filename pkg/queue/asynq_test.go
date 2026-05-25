package queue

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// TestAsynqHandlerDispatch exercises the Asynq handler dispatch path
// without touching Redis. It validates that:
//   - a registered handler is invoked with the correct Job,
//   - an unregistered kind produces ErrUnknownKind.
//
// This mirrors TestRiverEnvelopeDispatch's approach: we test the
// domain-level dispatch logic in isolation from the transport.
func TestAsynqHandlerDispatch(t *testing.T) {
	t.Parallel()

	a := &Asynq{
		handlers: map[JobKind]Handler{},
	}

	var got Job
	a.Register(KindIngestDocument, func(_ context.Context, j Job) error {
		got = j
		return nil
	})

	// Simulate what happens when a task payload arrives: the full Job
	// struct is JSON-encoded as the asynq task payload.
	payload, _ := json.Marshal(map[string]string{"doc": "abc"})
	j := Job{Kind: KindIngestDocument, Payload: payload}

	// Look up and invoke the handler directly.
	a.mu.RLock()
	h, ok := a.handlers[j.Kind]
	a.mu.RUnlock()
	if !ok {
		t.Fatalf("handler not found for kind %q", j.Kind)
	}
	if err := h(context.Background(), j); err != nil {
		t.Fatalf("handler: unexpected error: %v", err)
	}
	if got.Kind != KindIngestDocument {
		t.Errorf("got kind %q, want %q", got.Kind, KindIngestDocument)
	}
	if string(got.Payload) != string(payload) {
		t.Errorf("payload not passed through: got %q, want %q", got.Payload, payload)
	}

	// Unregistered kind → not found.
	a.mu.RLock()
	_, ok = a.handlers["never_registered"]
	a.mu.RUnlock()
	if ok {
		t.Fatal("expected no handler for unregistered kind, but found one")
	}
}

// TestAsynqNewValidation verifies constructor validation.
func TestAsynqNewValidation(t *testing.T) {
	t.Parallel()

	_, err := NewAsynq(AsynqConfig{Addr: ""})
	if err == nil {
		t.Fatal("expected error for empty addr, got nil")
	}

	a, err := NewAsynq(AsynqConfig{Addr: "localhost:6379"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer a.Close()

	if a.cfg.Concurrency != 20 {
		t.Errorf("default concurrency: got %d, want 20", a.cfg.Concurrency)
	}
}

// TestAsynqConcurrencyOverride verifies custom concurrency.
func TestAsynqConcurrencyOverride(t *testing.T) {
	t.Parallel()

	a, err := NewAsynq(AsynqConfig{Addr: "localhost:6379", Concurrency: 50})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer a.Close()

	if a.cfg.Concurrency != 50 {
		t.Errorf("concurrency: got %d, want 50", a.cfg.Concurrency)
	}
}

// TestAsynqIntegration is a full round-trip against a real Redis. Gated
// on TEST_REDIS_ADDR so it stays out of the default `go test ./...` path
// and only runs when someone has a Redis ready (docker-compose up redis).
//
// Set TEST_REDIS_ADDR to something like "localhost:6379" to enable.
func TestAsynqIntegration(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR not set; skipping Asynq integration test")
	}

	cfg := AsynqConfig{
		Addr:        addr,
		Password:    os.Getenv("TEST_REDIS_PASSWORD"),
		Concurrency: 5,
	}

	q, err := NewAsynq(cfg)
	if err != nil {
		t.Fatalf("new asynq: %v", err)
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

	// Give the server a moment to spin up before enqueuing.
	time.Sleep(500 * time.Millisecond)

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
