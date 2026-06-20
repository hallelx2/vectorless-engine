package db

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// openTestPool connects to the database named by VLE_TEST_DATABASE_URL and
// applies migrations, or skips the test when no test DB is configured. This
// keeps the suite green in environments without Postgres while still letting
// the idempotency guarantees be verified against a real database locally/CI.
func openTestPool(t *testing.T) *Pool {
	t.Helper()
	url := os.Getenv("VLE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("VLE_TEST_DATABASE_URL not set; skipping live-DB idempotency test")
	}
	ctx := context.Background()
	pool, err := Open(ctx, url, 4)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := pool.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestIdempotentIngestDedup(t *testing.T) {
	p := openTestPool(t)
	ctx := context.Background()

	org := "test-org-" + uuid.NewString()
	key := "vlbench:" + org + ":fb-001"
	docID := tree.DocumentID("doc_" + uuid.NewString())

	first := Document{
		ID: docID, OrgID: org, Title: "AMD 10-K", ContentType: "application/pdf",
		SourceRef: "documents/" + string(docID) + "/source.pdf", Status: StatusPending,
		ByteSize: 123, IdempotencyKey: key,
	}
	if err := p.NewDocument(ctx, first); err != nil {
		t.Fatalf("first NewDocument: %v", err)
	}
	t.Cleanup(func() { _, _ = p.Exec(ctx, `DELETE FROM documents WHERE org_id = $1`, org) })

	// Lookup returns the original.
	got, err := p.GetDocumentByIdempotencyKey(ctx, org, key)
	if err != nil {
		t.Fatalf("GetDocumentByIdempotencyKey: %v", err)
	}
	if got.ID != docID {
		t.Fatalf("looked up id = %q, want %q", got.ID, docID)
	}

	// A second insert under the same (org, key) — what a client/SDK retry would
	// attempt — must collide with ErrConflict, never create a duplicate row.
	dup := first
	dup.ID = tree.DocumentID("doc_" + uuid.NewString())
	dup.SourceRef = "documents/" + string(dup.ID) + "/source.pdf"
	if err := p.NewDocument(ctx, dup); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate insert err = %v, want ErrConflict", err)
	}

	// Exactly one row exists for this key.
	var n int
	if err := p.QueryRow(ctx,
		`SELECT count(*) FROM documents WHERE org_id = $1 AND idempotency_key = $2`,
		org, key).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows for key = %d, want 1", n)
	}
}

func TestIngestWithoutKeyAllowsDuplicates(t *testing.T) {
	p := openTestPool(t)
	ctx := context.Background()
	org := "test-org-" + uuid.NewString()
	t.Cleanup(func() { _, _ = p.Exec(ctx, `DELETE FROM documents WHERE org_id = $1`, org) })

	// No idempotency key → column is NULL → the partial unique index ignores
	// the rows, so two keyless ingests of the same bytes are allowed (today's
	// behavior is preserved for callers that don't opt in).
	for i := range 2 {
		id := tree.DocumentID("doc_" + uuid.NewString())
		err := p.NewDocument(ctx, Document{
			ID: id, OrgID: org, Title: "x", ContentType: "text/plain",
			SourceRef: "documents/" + string(id) + "/source.txt", Status: StatusPending,
		})
		if err != nil {
			t.Fatalf("keyless NewDocument %d: %v", i, err)
		}
	}
}
