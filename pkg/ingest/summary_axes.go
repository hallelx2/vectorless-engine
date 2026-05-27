package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// summaryAxesJSONSchema is the JSON-mode schema enforced by the
// Phase 2.5 structured summarizer. All four axes are optional at the
// schema level so a parse-tolerant model that produces a partially
// populated object still validates — we cap and validate downstream.
const summaryAxesJSONSchema = `{
  "type": "object",
  "properties": {
    "topics":   {"type": "array", "items": {"type": "string"}},
    "entities": {"type": "array", "items": {"type": "string"}},
    "numbers":  {"type": "array", "items": {"type": "string"}},
    "one_line": {"type": "string"}
  },
  "required": ["one_line"]
}`

// summaryAxesSystemPrompt returns a domain-aware system prompt for the
// Phase 2.5 structured summarizer. Compared to the legacy one-line
// prompt, this one asks for FOUR axes — topics, entities, numbers,
// and a one-line sentence — so the downstream retrieval prompt can
// match user queries on more than just paraphrased prose. The
// per-domain tweaks (research/medical/generic) shift the emphasis of
// what counts as an entity or a number but leave the schema intact.
func summaryAxesSystemPrompt(profile string) string {
	const rule = "You are summarising a section of a document for a retrieval engine. The retrieval engine reads your output to decide whether this section answers a user's question, so name concrete topics, entities, identifiers, and key numbers — not generic descriptions. Do NOT invent facts that aren't in the section."
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "research":
		return "You summarise sections of academic research papers along multiple axes. " + rule
	case "medical":
		return "You summarise sections of clinical and medical documents along multiple axes. " + rule
	default:
		return "You summarise sections of business, legal, and financial documents along multiple axes. " + rule
	}
}

// summaryAxesUserSuffix renders the per-axis instructions that follow
// the section's content in the user message. The caps are surfaced
// explicitly so a downstream operator can tune them via config
// without touching the prompt — the prompt itself stays declarative.
func summaryAxesUserSuffix(maxTopics, maxEntities, maxNumbers int) string {
	if maxTopics <= 0 {
		maxTopics = 4
	}
	if maxEntities <= 0 {
		maxEntities = 8
	}
	if maxNumbers <= 0 {
		maxNumbers = 6
	}
	return fmt.Sprintf(`Return a JSON object with:
- topics:    array of 1-%d topic keywords (lower-case, hyphenated)
- entities:  array of 0-%d proper-noun mentions found in the section (orgs, people, places, dates)
- numbers:   array of 0-%d standout numeric values WITH UNITS as they appear in the section ("$4.2B", "2.8%%", "Q3 2024")
- one_line:  one sentence (≤30 words) describing what the section is about

If a field has nothing to populate, return an empty array (or empty string for one_line). Return ONLY the JSON object — no prose, no markdown fences.`, maxTopics, maxEntities, maxNumbers)
}

// defaultSummaryAxesRetries is the number of EXTRA attempts (on top of
// the first) the structured summary call gets when parsing fails.
// Matches the HyDE / retrieval retry contract.
const defaultSummaryAxesRetries = 2

// runSummaryAxesWithRetry runs the structured summary LLM call and
// parses the response, retrying up to maxRetries additional times if
// parsing fails. Returns parsed axes on success, the raw response text
// (for OneLine fallback) on final parse failure with no transport
// error, and a non-nil error only for transport-level failures.
//
// ErrNotImplemented (stub LLM) is folded into the error return so
// callers can degrade to the text fallback path.
func runSummaryAxesWithRetry(ctx context.Context, client llmgate.Client, baseReq llmgate.Request, maxRetries int) (*tree.SummaryAxes, string, error) {
	if maxRetries < 0 {
		maxRetries = 0
	}
	var lastRaw string
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
			// ErrNotImplemented bubbles up so the caller can use a
			// purely text-based fallback. Transport errors do the same.
			return nil, lastRaw, err
		}
		lastRaw = resp.Content
		axes, parseErr := parseSummaryAxes(resp.Content)
		if parseErr == nil {
			return axes, lastRaw, nil
		}
	}
	// Final parse failure — return the raw text so the caller can
	// stash it as OneLine. Not an error: the document is still
	// ingestable, just with shallower retrieval signal.
	return nil, lastRaw, nil
}

// parseSummaryAxes extracts a SummaryAxes from an LLM JSON response.
// Tolerates code-fence wrappers and leading/trailing prose, matching
// ParseSelection / parseHyDEResponse.
func parseSummaryAxes(raw string) (*tree.SummaryAxes, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("empty response")
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
	var out tree.SummaryAxes
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("unmarshal summary_axes: %w", err)
	}
	out.Topics = trimNonEmpty(out.Topics)
	out.Entities = trimNonEmpty(out.Entities)
	out.Numbers = trimNonEmpty(out.Numbers)
	out.OneLine = strings.TrimSpace(out.OneLine)
	return &out, nil
}

// capStrings truncates *xs to at most max entries in place. A nil or
// non-positive max disables the cap.
func capStrings(xs *[]string, max int) {
	if xs == nil || max <= 0 {
		return
	}
	if len(*xs) > max {
		*xs = (*xs)[:max]
	}
}

// trimNonEmpty strips empty / whitespace-only entries from a slice.
// Preserves order. Returns nil for an all-empty input so the omitempty
// JSON tag on SummaryAxes drops the field from the persisted blob.
func trimNonEmpty(xs []string) []string {
	if len(xs) == 0 {
		return nil
	}
	out := xs[:0]
	for _, x := range xs {
		x = strings.TrimSpace(x)
		if x != "" {
			out = append(out, x)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
