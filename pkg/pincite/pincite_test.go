package pincite

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/hallelx2/llmgate"
)

// page builds a layout page from lines of text: each word 0.05 wide
// with a 0.01 gap, each line 0.012 tall at 0.02 spacing.
func page(n int, lines ...string) Page {
	p := Page{Number: n, Width: 612, Height: 792}
	for i, ln := range lines {
		l := Line{Top: 0.1 + float64(i)*0.02, Height: 0.012}
		x := 0.05
		for _, w := range strings.Fields(ln) {
			l.Words = append(l.Words, Word{Text: w, X0: x, X1: x + 0.05})
			x += 0.06
		}
		p.Lines = append(p.Lines, l)
	}
	return p
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestLocateExactSpansOnlyTheQuotedWords(t *testing.T) {
	p := page(7,
		"Total revenue for fiscal 2022 was",
		"$34.2 billion, up 4% from 2021.",
	)
	m, ok := Locate(&p, "revenue for fiscal 2022 was $34.2 billion")
	if !ok || m.Precision != PrecisionLine || m.Score != 1 || m.Page != 7 {
		t.Fatalf("got %+v ok=%v", m, ok)
	}
	if len(m.Regions) != 2 {
		t.Fatalf("want one region per line, got %d", len(m.Regions))
	}
	// First line starts at "revenue" (word 2), not at "Total".
	if !near(m.Regions[0].Left, 0.11) || !near(m.Regions[0].Width, 0.05*5+0.01*4) {
		t.Errorf("line 1 region = %+v", m.Regions[0])
	}
	// Second line ends at "billion," (word 2).
	if !near(m.Regions[1].Left, 0.05) || !near(m.Regions[1].Width, 0.11) {
		t.Errorf("line 2 region = %+v", m.Regions[1])
	}
	if !near(m.Regions[1].Top, 0.12) || !near(m.Regions[1].Height, 0.012) {
		t.Errorf("line 2 vertical = %+v", m.Regions[1])
	}
}

func TestLocateFoldsTypographyAndNumberPunctuation(t *testing.T) {
	p := page(1, "3M’s long–term debt was 14,001 million")
	if _, ok := Locate(&p, "3M's long-term debt was 14,001 million"); !ok {
		t.Fatal("typographic apostrophe/dash should match their ASCII forms")
	}
}

func TestLocateFuzzyCoversALongerPassage(t *testing.T) {
	// The quote drops the parenthetical, so the passage is longer than
	// the quote; the match must still start and end where the quote does.
	p := page(3,
		"Effective in 2022 the measure of segment profit/loss",
		"(business segment operating income) was updated for all periods presented.",
		"The change aligns with the CODM.",
	)
	m, ok := Locate(&p, "the measure of segment profit/loss was updated for all periods presented")
	if !ok || m.Score != 1 {
		t.Fatalf("got %+v ok=%v", m, ok)
	}
	if len(m.Regions) != 2 {
		t.Fatalf("regions = %+v", m.Regions)
	}
	if !near(m.Regions[0].Left, 0.05+0.06*3) { // starts at "the"
		t.Errorf("starts at %v", m.Regions[0].Left)
	}
	words := strings.Fields("(business segment operating income) was updated for all periods presented.")
	wantRight := 0.05 + 0.06*float64(len(words)-1) + 0.05
	if got := m.Regions[1].Left + m.Regions[1].Width; !near(got, wantRight) {
		t.Errorf("ends at %v, want %v (must not spill onto the next sentence)", got, wantRight)
	}
}

func TestLocateRefusesAWeakMatch(t *testing.T) {
	p := page(2, "Cash and cash equivalents were 3,655 at year end.")
	if m, ok := Locate(&p, "inventories rose sharply because of supply chain constraints"); ok {
		t.Fatalf("a quote not on the page must not be placed: %+v", m)
	}
	if _, ok := Locate(&p, "cash 9"); ok {
		t.Fatal("short quotes match exactly or not at all")
	}
}

func TestResolvePrefersExactAndFallsBackToPage(t *testing.T) {
	l := &Layout{Version: LayoutVersion, Pages: []Page{
		page(1, "Revenue was 10 billion in the year"),
		page(2, "Revenue was 12 billion in the year"),
	}}
	if m := Resolve(l, "Revenue was 12 billion in the year", []int{1, 2}); m.Page != 2 || m.Score != 1 {
		t.Fatalf("exact match on page 2 expected, got %+v", m)
	}
	m := Resolve(l, "net income declined", []int{2, 1})
	if m.Page != 2 || m.Precision != PrecisionPage || len(m.Regions) != 0 {
		t.Fatalf("unlocatable quote → page-level on the first candidate, got %+v", m)
	}
	if m := Resolve(l, "anything", nil); m.Precision != PrecisionSection {
		t.Fatalf("no pages → section-level, got %+v", m)
	}
}

func TestSentencesSplitsProseTablesAndHeadings(t *testing.T) {
	p := page(1,
		"PERFORMANCE BY BUSINESS SEGMENT",
		"Item 1, Business Segments, provides an overview. In addition, see Note 19. Effective in",
		"the first quarter of 2022, the measure changed.",
		"Long-term debt 14,001 16,056",
		"Total long-term debt, including current portion $ 15,939 $ 17,347",
		"Short-Term Borrowings and Current Portion of Long-Term Debt",
		"28",
	)
	got := Sentences(&p)
	want := []string{
		"PERFORMANCE BY BUSINESS SEGMENT",
		"Item 1, Business Segments, provides an overview.",
		"In addition, see Note 19.",
		"Effective in the first quarter of 2022, the measure changed.",
		"Long-term debt 14,001 16,056",
		"Total long-term debt, including current portion $ 15,939 $ 17,347",
		"Short-Term Borrowings and Current Portion of Long-Term Debt",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("sentences:\n got %q\nwant %q", got, want)
	}
}

func TestParseMarkersValidatesAndRenumbers(t *testing.T) {
	ans := "Revenue was $34.2 billion [2]. Debt fell to $14,001 million [1][9]. It also cites [2, 3]."
	marked, stripped := ParseMarkers(ans, 3)
	if stripped != 1 {
		t.Fatalf("stripped = %d, want 1 (the [9])", stripped)
	}
	if len(marked) != 4 {
		t.Fatalf("marked = %+v", marked)
	}
	if marked[0].Claim != "Revenue was $34.2 billion." || marked[0].Evidence != 2 {
		t.Errorf("first claim = %+v", marked[0])
	}
	ids := map[[2]int]int{}
	for i, m := range marked {
		ids[[2]int{m.Ordinal, m.Evidence}] = i + 1
	}
	got := Renumber(ans, ids)
	want := "Revenue was $34.2 billion [1]. Debt fell to $14,001 million [2]. It also cites [3][4]."
	if got != want {
		t.Fatalf("renumber:\n got %q\nwant %q", got, want)
	}
}

func TestRenumberDropsMarkersWithNoPincite(t *testing.T) {
	got := Renumber("A fact [1]. Another [2].", map[[2]int]int{{0, 1}: 1})
	if got != "A fact [1]. Another." {
		t.Fatalf("got %q", got)
	}
}

func TestChooseUsesTheJudgeAndHonoursNone(t *testing.T) {
	cands := []string{
		"Total revenue was $34.2 billion in 2022.",
		"Revenue in 2021 was $32.8 billion.",
		"Operating margin improved to 19.1%.",
	}
	j := &llmgate.MockJudge{Answers: map[string]llmgate.Answer{
		"c_0": llmgate.ChoiceAnswer{Choice: "s0", Probabilities: map[string]float64{"s0": 0.9, "s1": 0.05, "none": 0.05}, Confidence: 0.9},
		"c_1": llmgate.ChoiceAnswer{Choice: "none", Probabilities: map[string]float64{"s1": 0.2, "none": 0.8}, Confidence: 0.8},
	}}
	got, _, err := Choose(context.Background(), j, []Claim{
		{Claim: "Revenue was $34.2 billion in 2022.", Candidates: cands},
		{Claim: "Revenue in 2021 grew.", Candidates: cands},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Index != 0 || got[0].By != "judge" || got[0].P != 0.9 {
		t.Errorf("claim 0 = %+v", got[0])
	}
	if got[1].Index != -1 || got[1].By != "judge" {
		t.Errorf("claim 1: the Judge said none, so no sentence: %+v", got[1])
	}
	if n := len(j.Requests()); n != 1 {
		t.Errorf("all claims go in ONE batched request, got %d", n)
	}
}

func TestChooseFallsBackToLexicalWhenTheJudgeFails(t *testing.T) {
	cands := []string{"Inventories were 5,372 million.", "Total revenue was $34.2 billion in 2022."}
	j := &llmgate.MockJudge{Err: errors.New("provider down")}
	got, _, err := Choose(context.Background(), j, []Claim{{Claim: "revenue was 34.2 billion", Candidates: cands}})
	if err == nil {
		t.Fatal("the Judge's error must surface so the caller can log it")
	}
	if got[0].Index != 1 || got[0].By != "lexical" {
		t.Fatalf("lexical fallback = %+v", got[0])
	}
}
