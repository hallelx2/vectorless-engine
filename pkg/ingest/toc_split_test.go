package ingest

import (
	"context"
	"strings"
	"testing"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// splitJudge confirms contents entries and heading lines whose text
// contains any of the given words, resolves a title to the page whose
// excerpt opens with it, and rejects everything else.
func splitJudge(yes ...string) *llmgate.MockJudge {
	return &llmgate.MockJudge{Respond: func(_ context.Context, req llmgate.JudgeRequest) (*llmgate.Judgment, error) {
		st := req.State.(map[string]any)
		ans := map[string]llmgate.Answer{}
		for id := range req.Questions {
			item, _ := st[id].(map[string]any)
			p := 0.05
			switch {
			case strings.HasPrefix(id, "e_"), strings.HasPrefix(id, "h_"):
				text, _ := item["title"].(string)
				if text == "" {
					text, _ = item["line"].(string)
				}
				for _, w := range yes {
					if strings.Contains(strings.ToLower(text), w) {
						p = 0.9
					}
				}
			case strings.HasPrefix(id, "r_"):
				title, _ := item["title"].(string)
				ex, _ := item["excerpt"].(string)
				if strings.HasPrefix(strings.ToLower(ex), strings.ToLower(title)) {
					p = 0.9
				}
			}
			ans[id] = llmgate.NoulAnswer{Noul: p}
		}
		return &llmgate.Judgment{Model: "mock", Answers: ans, Usage: llmgate.Usage{TokensReported: true}}, nil
	}}
}

func item8Pages() []PageText {
	ps := []PageText{
		{54, "Item 8. Financial Statements and Supplementary Data\nIndex to the Consolidated Financial Statements\nPage\nConsolidated Statements of Operations 56\nConsolidated Statements of Financial Position 57\nNote 1 - Summary of Significant Accounting Policies 59\nNote 2 - Goodwill and Acquired Intangibles 70\nNote 21 - Legal Proceedings 113"},
		{55, "Table of Contents\nReport of Independent Registered Public Accounting Firm\nWe have audited the accompanying statements."},
		{56, "Table of Contents\nConsolidated Statements of Operations\nRevenues 66,608"},
		{57, "Table of Contents\nConsolidated Statements of Financial Position\nAssets\nCash 14,614"},
	}
	for p := 58; p <= 125; p++ {
		text := "Table of Contents\nNotes to the Consolidated Financial Statements\nprose about accounting " + strings.Repeat("x ", 50)
		switch p {
		case 59:
			text = "Table of Contents\nNote 1 - Summary of Significant Accounting Policies\nPrinciples of Consolidation and Basis of Presentation\nThe consolidated financial statements included."
		case 70:
			text = "Table of Contents\nNote 2 - Goodwill and Acquired Intangibles\nGoodwill is tested annually."
		case 113:
			text = "Table of Contents\nNote 21 - Legal Proceedings\nA class action filed in 2019 remains pending."
		}
		ps = append(ps, PageText{p, text})
	}
	return ps
}

func TestSplitLeafFromANestedContentsPage(t *testing.T) {
	b := &TOCBuilder{Judge: splitJudge("note", "consolidated statements")}
	nodes := []tree.TOCNode{{Structure: "2", Title: "PART II", StartPage: 35, EndPage: 125, Nodes: []tree.TOCNode{
		{Structure: "2.4", Title: "Item 7A. Quantitative and Qualitative Disclosures", StartPage: 53, EndPage: 53},
		{Structure: "2.5", Title: "Item 8. Financial Statements and Supplementary Data", StartPage: 54, EndPage: 125},
	}}}
	var usage Usage
	n := b.splitLargeLeaves(context.Background(), nodes, item8Pages(), 12, &usage)
	if n < 3 {
		t.Fatalf("sub-leaves added: %d, want the statements and notes", n)
	}
	item8 := nodes[0].Nodes[1]
	if len(item8.Nodes) != n {
		t.Fatalf("children on Item 8: %d", len(item8.Nodes))
	}
	byTitle := map[string]tree.TOCNode{}
	for _, c := range item8.Nodes {
		byTitle[c.Title] = c
	}
	n21, ok := byTitle["Note 21 - Legal Proceedings"]
	if !ok || n21.StartPage != 113 {
		t.Errorf("Note 21 should be a sub-leaf on page 113: %+v (all: %v)", n21, titlesOfNodes(item8.Nodes))
	}
	if n1 := byTitle["Note 1 - Summary of Significant Accounting Policies"]; n1.StartPage != 59 || n1.EndPage != 69 {
		t.Errorf("Note 1 span: %d-%d want 59-69", n1.StartPage, n1.EndPage)
	}
	if last := item8.Nodes[len(item8.Nodes)-1]; last.EndPage != 125 {
		t.Errorf("last sub-leaf should run to the parent's end: %d", last.EndPage)
	}
	if !strings.HasPrefix(item8.Nodes[0].Structure, "2.5.") {
		t.Errorf("sub-leaf structure: %q", item8.Nodes[0].Structure)
	}
	// The small leaf beside it is untouched.
	if len(nodes[0].Nodes[0].Nodes) != 0 {
		t.Errorf("a one-page leaf was split")
	}
	if len(usage.Degraded) != 0 {
		t.Errorf("degraded: %v", usage.Degraded)
	}
	// The pages before the first statement (index, auditor's report)
	// belong to an opening sub-leaf with the parent's title.
	if first := item8.Nodes[0]; first.StartPage != 54 || first.Title != item8.Title {
		t.Errorf("opening sub-leaf should cover the parent's head from page 54: %+v", first)
	}
}

func TestSplitLeafFromHeadingLinesWhenThereIsNoIndex(t *testing.T) {
	var ps []PageText
	for p := 22; p <= 52; p++ {
		text := "Table of Contents\nprose about results " + strings.Repeat("y ", 40)
		switch p {
		case 22:
			text = "Item 7. Management’s Discussion and Analysis\nBusiness Environment and Trends\nThe commercial aviation market continued to recover."
		case 30:
			text = "Table of Contents\nResults of Operations\nRevenues\nThe following table summarizes."
		case 41:
			text = "Table of Contents\nLiquidity and Capital Resources\nWe believe our cash position is adequate."
		case 45:
			text = "Table of Contents\nCritical Accounting Estimates\nProgram Accounting\nManagement uses estimates."
		}
		ps = append(ps, PageText{p, text})
	}
	b := &TOCBuilder{Judge: splitJudge("business environment", "results of operations", "liquidity", "critical accounting")}
	nodes := []tree.TOCNode{{Structure: "2.3", Title: "Item 7. Management’s Discussion and Analysis", StartPage: 22, EndPage: 52}}
	var usage Usage
	n := b.splitLargeLeaves(context.Background(), nodes, ps, 12, &usage)
	if n != 4 {
		t.Fatalf("sub-leaves: %d want 4: %v", n, titlesOfNodes(nodes[0].Nodes))
	}
	got := titlesOfNodes(nodes[0].Nodes)
	want := "Business Environment and Trends|Results of Operations|Liquidity and Capital Resources|Critical Accounting Estimates"
	if strings.Join(got, "|") != want {
		t.Errorf("got %v", got)
	}
	if nodes[0].Nodes[1].StartPage != 30 || nodes[0].Nodes[1].EndPage != 40 {
		t.Errorf("Results of Operations span %d-%d want 30-40", nodes[0].Nodes[1].StartPage, nodes[0].Nodes[1].EndPage)
	}
}

func TestHeadingCandidatesDropRunningHeadersAndFurniture(t *testing.T) {
	ps := []PageText{
		{1, "Table of Contents\nThe Boeing Company and Subsidiaries\nRevenue and Related Cost Recognition\nWe recognize revenue when."},
		{2, "Table of Contents\nThe Boeing Company and Subsidiaries\nAssets\n14,614\nUse of Estimates\nEstimates are used."},
		{3, "Table of Contents\nThe Boeing Company and Subsidiaries\nOperating Cycle\nOur operating cycle is long."},
	}
	got := headingCandidates(ps)
	titles := []string{}
	for _, c := range got {
		titles = append(titles, c.text)
	}
	joined := strings.Join(titles, "|")
	for _, bad := range []string{"Table of Contents", "The Boeing Company and Subsidiaries", "14,614"} {
		if strings.Contains(joined, bad) {
			t.Errorf("furniture admitted: %q in %v", bad, titles)
		}
	}
	for _, good := range []string{"Revenue and Related Cost Recognition", "Use of Estimates", "Operating Cycle", "Assets"} {
		if !strings.Contains(joined, good) {
			t.Errorf("heading missed: %q in %v", good, titles)
		}
	}
	if !strings.HasPrefix(got[0].excerpt, "Revenue and Related Cost Recognition\nWe recognize") {
		t.Errorf("excerpt should start at the heading: %q", got[0].excerpt)
	}
}

func TestLooksLikeSubHeading(t *testing.T) {
	yes := []string{"Note 21 - Legal Proceedings", "Revenue and Related Cost Recognition", "CONSOLIDATED BALANCE SHEET", "Use of Estimates", "Item 1A. Risk Factors"}
	no := []string{"We recognize revenue when control transfers.", "Table of Contents", "14,614", "Page", "the following table summarizes", "Revenues:", "Total Amounts Paid To Each Of The Named Executive Officers During The Year Ended"}
	for _, s := range yes {
		if !looksLikeSubHeading(s) {
			t.Errorf("%q should look like a heading", s)
		}
	}
	for _, s := range no {
		if looksLikeSubHeading(s) {
			t.Errorf("%q should not look like a heading", s)
		}
	}
}

func TestSplitIsOffWithoutAJudgeOrAThreshold(t *testing.T) {
	nodes := []tree.TOCNode{{Title: "Item 8", StartPage: 54, EndPage: 125}}
	var usage Usage
	if n := (&TOCBuilder{}).splitLargeLeaves(context.Background(), nodes, item8Pages(), 12, &usage); n != 0 {
		t.Errorf("no Judge: %d", n)
	}
	if n := (&TOCBuilder{Judge: splitJudge("note")}).splitLargeLeaves(context.Background(), nodes, item8Pages(), 0, &usage); n != 0 {
		t.Errorf("threshold 0 means off: %d", n)
	}
	if got := (&TOCBuilder{Judge: splitJudge("x"), SplitLeavesOver: -1}).splitLeavesOver(); got != 0 {
		t.Errorf("negative should disable: %d", got)
	}
	if got := (&TOCBuilder{Judge: splitJudge("x")}).splitLeavesOver(); got != defaultSplitLeavesOver {
		t.Errorf("default with a Judge: %d", got)
	}
	if got := (&TOCBuilder{}).splitLeavesOver(); got != 0 {
		t.Errorf("no Judge means off: %d", got)
	}
}

func titlesOfNodes(ns []tree.TOCNode) []string {
	out := []string{}
	for _, n := range ns {
		out = append(out, n.Title)
	}
	return out
}
