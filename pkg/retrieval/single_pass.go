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

	ids, usage, err := runSelectionWithRetry(ctx, s.LLM, req, defaultSelectionRetries)
	if err != nil {
		return nil, fmt.Errorf("single-pass llm call: %w", err)
	}

	return &Result{
		SelectedIDs: FilterKnownIDs(ids, view.Sections),
		ModelUsed:   model,
		Usage:       usage,
		HopsTaken:   1,
	}, nil
}

// defaultSelectionRetries is the number of EXTRA attempts (on top of the first)
// the selection LLM call gets when its response fails to parse as JSON. Gemini's
// JSON mode occasionally returns plain text ("The most relevant section is...");
// without retry, that surfaces as a 500 to the SDK on every such glitch.
const defaultSelectionRetries = 2

// runSelectionWithRetry runs a selection LLM call and parses the response,
// retrying up to maxRetries additional times if the model returns something
// that doesn't parse as JSON. Returns the parsed IDs and the cumulative usage
// across all attempts. An error is returned only on a transport/LLM failure —
// final parse failure degrades gracefully to an empty selection (logged) so a
// single LLM-formatting blip doesn't 500 the entire query.
func runSelectionWithRetry(ctx context.Context, client llmgate.Client, baseReq llmgate.Request, maxRetries int) ([]tree.SectionID, Usage, error) {
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
			return nil, totalUsage, err
		}
		totalUsage.Add(Usage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
			TotalTokens:  resp.Usage.TotalTokens,
			CostUSD:      resp.Usage.CostUSD,
			LLMCalls:     1,
		})
		ids, parseErr := ParseSelection(resp.Content)
		if parseErr == nil {
			return ids, totalUsage, nil
		}
		lastParseErr = parseErr
	}
	log.Printf("retrieval: selection parse failed after %d attempts (%v); returning empty selection", maxRetries+1, lastParseErr)
	return nil, totalUsage, nil
}

// --- shared prompt scaffolding ---

const selectionSystemPrompt = `You are a precise retrieval engine. Given a hierarchical outline of a document (titles + short summaries + stable section IDs) and a user query, pick the section IDs whose full content is most likely to answer the query.

Rules:
- Prefer leaf sections. Include a parent only if the parent's own body is directly relevant.
- Include as few sections as possible. Quality over quantity.
- Only return IDs present in the provided outline. Do not invent IDs.
- If nothing is relevant, return an empty list.`

const selectionJSONSchema = `{
  "type": "object",
  "properties": {
    "selected_section_ids": {"type": "array", "items": {"type": "string"}},
    "reasoning": {"type": "string"}
  },
  "required": ["selected_section_ids"]
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
	b.WriteString("\n\nReturn a JSON object with fields `selected_section_ids` (array of strings) and `reasoning` (string).")
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

// selectionPayload is the expected JSON-mode shape.
type selectionPayload struct {
	SelectedSectionIDs []string `json:"selected_section_ids"`
	Reasoning          string   `json:"reasoning"`
}

// ParseSelection extracts the section-ID list from an LLM JSON response.
// Tolerates code-fence wrappers and leading/trailing prose.
func ParseSelection(raw string) ([]tree.SectionID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
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
		return nil, fmt.Errorf("unmarshal selection: %w", err)
	}
	out := make([]tree.SectionID, 0, len(p.SelectedSectionIDs))
	for _, id := range p.SelectedSectionIDs {
		id = strings.TrimSpace(id)
		if id != "" {
			out = append(out, tree.SectionID(id))
		}
	}
	return out, nil
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
