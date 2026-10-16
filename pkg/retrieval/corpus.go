package retrieval

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// Corpus mode: one question over a whole collection (HAL-3075).
//
// JudgeNavigator answers a question about one document. A collection
// question ("did the two outlets agree on the figure?", "which company
// named in both reports...") needs facts from several documents, and
// nobody says which. Corpus mode keeps every decision a closed-set
// judgement on the Judge, and needs no index and no embedding:
//
//  1. Route. Every document is shown to the Judge as a short card —
//     its title, its top-level contents, how it opens — and the Judge
//     gives the probability that it holds any part of what the question
//     needs.
//  2. Read. The best documents are navigated in parallel with the
//     ordinary JudgeNavigator, in multipart wording: a page counts when
//     it states any of the needed facts.
//  3. Merge. The Judge's probabilities are calibrated against the same
//     question and the same criteria, so pages from different documents
//     rank on one scale. They are merged into one list, a few pages per
//     document at most, every contributing document represented.
//  4. Hop. The Judge is asked whether what was found contains every fact
//     the question needs. When it does not, the unread documents are
//     routed again with what was found in the state — would this one
//     supply what is still missing? — and the best are read. This is the
//     bridge step of a multi-hop question, taken by selection: no
//     sub-question is written.
//  5. Coverage. The final completeness probability is returned, so the
//     caller can decline a question the collection does not answer.

// CorpusDoc is one document a corpus question may read.
type CorpusDoc struct {
	ID    tree.DocumentID
	Title string
	// Card is what routing sees: title, top-level contents, opening text.
	Card string
	// Inputs are the document's leaves and page loader.
	Inputs *NavInputs
}

// CorpusEvidence is one page in the merged evidence list.
type CorpusEvidence struct {
	DocumentID    tree.DocumentID `json:"document_id"`
	DocumentTitle string          `json:"document_title,omitempty"`
	Page          int             `json:"page"`
	SectionTitle  string          `json:"section_title,omitempty"`
	Text          string          `json:"text"`
	Confidence    float64         `json:"confidence"`
	Hop           int             `json:"hop"`
}

// CorpusDocScore is a document's routing score: the probability that it
// holds part of what the question needs, from the hop that scored it last.
type CorpusDocScore struct {
	DocumentID tree.DocumentID `json:"document_id"`
	Title      string          `json:"title,omitempty"`
	Score      float64         `json:"score"`
	Hop        int             `json:"hop"`
	Read       bool            `json:"read"`
}

// CorpusHop is one round of route, read, merge and coverage.
type CorpusHop struct {
	Hop int `json:"hop"`
	// Routed is how many documents the Judge scored this hop.
	Routed int `json:"routed"`
	// Read are the documents navigated this hop, best route score first.
	Read []tree.DocumentID `json:"read"`
	// NewEvidence is how many pages this hop added to the merged list.
	NewEvidence int `json:"new_evidence"`
	// Coverage is the Judge's probability, after this hop, that the merged
	// evidence holds every fact the question needs.
	Coverage float64 `json:"coverage"`
}

// CorpusResult is what a corpus question found.
type CorpusResult struct {
	Evidence  []CorpusEvidence `json:"evidence"`
	Documents []CorpusDocScore `json:"documents"`
	Hops      []CorpusHop      `json:"hops"`
	// Coverage is the final completeness probability.
	Coverage float64 `json:"coverage"`
	// Considered is how many documents the question was asked over.
	Considered int `json:"documents_considered"`
	// Errors are documents that could not be read, by ID; the question is
	// still answered from the rest.
	Errors   map[tree.DocumentID]string `json:"errors,omitempty"`
	Usage    Usage                      `json:"usage"`
	Requests int                        `json:"judge_requests"`
}

// CorpusStep reports one stage as it completes, for display.
type CorpusStep struct {
	// Kind is "route", "read", "merge" or "coverage".
	Kind      string            `json:"kind"`
	Hop       int               `json:"hop"`
	Documents []CorpusDocScore  `json:"documents,omitempty"`
	Evidence  int               `json:"evidence,omitempty"`
	Coverage  float64           `json:"coverage,omitempty"`
	Read      []tree.DocumentID `json:"read,omitempty"`
}

// CorpusNavigator answers one question over many documents.
type CorpusNavigator struct {
	Judge llmgate.Judge

	// Navigator is the template each document is read with. Multipart is
	// forced on; SkimAll is set per document when its pages are in memory.
	Navigator JudgeNavigator

	// OnStep, when set, is told about each stage as it completes.
	OnStep func(CorpusStep)

	// RouteThreshold is the routing probability at or above which a
	// document is read. Zero selects 0.5.
	RouteThreshold float64
	// MinDocuments are read on the first hop even below the threshold:
	// a low-confidence best guess beats reading nothing. Zero selects 3.
	MinDocuments int
	// MaxDocuments bounds the documents read per hop. Zero selects 8.
	MaxDocuments int
	// MaxPagesPerDocument bounds one document's pages in the merged list.
	// Zero selects 4.
	MaxPagesPerDocument int
	// MaxEvidence bounds the merged list. Zero selects 16.
	MaxEvidence int
	// EvidenceThreshold is the page probability at or above which a page
	// joins the merged list. Zero selects the navigator's threshold (0.5).
	EvidenceThreshold float64
	// MaxHops bounds the rounds. Zero selects 3; 1 disables the bridge.
	MaxHops int
	// CoverageThreshold is the completeness probability at or above which
	// no further hop is taken. Zero selects 0.5.
	CoverageThreshold float64
	// CardChars bounds one card. Zero selects 900.
	CardChars int
	// FoundChars bounds the found-so-far state of the coverage and bridge
	// judgements. Zero selects 6000.
	FoundChars int
	// ReadConcurrency bounds documents navigated at once. Zero selects 8.
	ReadConcurrency int
}

const (
	defaultCorpusRouteThreshold = 0.5
	defaultCorpusMinDocs        = 3
	defaultCorpusMaxDocs        = 8
	defaultCorpusPagesPerDoc    = 4
	defaultCorpusMaxEvidence    = 16
	defaultCorpusMaxHops        = 3
	defaultCorpusCoverage       = 0.5
	defaultCorpusCardChars      = 900
	defaultCorpusFoundChars     = 6000
	defaultCorpusReadPar        = 8
	corpusFoundPageChars        = 900
)

func corpusOr[T int | float64](v, def T) T {
	if v > 0 {
		return v
	}
	return def
}

func (c *CorpusNavigator) step(s CorpusStep) {
	if c.OnStep != nil {
		c.OnStep(s)
	}
}

// Navigate runs the corpus question.
func (c *CorpusNavigator) Navigate(ctx context.Context, query string, docs []CorpusDoc) (*CorpusResult, error) {
	if c.Judge == nil {
		return nil, fmt.Errorf("corpus: no Judge configured")
	}
	res := &CorpusResult{Considered: len(docs), Errors: map[tree.DocumentID]string{}}
	if len(docs) == 0 {
		return res, nil
	}
	byID := make(map[tree.DocumentID]*CorpusDoc, len(docs))
	for i := range docs {
		byID[docs[i].ID] = &docs[i]
	}
	scores := map[tree.DocumentID]*CorpusDocScore{}
	read := map[tree.DocumentID]bool{}
	var pool []CorpusEvidence // every page any read document judged, before the merge

	maxHops := corpusOr(c.MaxHops, defaultCorpusMaxHops)
	for hop := 1; hop <= maxHops; hop++ {
		var unread []*CorpusDoc
		for i := range docs {
			if !read[docs[i].ID] {
				unread = append(unread, &docs[i])
			}
		}
		if len(unread) == 0 {
			break
		}
		found := ""
		if hop > 1 {
			found = c.foundState(res.Evidence)
		}
		routed, u, n, err := c.route(ctx, query, found, unread)
		if err != nil {
			return nil, fmt.Errorf("corpus: route (hop %d): %w", hop, err)
		}
		res.Usage.Add(u)
		res.Requests += n
		for _, r := range routed {
			r.Hop = hop
			cp := r
			scores[r.DocumentID] = &cp
		}
		chosen := c.choose(routed, hop)
		c.step(CorpusStep{Kind: "route", Hop: hop, Documents: topScores(routed, 8)})
		if len(chosen) == 0 {
			// A bridge hop where nothing unread looks useful: what was found
			// is all the collection has.
			res.Hops = append(res.Hops, CorpusHop{Hop: hop, Routed: len(routed), Coverage: res.Coverage})
			break
		}
		for _, id := range chosen {
			read[id] = true
			scores[id].Read = true
		}
		c.step(CorpusStep{Kind: "read", Hop: hop, Read: chosen})
		pages, ru, rn, errs := c.read(ctx, query, chosen, byID, hop)
		res.Usage.Add(ru)
		res.Requests += rn
		for id, e := range errs {
			res.Errors[id] = e.Error()
		}
		pool = append(pool, pages...)
		before := evidenceKeys(res.Evidence)
		res.Evidence = c.merge(pool)
		added := 0
		for _, e := range res.Evidence {
			if !before[evidenceKey(e)] {
				added++
			}
		}
		c.step(CorpusStep{Kind: "merge", Hop: hop, Evidence: len(res.Evidence)})

		cov, cu, cn, err := c.coverage(ctx, query, res.Evidence)
		if err != nil {
			return nil, fmt.Errorf("corpus: coverage (hop %d): %w", hop, err)
		}
		res.Usage.Add(cu)
		res.Requests += cn
		res.Coverage = cov
		res.Hops = append(res.Hops, CorpusHop{Hop: hop, Routed: len(routed), Read: chosen, NewEvidence: added, Coverage: cov})
		c.step(CorpusStep{Kind: "coverage", Hop: hop, Coverage: cov})
		if cov >= corpusOr(c.CoverageThreshold, defaultCorpusCoverage) || (added == 0 && hop > 1) {
			break
		}
	}

	for _, s := range scores {
		res.Documents = append(res.Documents, *s)
	}
	sort.SliceStable(res.Documents, func(i, j int) bool {
		if res.Documents[i].Score != res.Documents[j].Score {
			return res.Documents[i].Score > res.Documents[j].Score
		}
		return res.Documents[i].DocumentID < res.Documents[j].DocumentID
	})
	if len(res.Errors) == 0 {
		res.Errors = nil
	}
	return res, nil
}

// choose picks the documents to read this hop from the routing scores,
// best first: those at or above the threshold, at most MaxDocuments, and
// on the first hop at least MinDocuments.
func (c *CorpusNavigator) choose(routed []CorpusDocScore, hop int) []tree.DocumentID {
	th := corpusOr(c.RouteThreshold, defaultCorpusRouteThreshold)
	maxDocs := corpusOr(c.MaxDocuments, defaultCorpusMaxDocs)
	minDocs := 0
	if hop == 1 {
		minDocs = min(corpusOr(c.MinDocuments, defaultCorpusMinDocs), maxDocs)
	}
	var out []tree.DocumentID
	for i, r := range routed {
		if len(out) >= maxDocs {
			break
		}
		if r.Score >= th || i < minDocs {
			out = append(out, r.DocumentID)
		}
	}
	return out
}

// route scores every card against the question, and on a bridge hop
// against what was found so far. Cards are packed under the request
// budget and the batches sent together.
func (c *CorpusNavigator) route(ctx context.Context, query, found string, docs []*CorpusDoc) ([]CorpusDocScore, Usage, int, error) {
	var usage Usage
	out := make([]CorpusDocScore, len(docs))
	for i, d := range docs {
		out[i] = CorpusDocScore{DocumentID: d.ID, Title: d.Title}
	}
	cardChars := corpusOr(c.CardChars, defaultCorpusCardChars)
	budget := c.Navigator.reqTokens()
	type batch struct {
		state     map[string]any
		questions map[string]llmgate.Question
	}
	var batches []batch
	for start := 0; start < len(docs); {
		state := map[string]any{"question": query}
		if found != "" {
			state["found"] = found
		}
		questions := map[string]llmgate.Question{}
		used := countTokens(query) + countTokens(found) + 100
		end := start
		for end < len(docs) && end-start < navLeafBatch {
			card := docs[end].Card
			if len(card) > cardChars {
				card = card[:cardChars]
			}
			cost := countTokens(card) + 30
			if end > start && used+cost > budget {
				break
			}
			qk := fmt.Sprintf("d_%d", end)
			state[qk] = map[string]any{"title": docs[end].Title, "card": card}
			questions[qk] = routeQuestion(qk, found != "")
			used += cost
			end++
		}
		batches = append(batches, batch{state, questions})
		start = end
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		requests int
	)
	for _, b := range batches {
		wg.Add(1)
		go func(b batch) {
			defer wg.Done()
			jr, err := c.Judge.Judge(ctx, llmgate.JudgeRequest{State: b.state, Questions: b.questions})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				return
			}
			requests++
			usage.Add(judgeUsage(jr))
			for qk := range b.questions {
				p, err := jr.Noul(qk)
				if err != nil {
					continue
				}
				var i int
				_, _ = fmt.Sscanf(qk, "d_%d", &i)
				out[i].Score = p
			}
		}(b)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, usage, requests, firstErr
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].DocumentID < out[j].DocumentID
	})
	return out, usage, requests, nil
}

func routeQuestion(qk string, bridge bool) llmgate.Noul {
	if bridge {
		return llmgate.Noul{
			Instructions: fmt.Sprintf(
				"`question` is to be answered from a collection of documents. `found` is what has "+
					"already been retrieved from some of them. `%s` is another document in the "+
					"collection: its title and a card showing its contents and how it opens. Would "+
					"this document supply information the question still needs that `found` does not "+
					"already contain — the other entity, event, figure or source the question asks "+
					"about?", qk),
			Criteria: &llmgate.NoulCriteria{
				True:  "This document would add something the question needs and found lacks",
				False: "This document adds nothing the question still needs",
			},
		}
	}
	return llmgate.Noul{
		Instructions: fmt.Sprintf(
			"`question` is to be answered from a collection of documents; its answer may draw on "+
				"several of them. `%s` is one document in the collection: its title and a card "+
				"showing its contents and how it opens. Would this document hold any part of the "+
				"information the question needs — one of the entities, events, figures, sources or "+
				"claims it asks about?", qk),
		Criteria: &llmgate.NoulCriteria{
			True:  "This document holds at least part of what the question needs",
			False: "Nothing the question needs is in this document",
		},
	}
}

// read navigates the chosen documents in parallel and returns every page
// each judged, tagged with its document and hop. A document that fails is
// reported and skipped.
func (c *CorpusNavigator) read(ctx context.Context, query string, ids []tree.DocumentID, byID map[tree.DocumentID]*CorpusDoc, hop int) ([]CorpusEvidence, Usage, int, map[tree.DocumentID]error) {
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		usage    Usage
		requests int
		out      []CorpusEvidence
		errs     = map[tree.DocumentID]error{}
	)
	sem := make(chan struct{}, corpusOr(c.ReadConcurrency, defaultCorpusReadPar))
	results := make([][]CorpusEvidence, len(ids))
	for i, id := range ids {
		d := byID[id]
		if d == nil || d.Inputs == nil || len(d.Inputs.Leaves) == 0 {
			continue
		}
		wg.Add(1)
		go func(i int, d *CorpusDoc) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			nav := c.Navigator
			nav.Judge = c.Judge
			nav.Multipart = true
			nav.OnStep = nil
			nav.SkimAll = d.Inputs.Pages != nil
			r, err := nav.Navigate(ctx, query, d.Inputs.Leaves, d.Inputs.Load)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[d.ID] = err
				return
			}
			usage.Add(r.Usage)
			requests += r.Requests
			title := map[string]string{}
			for _, l := range d.Inputs.Leaves {
				title[l.ID] = l.Title
			}
			var ev []CorpusEvidence
			for _, p := range r.Evidence {
				ev = append(ev, CorpusEvidence{
					DocumentID: d.ID, DocumentTitle: d.Title, Page: p.Page.Number,
					SectionTitle: title[p.Page.LeafID], Text: p.Page.Text, Confidence: p.P, Hop: hop,
				})
			}
			results[i] = ev
		}(i, d)
	}
	wg.Wait()
	for _, ev := range results { // in route order, so equal scores stay deterministic
		out = append(out, ev...)
	}
	return out, usage, requests, errs
}

// merge builds the evidence list from every judged page: pages at or
// above the evidence threshold, best first, at most MaxPagesPerDocument
// from one document and MaxEvidence in all. Every document with a page
// over the threshold contributes its best page before any document
// contributes a second, so a strong document cannot crowd out the other
// half of a comparison. When no page clears the threshold the best
// pages are kept, flagged by their probability, as one document's
// navigation does.
func (c *CorpusNavigator) merge(pool []CorpusEvidence) []CorpusEvidence {
	th := c.EvidenceThreshold
	if th <= 0 {
		th = c.Navigator.threshold()
	}
	perDoc := corpusOr(c.MaxPagesPerDocument, defaultCorpusPagesPerDoc)
	limit := corpusOr(c.MaxEvidence, defaultCorpusMaxEvidence)

	// The same page judged twice (read on two hops) keeps its best score.
	best := map[string]CorpusEvidence{}
	var keys []string
	for _, e := range pool {
		k := evidenceKey(e)
		if old, ok := best[k]; !ok || e.Confidence > old.Confidence {
			if !ok {
				keys = append(keys, k)
			}
			best[k] = e
		}
	}
	all := make([]CorpusEvidence, 0, len(keys))
	for _, k := range keys {
		all = append(all, best[k])
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Confidence > all[j].Confidence })

	var above []CorpusEvidence
	for _, e := range all {
		if e.Confidence >= th {
			above = append(above, e)
		}
	}
	if len(above) == 0 {
		return all[:min(navMinEvidencePages, len(all))]
	}

	count := map[tree.DocumentID]int{}
	taken := map[string]bool{}
	var out []CorpusEvidence
	// First pass: each document's best page.
	for _, e := range above {
		if len(out) >= limit {
			break
		}
		if count[e.DocumentID] == 0 {
			count[e.DocumentID]++
			taken[evidenceKey(e)] = true
			out = append(out, e)
		}
	}
	// Second pass: fill by probability under the per-document cap.
	for _, e := range above {
		if len(out) >= limit {
			break
		}
		if taken[evidenceKey(e)] || count[e.DocumentID] >= perDoc {
			continue
		}
		count[e.DocumentID]++
		taken[evidenceKey(e)] = true
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Confidence > out[j].Confidence })
	return out
}

// coverage asks whether the merged evidence holds every fact the question
// needs.
func (c *CorpusNavigator) coverage(ctx context.Context, query string, ev []CorpusEvidence) (float64, Usage, int, error) {
	if len(ev) == 0 {
		return 0, Usage{}, 0, nil
	}
	state := map[string]any{"question": query, "found": c.foundState(ev)}
	q := map[string]llmgate.Question{
		"complete": llmgate.Noul{
			Instructions: "`question` is to be answered from a collection of documents. `found` lists " +
				"the passages retrieved for it, each with its document and page. Do these passages, " +
				"taken together, state every fact the question needs — each entity, event, figure, " +
				"date or source it asks about? Judge only what the passages state, not what could be " +
				"guessed. A question about something the passages never mention is not covered.",
			Criteria: &llmgate.NoulCriteria{
				True:  "Every fact the question needs is stated in the passages",
				False: "At least one fact the question needs is missing from the passages",
			},
		},
	}
	jr, err := c.Judge.Judge(ctx, llmgate.JudgeRequest{State: state, Questions: q})
	if err != nil {
		return 0, Usage{}, 0, err
	}
	p, err := jr.Noul("complete")
	if err != nil {
		return 0, judgeUsage(jr), 1, nil
	}
	return p, judgeUsage(jr), 1, nil
}

// foundState renders the merged evidence for a judgement's state, best
// first, each page trimmed, the whole bounded by FoundChars.
func (c *CorpusNavigator) foundState(ev []CorpusEvidence) string {
	limit := corpusOr(c.FoundChars, defaultCorpusFoundChars)
	var b strings.Builder
	for _, e := range ev {
		text := strings.Join(strings.Fields(e.Text), " ")
		if len(text) > corpusFoundPageChars {
			text = text[:corpusFoundPageChars]
		}
		title := e.DocumentTitle
		if title == "" {
			title = string(e.DocumentID)
		}
		entry := fmt.Sprintf("[%s, p.%d] %s\n", title, e.Page, text)
		if b.Len()+len(entry) > limit {
			break
		}
		b.WriteString(entry)
	}
	return b.String()
}

func evidenceKey(e CorpusEvidence) string { return fmt.Sprintf("%s#%d", e.DocumentID, e.Page) }

func evidenceKeys(ev []CorpusEvidence) map[string]bool {
	out := make(map[string]bool, len(ev))
	for _, e := range ev {
		out[evidenceKey(e)] = true
	}
	return out
}

func topScores(s []CorpusDocScore, k int) []CorpusDocScore {
	return append([]CorpusDocScore(nil), s[:min(k, len(s))]...)
}

// CorpusCard renders a document's routing card: its title, its top-level
// contents, and how it opens.
func CorpusCard(title string, in *NavInputs, opening string, chars int) string {
	if chars <= 0 {
		chars = defaultCorpusCardChars
	}
	var b strings.Builder
	b.WriteString(strings.TrimSpace(title))
	if in != nil && len(in.Leaves) > 1 {
		var tops []string
		seen := map[string]bool{}
		for _, l := range in.Leaves {
			top := l.Path
			if i := strings.Index(top, " > "); i >= 0 {
				top = top[:i]
			}
			top = strings.TrimSpace(top)
			if top == "" || seen[top] {
				continue
			}
			seen[top] = true
			tops = append(tops, top)
		}
		if len(tops) > 0 {
			contents := strings.Join(tops, "; ")
			if len(contents) > chars/2 {
				contents = contents[:chars/2]
			}
			b.WriteString("\nContents: ")
			b.WriteString(contents)
		}
	}
	if o := strings.Join(strings.Fields(opening), " "); o != "" {
		b.WriteString("\nOpens: ")
		b.WriteString(o)
	}
	s := b.String()
	if len(s) > chars {
		s = s[:chars]
	}
	return s
}
