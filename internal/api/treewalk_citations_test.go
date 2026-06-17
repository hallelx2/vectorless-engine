package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"testing"

	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// stubTOCProvider returns canned TOC bytes (or an error) for the
// citation builder's heading-path lookup.
type stubTOCProvider struct {
	raw []byte
	err error
}

func (s stubTOCProvider) GetTOC(context.Context, tree.DocumentID) ([]byte, error) {
	return s.raw, s.err
}

// citationTestTOC mirrors buildTreeWalkTestTree's page layout as a
// logical outline: Setup{Install 1-2, Configuration 3-4},
// Usage{Querying 5-7, Debt 8-9}.
func citationTestTOC(t *testing.T) []byte {
	t.Helper()
	toc := []tree.TOCNode{
		{Title: "Setup", StartPage: 1, EndPage: 4, Nodes: []tree.TOCNode{
			{Title: "Install", StartPage: 1, EndPage: 2},
			{Title: "Configuration", StartPage: 3, EndPage: 4},
		}},
		{Title: "Usage", StartPage: 5, EndPage: 9, Nodes: []tree.TOCNode{
			{Title: "Querying", StartPage: 5, EndPage: 7},
			{Title: "Debt", StartPage: 8, EndPage: 9},
		}},
	}
	raw, err := json.Marshal(toc)
	if err != nil {
		t.Fatalf("marshal toc: %v", err)
	}
	return raw
}

// depsWithTOC builds a minimal Deps for buildTreeWalkCitations: no LLM
// (so quote extraction is skipped) and a stub TOC provider on the
// strategy.
func depsWithTOC(toc []byte, tocErr error) Deps {
	return Deps{
		Logger: slog.Default(),
		TreeWalkStrategy: &retrieval.TreeWalkStrategy{
			TOC: stubTOCProvider{raw: toc, err: tocErr},
		},
	}
}

// TestBuildTreeWalkCitations_EmitsHeadingPath is the HAL-70 regression:
// a citation must carry the canonical heading path of its primary
// section, resolved from the TOC — the field the bench's
// path_correct@1 metric reads.
func TestBuildTreeWalkCitations_EmitsHeadingPath(t *testing.T) {
	d := depsWithTOC(citationTestTOC(t), nil)
	tr := buildTreeWalkTestTree()
	res := &retrieval.Result{CitedPages: [][2]int{{1, 2}}}

	cites := d.buildTreeWalkCitations(context.Background(), tr, res, "how do I install?", "")
	if len(cites) != 1 {
		t.Fatalf("want 1 citation, got %d: %v", len(cites), cites)
	}
	c := cites[0]

	// Primary section for pages 1-2 is the leaf sec_a1 (Install).
	ids, _ := c["section_ids"].([]tree.SectionID)
	if len(ids) == 0 || ids[0] != "sec_a1" {
		t.Fatalf("expected primary section sec_a1, got %v", c["section_ids"])
	}

	got, ok := c["title_path"].([]string)
	if !ok {
		t.Fatalf("citation is missing a title_path: %#v", c)
	}
	if want := []string{"Setup", "Install"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("title_path mismatch: got=%v want=%v", got, want)
	}
}

// TestBuildTreeWalkCitations_NoTOCOmitsHeadingPath: when no TOC is
// persisted (ErrNoTOC), the citation degrades gracefully — section_ids
// and pages are still present, title_path is simply absent.
func TestBuildTreeWalkCitations_NoTOCOmitsHeadingPath(t *testing.T) {
	d := depsWithTOC(nil, retrieval.ErrNoTOC)
	tr := buildTreeWalkTestTree()
	res := &retrieval.Result{CitedPages: [][2]int{{5, 7}}}

	cites := d.buildTreeWalkCitations(context.Background(), tr, res, "how do I query?", "")
	if len(cites) != 1 {
		t.Fatalf("want 1 citation, got %d", len(cites))
	}
	if _, present := cites[0]["title_path"]; present {
		t.Fatalf("title_path must be absent without a TOC, got %v", cites[0]["title_path"])
	}
	if _, present := cites[0]["section_ids"]; !present {
		t.Fatalf("section_ids must still be present")
	}
}
