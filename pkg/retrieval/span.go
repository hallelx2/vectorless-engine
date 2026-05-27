package retrieval

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hallelx2/llmgate"
)

// AnswerSpan is the most relevant substring of a section's content for
// a given query, with byte offsets back into the original content.
//
// Start and End are byte offsets such that content[Start:End] == Text
// after the locator step. When the span text does not appear verbatim
// in the content (the model paraphrased), Start and End are -1 and
// Text holds the model's quote.
type AnswerSpan struct {
	Start int    `json:"start"`
	End   int    `json:"end"`
	Text  string `json:"text"`
}

// SpanExtractor pulls the most query-relevant verbatim span out of a
// section's content with one LLM call.
type SpanExtractor struct {
	LLM   llmgate.Client
	Model string
	// MaxQuoteLen caps how many characters the model is allowed to quote.
	// Keeps the response tight and forces the model to pick a focused
	// span instead of returning the whole section. Default: 400.
	MaxQuoteLen int
}

// NewSpanExtractor constructs a SpanExtractor with sensible defaults.
func NewSpanExtractor(client llmgate.Client, model string) *SpanExtractor {
	return &SpanExtractor{LLM: client, Model: model, MaxQuoteLen: 400}
}

const spanSystemPrompt = `You are a precise quotation engine. Given a section of a document and a user query, extract the SHORTEST verbatim quote from the section that directly answers (or is the most relevant evidence for) the query.

Rules:
- Quote verbatim from the section. Do not paraphrase, summarize, or invent text.
- Pick the smallest contiguous span that contains the answer. One sentence is usually enough; a phrase is better.
- If the section contains nothing useful for the query, set "found" to false and return an empty quote.`

const spanJSONSchema = `{
  "type": "object",
  "properties": {
    "found": {"type": "boolean"},
    "quote": {"type": "string"}
  },
  "required": ["found", "quote"]
}`

// Extract runs one LLM call to pull the most relevant verbatim span
// from sectionContent for query. Returns nil (no error) when the
// section does not contain an answer; that is the no-evidence path,
// not a failure. A non-nil error is returned only on transport / LLM
// failure.
func (e *SpanExtractor) Extract(ctx context.Context, sectionContent, query string) (*AnswerSpan, Usage, error) {
	if strings.TrimSpace(sectionContent) == "" || strings.TrimSpace(query) == "" {
		return nil, Usage{}, nil
	}
	maxQuote := e.MaxQuoteLen
	if maxQuote <= 0 {
		maxQuote = 400
	}

	user := fmt.Sprintf(
		"Section content:\n---\n%s\n---\n\nUser query:\n%s\n\nReturn a JSON object with `found` (boolean) and `quote` (string, verbatim from the section, ≤ %d characters).",
		sectionContent, query, maxQuote,
	)
	req := llmgate.Request{
		Model: e.Model,
		Messages: []llmgate.Message{
			{Role: llmgate.RoleSystem, Content: spanSystemPrompt},
			{Role: llmgate.RoleUser, Content: user},
		},
		MaxTokens:   512,
		Temperature: 0,
		JSONMode:    true,
		JSONSchema:  []byte(spanJSONSchema),
	}

	resp, err := e.LLM.Complete(ctx, req)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("span-extract llm call: %w", err)
	}
	usage := Usage{
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		TotalTokens:  resp.Usage.TotalTokens,
		CostUSD:      resp.Usage.CostUSD,
		LLMCalls:     1,
	}

	quote, found, parseErr := parseSpanResponse(resp.Content)
	if parseErr != nil {
		return nil, usage, fmt.Errorf("parse span response: %w", parseErr)
	}
	if !found || strings.TrimSpace(quote) == "" {
		return nil, usage, nil
	}
	if len(quote) > maxQuote {
		quote = quote[:maxQuote]
	}

	start, end := locateQuote(sectionContent, quote)
	return &AnswerSpan{Start: start, End: end, Text: quote}, usage, nil
}

// locateQuote finds quote in content. Returns -1, -1 when the quote
// does not appear verbatim (the model paraphrased despite the
// instructions). First tries exact substring, then normalised
// whitespace.
func locateQuote(content, quote string) (int, int) {
	if i := strings.Index(content, quote); i >= 0 {
		return i, i + len(quote)
	}
	// Whitespace-normalised match: collapse runs of whitespace in both.
	normContent := collapseWS(content)
	normQuote := collapseWS(quote)
	if j := strings.Index(normContent, normQuote); j >= 0 {
		// Walk the original content counting normalised characters until
		// we reach j; that's our start. Then add normQuote length back
		// through the same walk for the end.
		start := mapNormToOriginal(content, j)
		end := mapNormToOriginal(content, j+len(normQuote))
		if start >= 0 && end > start {
			return start, end
		}
	}
	return -1, -1
}

func collapseWS(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevWS := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		ws := c == ' ' || c == '\t' || c == '\n' || c == '\r'
		if ws {
			if !prevWS {
				b.WriteByte(' ')
			}
			prevWS = true
			continue
		}
		b.WriteByte(c)
		prevWS = false
	}
	return b.String()
}

// mapNormToOriginal returns the index in s that corresponds to the
// normalised-character index n (where the normalised string is
// collapseWS(s)). Returns -1 if n is out of range.
func mapNormToOriginal(s string, n int) int {
	idx := 0
	prevWS := false
	for i := 0; i < len(s); i++ {
		if idx == n {
			return i
		}
		c := s[i]
		ws := c == ' ' || c == '\t' || c == '\n' || c == '\r'
		if ws {
			if !prevWS {
				idx++
			}
			prevWS = true
			continue
		}
		idx++
		prevWS = false
	}
	if idx == n {
		return len(s)
	}
	return -1
}

type spanPayload struct {
	Found bool   `json:"found"`
	Quote string `json:"quote"`
}

func parseSpanResponse(raw string) (quote string, found bool, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false, nil
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
	var p spanPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return "", false, fmt.Errorf("unmarshal span: %w", err)
	}
	return p.Quote, p.Found, nil
}
