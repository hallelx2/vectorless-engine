package retrieval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// A filing shaped like the FinanceBench ones HAL-3160 measured: a cover
// heading over the whole document, a part heading, a compact heading over
// one short note, and real statement sections.
const (
	incomeText   = "Consolidated Statement of Income. Net revenue was $86,392 million in 2022 against $79,474 million in 2021; cost of sales was $40,576 million and operating profit $11,512 million, after restructuring charges of $411 million."
	cashText     = "Consolidated Statement of Cash Flows. Depreciation and amortization was $2,763 million in 2022; net cash provided by operating activities was $10,811 million and capital spending was $5,207 million for the year."
	noteText     = "Restructuring. In 2022 the company recorded charges of $411 million under its productivity plan, mainly severance and asset impairments in Europe and Latin America, with most cash payments due within twelve months."
	riskText     = "Risk factors. Demand for our products depends on consumer spending, which falls in a downturn; commodity and freight costs can rise faster than we can raise prices, and currency moves change reported results."
	incomePage   = "Page 62 header. " + incomeText + " Earnings per share were $6.42."
	runnerUpPage = "Page 63. Consolidated Balance Sheet. Total assets were $92,187 million and total liabilities $74,914 million at year end, with inventories of $5,222 million and accounts payable of $9,201 million."
)

func filingTree() *tree.Tree {
	income := &tree.Section{ID: "sec_income", Title: "Consolidated Statement of Income", ContentRef: "income", TokenCount: len(incomeText) / 4, PageStart: 62, PageEnd: 62}
	cash := &tree.Section{ID: "sec_cash", Title: "Consolidated Statement of Cash Flows", ContentRef: "cash", TokenCount: len(cashText) / 4, PageStart: 64, PageEnd: 64}
	noteBody := &tree.Section{ID: "sec_note_body", Title: "", ContentRef: "note", TokenCount: len(noteText) / 4, PageStart: 78, PageEnd: 78}
	note := &tree.Section{ID: "sec_note", Title: "Note 3. Restructuring", ContentRef: "note_heading", PageStart: 78, PageEnd: 78, Children: []*tree.Section{noteBody}}
	risk := &tree.Section{ID: "sec_risk", Title: "Risk Factors", ContentRef: "risk", TokenCount: len(riskText) / 4, PageStart: 9, PageEnd: 12}
	part := &tree.Section{ID: "sec_part2", Title: "PART II", PageStart: 30, PageEnd: 90, Children: []*tree.Section{income, cash, note}}
	cover := &tree.Section{ID: "sec_cover", Title: "PEPSICO, INC.", ContentRef: "cover", PageStart: 1, PageEnd: 120, Children: []*tree.Section{risk, part}}
	return &tree.Tree{DocumentID: "doc_pep", Title: "PepsiCo 10-K", Root: cover}
}

func filingStore() ContentLoader {
	data := map[string]string{
		"income":       incomeText,
		"cash":         cashText,
		"note":         noteText,
		"note_heading": "Note 3. Restructuring",
		"risk":         riskText,
		"cover":        "PEPSICO, INC.",
	}
	return func(ctx context.Context, ref string) (string, error) {
		s, ok := data[ref]
		if !ok {
			return "", errors.New("not found")
		}
		return s, nil
	}
}

func ids(units []Unit) []string {
	var out []string
	for _, u := range units {
		if u.Kind == UnitPage {
			out = append(out, "page")
		} else {
			out = append(out, string(u.Section.ID))
		}
	}
	return out
}

func TestHeadingOnlyTopHitGivesItsSlotToRealEvidence(t *testing.T) {
	res := &Result{SelectedIDs: []tree.SectionID{"sec_cover", "sec_part2", "sec_cash", "sec_risk"}}
	units, stats := AssembleUnits(context.Background(), filingTree(), res, 2, filingStore())
	if got := strings.Join(ids(units), ","); got != "sec_cash,sec_risk" {
		t.Fatalf("units = %s, want the two sections with body text", got)
	}
	if stats.Empty != 2 {
		t.Errorf("empty = %d, want the cover and part headings counted", stats.Empty)
	}
}

// A cover heading spans the whole filing: gathering its text would return
// the document's opening pages, so it is dropped rather than expanded.
func TestCoverTitleIsDroppedNotExpanded(t *testing.T) {
	units, stats := AssembleUnits(context.Background(), filingTree(), &Result{SelectedIDs: []tree.SectionID{"sec_cover"}}, 0, filingStore())
	if len(units) != 0 {
		t.Fatalf("units = %v, want none for a cover heading", ids(units))
	}
	if stats.Expanded != 0 || stats.Empty != 1 {
		t.Errorf("stats = %+v", stats)
	}
}

func TestCompactHeadingTakesTheTextBeneathIt(t *testing.T) {
	units, stats := AssembleUnits(context.Background(), filingTree(), &Result{SelectedIDs: []tree.SectionID{"sec_note"}}, 5, filingStore())
	if len(units) != 1 || units[0].Section.ID != "sec_note" || !units[0].Expanded {
		t.Fatalf("units = %+v, want the note heading expanded", units)
	}
	if !strings.Contains(units[0].Content, "$411 million") {
		t.Errorf("content = %q, want the note's body", units[0].Content)
	}
	if stats.Expanded != 1 {
		t.Errorf("expanded = %d", stats.Expanded)
	}
}

func TestSectionRepeatingAnEvidencePageIsDropped(t *testing.T) {
	res := &Result{
		EvidencePages: []EvidencePage{{Page: 62, Text: incomePage, Confidence: 0.9}},
		SelectedIDs:   []tree.SectionID{"sec_income", "sec_cash"},
	}
	units, stats := AssembleUnits(context.Background(), filingTree(), res, 5, filingStore())
	if got := strings.Join(ids(units), ","); got != "page,sec_cash" {
		t.Fatalf("units = %s, want the page and the section it does not carry", got)
	}
	if stats.Duplicate != 1 {
		t.Errorf("duplicate = %d", stats.Duplicate)
	}
}

func TestOpenSlotsFillFromRunnerUpsUnderTheCap(t *testing.T) {
	res := &Result{
		EvidencePages: []EvidencePage{{Page: 62, Text: incomePage, Confidence: 0.9}},
		SelectedIDs:   []tree.SectionID{"sec_part2", "sec_income"},
		RunnerUpPages: []EvidencePage{{Page: 62, Text: incomePage}, {Page: 63, Text: runnerUpPage, Confidence: 0.3}},
		RunnerUpIDs:   []tree.SectionID{"sec_cash", "sec_risk"},
	}
	units, stats := AssembleUnits(context.Background(), filingTree(), res, 3, filingStore())
	if got := strings.Join(ids(units), ","); got != "page,page,sec_cash" {
		t.Fatalf("units = %s", got)
	}
	if !units[1].RunnerUp || units[1].Page.Page != 63 || !units[2].RunnerUp {
		t.Errorf("runner-ups not marked: %+v", units)
	}
	if stats.RunnerUps != 2 {
		t.Errorf("runner_ups = %d", stats.RunnerUps)
	}
	// Without a cap nothing is filled in.
	all, _ := AssembleUnits(context.Background(), filingTree(), res, 0, filingStore())
	if got := strings.Join(ids(all), ","); got != "page" {
		t.Errorf("uncapped units = %s, want only the evidence", got)
	}
}

func TestCapCountsPagesAndSectionsTogether(t *testing.T) {
	res := &Result{
		EvidencePages: []EvidencePage{{Page: 62, Text: incomePage}, {Page: 63, Text: runnerUpPage}},
		SelectedIDs:   []tree.SectionID{"sec_cash", "sec_risk"},
	}
	units, _ := AssembleUnits(context.Background(), filingTree(), res, 3, filingStore())
	if got := strings.Join(ids(units), ","); got != "page,page,sec_cash" {
		t.Fatalf("units = %s", got)
	}
}

func TestUnitAssemblerAcceptsOneUnitAtATime(t *testing.T) {
	a := NewUnitAssembler(filingTree(), 0, filingStore())
	if _, ok := a.AddSection(context.Background(), "sec_part2", false); ok {
		t.Error("part heading accepted")
	}
	if _, ok := a.AddSection(context.Background(), "sec_income", false); !ok {
		t.Error("income statement refused")
	}
	if _, ok := a.AddSection(context.Background(), "sec_income", false); ok {
		t.Error("the same section accepted twice")
	}
	if _, ok := a.AddPage(EvidencePage{Page: 62, Text: "   "}, false); ok {
		t.Error("blank page accepted")
	}
	if n := len(a.Units()); n != 1 {
		t.Errorf("units = %d", n)
	}
}

func TestAssembleUnitsNilResult(t *testing.T) {
	if units, _ := AssembleUnits(context.Background(), filingTree(), nil, 5, filingStore()); units != nil {
		t.Errorf("units = %v", units)
	}
}

func TestRunnerUpsSkipTheEvidenceAndKeepRankOrder(t *testing.T) {
	p := func(n int, prob float64) PageScore { return PageScore{Page: NavPage{Number: n, LeafID: "l"}, P: prob} }
	nav := &NavResult{
		Pages:    []PageScore{p(5, 0.9), p(7, 0.6), p(2, 0.4), p(9, 0.1)},
		Evidence: []PageScore{p(5, 0.9)},
	}
	got := runnerUps(nav)
	if len(got) != 3 || got[0].Page.Number != 7 || got[1].Page.Number != 2 || got[2].Page.Number != 9 {
		t.Fatalf("runner-ups = %+v", got)
	}
}

// A title whose case folding changes its byte length must not panic the
// request (U+212A KELVIN SIGN lower-cases to the 1-byte "k").
func TestSubstantiveSurvivesCaseFoldingThatChangesLength(t *testing.T) {
	if got := substantive("k", "\u212a"); got != "k" {
		t.Errorf("substantive = %q", got)
	}
	if got := substantive("\u212aelvin scale readings follow", "\u212aelvin"); got != "scale readings follow" {
		t.Errorf("substantive = %q", got)
	}
}

// A short document comes back entire whatever the cap: what navigation
// ranked first, then the rest in document order (HAL-3246).
func TestShortDocumentComesBackWhole(t *testing.T) {
	res := &Result{
		WholeDocument: true,
		EvidencePages: []EvidencePage{{Page: 63, Text: runnerUpPage, Confidence: 0.8}},
		DocumentPages: []EvidencePage{{Page: 62, Text: incomePage}, {Page: 63, Text: runnerUpPage}, {Page: 64, Text: "Page 64. " + cashText}},
	}
	units, stats := AssembleUnits(context.Background(), filingTree(), res, 2, filingStore())
	got := []int{}
	for _, u := range units {
		got = append(got, u.Page.Page)
	}
	if fmt.Sprint(got) != "[63 62 64]" {
		t.Fatalf("pages = %v, want the evidence first then the rest in order, past the cap of 2", got)
	}
	if units[0].Whole || !units[1].Whole || !units[2].Whole || !stats.Whole {
		t.Errorf("whole marks: %+v %+v", units, stats)
	}
}

func TestShortSectionDocumentComesBackWhole(t *testing.T) {
	res := &Result{WholeDocument: true, SelectedIDs: []tree.SectionID{"sec_cash"},
		DocumentIDs: []tree.SectionID{"sec_risk", "sec_income", "sec_cash", "sec_note_body"}}
	units, _ := AssembleUnits(context.Background(), filingTree(), res, 1, filingStore())
	if got := strings.Join(ids(units), ","); got != "sec_cash,sec_risk,sec_income,sec_note_body" {
		t.Fatalf("units = %s", got)
	}
}

// The page beside the top evidence page fills before pages ranked from
// elsewhere in the document.
func TestNeighbourFillsBeforeRunnerUps(t *testing.T) {
	res := &Result{
		EvidencePages:  []EvidencePage{{Page: 62, Text: incomePage, Confidence: 0.9}},
		NeighbourPages: []EvidencePage{{Page: 63, Text: runnerUpPage, NeighbourOf: 62}},
		RunnerUpPages:  []EvidencePage{{Page: 9, Text: "Page 9. " + riskText, Confidence: 0.2}},
	}
	units, stats := AssembleUnits(context.Background(), filingTree(), res, 2, filingStore())
	if len(units) != 2 || units[1].Page.Page != 63 || units[1].Page.NeighbourOf != 62 {
		t.Fatalf("units = %+v", units)
	}
	if stats.Neighbours != 1 || stats.RunnerUps != 0 {
		t.Errorf("stats = %+v", stats)
	}
}

func TestNeighbourPagesAreNextThenPreviousAndNeverEvidence(t *testing.T) {
	p := func(n int) NavPage { return NavPage{Number: n, Text: fmt.Sprintf("page %d text", n), LeafID: "l"} }
	pages := []NavPage{p(6), p(7), p(8), p(9)}
	nav := &NavResult{
		Evidence: []PageScore{{Page: p(7), P: 0.9}, {Page: p(6), P: 0.7}},
		Pages:    []PageScore{{Page: p(7), P: 0.9}, {Page: p(6), P: 0.7}, {Page: p(8), P: 0.3}},
	}
	got := neighbourPages(nav, pages, map[string]string{"l": "Business"})
	if len(got) != 1 || got[0].Page != 8 || got[0].NeighbourOf != 7 || got[0].Confidence != 0.3 || got[0].Title != "Business" {
		t.Fatalf("neighbours = %+v, want page 8 only (6 is evidence)", got)
	}
	if neighbourPages(&NavResult{}, pages, nil) != nil {
		t.Error("no evidence, no neighbours")
	}
}

func TestWholeDocumentThreshold(t *testing.T) {
	if (&JudgeWalkStrategy{}).wholeDocumentTokens() != DefaultWholeDocumentTokens {
		t.Error("zero takes the default")
	}
	if (&JudgeWalkStrategy{WholeDocumentTokens: -1}).wholeDocumentTokens() > 0 {
		t.Error("negative turns it off")
	}
}
