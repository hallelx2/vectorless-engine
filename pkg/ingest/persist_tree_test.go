package ingest

import (
	"context"
	"strings"
	"testing"

	"github.com/hallelx2/vectorless-engine/pkg/parser"
	"github.com/hallelx2/vectorless-engine/pkg/storage"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// persistTree hands the store one batch in which every parent precedes
// its children (the parent_id foreign key needs that), stores text only
// for sections that have some, and writes every one of those objects.
func TestPersistTreeBatchesInParentFirstOrder(t *testing.T) {
	st, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var leaves []parser.Section
	for i := 0; i < 60; i++ {
		leaves = append(leaves, parser.Section{Title: "leaf", Content: strings.Repeat("text ", i+1)})
	}
	doc := &parser.ParsedDoc{Title: "Doc", Sections: []parser.Section{
		{Title: "Part I", Children: []parser.Section{
			{Title: "Item 1", Content: "body", Children: leaves},
			{Title: "Empty heading"},
		}},
		{Title: "Part II", Content: "more"},
	}}
	store := &fakeDocStore{}
	p := &Pipeline{Storage: st}
	docID := tree.DocumentID("doc_persist")
	if err := p.persistTree(context.Background(), store, docID, doc, ""); err != nil {
		t.Fatal(err)
	}
	_, _, rows := store.snapshot()
	if len(rows) != 64 { // 2 parts, Item 1, 60 leaves, the empty heading
		t.Fatalf("got %d rows, want 64", len(rows))
	}
	seen := map[tree.SectionID]bool{}
	for _, r := range rows {
		if r.ParentID != "" && !seen[r.ParentID] {
			t.Fatalf("section %s comes before its parent %s", r.ID, r.ParentID)
		}
		seen[r.ID] = true
		ok, _ := st.Exists(context.Background(), r.ContentRef)
		switch {
		case r.Title == "Empty heading" || r.Title == "Part I":
			if r.ContentRef != "" {
				t.Errorf("%q has no text but got a ContentRef", r.Title)
			}
		case r.ContentRef == "" || !ok:
			t.Errorf("%q: text not stored (ref %q)", r.Title, r.ContentRef)
		}
	}
}
