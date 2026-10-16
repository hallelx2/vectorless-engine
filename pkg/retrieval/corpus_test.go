package retrieval

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// corpusJudge scripts a collection where one question needs two hops:
// two articles report Acme's figure, one of them names Beta Corp, and only
// a third article, about Beta Corp, holds the rest. Cards and pages that
// mention Acme score high; a card about Beta scores high only once what
// was found mentions Beta; the evidence is complete once it holds the
// Beta fact.
func corpusJudge(t *testing.T) (*llmgate.MockJudge, *corpusLog) {
	log := &corpusLog{}
	j := &llmgate.MockJudge{Respond: func(_ context.Context, req llmgate.JudgeRequest) (*llmgate.Judgment, error) {
		st := req.State.(map[string]any)
		found, _ := st["found"].(string)
		ans := map[string]llmgate.Answer{}
		for id, q := range req.Questions {
			log.add(id, fmt.Sprint(q.(llmgate.Noul).Instructions))
			p := 0.1
			switch {
			case id == "complete":
				if strings.Contains(found, "Beta-fact") {
					p = 0.9
				} else {
					p = 0.2
				}
			case strings.HasPrefix(id, "d_"):
				card := st[id].(map[string]any)["card"].(string)
				if found == "" && strings.Contains(card, "Acme") {
					p = 0.9
				}
				if found != "" && strings.Contains(card, "Beta") && strings.Contains(found, "Beta Corp") {
					p = 0.9
				}
			case strings.HasPrefix(id, "l_"):
				p = 0.9
			case strings.HasPrefix(id, "p_"):
				text := st[id].(map[string]any)["text"].(string)
				if strings.Contains(text, "Acme") || strings.Contains(text, "Beta-fact") {
					p = 0.95
				}
			}
			ans[id] = llmgate.NoulAnswer{Noul: p}
		}
		return &llmgate.Judgment{Model: "mock", Answers: ans, Usage: llmgate.Usage{InputTokens: 100, TotalTokens: 100, TokensReported: true}}, nil
	}}
	return j, log
}

type corpusLog struct {
	mu   sync.Mutex
	inst map[string][]string // question id prefix -> instructions seen
}

func (l *corpusLog) add(id, inst string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inst == nil {
		l.inst = map[string][]string{}
	}
	k := id
	if i := strings.IndexByte(id, '_'); i > 0 {
		k = id[:i]
	}
	l.inst[k] = append(l.inst[k], inst)
}

func article(id, title, text string) CorpusDoc {
	in := PagedNavInputs(title, []NavPage{{Number: 1, Text: text}})
	return CorpusDoc{ID: tree.DocumentID(id), Title: title, Card: CorpusCard(title, in, text, 0), Inputs: in}
}

func newsCorpus() []CorpusDoc {
	return []CorpusDoc{
		article("a", "TechNews: Acme results", "Acme reported revenue of 10 billion. Acme partnered with Beta Corp on chips."),
		article("b", "BizWire: Acme quarter", "Acme revenue came in at 10 billion, BizWire reports."),
		article("c", "Beta Corp profile", "Beta Corp is led by Dana Ruiz. Beta-fact: founded 1999."),
		article("d", "Sports roundup", "The home team won on Sunday."),
		article("e", "Weather", "Rain is expected over the weekend."),
		article("f", "Film review", "The sequel is longer than the original."),
	}
}

func TestCorpusBridgesToTheMissingDocument(t *testing.T) {
	j, log := corpusJudge(t)
	nav := CorpusNavigator{Judge: j, MinDocuments: 2}
	var steps []string
	nav.OnStep = func(s CorpusStep) { steps = append(steps, s.Kind) }
	res, err := nav.Navigate(context.Background(), "Which company did Acme partner with, and when was it founded?", newsCorpus())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hops) != 2 {
		t.Fatalf("want two hops (read Acme articles, then bridge to Beta), got %+v", res.Hops)
	}
	if got := res.Hops[0].Read; len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("hop 1 should read the two Acme articles in route order, got %v", got)
	}
	if got := res.Hops[1].Read; len(got) != 1 || got[0] != "c" {
		t.Fatalf("hop 2 should read only the Beta article, got %v", got)
	}
	if res.Coverage < 0.5 {
		t.Fatalf("coverage after the bridge should be high, got %v", res.Coverage)
	}
	var bridged bool
	for _, e := range res.Evidence {
		if e.DocumentID == "c" && e.Hop == 2 {
			bridged = true
		}
	}
	if !bridged {
		t.Fatalf("evidence should carry the Beta page from hop 2: %+v", res.Evidence)
	}
	if res.Considered != 6 || res.Usage.LLMCalls == 0 || res.Requests == 0 {
		t.Fatalf("accounting missing: considered=%d usage=%+v requests=%d", res.Considered, res.Usage, res.Requests)
	}
	for _, d := range res.Documents {
		if (d.DocumentID == "a" || d.DocumentID == "c") && !d.Read {
			t.Fatalf("document %s was read but not marked: %+v", d.DocumentID, d)
		}
	}
	if strings.Join(steps, ",") != "route,read,merge,coverage,route,read,merge,coverage" {
		t.Fatalf("steps reported out of order: %v", steps)
	}
	// Pages and leaves inside a corpus read are asked in multipart wording.
	for _, inst := range log.inst["p"] {
		if !strings.Contains(inst, "several documents") {
			t.Fatalf("page judged with single-document wording: %q", inst)
		}
	}
	if len(log.inst["l"]) == 0 || !strings.Contains(log.inst["l"][0], "several documents") {
		t.Fatalf("leaf judged with single-document wording: %v", log.inst["l"])
	}
}

func TestCorpusStopsWhenCoverageIsReached(t *testing.T) {
	j, _ := corpusJudge(t)
	nav := CorpusNavigator{Judge: j, MinDocuments: 2}
	docs := newsCorpus()
	docs[0] = article("a", "TechNews: Acme results", "Acme reported revenue of 10 billion. Beta-fact: none needed.")
	res, err := nav.Navigate(context.Background(), "What revenue did Acme report?", docs)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hops) != 1 {
		t.Fatalf("complete evidence after hop 1 should end the walk, got %d hops", len(res.Hops))
	}
}

func TestCorpusWithNothingRelevantReportsLowCoverage(t *testing.T) {
	j, _ := corpusJudge(t)
	nav := CorpusNavigator{Judge: j}
	docs := newsCorpus()[3:] // sports, weather, film
	res, err := nav.Navigate(context.Background(), "What revenue did Acme report?", docs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Coverage >= 0.5 {
		t.Fatalf("a collection with no answer must not look covered: %v", res.Coverage)
	}
	if len(res.Hops) > 2 {
		t.Fatalf("with nothing useful left to route, the bridge must stop: %+v", res.Hops)
	}
	for _, e := range res.Evidence {
		if e.Confidence >= 0.5 {
			t.Fatalf("no page should clear the threshold: %+v", e)
		}
	}
}

func TestCorpusMergeRepresentsEveryDocumentBeforeASecondPage(t *testing.T) {
	nav := CorpusNavigator{MaxPagesPerDocument: 4, MaxEvidence: 3}
	pool := []CorpusEvidence{
		{DocumentID: "x", Page: 1, Confidence: 0.99},
		{DocumentID: "x", Page: 2, Confidence: 0.98},
		{DocumentID: "x", Page: 3, Confidence: 0.97},
		{DocumentID: "x", Page: 4, Confidence: 0.96},
		{DocumentID: "x", Page: 5, Confidence: 0.95},
		{DocumentID: "y", Page: 9, Confidence: 0.6},
		{DocumentID: "z", Page: 1, Confidence: 0.2}, // under threshold
		{DocumentID: "x", Page: 1, Confidence: 0.5}, // the same page judged again, lower
	}
	got := nav.merge(pool)
	if len(got) != 3 {
		t.Fatalf("want 3 pages, got %+v", got)
	}
	if got[0].DocumentID != "x" || got[0].Confidence != 0.99 {
		t.Fatalf("best page first, keeping its best score: %+v", got[0])
	}
	var hasY bool
	for _, e := range got {
		hasY = hasY || e.DocumentID == "y"
		if e.DocumentID == "z" {
			t.Fatalf("a page under the threshold joined the list: %+v", got)
		}
	}
	if !hasY {
		t.Fatalf("the weaker document must still be represented: %+v", got)
	}

	capNav := CorpusNavigator{MaxPagesPerDocument: 2, MaxEvidence: 10}
	capped := capNav.merge(pool)
	n := 0
	for _, e := range capped {
		if e.DocumentID == "x" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("per-document cap ignored: %d pages from x", n)
	}
}

func TestCorpusMergeKeepsBestPagesWhenNoneClear(t *testing.T) {
	var nav CorpusNavigator
	got := nav.merge([]CorpusEvidence{
		{DocumentID: "a", Page: 1, Confidence: 0.3},
		{DocumentID: "b", Page: 1, Confidence: 0.4},
		{DocumentID: "c", Page: 1, Confidence: 0.1},
	})
	if len(got) != 2 || got[0].DocumentID != "b" {
		t.Fatalf("want the two best low-confidence pages, got %+v", got)
	}
}

func TestCorpusRouteBatchesUnderTheBudget(t *testing.T) {
	var requests atomic.Int32
	j := &llmgate.MockJudge{Respond: func(_ context.Context, req llmgate.JudgeRequest) (*llmgate.Judgment, error) {
		requests.Add(1)
		ans := map[string]llmgate.Answer{}
		for id := range req.Questions {
			ans[id] = llmgate.NoulAnswer{Noul: 0.1}
		}
		return &llmgate.Judgment{Answers: ans}, nil
	}}
	var docs []*CorpusDoc
	for i := 0; i < 300; i++ {
		d := article(string(rune('a'+i%26))+strings.Repeat("x", i), "doc", strings.Repeat("word ", 180))
		docs = append(docs, &d)
	}
	nav := CorpusNavigator{Judge: j, Navigator: JudgeNavigator{RequestBudgetTokens: 24000}}
	scores, _, n, err := nav.route(context.Background(), "q", "", docs)
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 300 {
		t.Fatalf("every document scored once, got %d", len(scores))
	}
	if n < 3 || int(requests.Load()) != n {
		t.Fatalf("300 cards of ~225 tokens need several requests under 24k, got %d", n)
	}
}

func TestCorpusCardShowsContentsAndOpening(t *testing.T) {
	in := &NavInputs{Leaves: []NavLeaf{
		{ID: "1", Title: "Business", Path: "PART I > Business"},
		{ID: "2", Title: "Risk", Path: "PART I > Risk"},
		{ID: "3", Title: "Statements", Path: "PART II > Statements"},
	}}
	card := CorpusCard("Acme 10-K", in, "Acme Corp annual report for fiscal 2023", 0)
	for _, want := range []string{"Acme 10-K", "Contents: PART I; PART II", "Opens: Acme Corp annual report"} {
		if !strings.Contains(card, want) {
			t.Fatalf("card missing %q:\n%s", want, card)
		}
	}
	if len(CorpusCard("t", in, strings.Repeat("a", 5000), 300)) > 300 {
		t.Fatal("card not bounded")
	}
}

type countingPages struct {
	calls atomic.Int32
	pages map[tree.DocumentID][]NavPage
}

func (c *countingPages) LoadPages(_ context.Context, id tree.DocumentID) ([]NavPage, error) {
	c.calls.Add(1)
	return c.pages[id], nil
}

func TestCorpusLibraryCachesUntilTheDocumentChanges(t *testing.T) {
	ps := &countingPages{pages: map[tree.DocumentID][]NavPage{
		"a": {{Number: 2, Text: "second"}, {Number: 1, Text: "Acme opens here"}},
	}}
	lib := NewCorpusLibrary(&JudgeWalkStrategy{Pages: ps}, nil)
	t0 := time.Unix(100, 0)
	ref := CorpusRef{ID: "a", Title: "Acme", UpdatedAt: t0}
	docs, errs := lib.Docs(context.Background(), "org", "", []CorpusRef{ref})
	if len(errs) != 0 || len(docs) != 1 {
		t.Fatalf("build failed: %v", errs)
	}
	if !strings.Contains(docs[0].Card, "Opens: Acme opens here") {
		t.Fatalf("card should open on the first page, got %q", docs[0].Card)
	}
	if docs[0].Inputs.Pages[0].Number != 1 {
		t.Fatal("pages should be in order")
	}
	_, _ = lib.Docs(context.Background(), "org", "", []CorpusRef{ref})
	if ps.calls.Load() != 1 {
		t.Fatalf("second question should hit the cache, loaded %d times", ps.calls.Load())
	}
	ref.UpdatedAt = t0.Add(time.Hour)
	_, _ = lib.Docs(context.Background(), "org", "", []CorpusRef{ref})
	if ps.calls.Load() != 2 {
		t.Fatal("a re-ingested document must be rebuilt")
	}
	if n, b := lib.CachedDocuments(); n != 1 || b <= 0 {
		t.Fatalf("cache should hold one document, holds %d (%d bytes)", n, b)
	}

	lib.MaxBytes = -1
	_, _ = lib.Docs(context.Background(), "org", "", []CorpusRef{{ID: "a", Title: "Acme", UpdatedAt: t0.Add(2 * time.Hour)}})
	_, _ = lib.Docs(context.Background(), "org", "", []CorpusRef{{ID: "a", Title: "Acme", UpdatedAt: t0.Add(2 * time.Hour)}})
	if ps.calls.Load() != 4 {
		t.Fatalf("a disabled cache must rebuild every time, loaded %d", ps.calls.Load())
	}
}

func TestCorpusLibraryReportsADocumentItCannotRead(t *testing.T) {
	lib := NewCorpusLibrary(&JudgeWalkStrategy{Pages: &countingPages{}}, func(context.Context, tree.DocumentID, string, string) (*tree.Tree, error) {
		return &tree.Tree{Root: &tree.Section{}}, nil
	})
	docs, errs := lib.Docs(context.Background(), "org", "", []CorpusRef{{ID: "empty"}})
	if len(docs) != 0 || errs["empty"] == nil {
		t.Fatalf("an unreadable document must be reported, not returned: docs=%v errs=%v", docs, errs)
	}
}

// A Markdown or text document has no pages. It must still be navigable:
// its content sections, in order, are the units (HAL-3075's MultiHop smoke
// returned nothing for 609 articles until this held).
func TestSectionTreeWithoutPagesIsNavigable(t *testing.T) {
	tr := &tree.Tree{DocumentID: "md", Root: &tree.Section{ID: "root", Children: []*tree.Section{
		{ID: "intro", Title: "Intro", ContentRef: "ref/intro"},
		{ID: "body", Title: "Acme deal", ContentRef: "ref/body"},
		{ID: "empty", Title: "Heading only"},
	}}}
	loader := pageLoaderFunc(func(_ context.Context, ref string) ([]byte, error) {
		if ref == "ref/body" {
			return []byte("Acme partnered with Beta Corp."), nil
		}
		return []byte("Welcome."), nil
	})
	s := &JudgeWalkStrategy{PageLoader: loader}
	in := s.SectionTreeNavInputs(tr)
	if in == nil || len(in.Leaves) != 2 || !in.Ordinal {
		t.Fatalf("want two ordinal leaves, got %+v", in)
	}
	if in.Leaves[1].Start != 2 || in.Leaves[1].Title != "Acme deal" {
		t.Fatalf("leaves numbered in document order: %+v", in.Leaves)
	}
	ps, err := in.Load(context.Background(), in.Leaves[1])
	if err != nil || len(ps) != 1 || !strings.Contains(ps[0].Text, "Beta Corp") {
		t.Fatalf("load: %v %+v", err, ps)
	}

	j, _ := corpusJudge(t)
	nav := CorpusNavigator{Judge: j, MinDocuments: 1}
	res, err := nav.Navigate(context.Background(), "Who did Acme partner with?",
		[]CorpusDoc{{ID: "md", Title: "Acme news", Card: "Acme news", Inputs: in}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Evidence) == 0 || !res.Evidence[0].Ordinal || res.Evidence[0].SectionTitle != "Acme deal" {
		t.Fatalf("an unpaged document should yield ordinal evidence naming its section: %+v", res.Evidence)
	}

	// The single-document strategy runs over the same units, and cites no
	// invented pages.
	single := &JudgeWalkStrategy{Navigator: JudgeNavigator{Judge: j}, PageLoader: loader}
	r, err := single.SelectWithCost(context.Background(), tr, "Who did Acme partner with?", ContextBudget{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.SelectedIDs) == 0 || len(r.CitedPages) != 0 {
		t.Fatalf("single-document navigation of an unpaged tree: ids=%v cited=%v", r.SelectedIDs, r.CitedPages)
	}
}

func TestCorpusEmptyListsAreNotNull(t *testing.T) {
	j, _ := corpusJudge(t)
	res, err := (&CorpusNavigator{Judge: j}).Navigate(context.Background(), "q", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Evidence == nil || res.Documents == nil || res.Hops == nil {
		t.Fatalf("empty lists must encode as [], got %+v", res)
	}
}

type pageLoaderFunc func(ctx context.Context, ref string) ([]byte, error)

func (f pageLoaderFunc) Load(ctx context.Context, ref string) ([]byte, error) { return f(ctx, ref) }

// Every list in a corpus response encodes as a list: a bridge hop that
// reads nothing is "read": [], not null (the SDK rejected null).
func TestCorpusJSONHasNoNullLists(t *testing.T) {
	j, _ := corpusJudge(t)
	nav := CorpusNavigator{Judge: j, MinDocuments: 2}
	all := newsCorpus()
	docs := append([]CorpusDoc{all[0], all[1]}, all[3:]...) // no Beta article: the bridge finds nothing
	res, err := nav.Navigate(context.Background(), "Which company did Acme partner with, and when was it founded?", docs)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hops) < 2 {
		t.Fatalf("expected a bridge hop that reads nothing, got %+v", res.Hops)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "null") {
		t.Fatalf("null in corpus JSON: %s", raw)
	}
}
