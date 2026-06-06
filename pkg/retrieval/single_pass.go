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

// SinglePass is the simplest Strategy: feed the entire tree view to the model
// in one call and ask it to pick relevant section IDs.
//
// Use when the tree fits comfortably in the model's context window. For
// larger documents, ChunkedTree is the right choice.
type SinglePass struct {
	LLM llmgate.Client
}

// NewSinglePass constructs a SinglePass strategy backed by client.
func NewSinglePass(client llmgate.Client) *SinglePass {
	return &SinglePass{LLM: client}
}

// Name implements Strategy.
func (s *SinglePass) Name() string { return "single-pass" }

// Select implements Strategy.
func (s *SinglePass) Select(ctx context.Context, t *tree.Tree, query string, budget ContextBudget) ([]tree.SectionID, error) {
	r, err := s.SelectWithCost(ctx, t, query, budget)
	if err != nil {
		return nil, err
	}
	return r.SelectedIDs, nil
}

// SelectWithCost implements CostStrategy.
func (s *SinglePass) SelectWithCost(ctx context.Context, t *tree.Tree, query string, budget ContextBudget) (*Result, error) {
	if t == nil || t.Root == nil {
		return &Result{}, nil
	}
	view := t.BuildView()

	model := budget.ModelName
	prompt := BuildSelectionPrompt("Document: "+view.Title, view.Sections, nil, query)

	req := llmgate.Request{
		Model: model,
		Messages: []llmgate.Message{
			{Role: llmgate.RoleSystem, Content: selectionSystemPrompt},
			{Role: llmgate.RoleUser, Content: prompt},
		},
		MaxTokens:   2048,
		Temperature: 0,
		JSONMode:    true,
		JSONSchema:  []byte(selectionJSONSchema),
	}

	ids, confidences, usage, err := runSelectionWithRetry(ctx, s.LLM, req, defaultSelectionRetries)
	if err != nil {
		return nil, fmt.Errorf("single-pass llm call: %w", err)
	}

	selected := FilterKnownIDs(ids, view.Sections)
	filteredConfidences := filterConfidences(confidences, selected)
	return &Result{
		SelectedIDs: selected,
		Confidences: filteredConfidences,
		ModelUsed:   model,
		Usage:       usage,
		HopsTaken:   1,
		TraceToken:  ComputeTraceToken(t.DocumentID, traceDocVersionV1, model, selected),
	}, nil
}

// filterConfidences keeps only entries whose key appears in keep, so a
// strategy never surfaces a confidence for an ID it didn't ultimately
// select (post-filter / post-merge). Returns nil when src is nil or
// empty after filtering — preserving the "no confidence signal"
// distinction the API layer relies on for abstention.
func filterConfidences(src map[tree.SectionID]float64, keep []tree.SectionID) map[tree.SectionID]float64 {
	if len(src) == 0 {
		return nil
	}
	out := make(map[tree.SectionID]float64, len(keep))
	for _, id := range keep {
		if v, ok := src[id]; ok {
			out[id] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// traceDocVersionV1 is the placeholder document version used by every
// strategy until Phase 3.2 wires real per-document versioning. Defined
// once so the bump is a one-line change.
const traceDocVersionV1 = "1"

// defaultSelectionRetries is the number of EXTRA attempts (on top of the first)
// the selection LLM call gets when its response fails to parse as JSON. Gemini's
// JSON mode occasionally returns plain text ("The most relevant section is...");
// without retry, that surfaces as a 500 to the SDK on every such glitch.
const defaultSelectionRetries = 2

// runSelectionWithRetry runs a selection LLM call and parses the response,
// retrying up to maxRetries additional times if the model returns something
// that doesn't parse as JSON. Returns the parsed IDs, per-ID confidences
// (nil when the model returned the legacy shape without confidence), and
// the cumulative usage across all attempts. An error is returned only on a
// transport/LLM failure — final parse failure degrades gracefully to an
// empty selection (logged) so a single LLM-formatting blip doesn't 500
// the entire query.
func runSelectionWithRetry(ctx context.Context, client llmgate.Client, baseReq llmgate.Request, maxRetries int) ([]tree.SectionID, map[tree.SectionID]float64, Usage, error) {
	if maxRetries < 0 {
		maxRetries = 0
	}
	var totalUsage Usage
	var lastParseErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		req := baseReq
		if attempt > 0 {
			// Strengthen the last user message on retry; some models (notably
			// Gemini) sometimes ignore JSON mode on the first try.
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
			return nil, nil, totalUsage, err
		}
		totalUsage.Add(Usage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
			TotalTokens:  resp.Usage.TotalTokens,
			CostUSD:      resp.Usage.CostUSD,
			LLMCalls:     1,
		})
		ids, confidences, parseErr := ParseSelection(resp.Content)
		if parseErr == nil {
			return ids, confidences, totalUsage, nil
		}
		lastParseErr = parseErr
	}
	log.Printf("retrieval: selection parse failed after %d attempts (%v); returning empty selection", maxRetries+1, lastParseErr)
	return nil, nil, totalUsage, nil
}

// --- shared prompt scaffolding ---

const selectionSystemPrompt = `You are a precise retrieval engine. Given a hierarchical outline of a document (titles + short summaries + stable section IDs) and a user query, pick the section IDs whose full content is most likely to answer the query.

Rules:
- Prefer leaf sections. Include a parent only if the parent's own body is directly relevant.
- Include as few sections as possible. Quality over quantity.
- Only return IDs present in the provided outline. Do not invent IDs.
- If nothing is relevant, return an empty list.
- Attach a confidence score in [0.0, 1.0] to every pick reflecting how
  likely that section's body answers the query. Use the full range —
  do NOT score every pick at 1.0. 0.0 means "no signal", 1.0 means
  "near-certain". If you cannot reason about confidence at all, omit
  the picks array and return the legacy selected_section_ids form
  instead; the engine accepts both shapes.`

// selectionJSONSchema is intentionally permissive: it accepts EITHER the
// legacy { selected_section_ids: [...] } shape OR the new
// { picks: [{id, confidence}] } shape so older / weaker models that
// can't reason about confidence still work. ParseSelection accepts
// both and returns confidences when present.
const selectionJSONSchema = `{
  "type": "object",
  "properties": {
    "picks": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "id": {"type": "string"},
          "confidence": {"type": "number", "minimum": 0, "maximum": 1}
        },
        "required": ["id"]
      }
    },
    "selected_section_ids": {"type": "array", "items": {"type": "string"}},
    "reasoning": {"type": "string"}
  }
}`

// BuildSelectionPrompt renders the user-side prompt for a selection call.
// breadcrumb orients the model in the document; sections is the outline
// portion the model can pick from; siblings (optional) gives peripheral
// context about adjacent subtrees not included in full.
func BuildSelectionPrompt(breadcrumb string, sections []tree.SectionView, siblings []tree.SectionView, query string) string {
	var b strings.Builder
	if breadcrumb != "" {
		b.WriteString(breadcrumb)
		b.WriteString("\n\n")
	}
	b.WriteString("Outline (sections you may select from):\n")
	for _, sv := range sections {
		writeSectionLine(&b, sv)
	}
	if len(siblings) > 0 {
		b.WriteString("\nPeripheral siblings (context only — NOT selectable):\n")
		for _, sv := range siblings {
			writeSectionLine(&b, sv)
		}
	}
	b.WriteString("\nUser query:\n")
	b.WriteString(query)
	b.WriteString("\n\nReturn a JSON object. Preferred shape:\n")
	b.WriteString(`  {"picks": [{"id": "sec_x", "confidence": 0.82}, ...], "reasoning": "..."}` + "\n")
	b.WriteString("confidence is a float in [0.0, 1.0] reflecting how likely the section's body answers the query. Use the full range; do not score every pick at 1.0.\n")
	b.WriteString("Fallback shape (use ONLY if you cannot reason about confidence):\n")
	b.WriteString(`  {"selected_section_ids": ["sec_x", ...], "reasoning": "..."}`)
	return b.String()
}

func writeSectionLine(b *strings.Builder, sv tree.SectionView) {
	for i := 0; i < sv.Depth; i++ {
		b.WriteString("  ")
	}
	b.WriteString("- [")
	b.WriteString(string(sv.ID))
	b.WriteString("] ")
	b.WriteString(sv.Title)
	if sv.Summary != "" {
		b.WriteString(" — ")
		b.WriteString(sv.Summary)
	}
	// Phase 2.5: append entities + numbers from the structured axes
	// when present. They live on the SAME outline line as the summary,
	// truncated to the first 3 each, so the retrieval prompt sees the
	// section's proper-noun + numeric surface without doubling the
	// per-section budget. Older sections (axes==nil) skip this block,
	// so un-backfilled docs see the prompt unchanged.
	if sv.SummaryAxes != nil {
		if ents := firstN(sv.SummaryAxes.Entities, 3); len(ents) > 0 {
			b.WriteString(" — entities: ")
			b.WriteString(strings.Join(ents, ", "))
		}
		if nums := firstN(sv.SummaryAxes.Numbers, 3); len(nums) > 0 {
			b.WriteString(" — numbers: ")
			b.WriteString(strings.Join(nums, ", "))
		}
	}
	b.WriteByte('\n')
	// HyDE: surface the first candidate question (truncated) as an
	// "answers:" hint. Keeps the prompt budget impact small (~120 chars
	// per section) while widening the lexical/semantic overlap the
	// retrieval model sees vs. an unfamiliarly-worded user query.
	if q := firstCandidateQuestion(sv.CandidateQuestions); q != "" {
		for i := 0; i < sv.Depth; i++ {
			b.WriteString("  ")
		}
		b.WriteString("    answers: ")
		b.WriteString(q)
		b.WriteByte('\n')
	}
}

// firstN returns up to n non-empty trimmed entries from xs in order.
// Used by writeSectionLine to cap entities / numbers per section so a
// section whose axes carry 30 entities doesn't blow up the retrieval
// prompt's token budget.
func firstN(xs []string, n int) []string {
	if n <= 0 || len(xs) == 0 {
		return nil
	}
	out := make([]string, 0, n)
	for _, x := range xs {
		x = strings.TrimSpace(x)
		if x == "" {
			continue
		}
		out = append(out, x)
		if len(out) >= n {
			break
		}
	}
	return out
}

// firstCandidateQuestion returns the first non-empty candidate question,
// truncated to ~120 chars so the outline doesn't blow up. Returns ""
// when no usable question is present.
func firstCandidateQuestion(qs []string) string {
	const max = 120
	for _, q := range qs {
		q = strings.TrimSpace(q)
		if q == "" {
			continue
		}
		if len(q) > max {
			// Cut at a word boundary if one is near the cap; otherwise
			// hard-cut so we always respect the budget.
			if cut := strings.LastIndex(q[:max], " "); cut > max-20 {
				q = q[:cut] + "…"
			} else {
				q = q[:max] + "…"
			}
		}
		return q
	}
	return ""
}

// selectionPick is one entry in the new-shape selection response. The
// `Confidence` field is a pointer so we can distinguish "model
// returned 0.0" from "model omitted the field" — the latter means
// "no signal for this pick" and skips the abstention check.
type selectionPick struct {
	ID         string   `json:"id"`
	Confidence *float64 `json:"confidence,omitempty"`
}

// selectionPayload accepts both response shapes:
//
//   - New shape (preferred): {"picks": [{"id": "...", "confidence": 0.8}], ...}
//   - Legacy shape: {"selected_section_ids": ["..."], ...}
//
// When `Picks` is non-empty it wins; otherwise `SelectedSectionIDs`
// is used. This keeps backward compatibility with older models that
// can't reason about confidence (or with the legacy schema enforced
// by some provider integrations).
type selectionPayload struct {
	Picks              []selectionPick `json:"picks"`
	SelectedSectionIDs []string        `json:"selected_section_ids"`
	Reasoning          string          `json:"reasoning"`
}

// ParseSelection extracts the section-ID list and (when present) per-ID
// confidence scores from an LLM JSON response. Tolerates code-fence
// wrappers and leading/trailing prose.
//
// Returns:
//
//   - ids:         the section IDs the model picked, in the order the
//     model returned them.
//   - confidences: map[id]float64 of per-pick confidences in [0.0, 1.0],
//     populated only when the model returned the new-shape
//     `picks` array. Returns nil (not an empty map) when
//     the response was the legacy shape OR when every pick
//     omitted its confidence — the distinction matters for
//     abstention, which fires only when confidence signal
//     is explicitly present.
//   - err:         non-nil only when the JSON cannot be decoded at all.
func ParseSelection(raw string) ([]tree.SectionID, map[tree.SectionID]float64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil, nil
	}
	// Strip ```json ... ``` fences if present.
	if strings.HasPrefix(raw, "```") {
		if i := strings.Index(raw, "\n"); i >= 0 {
			raw = raw[i+1:]
		}
		raw = strings.TrimSuffix(raw, "```")
		raw = strings.TrimSpace(raw)
	}
	// Find the first { and last } — models occasionally wrap with a sentence.
	if i := strings.Index(raw, "{"); i > 0 {
		raw = raw[i:]
	}
	if j := strings.LastIndex(raw, "}"); j >= 0 && j < len(raw)-1 {
		raw = raw[:j+1]
	}

	var p selectionPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, nil, fmt.Errorf("unmarshal selection: %w", err)
	}

	// New shape wins. Even a single populated `picks` entry means the
	// model attempted to follow the confidence protocol, so we honour
	// it. Mixed responses (some picks with confidence, some without)
	// surface only the present confidences — the missing ones are
	// silently dropped from the confidence map, NOT defaulted to 0.
	if len(p.Picks) > 0 {
		ids := make([]tree.SectionID, 0, len(p.Picks))
		confidences := make(map[tree.SectionID]float64, len(p.Picks))
		seen := make(map[tree.SectionID]struct{}, len(p.Picks))
		for _, pk := range p.Picks {
			id := strings.TrimSpace(pk.ID)
			if id == "" {
				continue
			}
			sid := tree.SectionID(id)
			if _, dup := seen[sid]; dup {
				continue
			}
			seen[sid] = struct{}{}
			ids = append(ids, sid)
			if pk.Confidence != nil {
				c := *pk.Confidence
				// Clamp into [0, 1]. The model is instructed to stay
				// in range; clamping is a defence-in-depth so a
				// runaway value never poisons the abstention check.
				if c < 0 {
					c = 0
				} else if c > 1 {
					c = 1
				}
				confidences[sid] = c
			}
		}
		if len(confidences) == 0 {
			// New-shape response but no confidences populated → treat
			// as legacy for abstention purposes.
			confidences = nil
		}
		return ids, confidences, nil
	}

	// Legacy shape.
	out := make([]tree.SectionID, 0, len(p.SelectedSectionIDs))
	for _, id := range p.SelectedSectionIDs {
		id = strings.TrimSpace(id)
		if id != "" {
			out = append(out, tree.SectionID(id))
		}
	}
	return out, nil, nil
}

// FilterKnownIDs drops any IDs not present in the supplied section views and
// deduplicates. Preserves the first-seen order.
func FilterKnownIDs(ids []tree.SectionID, sections []tree.SectionView) []tree.SectionID {
	known := make(map[tree.SectionID]struct{}, len(sections))
	for _, sv := range sections {
		known[sv.ID] = struct{}{}
	}
	seen := map[tree.SectionID]struct{}{}
	out := make([]tree.SectionID, 0, len(ids))
	for _, id := range ids {
		if _, ok := known[id]; !ok {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
