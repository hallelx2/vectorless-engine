package ingest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

func pagesFrom(texts ...string) []PageText {
	out := make([]PageText, 0, len(texts))
	for i, t := range texts {
		out = append(out, PageText{PageNumber: i + 1, Text: t})
	}
	return out
}

// answerAll builds a MockJudge that answers every Noul with p.
func answerAll(p float64) *llmgate.MockJudge {
	return &llmgate.MockJudge{
		Respond: func(_ context.Context, req llmgate.JudgeRequest) (*llmgate.Judgment, error) {
			ans := make(map[string]llmgate.Answer, len(req.Questions))
			for id := range req.Questions {
				ans[id] = llmgate.NoulAnswer{Noul: p}
			}
			return &llmgate.Judgment{Model: "mock", Answers: ans,
				Usage: llmgate.Usage{InputTokens: 10, TotalTokens: 10, TokensReported: true}}, nil
		},
	}
}

// The property the whole change exists for: N pages become a small
// number of requests, not N.
func TestDetectAsksOneBatchNotOneCallPerPage(t *testing.T) {
	var requests int
	j := &llmgate.MockJudge{
		Respond: func(_ context.Context, req llmgate.JudgeRequest) (*llmgate.Judgment, error) {
			requests++
			ans := map[string]llmgate.Answer{}
			for id := range req.Questions {
				// Only page 2 is a TOC.
				p := 0.02
				if strings.HasSuffix(id, "002") {
					p = 0.97
				}
				ans[id] = llmgate.NoulAnswer{Noul: p}
			}
			return &llmgate.Judgment{Model: "mock", Answers: ans}, nil
		},
	}

	b := &TOCBuilder{Judge: j}
	pages := pagesFrom("cover page text", "TABLE OF CONTENTS\n1. Intro .... 4", "body text here")

	got, handled := b.detectTOCPagesJudge(context.Background(), pages, 20, &Usage{})
	if !handled {
		t.Fatal("handled = false, want the Judge to have answered")
	}
	if requests != 1 {
		t.Errorf("made %d requests for 3 pages, want 1", requests)
	}
	if len(got) != 1 || got[0] != 2 {
		t.Errorf("detected %v, want [2]", got)
	}
}

// A nil Judge must be invisible: the caller falls back and behaviour is
// exactly what it was before this existed.
func TestNilJudgeDeclines(t *testing.T) {
	b := &TOCBuilder{}
	if _, handled := b.detectTOCPagesJudge(context.Background(), pagesFrom("x"), 20, &Usage{}); handled {
		t.Error("a nil Judge reported handled = true")
	}
	if _, handled := b.verifyTitlesJudge(context.Background(), nil, nil, &Usage{}); handled {
		t.Error("a nil Judge reported handled = true for verification")
	}
}

// A transport failure must decline the whole phase rather than return
// what it managed. A partial detection silently truncates the scanned
// range and is indistinguishable from "no TOC here".
func TestTransportFailureDeclinesRatherThanTruncating(t *testing.T) {
	b := &TOCBuilder{Judge: &llmgate.MockJudge{Err: errors.New("boom")}}

	got, handled := b.detectTOCPagesJudge(context.Background(), pagesFrom("a", "b"), 20, &Usage{})
	if handled {
		t.Error("handled = true after a transport failure")
	}
	if got != nil {
		t.Errorf("returned %v after a failure, want nil", got)
	}
}

func TestThresholdDecidesYes(t *testing.T) {
	pages := pagesFrom("some text")

	// 0.6 with a 0.5 threshold is a yes.
	b := &TOCBuilder{Judge: answerAll(0.6)}
	if got, _ := b.detectTOCPagesJudge(context.Background(), pages, 20, &Usage{}); len(got) != 1 {
		t.Errorf("0.6 against the default threshold gave %v, want one page", got)
	}

	// The same answer with a stricter threshold is a no.
	b = &TOCBuilder{Judge: answerAll(0.6), JudgeThreshold: 0.9}
	if got, _ := b.detectTOCPagesJudge(context.Background(), pages, 20, &Usage{}); len(got) != 0 {
		t.Errorf("0.6 against a 0.9 threshold gave %v, want none", got)
	}
}

// Batching is by measured tokens, not page count.
//
// Asserted on batchByTokens directly rather than through a request
// count, because a request count conflates the budget with how dense
// the test's filler text happens to be — 20 pages of sparse filler fits
// in one request and 20 pages of a real filing does not. The mechanism
// is what needs testing.
func TestBatchesByTokenBudgetNotPageCount(t *testing.T) {
	// Self-calibrating: measure what one page actually costs, then use
	// enough pages that a split is arithmetically required. Hard-coding
	// a page count instead would make the test a statement about how the
	// tokenizer happens to compress the filler text.
	page := strings.Repeat("the quick brown fox jumps over the lazy dog ", 200)
	perPage := estimateTokens(page)
	if perPage == 0 || perPage > judgeQuestionBudget {
		t.Fatalf("test page estimates at %d tokens, unusable against a %d per-question budget",
			perPage, judgeQuestionBudget)
	}

	n := judgeBudget/perPage + 2
	texts := make([]string, n)
	for i := range texts {
		texts[i] = page
	}

	batches := batchByTokens(pagesFrom(texts...), len(page))
	if len(batches) < 2 {
		t.Fatalf("%d pages at ~%d tokens each (budget %d) produced %d batch(es); "+
			"the budget was not enforced", n, perPage, judgeBudget, len(batches))
	}

	var total int
	for _, b := range batches {
		if len(b) == 0 {
			t.Error("emitted an empty batch")
		}
		total += len(b)
	}
	if total != n {
		t.Errorf("batches hold %d pages in total, want all %d", total, n)
	}
}

// Every page has to end up in exactly one batch. A page silently dropped
// during packing reads downstream as "not a table of contents", which is
// indistinguishable from a real negative.
func TestBatchingLosesNoPages(t *testing.T) {
	texts := make([]string, 25)
	for i := range texts {
		texts[i] = strings.Repeat("content ", 500*(i%4+1))
	}
	pages := pagesFrom(texts...)

	seen := map[int]int{}
	for _, b := range batchByTokens(pages, tocDetectorMaxChars) {
		for _, p := range b {
			seen[p.PageNumber]++
		}
	}
	for _, p := range pages {
		if seen[p.PageNumber] != 1 {
			t.Errorf("page %d appears in %d batches, want exactly 1", p.PageNumber, seen[p.PageNumber])
		}
	}
}

// Usage has to survive the switch, or a Judge-accelerated ingest silently
// reports spending nothing.
func TestUsageIsAccumulated(t *testing.T) {
	var u Usage
	b := &TOCBuilder{Judge: answerAll(0.1)}
	if _, handled := b.detectTOCPagesJudge(context.Background(), pagesFrom("a", "b"), 20, &u); !handled {
		t.Fatal("handled = false")
	}
	if u.LLMCalls == 0 || u.InputTokens == 0 {
		t.Errorf("usage not recorded: %+v", u)
	}
}

func leaf(title string, page int) tree.TOCNode {
	return tree.TOCNode{Title: title, StartPage: page}
}

func TestVerificationClearsUnconfirmedPages(t *testing.T) {
	nodes := []tree.TOCNode{leaf("Introduction", 1), leaf("Methods", 2)}
	pages := pagesFrom("Introduction\nbody", "something else entirely")

	j := &llmgate.MockJudge{
		Respond: func(_ context.Context, req llmgate.JudgeRequest) (*llmgate.Judgment, error) {
			ans := map[string]llmgate.Answer{}
			for id := range req.Questions {
				p := 0.95
				if strings.HasPrefix(id, "s2_") {
					p = 0.03 // Methods does not start on page 2
				}
				ans[id] = llmgate.NoulAnswer{Noul: p}
			}
			return &llmgate.Judgment{Model: "mock", Answers: ans}, nil
		},
	}

	b := &TOCBuilder{Judge: j}
	verdicts, handled := b.verifyTitlesJudge(context.Background(), nodes, pages, &Usage{})
	if !handled {
		t.Fatal("handled = false")
	}
	applyJudgeVerdicts(nodes, verdicts)

	if nodes[0].StartPage != 1 {
		t.Errorf("confirmed page was cleared: %d", nodes[0].StartPage)
	}
	if nodes[1].StartPage != 0 {
		t.Errorf("unconfirmed page = %d, want 0", nodes[1].StartPage)
	}
}

// A question that was never asked must not be treated as a "no".
// Clearing on absence would silently discard correct page numbers for
// any section whose page text was too large to send.
func TestMissingVerdictLeavesThePageAlone(t *testing.T) {
	nodes := []tree.TOCNode{leaf("Introduction", 1)}
	applyJudgeVerdicts(nodes, map[string]bool{"s9_somethingelse": false})

	if nodes[0].StartPage != 1 {
		t.Errorf("StartPage = %d after an unrelated verdict, want 1 (unchanged)", nodes[0].StartPage)
	}
}

// Two identically-titled sections on the same page must get distinct
// keys, or the second is silently never verified.
func TestDuplicateTitlesGetDistinctKeys(t *testing.T) {
	nodes := []tree.TOCNode{leaf("Notes", 3), leaf("Notes", 3)}
	byPage := map[int]string{3: "Notes\nbody"}

	claims := collectLeafClaims(nodes, byPage, judgeVerdictKey)
	if len(claims) != 2 {
		t.Fatalf("collected %d claims, want 2", len(claims))
	}
	if claims[0].key == claims[1].key {
		t.Errorf("both claims share the key %q", claims[0].key)
	}
}

// Internal nodes inherit their start page from their first child, so
// verifying them separately asks and pays for the same question twice.
func TestOnlyLeavesAreVerified(t *testing.T) {
	nodes := []tree.TOCNode{{
		Title: "Part I", StartPage: 1,
		Nodes: []tree.TOCNode{leaf("Chapter 1", 1), leaf("Chapter 2", 2)},
	}}
	byPage := map[int]string{1: "Chapter 1\nbody", 2: "Chapter 2\nbody"}

	claims := collectLeafClaims(nodes, byPage, judgeVerdictKey)
	if len(claims) != 2 {
		t.Fatalf("collected %d claims, want 2 (leaves only)", len(claims))
	}
	for _, c := range claims {
		if c.title == "Part I" {
			t.Error("an internal node was collected for verification")
		}
	}
}

func TestSlugForKeyIsUsableAndStable(t *testing.T) {
	cases := map[string]string{
		"Item 1A. Risk Factors": "item_1a_risk_factors",
		"":                      "untitled",
		"!!!":                   "untitled",
	}
	for in, want := range cases {
		if got := slugForKey(in); got != want {
			t.Errorf("slugForKey(%q) = %q, want %q", in, got, want)
		}
	}
	// Long titles must stay bounded — a state field name is not a place
	// to put a whole heading.
	long := slugForKey(strings.Repeat("verylongsection ", 20))
	if len(long) > 45 {
		t.Errorf("slug is %d chars, want it bounded", len(long))
	}
}

// A page too large to ask about at all must be skipped, not allowed to
// fail every batch it would join.
func TestOversizedPageIsSkippedNotFatal(t *testing.T) {
	huge := strings.Repeat("the quick brown fox ", judgeBudget)
	pages := []PageText{
		{PageNumber: 1, Text: "normal page"},
		{PageNumber: 2, Text: huge},
	}

	batches := batchByTokens(pages, len(huge))
	_ = judgeQuestionBudget // the per-question ceiling is separate; see toc_judge.go
	var total int
	for _, b := range batches {
		total += len(b)
	}
	if total != 1 {
		t.Errorf("batched %d pages, want 1 — the oversized page should be dropped", total)
	}
}

func TestEmptyPagesAreNotAsked(t *testing.T) {
	var requests int
	j := &llmgate.MockJudge{
		Respond: func(_ context.Context, _ llmgate.JudgeRequest) (*llmgate.Judgment, error) {
			requests++
			return &llmgate.Judgment{Model: "mock", Answers: map[string]llmgate.Answer{}}, nil
		},
	}
	b := &TOCBuilder{Judge: j}

	_, handled := b.detectTOCPagesJudge(context.Background(), pagesFrom("", "   ", "\n"), 20, &Usage{})
	if !handled {
		t.Error("handled = false for a page set that is simply empty")
	}
	if requests != 0 {
		t.Errorf("made %d requests for blank pages, want 0", requests)
	}
}

// Every question must name the state field it is about. Question IDs are
// not sent to the model, so without it the model sees one state and many
// indistinguishable questions.
func TestQuestionsNameTheirStateField(t *testing.T) {
	var seen bool
	j := &llmgate.MockJudge{
		Respond: func(_ context.Context, req llmgate.JudgeRequest) (*llmgate.Judgment, error) {
			ans := map[string]llmgate.Answer{}
			for id, q := range req.Questions {
				n, ok := q.(llmgate.Noul)
				if !ok {
					t.Errorf("question %q is %T, want llmgate.Noul", id, q)
				}
				if s, _ := n.Instructions.(string); strings.Contains(s, fmt.Sprintf("`%s`", id)) {
					seen = true
				}
				ans[id] = llmgate.NoulAnswer{Noul: 0.1}
			}
			return &llmgate.Judgment{Model: "mock", Answers: ans}, nil
		},
	}

	b := &TOCBuilder{Judge: j}
	b.detectTOCPagesJudge(context.Background(), pagesFrom("page one text"), 20, &Usage{})
	if !seen {
		t.Error("no question referenced its own state field by name")
	}
}
