package ingest

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/hallelx2/llmgate"
	"github.com/hallelx2/llmgate/judge/typesafe"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// toc_judge.go runs the TOC builder's two yes/no phases on a System One
// model instead of a chat model.
//
// Both phases ask the same question of many pages: "is this a table of
// contents" and "does this section start here". Neither needs text
// generated, and neither needs a paragraph of reasoning — they need a
// decision per page. A chat model can only answer one at a time, so
// detection is a sequential loop of up to 20 round-trips before tree
// building even starts, and verification is one call per leaf at a
// concurrency of 4.
//
// A Judge answers a whole batch against one reading of the state, so the
// same work becomes a couple of requests. Measured against the live API
// on 2026-09-17: 20 questions over shared state returned in 4.31s, and
// 20x the questions cost 2.82x the wall-clock of one.
//
// # Why this is batched by tokens and not by page count
//
// Jev reads the state once and runs every question against it, so the
// limits are 64k tokens for state plus all questions and 32k for state
// plus the longest single question. Twenty pages of a filing is roughly
// 40k tokens of state on its own — over the per-question limit. A fixed
// "20 pages per request" would therefore be rejected on exactly the
// documents this is meant to speed up. Batches are sized by measured
// tokens, and a single page too large to fit is skipped rather than
// allowed to fail the batch around it.
//
// # Degradation
//
// Every entry point here returns (result, handled bool). A false means
// "I did not answer this" — no Judge configured, a transport failure, a
// batch that could not be formed — and the caller runs its existing
// generative path unchanged. A Judge is an accelerator, never a
// dependency: a self-hoster without a TypeSafe key must see identical
// behaviour to today.

// judgeBudget caps the SHARED STATE of one request.
//
// The number that binds here is not the API's 64k total — it is the 32k
// limit on "state plus the longest single question". When many questions
// share one state, that state counts against every one of them, so the
// per-question limit becomes a ceiling on the state itself and the 64k
// total never comes into play.
//
// Getting this wrong is not a near-miss. A batch whose state is 36k
// tokens fails for EVERY question in it, not just a long one, and the
// whole phase falls back. That is exactly what happened on the first
// live run against a 20-page scan — the unit tests could not catch it,
// because a mock does not enforce the provider's limits.
//
// 24k leaves room for the questions themselves plus margin on an
// estimate that is approximate by construction: Jev's tokenizer is not
// published, so llmgate estimates with cl100k_base.
const judgeBudget = 24_000

// judgeQuestionBudget caps what one question may add on top of the
// shared state. Small, because these questions are one sentence; a
// question near this size means the state is being smuggled into it.
const judgeQuestionBudget = 4_000

// tocDetectInstructions is the detector question.
//
// It carries the exclusions the generative prompt spells out, because
// Jev reads instructions literally: it answers the question as written,
// not the one that was meant. "Is there a table of contents" would
// happily say yes to a list of figures.
const tocDetectInstructions = "Is this page a table of contents — a list of " +
	"the document's sections or chapters with their page numbers?"

func tocDetectCriteria() *llmgate.NoulCriteria {
	return &llmgate.NoulCriteria{
		True: "A list of section or chapter titles paired with page numbers",
		False: "Ordinary body text, an abstract, a summary, a notation list, " +
			"a list of figures, or a list of tables",
	}
}

// detectTOCPagesJudge answers "is this a table of contents" for every
// scanned page, in as few requests as the token budget allows.
//
// Returns handled=false when no Judge is configured or the request
// fails, so the caller falls back to its sequential path.
func (b *TOCBuilder) detectTOCPagesJudge(ctx context.Context, pages []PageText, limit int, usage *Usage) (found []int, handled bool) {
	found, handled, err := b.detectTOCPagesJudgeErr(ctx, pages, limit, usage)
	if err != nil {
		// Falling back silently would leave an operator paying for the
		// generative path forever with no sign their Judge is broken.
		log.Printf("toc: judge detection failed, falling back to the generative path: %v", err)
	}
	return found, handled
}

func (b *TOCBuilder) detectTOCPagesJudgeErr(ctx context.Context, pages []PageText, limit int, usage *Usage) (found []int, handled bool, err error) {
	if b.Judge == nil {
		return nil, false, nil
	}
	if limit > len(pages) {
		limit = len(pages)
	}

	candidates := make([]PageText, 0, limit)
	for i := 0; i < limit; i++ {
		if strings.TrimSpace(pages[i].Text) != "" {
			candidates = append(candidates, pages[i])
		}
	}
	if len(candidates) == 0 {
		return nil, true, nil // nothing to ask; a real answer, not a failure
	}

	if !b.MinimalContext {
		found, err = b.judgeTOCBatches(ctx, candidates, tocDetectorMaxChars, usage)
		if err != nil {
			return nil, false, err
		}
		return found, true, nil
	}

	// Minimal-context path: three cuts, each of which sends less.
	maxChars := b.DetectChars
	if maxChars <= 0 {
		maxChars = detectCharsMinimal
	}
	first := b.FirstPass
	if first <= 0 {
		first = firstPassDefault
	}
	if first > len(candidates) {
		first = len(candidates)
	}

	// Cut 1: the pre-filter. Pages with zero structural sign of a
	// contents page are not sent. Zero, not low — see prefilter.go.
	keep := func(ps []PageText) []PageText {
		out := ps[:0:0]
		for _, p := range ps {
			if prefilterTOC(p.Text).Any() {
				out = append(out, p)
			}
		}
		return out
	}

	// Cut 3: two stages. The first few pages, then the rest only on a
	// miss. Cut 2 (truncation) is applied inside judgeTOCBatches.
	stage1 := keep(candidates[:first])
	if len(stage1) > 0 {
		found, err = b.judgeTOCBatches(ctx, stage1, maxChars, usage)
		if err != nil {
			return nil, false, err
		}
		if len(found) > 0 {
			return found, true, nil
		}
	}
	stage2 := keep(candidates[first:])
	if len(stage2) == 0 {
		return nil, true, nil
	}
	found, err = b.judgeTOCBatches(ctx, stage2, maxChars, usage)
	if err != nil {
		return nil, false, err
	}
	return found, true, nil
}

// Defaults for the minimal-context cuts. Both are measured, not guessed:
// 2,000 chars covers the entries a contents page opens with (3M's runs
// to ~3,800 including a prose preamble, and the entries start well
// inside 2,000), and 6 pages covers page 2–3, where 20 of 21 filings put
// their TOC on 2026-09-18.
const (
	detectCharsMinimal = 2000
	firstPassDefault   = 6
)

// judgeTOCBatches asks the detection question of every page in pages,
// truncating each to maxChars, in as few requests as the token budget
// allows. It returns the page numbers judged to be a table of contents.
func (b *TOCBuilder) judgeTOCBatches(ctx context.Context, pages []PageText, maxChars int, usage *Usage) ([]int, error) {
	var found []int
	for _, batch := range batchByTokens(pages, maxChars) {
		state := map[string]any{}
		questions := map[string]llmgate.Question{}
		for _, p := range batch {
			key := pageKey(p.PageNumber)
			state[key] = truncate(p.Text, maxChars)
			questions[key] = llmgate.Noul{
				// The question names the field it is about. Question IDs
				// are not sent to the model, so without this the model
				// sees one state with many identical questions and has
				// no way to tell which page each refers to.
				Instructions: fmt.Sprintf("%s Answer only about the text in `%s`.",
					tocDetectInstructions, key),
				Criteria: tocDetectCriteria(),
			}
		}

		res, jerr := b.Judge.Judge(ctx, llmgate.JudgeRequest{
			State:     state,
			Questions: questions,
		})
		if jerr != nil {
			// Partial results would silently truncate the scanned range
			// and look like "no TOC here", so abandon the whole phase
			// and let the generative path redo it properly.
			return nil, jerr
		}
		addJudgeUsage(usage, res)

		for _, p := range batch {
			prob, err := res.Noul(pageKey(p.PageNumber))
			if err != nil {
				continue
			}
			if prob > b.judgeThreshold() {
				found = append(found, p.PageNumber)
			}
		}
	}
	sort.Ints(found)
	return found, nil
}

// verifyTitlesJudge answers "does this section start at the top of this
// page" for every leaf with a claimed start page.
//
// Same contract as the detector: handled=false means the caller runs its
// own concurrent generative path.
func (b *TOCBuilder) verifyTitlesJudge(ctx context.Context, nodes []tree.TOCNode, pages []PageText, usage *Usage) (verdicts map[string]bool, handled bool) {
	verdicts, handled, err := b.verifyTitlesJudgeErr(ctx, nodes, pages, usage)
	if err != nil {
		log.Printf("toc: judge verification failed, falling back to the generative path: %v", err)
	}
	return verdicts, handled
}

func (b *TOCBuilder) verifyTitlesJudgeErr(ctx context.Context, nodes []tree.TOCNode, pages []PageText, usage *Usage) (verdicts map[string]bool, handled bool, err error) {
	if b.Judge == nil {
		return nil, false, nil
	}

	byPage := map[int]string{}
	for _, p := range pages {
		byPage[p.PageNumber] = p.Text
	}

	claims := collectLeafClaims(nodes, byPage, judgeVerdictKey)
	if len(claims) == 0 {
		return map[string]bool{}, true, nil
	}

	verdicts = make(map[string]bool, len(claims))

	// Each question carries its own page text, so the per-question
	// budget is what binds here rather than a shared state.
	for start := 0; start < len(claims); {
		state := map[string]any{}
		questions := map[string]llmgate.Question{}
		used := 0

		for ; start < len(claims); start++ {
			c := claims[start]
			body := truncate(c.text, verifyMaxChars)
			cost := estimateTokens(body) + estimateTokens(c.title) + 64
			if used > 0 && used+cost > judgeBudget {
				break
			}
			if cost > judgeBudget {
				// One oversized page cannot be asked about at all.
				// Leave it unverified rather than failing its batch —
				// downstream treats an absent verdict as unknown.
				continue
			}
			state[c.key] = map[string]any{"title": c.title, "page_text": body}
			questions[c.key] = llmgate.Noul{
				Instructions: fmt.Sprintf(
					"Does the section titled `%s.title` begin at the very start of `%s.page_text`? "+
						"Match the title loosely — ignore differences in spacing, capitalisation and punctuation. "+
						"Answer no if other content comes before it.", c.key, c.key),
				Criteria: &llmgate.NoulCriteria{
					True:  "The section's title is the first meaningful content on the page",
					False: "Other content appears before the title, or the title is absent",
				},
			}
			used += cost
		}

		if len(questions) == 0 {
			continue
		}

		res, jerr := b.Judge.Judge(ctx, llmgate.JudgeRequest{
			State:     state,
			Questions: questions,
		})
		if jerr != nil {
			return nil, false, jerr
		}
		addJudgeUsage(usage, res)

		for key := range questions {
			if prob, err := res.Noul(key); err == nil {
				verdicts[key] = prob > b.judgeThreshold()
			}
		}
	}

	return verdicts, true, nil
}

// judgeThreshold is the probability above which a Noul counts as yes.
//
// 0.5 is the neutral reading and the right default, but it is exposed
// because it is the one number that must be recalibrated per corpus:
// Jev returns a calibrated probability where the old prompt returned a
// self-reported yes/no, and those are not the same quantity.
func (b *TOCBuilder) judgeThreshold() float64 {
	if b.JudgeThreshold > 0 {
		return b.JudgeThreshold
	}
	return 0.5
}

// batchByTokens groups pages so each batch fits the working budget.
//
// A page larger than the per-question budget on its own is dropped: it
// cannot be asked about in any batch, and carrying it would fail every
// request it joined.
func batchByTokens(pages []PageText, maxChars int) [][]PageText {
	var out [][]PageText
	var cur []PageText
	used := 0

	for _, p := range pages {
		cost := estimateTokens(truncate(p.Text, maxChars)) + 96 // + question overhead
		// A page that cannot fit a request even alone is skipped: it
		// would fail every batch it joined, taking the others with it.
		if cost > judgeBudget {
			continue
		}
		if len(cur) > 0 && used+cost > judgeBudget {
			out = append(out, cur)
			cur, used = nil, 0
		}
		cur = append(cur, p)
		used += cost
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// estimateTokens is llmgate's cl100k_base approximation, with a
// character-based fallback so a tokenizer failure degrades to a
// conservative guess rather than a zero that would overfill a batch.
func estimateTokens(s string) int {
	if n, err := typesafe.EstimateTokens(s); err == nil {
		return n
	}
	return len(s)/3 + 1
}

// pageKey names a page's field in the shared state. Zero-padded so the
// keys sort in page order when a human reads the payload.
func pageKey(n int) string { return fmt.Sprintf("page_%03d", n) }

// slugForKey reduces a title to something usable as a state field name.
func slugForKey(title string) string {
	var b strings.Builder
	for _, r := range title {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		case b.Len() > 0 && b.String()[b.Len()-1] != '_':
			b.WriteByte('_')
		}
		if b.Len() > 40 {
			break
		}
	}
	s := strings.Trim(b.String(), "_")
	if s == "" {
		return "untitled"
	}
	return s
}

// addJudgeUsage folds a judgment's cost into the builder's running total
// so a Judge-accelerated ingest still reports what it spent.
func addJudgeUsage(u *Usage, res *llmgate.Judgment) {
	if u == nil || res == nil {
		return
	}
	u.InputTokens += res.Usage.InputTokens
	u.OutputTokens += res.Usage.OutputTokens
	u.TotalTokens += res.Usage.TotalTokens
	u.CostUSD += res.Usage.CostUSD
	u.LLMCalls++
}

// collectLeafClaims walks the tree and gathers every leaf that claims a
// start page we have text for.
//
// Leaves only, matching the generative path: an internal node's start
// page is its first child's, so verifying it separately would ask the
// same question twice and pay for it twice.
//
// keyFor must produce a unique field name per claim. A document with two
// identically-titled sections on the same page would otherwise collapse
// into one question and one answer, silently leaving the second
// unverified.
func collectLeafClaims(
	nodes []tree.TOCNode,
	pageText map[int]string,
	keyFor func(title string, page int) string,
) []leafClaim {
	var out []leafClaim
	seen := map[string]int{}

	var walk func(ns []tree.TOCNode)
	walk = func(ns []tree.TOCNode) {
		for _, n := range ns {
			if len(n.Nodes) > 0 {
				walk(n.Nodes)
				continue
			}
			if n.StartPage <= 0 || strings.TrimSpace(n.Title) == "" {
				continue
			}
			text, ok := pageText[n.StartPage]
			if !ok || strings.TrimSpace(text) == "" {
				continue
			}

			key := keyFor(n.Title, n.StartPage)
			if seen[key] > 0 {
				key = fmt.Sprintf("%s_%d", key, seen[key])
			}
			seen[key]++

			out = append(out, leafClaim{key: key, title: n.Title, text: text})
		}
	}
	walk(nodes)
	return out
}

// leafClaim is one "does this section start here" question: the state
// field name, the title being looked for, and the page text to look in.
type leafClaim struct {
	key   string
	title string
	text  string
}

// judgeVerdictKey rebuilds the key verifyTitlesJudge used for a node, so
// the caller can look its verdict up.
func judgeVerdictKey(title string, page int) string {
	return fmt.Sprintf("s%d_%s", page, slugForKey(title))
}

// applyJudgeVerdicts clears the start page of every leaf the Judge did
// not confirm, matching what the generative verifier does with a "no".
//
// A missing verdict leaves the page alone. Absence means the question
// was never asked — an oversized page, or a title we had no text for —
// and treating "not asked" as "answered no" would silently discard
// correct page numbers.
func applyJudgeVerdicts(nodes []tree.TOCNode, verdicts map[string]bool) {
	if len(verdicts) == 0 {
		return
	}
	seen := map[string]int{}

	var walk func(ns []tree.TOCNode)
	walk = func(ns []tree.TOCNode) {
		for i := range ns {
			if len(ns[i].Nodes) > 0 {
				walk(ns[i].Nodes)
				continue
			}
			if ns[i].StartPage <= 0 || strings.TrimSpace(ns[i].Title) == "" {
				continue
			}
			// Rebuild the same key collectLeafClaims generated, including
			// its duplicate-disambiguation, by walking in the same order.
			key := judgeVerdictKey(ns[i].Title, ns[i].StartPage)
			if seen[key] > 0 {
				key = fmt.Sprintf("%s_%d", key, seen[key])
			}
			seen[key]++

			if ok, asked := verdicts[key]; asked && !ok {
				ns[i].StartPage = 0
			}
		}
	}
	walk(nodes)
}
