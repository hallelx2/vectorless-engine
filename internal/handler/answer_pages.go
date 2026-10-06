package handler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/pincite"
	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// defaultEvidencePages is how many of the navigator's evidence pages the
// answer is written from when the request does not say.
const defaultEvidencePages = 5

// evidencePageChars caps one page in the answer prompt. A 10-K page is
// 3–6K characters; a dense table page can be far more.
const evidencePageChars = 9000

// answerFromPages writes the answer from the navigator's evidence pages
// and binds every claim to the sentence on its page that states it.
//
// The order of work is the trust argument. The Judge chose the pages;
// one generative call writes prose with an [n] marker on each claim;
// the markers are validated against the evidence list (out-of-range
// ones stripped and counted); the Judge then picks, for each marked
// claim, the page's own sentence that states it; and the resolver
// places that sentence on the page from the PDF's word positions. No
// model writes a quote and no model places a highlight, so a highlight
// always lands on text that is really on the page.
func (h *AnswerHandler) answerFromPages(
	ctx context.Context,
	t *tree.Tree,
	src pincite.Source,
	body answerRequest,
	res *retrieval.Result,
	started time.Time,
	emit func(string, any),
) (map[string]any, []tree.SectionID, string, string, error) {
	total := retrieval.Usage{}
	total.Add(res.Usage)

	k := body.MaxSections
	if k <= 0 {
		k = h.answer.MaxSections
	}
	if k <= 0 {
		k = defaultEvidencePages
	}
	evidence := res.EvidencePages
	if len(evidence) > k {
		evidence = evidence[:k]
	}
	retrievalMS := time.Since(started).Milliseconds()

	evList := make([]map[string]any, 0, len(evidence))
	for i, ev := range evidence {
		evList = append(evList, map[string]any{"index": i + 1, "page": ev.Page, "title": ev.Title, "confidence": ev.Confidence})
	}
	emit("retrieved", map[string]any{
		"evidence":     evList,
		"retrieval_ms": retrievalMS,
		"requests":     res.HopsTaken,
		"usage":        usageMap(res.Usage),
	})

	model := h.synthModel(body.Model)
	answer, synthUsage, err := synthesiseFromPages(ctx, h.llm, model, body.Query, evidence, h.maxAnswerTokens(body, 2048))
	if err != nil {
		return nil, nil, "", "", fmt.Errorf("synthesis failed: %w", err)
	}
	total.Add(synthUsage)
	answerMS := time.Since(started).Milliseconds() - retrievalMS

	var layout *pincite.Layout
	if h.pincites != nil && src.IsPDF() {
		l, lerr := h.pincites.Layout(ctx, src)
		if lerr != nil && !errors.Is(lerr, pincite.ErrNoGeometry) {
			h.logger.Warn("answer: page layout unavailable; pincites stay page-level", "document_id", src.DocumentID, "err", lerr)
		}
		layout = l
	}

	marked, stripped := pincite.ParseMarkers(answer, len(evidence))
	inline := len(marked) > 0
	if !inline {
		// The model wrote no usable markers. Every evidence page still
		// gets a citation, its sentence chosen against the whole answer,
		// so nothing is silently uncited; the prose just carries no chips.
		for i := range evidence {
			marked = append(marked, pincite.Marked{Evidence: i + 1, Claim: answer, Ordinal: -1 - i})
		}
	}

	claims := make([]pincite.Claim, len(marked))
	for i, m := range marked {
		ev := evidence[m.Evidence-1]
		var cands []string
		if layout != nil {
			cands = pincite.Sentences(layout.Page(ev.Page))
		}
		if len(cands) == 0 {
			cands = textSentences(ev.Text)
		}
		claims[i] = pincite.Claim{Claim: m.Claim, Candidates: cands}
	}
	chooseStart := time.Now()
	choices, judgeUsage, jerr := pincite.Choose(ctx, h.judge, claims)
	if jerr != nil {
		h.logger.Warn("answer: judge could not choose pincite sentences; chose lexically", "err", jerr)
	}
	if judgeUsage.TotalTokens > 0 || judgeUsage.InputTokens > 0 {
		total.Add(retrieval.Usage{
			InputTokens:  judgeUsage.InputTokens,
			OutputTokens: judgeUsage.OutputTokens,
			TotalTokens:  judgeUsage.TotalTokens,
			CostUSD:      judgeUsage.CostUSD,
			LLMCalls:     1,
		})
	}
	chooseMS := time.Since(chooseStart).Milliseconds()

	// Build the pincites in answer order and the citation list, one per
	// evidence page any claim cited.
	var (
		pincites  []map[string]any
		ids       = map[[2]int]int{}
		citations []map[string]any
		citeByEv  = map[int]map[string]any{}
	)
	sections := flattenByPage(t)
	for i, m := range marked {
		ev := evidence[m.Evidence-1]
		ch := choices[i]
		quote := ""
		if ch.Index >= 0 {
			quote = claims[i].Candidates[ch.Index]
		}
		match := locate(layout, src, ev.Page, quote)
		id := len(pincites) + 1
		p := map[string]any{
			"id":          id,
			"citation":    m.Evidence,
			"page":        ev.Page,
			"quote":       quote,
			"regions":     nonNilRegions(match.Regions),
			"precision":   match.Precision,
			"claim":       m.Claim,
			"selected_by": publicSelector(ch.By),
		}
		if ch.By == "judge" {
			p["selection_confidence"] = ch.P
		}
		pincites = append(pincites, p)
		if inline {
			ids[[2]int{m.Ordinal, m.Evidence}] = id
		}

		c, ok := citeByEv[m.Evidence]
		if !ok {
			c = map[string]any{
				"index":       m.Evidence,
				"page":        ev.Page,
				"start_page":  ev.Page,
				"end_page":    ev.Page,
				"title":       ev.Title,
				"confidence":  ev.Confidence,
				"section_ids": sectionsOnPage(sections, ev.Page),
				"quote":       quote,
				"regions":     nonNilRegions(match.Regions),
				"precision":   match.Precision,
				"pincites":    []int{},
			}
			citeByEv[m.Evidence] = c
			citations = append(citations, c)
		} else if c["quote"] == "" && quote != "" {
			c["quote"], c["regions"], c["precision"] = quote, nonNilRegions(match.Regions), match.Precision
		}
		c["pincites"] = append(c["pincites"].([]int), id)
	}

	finalAnswer := answer
	if inline {
		finalAnswer = pincite.Renumber(answer, ids)
	}

	var finalIDs []tree.SectionID
	seen := map[tree.SectionID]bool{}
	for _, c := range citations {
		for _, id := range c["section_ids"].([]tree.SectionID) {
			if !seen[id] {
				seen[id] = true
				finalIDs = append(finalIDs, id)
			}
		}
	}
	traceToken := retrieval.ComputeTraceToken(body.DocumentID, "1", model, finalIDs)

	resp := map[string]any{
		"document_id":      body.DocumentID,
		"query":            body.Query,
		"answer":           finalAnswer,
		"citations":        citations,
		"pincites":         pincites,
		"evidence":         evList,
		"markers_inline":   inline,
		"markers_stripped": stripped,
		"strategy":         h.strategy.Name(),
		"model":            model,
		"confidence":       res.Confidence,
		"usage":            usageMap(total),
		"timings_ms": map[string]any{
			"retrieval": retrievalMS,
			"answer":    answerMS,
			"pincites":  chooseMS,
		},
		"elapsed_ms":  time.Since(started).Milliseconds(),
		"trace_token": traceToken,
	}
	return resp, finalIDs, model, traceToken, nil
}

// publicSelector names how a pincite's sentence was chosen, without
// exposing which model makes the choice: "model" or "overlap" (the
// lexical fallback).
func publicSelector(by string) string {
	switch by {
	case "judge":
		return "model"
	case "lexical":
		return "overlap"
	}
	return ""
}

// locate places a chosen sentence on its page. With no layout the
// citation is page-level for a PDF and section-level otherwise; with no
// sentence it is page-level.
func locate(layout *pincite.Layout, src pincite.Source, page int, quote string) pincite.Match {
	switch {
	case layout == nil && src.IsPDF():
		return pincite.Match{Page: page, Precision: pincite.PrecisionPage}
	case layout == nil:
		return pincite.Match{Page: page, Precision: pincite.PrecisionSection}
	case quote == "":
		return pincite.Match{Page: page, Precision: pincite.PrecisionPage}
	}
	return pincite.Resolve(layout, quote, []int{page})
}

// synthesiseFromPages is the one generative call: the answer, written
// from the evidence pages only, with an [n] marker on each claim.
func synthesiseFromPages(ctx context.Context, client llmgate.Client, model, query string, evidence []retrieval.EvidencePage, maxTokens int) (string, retrieval.Usage, error) {
	var b strings.Builder
	b.WriteString("Answer the question using ONLY the evidence pages below.\n\nQuestion:\n")
	b.WriteString(query)
	b.WriteString("\n\nEvidence (each block is one page of the document):\n")
	for i, ev := range evidence {
		text := ev.Text
		if len(text) > evidencePageChars {
			text = text[:evidencePageChars]
		}
		fmt.Fprintf(&b, "\n[%d] page %d", i+1, ev.Page)
		if ev.Title != "" {
			fmt.Fprintf(&b, " — %s", ev.Title)
		}
		fmt.Fprintf(&b, "\n%s\n", text)
	}
	b.WriteString("\nWrite a concise, direct answer in plain prose (no headings, no JSON). ")
	b.WriteString("End every sentence that states a fact from the evidence with the marker of the evidence block it came from, like [2]; ")
	b.WriteString("use only the numbers shown above, and use two markers like [1][3] only when a sentence draws on two blocks. ")
	b.WriteString("Give figures exactly as the page states them, with their units and periods. ")
	b.WriteString("If the evidence does not answer the question, say so plainly and do not guess.")

	resp, err := client.Complete(ctx, llmgate.Request{
		Model: model,
		Messages: []llmgate.Message{
			{Role: llmgate.RoleSystem, Content: "You answer questions about a document from its retrieved pages. Never state anything the pages do not show, and cite every claim."},
			{Role: llmgate.RoleUser, Content: b.String()},
		},
		MaxTokens:   maxTokens,
		Temperature: llmgate.Float64(0),
	})
	if err != nil {
		return "", retrieval.Usage{}, err
	}
	return strings.TrimSpace(resp.Content), retrieval.Usage{
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		TotalTokens:  resp.Usage.TotalTokens,
		CostUSD:      resp.Usage.CostUSD,
		LLMCalls:     1,
	}, nil
}

// textSentences is the candidate list when there is no layout: the
// page text's lines, each a candidate. It still lets the Judge choose a
// sentence for the quote; the citation is just not located.
func textSentences(text string) []string {
	var out []string
	for _, ln := range strings.Split(text, "\n") {
		ln = strings.TrimSpace(ln)
		if len(pincite.Tokenize(ln)) >= 3 {
			out = append(out, ln)
		}
	}
	return out
}

type pageSection struct {
	id         tree.SectionID
	start, end int
}

func flattenByPage(t *tree.Tree) []pageSection {
	var out []pageSection
	if t == nil || t.Root == nil {
		return out
	}
	var walk func(s *tree.Section)
	walk = func(s *tree.Section) {
		if s.PageStart > 0 {
			end := s.PageEnd
			if end < s.PageStart {
				end = s.PageStart
			}
			out = append(out, pageSection{id: s.ID, start: s.PageStart, end: end})
		}
		for _, c := range s.Children {
			walk(c)
		}
	}
	walk(t.Root)
	return out
}

// sectionsOnPage returns the deepest-first sections covering page: the
// narrowest section is the one a reader wants to open.
func sectionsOnPage(secs []pageSection, page int) []tree.SectionID {
	var hits []pageSection
	for _, s := range secs {
		if s.start <= page && page <= s.end {
			hits = append(hits, s)
		}
	}
	out := make([]tree.SectionID, 0, len(hits))
	for i := len(hits) - 1; i >= 0; i-- {
		out = append(out, hits[i].id)
	}
	return out
}
