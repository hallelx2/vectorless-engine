package ingest

import (
	"context"
	"strings"
	"testing"

	"github.com/hallelx2/vectorless-engine/pkg/pincite"
	"github.com/hallelx2/vectorless-engine/pkg/storage"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// Every object a document writes, at the keys the pipeline and the pincite
// service actually use, is gone after PurgeDocument; a neighbour's are not.
func TestPurgeDocumentRemovesEveryStoredObject(t *testing.T) {
	st, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	objects := func(id tree.DocumentID) []string {
		return []string{
			SourceKey(id, "report.pdf"),
			"documents/" + string(id) + "/sections/sec-1.txt",
			PagesKey(id),
			pincite.LayoutKey(string(id)),
			pincite.PageKey(string(id), 3),
		}
	}
	gone, kept := tree.DocumentID("doc-1"), tree.DocumentID("doc-10")
	for _, k := range append(objects(gone), objects(kept)...) {
		if err := st.Put(ctx, k, strings.NewReader("x"), storage.Metadata{}); err != nil {
			t.Fatal(err)
		}
	}

	n, err := PurgeDocument(ctx, st, gone)
	if err != nil {
		t.Fatalf("PurgeDocument: %v", err)
	}
	if n != len(objects(gone)) {
		t.Errorf("removed %d objects, want %d", n, len(objects(gone)))
	}
	for _, k := range objects(gone) {
		if ok, _ := st.Exists(ctx, k); ok {
			t.Errorf("%s survived the purge", k)
		}
	}
	for _, k := range objects(kept) {
		if ok, _ := st.Exists(ctx, k); !ok {
			t.Errorf("%s belongs to another document and was removed", k)
		}
	}
	if _, err := PurgeDocument(ctx, st, ""); err == nil {
		t.Error("PurgeDocument with an empty id must refuse")
	}
}
