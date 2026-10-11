package retrieval

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hallelx2/llmgate"
)

// bigTenKLoad gives every leaf all its pages, with the answer on page
// 77 of Item 8 — deep enough that the coarse pass must find it.
func bigTenKLoad(_ context.Context, l NavLeaf) ([]NavPage, error) {
	var ps []NavPage
	for p := l.Start; p <= l.End; p++ {
		text := fmt.Sprintf("Page %d of the filing. %s", p, strings.Repeat("narrative ", 40))
		if p == 77 {
			text = "CONSOLIDATED STATEMENTS OF INCOME\nTotal revenue 17,606 15,785"
		}
		ps = append(ps, NavPage{Number: p, Text: text})
	}
	return ps, nil
}

func evidencePages(r *NavResult) []int {
	var out []int
	for _, e := range r.Evidence {
		out = append(out, e.Page.Number)
	}
	sort.Ints(out)
	return out
}

func readPages(r *NavResult) []int {
	var out []int
	for _, p := range r.Pages {
		out = append(out, p.Page.Number)
	}
	sort.Ints(out)
	return out
}

// The point of SkimAll: the section ranking and the head skim are in
// flight at the same time. The mock holds the leaf request until a head
// request has arrived; run sequentially, the head request never comes
// and the leaf request times out.
func TestSkimAllSendsRankingAndSkimTogether(t *testing.T) {
	base, _ := navJudge("financial statements", "Total revenue 17,606")
	headArrived := make(chan struct{})
	var once sync.Once
	j := &llmgate.MockJudge{Respond: func(ctx context.Context, req llmgate.JudgeRequest) (*llmgate.Judgment, error) {
		isLeaf, isHead := false, false
		for id, q := range req.Questions {
			if strings.HasPrefix(id, "l_") {
				isLeaf = true
			}
			if nq, ok := q.(llmgate.Noul); ok && strings.Contains(fmt.Sprint(nq.Instructions), "is the START of page") {
				isHead = true
			}
		}
		if isHead {
			once.Do(func() { close(headArrived) })
		}
		if isLeaf {
			select {
			case <-headArrived:
			case <-time.After(2 * time.Second):
				return nil, fmt.Errorf("the section ranking went out alone: the head skim was not in flight with it")
			}
		}
		return base.Respond(ctx, req)
	}}
	n := &JudgeNavigator{Judge: j, MaxPages: 10, SkimAll: true}
	res, err := n.Navigate(context.Background(), "What was total revenue in FY2022?", tenKLeaves(), bigTenKLoad)
	if err != nil {
		t.Fatal(err)
	}
	if got := evidencePages(res); len(got) == 0 || res.Evidence[0].Page.Number != 77 {
		t.Fatalf("evidence %v, want page 77 first", got)
	}
	if res.Coarse == nil {
		t.Error("the coarse result should record the heads that were skimmed")
	}
}

// Skimming everything must not change what is read: the candidates are
// still the ranked sections' pages, ordered by the same head scores.
func TestSkimAllReadsTheSamePagesAsTheSequentialSkim(t *testing.T) {
	for _, maxP := range []int{5, 10, 40} {
		seqJ, _ := navJudge("financial statements", "Total revenue 17,606")
		allJ, _ := navJudge("financial statements", "Total revenue 17,606")
		seq := &JudgeNavigator{Judge: seqJ, MaxPages: maxP, CoarsePages: 60}
		all := &JudgeNavigator{Judge: allJ, MaxPages: maxP, CoarsePages: 60, SkimAll: true}
		q := "What was total revenue in FY2022?"
		a, err := seq.Navigate(context.Background(), q, tenKLeaves(), bigTenKLoad)
		if err != nil {
			t.Fatal(err)
		}
		b, err := all.Navigate(context.Background(), q, tenKLeaves(), bigTenKLoad)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(readPages(a)) != fmt.Sprint(readPages(b)) {
			t.Errorf("maxPages=%d: sequential read %v, SkimAll read %v", maxP, readPages(a), readPages(b))
		}
		if fmt.Sprint(evidencePages(a)) != fmt.Sprint(evidencePages(b)) {
			t.Errorf("maxPages=%d: evidence %v vs %v", maxP, evidencePages(a), evidencePages(b))
		}
		if fmt.Sprint(a.Selected) != fmt.Sprint(b.Selected) {
			t.Errorf("maxPages=%d: selected sections differ", maxP)
		}
	}
}

// A document past SkimAllMaxPages skims sequentially, so the extra
// heads stay bounded on a very long document.
func TestSkimAllFallsBackOnALongDocument(t *testing.T) {
	j, _ := navJudge("financial statements", "Total revenue 17,606")
	n := &JudgeNavigator{Judge: j, MaxPages: 10, CoarsePages: 60, SkimAll: true, SkimAllMaxPages: 50}
	res, err := n.Navigate(context.Background(), "What was total revenue in FY2022?", tenKLeaves(), bigTenKLoad)
	if err != nil {
		t.Fatal(err)
	}
	// The document is 90 pages; the sequential skim reads only the
	// gathered candidates, never more than CoarsePages.
	if len(res.Coarse) == 0 || len(res.Coarse) > 60 {
		t.Errorf("coarse skimmed %d heads; over the cap it should be the sequential skim of at most 60", len(res.Coarse))
	}
}

// A tree with more leaves than one request holds sends every batch at
// once. Each request waits until all three have arrived.
func TestRankLeavesSendsItsBatchesTogether(t *testing.T) {
	var leaves []NavLeaf
	for i := 0; i < 2*navLeafBatch+10; i++ {
		leaves = append(leaves, NavLeaf{ID: fmt.Sprint(i), Title: fmt.Sprintf("Note %d", i), Start: i + 1, End: i + 1})
	}
	var wg sync.WaitGroup
	wg.Add(3)
	j := &llmgate.MockJudge{Respond: func(ctx context.Context, req llmgate.JudgeRequest) (*llmgate.Judgment, error) {
		wg.Done()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			return nil, fmt.Errorf("leaf batches were sent one at a time")
		}
		ans := map[string]llmgate.Answer{}
		for id := range req.Questions {
			ans[id] = llmgate.NoulAnswer{Noul: 0.5}
		}
		return &llmgate.Judgment{Answers: ans}, nil
	}}
	n := &JudgeNavigator{Judge: j}
	scores, _, reqs, err := n.RankLeaves(context.Background(), "q", leaves)
	if err != nil {
		t.Fatal(err)
	}
	if reqs != 3 || len(scores) != len(leaves) {
		t.Errorf("requests %d, scores %d", reqs, len(scores))
	}
}
