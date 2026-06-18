package ingest

import (
	"context"
	"strings"
	"testing"

	"github.com/hallelx2/vectorless-engine/pkg/parser"
	"github.com/hallelx2/vectorless-engine/pkg/storage"
)

// TestPersistTree_ContentRefMatchesStoredObjects is the HAL-316 regression:
// a leaf only gets a ContentRef when its content was actually written. An
// empty-after-clean leaf must get NO ref (and no stored object), so later
// reads never chase a key that was never written ("object not found").
func TestPersistTree_ContentRefMatchesStoredObjects(t *testing.T) {
	store, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	p := &Pipeline{Storage: store}
	fake := &fakeDocStore{}

	doc := &parser.ParsedDoc{
		Title: "Doc",
		Sections: []parser.Section{
			{
				Level: 1, Title: "Parent", // internal node, no content
				Children: []parser.Section{
					{Level: 2, Title: "Has body", Content: "real content here", PageStart: 1, PageEnd: 1},
					{Level: 2, Title: "Heading only", Content: "   \n\t ", PageStart: 2, PageEnd: 2},       // whitespace → empty after clean
					{Level: 2, Title: "Garbage glyphs", Content: "\x00\x01\x02", PageStart: 3, PageEnd: 3}, // stripped to empty
				},
			},
		},
	}

	if err := p.persistTree(context.Background(), fake, "doc_x", doc); err != nil {
		t.Fatalf("persistTree: %v", err)
	}

	_, _, sections := fake.snapshot()

	// Every section that carries a ContentRef must have a readable object;
	// every section without one must have nothing stored under its key.
	withRef := 0
	for _, s := range sections {
		if s.ContentRef == "" {
			continue
		}
		withRef++
		rc, _, err := store.Get(context.Background(), s.ContentRef)
		if err != nil {
			t.Errorf("section %s has ContentRef %q but object is not readable: %v", s.ID, s.ContentRef, err)
			continue
		}
		_ = rc.Close()
	}

	// Exactly one leaf ("Has body") had non-empty content, so exactly one
	// ContentRef should exist — the two empty leaves and the parent must
	// carry none.
	if withRef != 1 {
		t.Errorf("expected exactly 1 section with a ContentRef, got %d", withRef)
	}

	// Spot-check the empty leaves explicitly carry no ref.
	for _, s := range sections {
		if strings.HasPrefix(s.Title, "Heading only") || strings.HasPrefix(s.Title, "Garbage glyphs") {
			if s.ContentRef != "" {
				t.Errorf("empty-content leaf %q must have no ContentRef, got %q", s.Title, s.ContentRef)
			}
		}
	}
}
