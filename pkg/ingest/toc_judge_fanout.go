package ingest

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/hallelx2/llmgate"
)

// toc_judge_fanout.go asks every question the TOC phases need about a
// page in ONE request, including the ones that may turn out to be
// irrelevant.
//
// # Why this is not just batching again
//
// toc_judge.go already batches detection across pages. This goes
// further and batches across PHASES, which is a different saving.
//
// The pipeline naturally reads as a sequence: detect where the table of
// contents is, extract it, then verify each claimed start page. The
// middle step genuinely needs the first — you cannot verify a section
// you have not extracted. But the THIRD step's question, asked of a
// page, does not actually depend on the first: "does a section heading
// begin at the top of this page" is answerable from the page alone.
//
// So we ask it up front, for every page in the scanned prefix, at the
// same time as the detection question — and throw away the answers for
// pages that turn out not to matter. TypeSafe's docs are explicit that
// questions in one request are evaluated in parallel and that "adding
// more questions to a call typically doesn't add any latency". The
// speculative answers are therefore close to free, and they remove a
// whole round-trip from the critical path.
//
// # The cost model this exploits
//
// Measured against jev-1.13.0: a minimal one-question call has a floor
// of roughly 800ms, and twenty questions over shared state cost 2.82x
// the wall-clock of one — not 20x. Latency is dominated by the number of
// REQUESTS, not the number of questions. Every question moved into an
// existing request is nearly free; every new request costs the floor.
//
// That is the whole optimisation, and it is why speculation pays here
// when it would not against a chat model, where a discarded answer is a
// discarded call.

// pageJudgement is everything asked about one page in the fan-out.
type pageJudgement struct {
	IsTOC         float64 // probability this page is a table of contents
	SectionStarts float64 // probability a section heading opens this page
	Asked         bool
}

// sectionStartInstructions is the speculative question.
//
// Phrased about the page rather than about a named title, because at
// fan-out time we do not yet know which titles exist — extraction has
// not run. That makes it weaker evidence than the targeted verification
// question, so it is used as a prior to skip work, never to overrule a
// real verification.
const sectionStartInstructions = "Does a numbered or titled section heading " +
	"begin at the very start of this page, before any body text?"

// judgePagesFanout answers both page-level questions in one pass.
//
// Returns handled=false on any failure, leaving the caller to run the
// ordinary sequential path — the same contract as every other Judge
// entry point here.
func (b *TOCBuilder) judgePagesFanout(ctx context.Context, pages []PageText, limit int, usage *Usage) (map[int]pageJudgement, bool) {
	out, handled, err := b.judgePagesFanoutErr(ctx, pages, limit, usage)
	if err != nil {
		log.Printf("toc: judge fan-out failed, falling back: %v", err)
	}
	return out, handled
}

func (b *TOCBuilder) judgePagesFanoutErr(ctx context.Context, pages []PageText, limit int, usage *Usage) (map[int]pageJudgement, bool, error) {
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
		return map[int]pageJudgement{}, true, nil
	}

	out := make(map[int]pageJudgement, len(candidates))

	for _, batch := range batchByTokens(candidates, tocDetectorMaxChars) {
		state := map[string]any{}
		questions := map[string]llmgate.Question{}

		for _, p := range batch {
			key := pageKey(p.PageNumber)
			state[key] = truncate(p.Text, tocDetectorMaxChars)

			questions["toc_"+key] = llmgate.Noul{
				Instructions: fmt.Sprintf("%s Answer only about the text in `%s`.",
					tocDetectInstructions, key),
				Criteria: tocDetectCriteria(),
			}
			// The speculative one. Most pages are not section openings and
			// most of these answers go unused — which is the point: they
			// ride along in a request that was being sent anyway.
			questions["sec_"+key] = llmgate.Noul{
				Instructions: fmt.Sprintf("%s Answer only about the text in `%s`.",
					sectionStartInstructions, key),
				Criteria: &llmgate.NoulCriteria{
					True:  "A section or item heading is the first meaningful content",
					False: "The page opens with body text, a table, or a continuation",
				},
			}
		}

		res, err := b.Judge.Judge(ctx, llmgate.JudgeRequest{State: state, Questions: questions})
		if err != nil {
			return nil, false, err
		}
		addJudgeUsage(usage, res)

		for _, p := range batch {
			key := pageKey(p.PageNumber)
			j := pageJudgement{Asked: true}
			if v, err := res.Noul("toc_" + key); err == nil {
				j.IsTOC = v
			}
			if v, err := res.Noul("sec_" + key); err == nil {
				j.SectionStarts = v
			}
			out[p.PageNumber] = j
		}
	}

	return out, true, nil
}

// tocPagesFrom reads the detection answer out of a fan-out result.
func tocPagesFrom(j map[int]pageJudgement, threshold float64) []int {
	var found []int
	for page, pj := range j {
		if pj.Asked && pj.IsTOC > threshold {
			found = append(found, page)
		}
	}
	sort.Ints(found)
	return found
}
