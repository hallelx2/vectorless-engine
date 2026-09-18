package ingest

import (
	"context"
	"strings"
	"testing"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// A filing in miniature: cover, contents, body. Printed page numbers in
// the contents are offset from PDF pages by two — the classic shape.
func miniFiling() []PageText {
	return []PageText{
		{1, "UNITED STATES SECURITIES AND EXCHANGE COMMISSION FORM 10-K ACME INC."},
		{2, "ACME INC. FORM 10-K TABLE OF CONTENTS Page No. PART I Item 1. Business 3 Item 1A. Risk Factors 5 Item 2. Properties 7"},
		{3, "PART I\nItem 1. Business\nAcme makes widgets. As discussed in Item 1A. Risk Factors, demand varies."},
		{4, "continued business text about widgets and markets and customers."},
		{5, "Item 1A. Risk Factors\nInvesting in our stock involves risk. The following risks..."},
		{6, "more risk factors, continued from the prior page."},
		{7, "Item 2. Properties\nWe lease offices in three cities."},
	}
}

func pagesOf(hs []pageHit) []int {
	out := make([]int, 0, len(hs))
	for _, h := range hs {
		out = append(out, h.page)
	}
	return out
}

func containsPage(hs []pageHit, p int) bool { return hasPage(hs, p) }

func TestFindCandidatesHitsTheRealOpeningAndTheTOC(t *testing.T) {
	got := findCandidatePages("Item 1A. Risk Factors", miniFiling())
	// Page 5 is the real opening; page 2 is the contents page, where the
	// title is an entry. Both are heading-shaped to a regex, so both are
	// candidates — code finds, exclusion and the model discriminate.
	// Page 3 mentions it mid-sentence and must NOT be a candidate.
	if !containsPage(got, 5) {
		t.Fatalf("candidates %v do not include the real opening page 5", pagesOf(got))
	}
	if containsPage(got, 3) {
		t.Errorf("a mid-sentence cross-reference on page 3 became a candidate: %v", pagesOf(got))
	}
}

// A section that is third on its page sits far past any head window. It
// is found by searching the whole page for a heading-shaped match, and
// the Judge is shown a window around it, not the page head.
func TestFindCandidatesReachesASectionDeepInAPackedPage(t *testing.T) {
	long := strings.Repeat("prose about properties. ", 80) // ~1,900 chars
	pages := []PageText{{34,
		"Item 1B. Unresolved Staff Comments\nNone.\n" +
			"Item 2. Properties\n" + long + "\n" +
			"Item 3. Legal Proceedings\nSee Note 14.\n" +
			"Item 4. Mine Safety Disclosures\nNot applicable."}}
	got := findCandidatePages("Item 4. Mine Safety Disclosures", pages)
	if !containsPage(got, 34) {
		t.Fatalf("fourth section on the page not found: %v", pagesOf(got))
	}
	if got[0].offset < headChars {
		t.Errorf("offset %d is inside the old head window; the test is not exercising the deep case", got[0].offset)
	}
	ex := excerptAround(pages[0].Text, got[0].offset)
	if !strings.Contains(ex, "Item 4. Mine Safety") || !strings.Contains(ex, "Not applicable") {
		t.Errorf("excerpt does not show the heading and the section start: %q", ex)
	}
}

// The parser joins a part label to the item that opens it. That is still
// a heading; a sentence before it is not.
func TestFindCandidatesAcceptsAPartLabelBeforeTheHeading(t *testing.T) {
	pages := []PageText{
		{3, "Forward-Looking Statements\nsome prose.\n\nPART I ITEM 1. BUSINESS OVERVIEW\nFounded in 1982, Adobe is"},
		{7, "our results, as described in Item 1. Business above, and\nmore prose"},
	}
	got := findCandidatePages("Item 1. Business", pages)
	if !containsPage(got, 3) {
		t.Fatalf("heading after a part label was not found: %v", pagesOf(got))
	}
	if containsPage(got, 7) {
		t.Errorf("a mid-sentence mention was admitted: %v", pagesOf(got))
	}
	for _, p := range []string{"PART I", "Part II.", "PART IV -", "part 3"} {
		if !isLineStart(p+" Item 1. Business", len(p)+1) {
			t.Errorf("%q not accepted as a part label", p)
		}
	}
	if isLineStart("In Part I we said Item 1. Business", len("In Part I we said ")) {
		t.Errorf("a sentence containing a part label was accepted")
	}
}

func TestFindCandidatesIgnoresMidLineMentions(t *testing.T) {
	pages := []PageText{
		{1, strings.Repeat("filler text. ", 200) + "see Item 1A. Risk Factors for details."},
		{2, "Item 1A. Risk Factors\nThe risks are as follows."},
	}
	got := findCandidatePages("Item 1A. Risk Factors", pages)
	if containsPage(got, 1) {
		t.Errorf("a mid-line mention was treated as a candidate: %v", pagesOf(got))
	}
	if !containsPage(got, 2) {
		t.Errorf("the real opening was missed: %v", pagesOf(got))
	}
}

func TestFindCandidatesIsCaseAndPunctuationInsensitive(t *testing.T) {
	pages := []PageText{{9, "ITEM 1A — RISK FACTORS\nbody"}}
	if got := findCandidatePages("Item 1A. Risk Factors", pages); !containsPage(got, 9) {
		t.Errorf("restyled heading not matched: %v", pagesOf(got))
	}
}

func TestFindCandidatesRejectsTinyTitles(t *testing.T) {
	// "Notes" would match everywhere; a title this short is not evidence.
	pages := []PageText{{1, "notes on things"}, {2, "more notes"}}
	if got := findCandidatePages("Notes", pages); len(got) != 0 {
		t.Errorf("a five-letter title produced candidates: %v", got)
	}
}

// A judge that says yes only when the page genuinely opens with the title.
func openingJudge(pages []PageText) *llmgate.MockJudge {
	byPage := map[int]string{}
	for _, p := range pages {
		byPage[p.PageNumber] = normalise(p.Text)
	}
	return &llmgate.MockJudge{
		Respond: func(_ context.Context, req llmgate.JudgeRequest) (*llmgate.Judgment, error) {
			ans := map[string]llmgate.Answer{}
			st := req.State.(map[string]any)
			for qk := range req.Questions {
				item := st[qk].(map[string]any)
				title := strings.ToLower(item["title"].(string))
				head := strings.ToLower(item["excerpt"].(string))
				// A heading on the page, anywhere; a contents page is
				// handled by exclusion, not by the model, so the mock
				// need not model it.
				// A heading opens a line; a cross-reference sits inside
				// one. Modelling that distinction is the whole point of
				// the mock — without it, "as discussed in Item 1A" on
				// page 3 ties with the real opening on page 5.
				p := 0.05
				if strings.HasPrefix(head, title) || strings.Contains(head, "\n"+title) {
					p = 0.95
				}
				ans[qk] = llmgate.NoulAnswer{Noul: p}
			}
			return &llmgate.Judgment{Model: "mock", Answers: ans,
				Usage: llmgate.Usage{InputTokens: 10, TotalTokens: 10, TokensReported: true}}, nil
		},
	}
}

// The whole point: leaves that extraction left at page 0 — or gave the
// printed number, which is wrong by the offset — come back with the page
// the section actually opens on.
func TestResolveRecoversPagesExtractionLost(t *testing.T) {
	pages := miniFiling()
	nodes := []tree.TOCNode{{Title: "PART I", StartPage: 3, Nodes: []tree.TOCNode{
		{Title: "Item 1. Business", StartPage: 0},      // extraction nulled it
		{Title: "Item 1A. Risk Factors", StartPage: 5}, // extraction guessed the printed page — happens to be off by... no: printed 5, PDF 5. Fine.
		{Title: "Item 2. Properties", StartPage: 0},
	}}}

	b := &TOCBuilder{Judge: openingJudge(pages)}
	resolved, handled := b.resolvePagesJudge(context.Background(), nodes, pages, []int{2}, &Usage{})
	if !handled {
		t.Fatal("handled = false")
	}
	applyResolvedPages(nodes, resolved)

	want := map[string]int{"Item 1. Business": 3, "Item 1A. Risk Factors": 5, "Item 2. Properties": 7}
	for _, n := range nodes[0].Nodes {
		if n.StartPage != want[n.Title] {
			t.Errorf("%q resolved to page %d, want %d", n.Title, n.StartPage, want[n.Title])
		}
	}
}

// The contents page contains every title and is excluded in code, not
// left to the model. With page 2 excluded, page 7 is the only candidate.
func TestResolveDoesNotPickTheContentsPage(t *testing.T) {
	pages := miniFiling()
	nodes := []tree.TOCNode{{Title: "Item 2. Properties", StartPage: 0}}

	b := &TOCBuilder{Judge: openingJudge(pages)}
	resolved, _ := b.resolvePagesJudge(context.Background(), nodes, pages, []int{2}, &Usage{})
	applyResolvedPages(nodes, resolved)
	if nodes[0].StartPage == 2 {
		t.Error("resolved to the contents page")
	}
	if nodes[0].StartPage != 7 {
		t.Errorf("resolved to %d, want 7", nodes[0].StartPage)
	}
}

// A title that appears at the head of many pages is a running header.
// Refusing to place it beats placing it wrong.
// The reason the question is "on this page" and not "at the very start":
// page 3 opens with "PART I" and Item 1 comes second. It still starts
// there, and the resolver must say so.
func TestResolveAcceptsASectionThatIsNotFirstOnItsPage(t *testing.T) {
	pages := miniFiling()
	nodes := []tree.TOCNode{{Title: "Item 1. Business"}}
	b := &TOCBuilder{Judge: openingJudge(pages)}
	resolved, _ := b.resolvePagesJudge(context.Background(), nodes, pages, []int{2}, &Usage{})
	applyResolvedPages(nodes, resolved)
	if nodes[0].StartPage != 3 {
		t.Errorf("a second-on-page section resolved to %d, want 3", nodes[0].StartPage)
	}
}

func TestResolveGivesUpOnRunningHeaders(t *testing.T) {
	var pages []PageText
	for i := 1; i <= 8; i++ {
		pages = append(pages, PageText{i, "ACME ANNUAL REPORT 2024\nbody text for page"})
	}
	nodes := []tree.TOCNode{{Title: "ACME Annual Report 2024", StartPage: 0}}
	claims := collectResolveClaims(nodes, pages, nil)
	if len(claims) != 1 || len(claims[0].candidates) != 0 {
		t.Errorf("a running header produced candidates: %+v", claims)
	}
}

// Everything in one request when it fits — the reason this is fast.
func TestResolveBatchesAllLeavesTogether(t *testing.T) {
	pages := miniFiling()
	nodes := []tree.TOCNode{
		{Title: "Item 1. Business"}, {Title: "Item 1A. Risk Factors"}, {Title: "Item 2. Properties"},
	}
	var requests int
	inner := openingJudge(pages)
	counting := &llmgate.MockJudge{Respond: func(ctx context.Context, r llmgate.JudgeRequest) (*llmgate.Judgment, error) {
		requests++
		return inner.Judge(ctx, r)
	}}
	b := &TOCBuilder{Judge: counting}
	if _, handled := b.resolvePagesJudge(context.Background(), nodes, pages, []int{2}, &Usage{}); !handled {
		t.Fatal("handled = false")
	}
	if requests != 1 {
		t.Errorf("3 leaves took %d requests, want 1", requests)
	}
}

func TestResolveNilJudgeDeclines(t *testing.T) {
	b := &TOCBuilder{}
	if _, handled := b.resolvePagesJudge(context.Background(), nil, nil, []int{2}, &Usage{}); handled {
		t.Error("nil Judge reported handled")
	}
}

func TestSplitProbeKey(t *testing.T) {
	k, p := splitProbeKey("r_item_1a_risk_factors_p22")
	if k != "r_item_1a_risk_factors" || p != 22 {
		t.Errorf("got (%q, %d)", k, p)
	}
}
