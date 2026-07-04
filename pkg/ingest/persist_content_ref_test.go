package ingest

import (
	"context"
	"strings"
	"testing"

	"github.com/hallelx2/vectorless-engine/pkg/parser"
	"github.com/hallelx2/vectorless-engine/pkg/storage"
)

// TestPersistTree_TitleOverrideIsSticky verifies that an explicit
// caller-supplied title is never clobbered by the parsed title, while a
// blank override still lets a usable parsed title through.
func TestPersistTree_TitleOverrideIsSticky(t *testing.T) {
	store, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	p := &Pipeline{Storage: store}
	doc := &parser.ParsedDoc{
		Title: "Some Parsed Title",
		Sections: []parser.Section{
			{Level: 1, Title: "S", Content: "body", PageStart: 1, PageEnd: 1},
		},
	}

	// With an override, persistTree must NOT push the parsed title (the row
	// already carries the override from upload time → SetDocumentTitle
	// stays uncalled, so the fake's title remains empty).
	fake := &fakeDocStore{}
	if err := p.persistTree(context.Background(), fake, "doc_x", doc, "Attention Is All You Need"); err != nil {
		t.Fatalf("persistTree (override): %v", err)
	}
	if fake.title != "" {
		t.Errorf("override present: parsed title must not overwrite it; SetDocumentTitle called with %q", fake.title)
	}

	// With no override, a usable parsed title IS applied.
	fake2 := &fakeDocStore{}
	if err := p.persistTree(context.Background(), fake2, "doc_y", doc, ""); err != nil {
		t.Fatalf("persistTree (no override): %v", err)
	}
	if fake2.title != "Some Parsed Title" {
		t.Errorf("no override: parsed title should apply, got %q", fake2.title)
	}
}

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

	if err := p.persistTree(context.Background(), fake, "doc_x", doc, ""); err != nil {
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
