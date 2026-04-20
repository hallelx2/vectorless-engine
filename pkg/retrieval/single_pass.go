package retrieval

import (
	"context"
	"encoding/json"
	"fmt"
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
	if t == nil || t.Root == nil {
		return nil, nil
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

	resp, err := s.LLM.Complete(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("single-pass llm call: %w", err)
	}

	ids, err := ParseSelection(resp.Content)
	if err != nil {
		return nil, fmt.Errorf("single-pass parse: %w", err)
	}
	return FilterKnownIDs(ids, view.Sections), nil
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
