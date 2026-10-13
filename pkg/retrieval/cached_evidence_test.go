package retrieval_test

import (
	"context"
	"testing"
	"time"

	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// pageStrategy stands in for judgewalk: a page-based strategy whose
// result carries evidence pages, cited pages and confidences.
type pageStrategy struct{ calls int }

func (p *pageStrategy) Name() string { return "pages" }

func (p *pageStrategy) Select(ctx context.Context, t *tree.Tree, q string, b retrieval.ContextBudget) ([]tree.SectionID, error) {
	r, err := p.SelectWithCost(ctx, t, q, b)
	if err != nil {
		return nil, err
	}
	return r.SelectedIDs, nil
}

func (p *pageStrategy) SelectWithCost(_ context.Context, t *tree.Tree, _ string, _ retrieval.ContextBudget) (*retrieval.Result, error) {
	p.calls++
	ids := []tree.SectionID{"sec_b"}
	return &retrieval.Result{
		SelectedIDs:   ids,
		Confidences:   map[tree.SectionID]float64{"sec_b": 0.9},
		Usage:         retrieval.Usage{LLMCalls: 3, InputTokens: 900},
		PagesRead:     []retrieval.PageReadEntry{{StartPage: 60, EndPage: 60}},
		EvidencePages: []retrieval.EvidencePage{{Page: 60, Title: "Cash flows", Text: "Purchases of PP&E (1,577) (1,373)", Confidence: 0.9}},
		CitedPages:    [][2]int{{60, 60}},
		Confidence:    0.8,
		TraceToken:    "tok-from-strategy",
	}, nil
}

// A cache hit must return what the strategy found, not just the section
// IDs. It used to rebuild a bare Result from the IDs, so the second time
// anyone asked a question the evidence pages were gone and the answer
// came back with no pincites (HAL-2846).
func TestCachedHitKeepsEvidencePages(t *testing.T) {
	inner := &pageStrategy{}
	cached := retrieval.NewCached(inner, retrieval.CachedConfig{TTL: time.Minute})
	tr := buildTree()
	budget := retrieval.ContextBudget{MaxTokens: 100000, ModelName: "m"}

	first, err := cached.SelectWithCost(context.Background(), tr, "capex?", budget)
	if err != nil {
		t.Fatal(err)
	}
	// A caller mutating its result must not reach into the cache.
	first.EvidencePages[0].Text = "mutated"
	first.Confidences["sec_b"] = 0

	hit, err := cached.SelectWithCost(context.Background(), tr, "capex?", budget)
	if err != nil {
		t.Fatal(err)
	}
	if inner.calls != 1 {
		t.Fatalf("second call should be a cache hit: strategy ran %d times", inner.calls)
	}
	if len(hit.EvidencePages) != 1 || hit.EvidencePages[0].Page != 60 || hit.EvidencePages[0].Text != "Purchases of PP&E (1,577) (1,373)" {
		t.Fatalf("evidence pages lost or aliased on a hit: %+v", hit.EvidencePages)
	}
	if len(hit.CitedPages) != 1 || len(hit.PagesRead) != 1 || hit.Confidence != 0.8 || hit.Confidences["sec_b"] != 0.9 {
		t.Fatalf("page result lost on a hit: cited=%v read=%v conf=%v confs=%v", hit.CitedPages, hit.PagesRead, hit.Confidence, hit.Confidences)
	}
	if hit.Usage.LLMCalls != 0 || hit.Usage.InputTokens != 0 {
		t.Fatalf("a hit costs nothing: usage = %+v", hit.Usage)
	}
	if hit.TraceToken != "tok-from-strategy" || hit.Reasoning != "cached" {
		t.Fatalf("hit trace token %q reasoning %q", hit.TraceToken, hit.Reasoning)
	}

	// Select on the same key returns the same IDs.
	ids, err := cached.Select(context.Background(), tr, "capex?", budget)
	if err != nil || len(ids) != 1 || ids[0] != "sec_b" || inner.calls != 1 {
		t.Fatalf("Select hit: ids=%v err=%v calls=%d", ids, err, inner.calls)
	}
}
