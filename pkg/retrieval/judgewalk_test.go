package retrieval

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// navJudge answers leaf questions by title keyword and page questions
// by text keyword, and counts requests.
func navJudge(leafHit, pageHit string) (*llmgate.MockJudge, *int) {
	calls := 0
	j := &llmgate.MockJudge{Respond: func(_ context.Context, req llmgate.JudgeRequest) (*llmgate.Judgment, error) {
		calls++
		st := req.State.(map[string]any)
		ans := map[string]llmgate.Answer{}
		for id := range req.Questions {
			item := st[id].(map[string]any)
			p := 0.1
			if strings.HasPrefix(id, "l_") && strings.Contains(strings.ToLower(item["title"].(string)), leafHit) {
				p = 0.9
			}
			if strings.HasPrefix(id, "p_") && strings.Contains(item["text"].(string), pageHit) {
				p = 0.95
			}
			ans[id] = llmgate.NoulAnswer{Noul: p}
		}
		return &llmgate.Judgment{Model: "mock", Answers: ans, Usage: llmgate.Usage{InputTokens: 10, TotalTokens: 10, TokensReported: true}}, nil
	}}
	return j, &calls
}

func tenKLeaves() []NavLeaf {
	return []NavLeaf{
		{ID: "1", Title: "Item 1. Business", Path: "PART I > Item 1. Business", Start: 3, End: 19},
		{ID: "2", Title: "Item 1A. Risk Factors", Start: 20, End: 33},
		{ID: "7", Title: "Item 7. Management's Discussion and Analysis", Start: 36, End: 49},
		{ID: "8", Title: "Item 8. Financial Statements and Supplementary Data", Start: 52, End: 91},
		{ID: "9", Title: "Item 9. Changes in and Disagreements with Accountants", Start: 92, End: 92},
	}
}

func TestNavigateReadsTheBestLeafAndFindsTheEvidencePage(t *testing.T) {
	j, calls := navJudge("financial statements", "Total revenue 17,606")
	n := &JudgeNavigator{Judge: j, MaxLeaves: 1}
	load := func(_ context.Context, l NavLeaf) ([]NavPage, error) {
		var ps []NavPage
		for p := l.Start; p <= l.End && p < l.Start+6; p++ {
			text := "Notes to the statements, page " + strings.Repeat("x", 20)
			if p == l.Start+2 {
				text = "CONSOLIDATED STATEMENTS OF INCOME\nTotal revenue 17,606 15,785"
			}
			ps = append(ps, NavPage{Number: p, Text: text})
		}
		return ps, nil
	}
	res, err := n.Navigate(context.Background(), "What was Adobe's total revenue in FY2022?", tenKLeaves(), load)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Selected) != 1 || res.Selected[0].ID != "8" {
		t.Fatalf("selected %+v, want Item 8", res.Selected)
	}
	if len(res.Evidence) == 0 || res.Evidence[0].Page.Number != 54 {
		t.Fatalf("evidence %+v, want page 54 first", res.Evidence)
	}
	if res.Evidence[0].Page.LeafID != "8" {
		t.Errorf("evidence page should carry its leaf: %+v", res.Evidence[0].Page)
	}
	if *calls != 2 || res.Requests != 2 {
		t.Errorf("requests: mock saw %d, result says %d; want 2 (leaves, pages)", *calls, res.Requests)
	}
}

func TestNavigateNeverReturnsEmptyEvidence(t *testing.T) {
	j, _ := navJudge("nothing matches", "nothing matches")
	n := &JudgeNavigator{Judge: j, MaxLeaves: 2}
	load := func(_ context.Context, l NavLeaf) ([]NavPage, error) {
		return []NavPage{{Number: l.Start, Text: "prose"}, {Number: l.Start + 1, Text: "more prose"}}, nil
	}
	res, err := n.Navigate(context.Background(), "q", tenKLeaves(), load)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Selected) != 2 {
		t.Errorf("below-threshold leaves should still be read up to MaxLeaves: %d", len(res.Selected))
	}
	if len(res.Evidence) != navMinEvidencePages {
		t.Errorf("want the best %d pages as low-confidence evidence, got %d", navMinEvidencePages, len(res.Evidence))
	}
	if res.Evidence[0].P >= n.threshold() {
		t.Errorf("fallback evidence should carry its real (low) probability, got %v", res.Evidence[0].P)
	}
}

func TestRankPagesBatchesUnderTheRequestBudget(t *testing.T) {
	j, calls := navJudge("", "needle")
	n := &JudgeNavigator{Judge: j, RequestBudgetTokens: 1000, PageChars: 2000}
	var pages []NavPage
	for i := 0; i < 10; i++ {
		text := strings.Repeat("y", 1900)
		if i == 7 {
			text = "needle " + text
		}
		pages = append(pages, NavPage{Number: i + 1, Text: text})
	}
	scored, _, reqs, err := n.RankPages(context.Background(), "q", pages)
	if err != nil {
		t.Fatal(err)
	}
	if reqs < 5 || *calls != reqs {
		t.Errorf("10 pages of ~500 tokens under a 1000-token budget should take ≥5 requests, took %d (mock saw %d)", reqs, *calls)
	}
	if scored[0].Page.Number != 8 {
		t.Errorf("best page should be the one with the needle, got %d", scored[0].Page.Number)
	}
}

func TestNavigateSurfacesAJudgeFailure(t *testing.T) {
	j := &llmgate.MockJudge{Err: errors.New("typesafe: request failed")}
	n := &JudgeNavigator{Judge: j}
	_, err := n.Navigate(context.Background(), "q", tenKLeaves(), func(context.Context, NavLeaf) ([]NavPage, error) { return nil, nil })
	if err == nil || !strings.Contains(err.Error(), "request failed") {
		t.Errorf("a failed Judge must be an error, not an empty result: %v", err)
	}
}

type mapLoader map[string]string

func (m mapLoader) Load(_ context.Context, ref string) ([]byte, error) { return []byte(m[ref]), nil }

func TestJudgeWalkStrategyOverATree(t *testing.T) {
	j, _ := navJudge("financial", "Total revenue")
	s := NewJudgeWalkStrategy(j)
	s.Navigator.MaxLeaves = 1
	s.PageLoader = mapLoader{"ref8": "CONSOLIDATED STATEMENTS OF INCOME\nTotal revenue 17,606", "ref1": "We are a company."}
	tr := &tree.Tree{DocumentID: "d", Root: &tree.Section{ID: "root", Children: []*tree.Section{
		{ID: "s1", Title: "Item 1. Business", PageStart: 3, PageEnd: 19, ContentRef: "ref1"},
		{ID: "s8", Title: "Item 8. Financial Statements", PageStart: 52, PageEnd: 91, ContentRef: "ref8"},
	}}}
	res, err := s.SelectWithCost(context.Background(), tr, "total revenue?", ContextBudget{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.SelectedIDs) != 1 || res.SelectedIDs[0] != "s8" {
		t.Fatalf("selected %v want [s8]", res.SelectedIDs)
	}
	if len(res.CitedPages) != 1 || res.CitedPages[0] != [2]int{52, 91} {
		t.Errorf("cited pages %v", res.CitedPages)
	}
	if res.Usage.LLMCalls != 2 || res.HopsTaken != 2 {
		t.Errorf("usage %+v hops %d", res.Usage, res.HopsTaken)
	}
	if s.Name() != "judgewalk" {
		t.Errorf("name %q", s.Name())
	}
}

func TestChunkBodyNumbersChunksThroughThePageRange(t *testing.T) {
	body := strings.Repeat("line of text\n", 1000) // 13k chars
	chunks := chunkBody(body, 10, 19, 6000)
	if len(chunks) != 3 {
		t.Fatalf("chunks: %d", len(chunks))
	}
	if chunks[0].Number != 10 || chunks[2].Number > 19 || chunks[1].Number < 10 {
		t.Errorf("chunk pages: %d %d %d", chunks[0].Number, chunks[1].Number, chunks[2].Number)
	}
}
