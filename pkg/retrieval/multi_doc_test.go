package retrieval_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// buildTree2 creates a second test tree distinct from buildTree.
func buildTree2() *tree.Tree {
	root := &tree.Section{
		ID: "sec2_root", Title: "Beta",
		Children: []*tree.Section{
			{ID: "sec2_a", ParentID: "sec2_root", Title: "Install", Summary: "getting started"},
			{ID: "sec2_b", ParentID: "sec2_root", Title: "Config", Summary: "yaml config"},
		},
	}
	return &tree.Tree{DocumentID: "doc_y", Title: "Beta", Root: root}
}

// stubLoader returns a TreeLoader that resolves known doc IDs.
func stubLoader(trees map[tree.DocumentID]*tree.Tree) retrieval.TreeLoader {
	return func(ctx context.Context, docID tree.DocumentID, orgID, storeID string) (*tree.Tree, error) {
		t, ok := trees[docID]
		if !ok {
			return nil, fmt.Errorf("document %s not found", docID)
		}
		return t, nil
	}
}

func TestMultiDocSingleDocument(t *testing.T) {
	t.Parallel()

	tr := buildTree()
	m := &mockLLM{pickIfPresent: []tree.SectionID{"sec_b"}}
	s := retrieval.NewSinglePass(m)

	loader := stubLoader(map[tree.DocumentID]*tree.Tree{
		"doc_x": tr,
	})

	md := retrieval.NewMultiDoc(s, loader)
	result, err := md.Query(context.Background(), "org_test", "",
		[]tree.DocumentID{"doc_x"},
		"how to query?",
		retrieval.ContextBudget{MaxTokens: 100000},
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	dr, ok := result.Documents["doc_x"]
	if !ok {
		t.Fatal("doc_x not in results")
	}
	if len(dr.SelectedIDs) != 1 || dr.SelectedIDs[0] != "sec_b" {
		t.Errorf("want [sec_b], got %v", dr.SelectedIDs)
	}
}

func TestMultiDocMultipleDocuments(t *testing.T) {
	t.Parallel()

	tr1 := buildTree()
	tr2 := buildTree2()

	m := &mockLLM{pickIfPresent: []tree.SectionID{"sec_b", "sec2_a"}}
	s := retrieval.NewSinglePass(m)

	loader := stubLoader(map[tree.DocumentID]*tree.Tree{
		"doc_x": tr1,
		"doc_y": tr2,
	})

	md := retrieval.NewMultiDoc(s, loader)
	result, err := md.Query(context.Background(), "org_test", "",
		[]tree.DocumentID{"doc_x", "doc_y"},
		"how to query?",
		retrieval.ContextBudget{MaxTokens: 100000, MaxParallelCalls: 4},
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	if len(result.Documents) != 2 {
		t.Fatalf("want 2 docs in results, got %d", len(result.Documents))
	}

	drX := result.Documents["doc_x"]
	if len(drX.SelectedIDs) != 1 || drX.SelectedIDs[0] != "sec_b" {
		t.Errorf("doc_x: want [sec_b], got %v", drX.SelectedIDs)
	}

	drY := result.Documents["doc_y"]
	if len(drY.SelectedIDs) != 1 || drY.SelectedIDs[0] != "sec2_a" {
		t.Errorf("doc_y: want [sec2_a], got %v", drY.SelectedIDs)
	}

	// Both docs should have been queried in parallel (at least 2 LLM calls).
	if c := atomic.LoadInt32(&m.calls); c < 2 {
		t.Errorf("want >= 2 LLM calls (one per doc), got %d", c)
	}
}

func TestMultiDocPartialFailure(t *testing.T) {
	t.Parallel()

	tr := buildTree()
	m := &mockLLM{pickIfPresent: []tree.SectionID{"sec_a"}}
	s := retrieval.NewSinglePass(m)

	// Only doc_x exists; doc_missing does not.
	loader := stubLoader(map[tree.DocumentID]*tree.Tree{
		"doc_x": tr,
	})

	md := retrieval.NewMultiDoc(s, loader)
	result, err := md.Query(context.Background(), "org_test", "",
		[]tree.DocumentID{"doc_x", "doc_missing"},
		"query",
		retrieval.ContextBudget{MaxTokens: 100000},
	)
	if err != nil {
		t.Fatalf("expected partial success, got error: %v", err)
	}

	// doc_x should succeed.
	if _, ok := result.Documents["doc_x"]; !ok {
		t.Error("doc_x should be in results")
	}

	// doc_missing should be in errors.
	if _, ok := result.Errors["doc_missing"]; !ok {
		t.Error("doc_missing should be in errors")
	}
}

func TestMultiDocAllFail(t *testing.T) {
	t.Parallel()

	m := &mockLLM{pickIfPresent: []tree.SectionID{"sec_a"}}
	s := retrieval.NewSinglePass(m)

	// No documents exist.
	loader := stubLoader(map[tree.DocumentID]*tree.Tree{})

	md := retrieval.NewMultiDoc(s, loader)
	_, err := md.Query(context.Background(), "org_test", "",
		[]tree.DocumentID{"nope1", "nope2"},
		"query",
		retrieval.ContextBudget{MaxTokens: 100000},
	)
	if err == nil {
		t.Fatal("expected error when all documents fail")
	}
}

func TestMultiDocEmptyIDs(t *testing.T) {
	t.Parallel()

	m := &mockLLM{}
	s := retrieval.NewSinglePass(m)
	loader := stubLoader(map[tree.DocumentID]*tree.Tree{})

	md := retrieval.NewMultiDoc(s, loader)
	_, err := md.Query(context.Background(), "org_test", "",
		nil,
		"query",
		retrieval.ContextBudget{MaxTokens: 100000},
	)
	if err == nil {
		t.Fatal("expected error for empty doc IDs")
	}
}

func TestMultiDocAllSelectedIDs(t *testing.T) {
	t.Parallel()

	result := &retrieval.MultiDocResult{
		Documents: map[tree.DocumentID]*retrieval.DocResult{
			"doc1": {SelectedIDs: []tree.SectionID{"a", "b"}},
			"doc2": {SelectedIDs: []tree.SectionID{"x"}},
		},
	}

	all := result.AllSelectedIDs()
	if len(all) != 3 {
		t.Fatalf("want 3 composite IDs, got %d: %v", len(all), all)
	}
	// Should be sorted.
	for i := 1; i < len(all); i++ {
		if all[i] < all[i-1] {
			t.Errorf("not sorted: %v", all)
		}
	}
}

func TestMultiDocStream(t *testing.T) {
	t.Parallel()

	tr1 := buildTree()
	tr2 := buildTree2()

	m := &mockLLM{pickIfPresent: []tree.SectionID{"sec_b", "sec2_a"}}
	s := retrieval.NewSinglePass(m)

	loader := stubLoader(map[tree.DocumentID]*tree.Tree{
		"doc_x": tr1,
		"doc_y": tr2,
	})

	md := retrieval.NewMultiDoc(s, loader)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ch := md.QueryStream(ctx, "org_test", "",
		[]tree.DocumentID{"doc_x", "doc_y"},
		"how to query?",
		retrieval.ContextBudget{MaxTokens: 100000, MaxParallelCalls: 4},
	)

	var events []retrieval.MultiDocStreamEvent
	for ev := range ch {
		events = append(events, ev)
	}

	if len(events) == 0 {
		t.Fatal("expected at least some stream events")
	}

	// Check that we got events from both documents.
	docsSeen := map[tree.DocumentID]bool{}
	for _, ev := range events {
		docsSeen[ev.DocumentID] = true
	}
	if !docsSeen["doc_x"] || !docsSeen["doc_y"] {
		t.Errorf("expected events from both docs, saw: %v", docsSeen)
	}
}
