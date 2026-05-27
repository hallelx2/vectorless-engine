package retrieval

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// ScoredSection is one entry in the re-ranker's output: a section ID,
// the model's relevance score (0-100), and an optional short reason.
//
// Scores are returned as float64 so the caller can sort stably and so
// future re-rankers (e.g. combining the model score with a cheap
// lexical prior) don't have to re-encode the field. The 0-100 scale is
// what the prompt asks the model to use; rerank.go preserves whatever
// the model returns (clamped to non-negative) rather than rescaling.
type ScoredSection struct {
	ID     tree.SectionID `json:"id"`
	Score  float64        `json:"score"`
	Reason string         `json:"reason,omitempty"`
}

// SectionContent is one candidate passed to the re-ranker. The caller is
// responsible for loading Content from storage; the re-ranker does not
// touch the storage layer itself.
//
// Title is included separately from Content so the prompt can present
// both even when the section's body is empty (e.g. a structural-only
// section whose children carry the real text). Models tend to ignore a
// section whose body is blank if the title isn't surfaced explicitly.
type SectionContent struct {
	ID      tree.SectionID
	Title   string
	Content string
}

// ReRanker re-orders a strategy's candidate sections by reading the first
// chunk of each section's content and asking the LLM which sections
// actually answer the query. This rescues the case where the retrieval
// strategy reasoned over titles + summaries + HyDE candidate questions
// alone and got fooled by surface-level signals.
//
// One LLM call per query, regardless of candidate count. Cost is bounded
// by MaxContentChars per section × candidate count, plus a small
// prompt overhead.
//
// Re-rank is intentionally tolerant about model output: bad JSON,
// unknown IDs, and missing IDs are all handled gracefully so a single
// model blip never drops a candidate. See ReRank for the exact contract.
type ReRanker struct {
	// LLM is the client used for the re-rank call.
	LLM llmgate.Client

	// Model is the model name passed to the LLM. Callers should point
	// this at a small/fast model — the re-rank call is short and
	// running it on the flagship model would defeat the cost story.
	Model string

	// MaxContentChars caps how many characters of each section's content
	// are sent to the model. Default: defaultReRankMaxContentChars.
	// Set higher when sections are long and the query needs more
	// context to decide; set lower to tighten the budget.
	MaxContentChars int

	// MaxRetries bounds JSON-parse retries on a single re-rank call.
	// Zero defaults to defaultReRankRetries.
	MaxRetries int
}

// defaultReRankMaxContentChars is the per-section content budget. ~2000
// chars × 5 candidates ≈ 10k chars ≈ 2.5k tokens — comfortable inside
// any modern model's context window and cheap on gemini-2.5-flash.
const defaultReRankMaxContentChars = 2000

// defaultReRankRetries mirrors the planning + selection retry counts.
// Models occasionally drop out of JSON mode; one retry usually recovers.
const defaultReRankRetries = 2

// NewReRanker constructs a ReRanker with sensible defaults. Callers can
// override MaxContentChars/MaxRetries on the returned struct.
func NewReRanker(client llmgate.Client, model string) *ReRanker {
	return &ReRanker{
		LLM:             client,
		Model:           model,
		MaxContentChars: defaultReRankMaxContentChars,
		MaxRetries:      defaultReRankRetries,
	}
}

// ReRank scores and reorders candidates by relevance to query. Returns
// a slice the same length as candidates (in descending score order),
// the cumulative LLM Usage, and an optional error.
//
// Failure semantics — the whole point of this method is to be safer
// than a hard re-rank that can drop sections on model flakes:
//
//   - Empty candidates → returns (nil, zero Usage, nil) without any
//     LLM call. Callers can pass an empty list unconditionally.
//   - LLM transport failure → returns the input order (each ID with
//     score=0) plus a non-nil error. The caller logs and keeps moving.
//   - All retry attempts return un-parseable JSON → returns the input
//     order with score=0 and a nil error. This mirrors how
//     runSelectionWithRetry degrades: a single JSON glitch must not
//     500 the request.
//   - Response references unknown IDs → those entries are dropped;
//     only IDs present in candidates surface.
//   - Response is missing some input IDs → those IDs get score=0 and
//     appear at the bottom of the output in their original relative
//     order.
//
// In all cases, every input ID appears in the output exactly once.
// This is the load-bearing invariant: re-rank can reorder, but it
// never drops candidates.
func (r *ReRanker) ReRank(ctx context.Context, query string, candidates []SectionContent) ([]ScoredSection, Usage, error) {
	if r == nil || r.LLM == nil {
		// A nil re-ranker is treated as a no-op (returns input order)
		// so production wiring can pass nil when re-rank is disabled.
		return inputOrderScored(candidates), Usage{}, nil
	}
	if len(candidates) == 0 {
		return nil, Usage{}, nil
	}

	maxChars := r.MaxContentChars
	if maxChars <= 0 {
		maxChars = defaultReRankMaxContentChars
	}
	maxRetries := r.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}
	if maxRetries == 0 {
		maxRetries = defaultReRankRetries
	}

	prompt := buildReRankPrompt(query, candidates, maxChars)
	baseReq := llmgate.Request{
		Model: r.Model,
		Messages: []llmgate.Message{
			{Role: llmgate.RoleSystem, Content: reRankSystemPrompt},
			{Role: llmgate.RoleUser, Content: prompt},
		},
		MaxTokens:   1024,
		Temperature: 0,
		JSONMode:    true,
		JSONSchema:  []byte(reRankJSONSchema),
	}

	scored, usage, err := runReRankWithRetry(ctx, r.LLM, baseReq, maxRetries)
	if err != nil {
		// Transport failure: preserve input order, surface the error
		// so the caller can decide whether to log loud or quiet.
		return inputOrderScored(candidates), usage, fmt.Errorf("rerank llm call: %w", err)
	}
	if scored == nil {
		// All retries failed to parse. Degrade gracefully — same shape
		// as a nil-rerank result so the response stays consistent.
		return inputOrderScored(candidates), usage, nil
	}

	return mergeScored(candidates, scored), usage, nil
}

// reRankSystemPrompt frames the task. The 0-100 scale was picked over
// 0-1 because models are noticeably better at returning a coarse
// integer score than a fine-grained float, and the downstream code
// only needs ordering. "The answer is in this section" is deliberately
// phrased as "directly answers OR provides the load-bearing evidence
// for" — the goal is to surface sections that close out the query, not
// sections that merely mention the topic.
const reRankSystemPrompt = `You are a precise relevance scorer. Given a user query and a list of candidate document sections (each shown with its ID, title, and the first portion of its content), score how well each section actually answers the query.

Rules:
- Score each section on a 0-100 integer scale where:
  - 90-100: the section directly answers the query OR provides the load-bearing evidence the answer relies on.
  - 60-89: the section is highly relevant — it discusses the topic the query is about and likely contributes to the answer.
  - 30-59: the section is tangentially related — it mentions the topic but probably does not answer the query.
  - 0-29: the section is not useful for this query. Generic mentions, off-topic content, or content that only matches on a shared keyword without real relevance.
- Score every section in the input list. Do not skip any.
- Use the section IDs exactly as provided. Do not invent IDs.
- For each score include a one-line "reason" (≤120 chars) explaining the score. Keep it concrete — quote a phrase from the section when possible.

Return only the JSON object described in the schema. No prose, no markdown.`

const reRankJSONSchema = `{
  "type": "object",
  "properties": {
    "scores": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "id": {"type": "string"},
          "score": {"type": "number"},
          "reason": {"type": "string"}
        },
        "required": ["id", "score"]
      }
    }
  },
  "required": ["scores"]
}`

// buildReRankPrompt renders the user message. Sections are presented as
// "[id] Title\nContent excerpt" blocks, separated by blank lines so the
// model sees clear boundaries. Content is truncated at maxChars with an
// ellipsis when cut so the model can tell a section was long without
// having to count.
func buildReRankPrompt(query string, candidates []SectionContent, maxChars int) string {
	var b strings.Builder
	b.WriteString("User query:\n")
	b.WriteString(query)
	b.WriteString("\n\nCandidate sections:\n")

	for i, c := range candidates {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "[%s] %s\n", string(c.ID), c.Title)
		excerpt := strings.TrimSpace(c.Content)
		if excerpt == "" {
			b.WriteString("(section body is empty)\n")
			continue
		}
		if len(excerpt) > maxChars {
			excerpt = excerpt[:maxChars] + "…"
		}
		b.WriteString(excerpt)
		b.WriteByte('\n')
	}

	b.WriteString("\nReturn a JSON object with a `scores` array. Each entry has `id` (string, exactly as shown above), `score` (number, 0-100), and `reason` (string, ≤120 chars). Score every section in the candidate list.")
	return b.String()
}

// reRankPayload is the expected JSON shape.
type reRankPayload struct {
	Scores []reRankItem `json:"scores"`
}

type reRankItem struct {
	ID     string  `json:"id"`
	Score  float64 `json:"score"`
	Reason string  `json:"reason"`
}

// ParseReRank extracts a re-rank result from an LLM JSON response.
// Tolerates code-fence wrappers and leading/trailing prose, like
// ParseSelection and ParsePlan. Returns the parsed []ScoredSection
// (un-merged with the input candidate list — that's mergeScored's job)
// and any parse error.
func ParseReRank(raw string) ([]ScoredSection, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty rerank response")
	}
	if strings.HasPrefix(raw, "```") {
		if i := strings.Index(raw, "\n"); i >= 0 {
			raw = raw[i+1:]
		}
		raw = strings.TrimSuffix(raw, "```")
		raw = strings.TrimSpace(raw)
	}
	if i := strings.Index(raw, "{"); i > 0 {
		raw = raw[i:]
	}
	if j := strings.LastIndex(raw, "}"); j >= 0 && j < len(raw)-1 {
		raw = raw[:j+1]
	}

	var p reRankPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, fmt.Errorf("unmarshal rerank: %w", err)
	}
	out := make([]ScoredSection, 0, len(p.Scores))
	for _, it := range p.Scores {
		id := strings.TrimSpace(it.ID)
		if id == "" {
			continue
		}
		score := it.Score
		if score < 0 {
			score = 0
		}
		out = append(out, ScoredSection{
			ID:     tree.SectionID(id),
			Score:  score,
			Reason: strings.TrimSpace(it.Reason),
		})
	}
	return out, nil
}

// runReRankWithRetry issues the re-rank LLM call, retrying up to
// maxRetries additional times when the response does not parse. Mirrors
// runSelectionWithRetry / runPlanningWithRetry. Returns (nil, usage, nil)
// when retries are exhausted so the caller falls back to input order.
func runReRankWithRetry(ctx context.Context, client llmgate.Client, baseReq llmgate.Request, maxRetries int) ([]ScoredSection, Usage, error) {
	var totalUsage Usage
	var lastParseErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		req := baseReq
		if attempt > 0 {
			msgs := make([]llmgate.Message, len(baseReq.Messages))
			copy(msgs, baseReq.Messages)
			tail := len(msgs) - 1
			msgs[tail] = llmgate.Message{
				Role:    msgs[tail].Role,
				Content: msgs[tail].Content + "\n\nIMPORTANT: respond with ONLY a JSON object matching the schema. Do not include prose, explanation, or markdown fences.",
			}
			req.Messages = msgs
		}
		resp, err := client.Complete(ctx, req)
		if err != nil {
			return nil, totalUsage, err
		}
		totalUsage.Add(Usage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
			TotalTokens:  resp.Usage.TotalTokens,
			CostUSD:      resp.Usage.CostUSD,
			LLMCalls:     1,
		})
		scored, parseErr := ParseReRank(resp.Content)
		if parseErr == nil {
			return scored, totalUsage, nil
		}
		lastParseErr = parseErr
	}
	log.Printf("retrieval: rerank parse failed after %d attempts (%v); preserving input order", maxRetries+1, lastParseErr)
	return nil, totalUsage, nil
}

// inputOrderScored returns the candidates as ScoredSection entries with
// score=0, preserving the input order. Used as the safe fallback when
// the re-ranker can't produce a real score for any reason.
func inputOrderScored(candidates []SectionContent) []ScoredSection {
	if len(candidates) == 0 {
		return nil
	}
	out := make([]ScoredSection, len(candidates))
	for i, c := range candidates {
		out[i] = ScoredSection{ID: c.ID, Score: 0}
	}
	return out
}

// mergeScored combines the input candidates with the model's scored
// output. Every input ID appears in the result exactly once:
//
//   - Known IDs (present in both input + model output) carry the
//     model's score and reason, sorted descending by score.
//   - Unknown IDs from the model output are dropped — the model
//     hallucinated.
//   - Input IDs missing from the model output get score=0 and appear
//     at the bottom in input order (so the response is still useful
//     to the caller).
//
// Ties on score preserve the original input order. This makes the
// output deterministic when the model returns equal scores and keeps
// reasonable behaviour when the model returns uniform scores (e.g.
// "everything is a 50") — the strategy's original order wins.
func mergeScored(candidates []SectionContent, scored []ScoredSection) []ScoredSection {
	if len(candidates) == 0 {
		return nil
	}
	// Position of each input ID, for stable ordering on ties.
	pos := make(map[tree.SectionID]int, len(candidates))
	for i, c := range candidates {
		pos[c.ID] = i
	}

	// Index scored entries by ID. If the model returned the same ID
	// twice, the first wins — defensive against duplicate entries.
	byID := make(map[tree.SectionID]ScoredSection, len(scored))
	for _, s := range scored {
		if _, known := pos[s.ID]; !known {
			continue // hallucinated ID
		}
		if _, seen := byID[s.ID]; seen {
			continue
		}
		byID[s.ID] = s
	}

	out := make([]ScoredSection, 0, len(candidates))
	missing := make([]ScoredSection, 0)
	for _, c := range candidates {
		if s, ok := byID[c.ID]; ok {
			out = append(out, s)
		} else {
			missing = append(missing, ScoredSection{ID: c.ID, Score: 0})
		}
	}

	// Stable descending sort by score with original-order tiebreak.
	// Hand-rolled because we want strict stability across equal
	// scores and Go's sort.Slice is not stable.
	insertionSortByScore(out, pos)

	// Append missing IDs (all score=0) at the bottom, in input order.
	out = append(out, missing...)
	return out
}

// insertionSortByScore sorts ss descending by Score, with ties broken
// by the input position recorded in pos (lower pos → earlier). O(n²)
// is fine here: re-rank candidates are typically ≤20.
func insertionSortByScore(ss []ScoredSection, pos map[tree.SectionID]int) {
	for i := 1; i < len(ss); i++ {
		cur := ss[i]
		curPos := pos[cur.ID]
		j := i - 1
		for j >= 0 {
			cmp := ss[j]
			cmpPos := pos[cmp.ID]
			if cmp.Score > cur.Score || (cmp.Score == cur.Score && cmpPos <= curPos) {
				break
			}
			ss[j+1] = ss[j]
			j--
		}
		ss[j+1] = cur
	}
}
