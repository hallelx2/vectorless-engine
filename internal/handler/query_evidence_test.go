package handler

import (
	"context"
	"testing"

	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// evidenceStrategy is a page-based strategy stub: it picks both sections
// in document order and names page 4 (in section B) as the evidence.
type evidenceStrategy struct{}

func (evidenceStrategy) Name() string { return "judgewalk" }

func (evidenceStrategy) Select(ctx context.Context, t *tree.Tree, q string, b retrieval.ContextBudget) ([]tree.SectionID, error) {
	return []tree.SectionID{"sec_a", "sec_b"}, nil
}

func (evidenceStrategy) SelectWithCost(ctx context.Context, t *tree.Tree, q string, b retrieval.ContextBudget) (*retrieval.Result, error) {
	return &retrieval.Result{
		SelectedIDs:   []tree.SectionID{"sec_a", "sec_b"},
		EvidencePages: []retrieval.EvidencePage{{Page: 4, Title: "B", Text: "capex was $1,577 million", Confidence: 0.93}},
	}, nil
}

func TestQueryLeadsWithEvidencePages(t *testing.T) {
	h := newQueryStrategyHandler(evidenceStrategy{}, nil)
	rec, resp := doQuery(t, h, `{"document_id":"doc_x","query":"capex?"}`)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	units := resp["sections"].([]any)
	if len(units) != 3 {
		t.Fatalf("want the evidence page plus both sections, got %d units", len(units))
	}
	first := units[0].(map[string]any)
	if first["id"] != "page_4" || first["title"] != "B · p.4" || first["page"] != float64(4) {
		t.Errorf("first unit = %v, want the evidence page", first)
	}
	if first["content"] != "capex was $1,577 million" {
		t.Errorf("evidence page content = %v", first["content"])
	}
}

// The bug HAL-3071 fixes: a small max_sections must keep the evidence, not
// the first sections of the document.
func TestMaxSectionsKeepsTheEvidence(t *testing.T) {
	h := newQueryStrategyHandler(evidenceStrategy{}, nil)
	_, resp := doQuery(t, h, `{"document_id":"doc_x","query":"capex?","max_sections":2}`)
	units := resp["sections"].([]any)
	if len(units) != 2 {
		t.Fatalf("max_sections 2 returned %d units", len(units))
	}
	if units[0].(map[string]any)["id"] != "page_4" || units[1].(map[string]any)["id"] != "sec_a" {
		t.Errorf("units = %v, want page_4 then sec_a", units)
	}
	_, one := doQuery(t, h, `{"document_id":"doc_x","query":"capex?","max_sections":1}`)
	if u := one["sections"].([]any); len(u) != 1 || u[0].(map[string]any)["id"] != "page_4" {
		t.Errorf("max_sections 1 = %v, want only the evidence page", u)
	}
}
