package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/db"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// generateCandidateQuestions runs the HyDE-style stage: for each leaf
// section it asks the LLM to enumerate a handful of concrete questions
// the section's content can answer, and persists the result.
//
// The questions are folded into the retrieval prompt at query time so
// the section text overlaps lexically/semantically with a wider range
// of user phrasings than its summary alone would cover. This is a
// retrieval-quality booster — failures are non-fatal.
//
// Mirrors summarize: per-depth processing isn't required (leaves only),
// but we still use a sem-bounded errgroup so a large doc doesn't open
// 200 concurrent LLM calls.
func (p *Pipeline) generateCandidateQuestions(ctx context.Context, docID tree.DocumentID, profile string) error {
	sections, err := p.DB.ListSectionsForWorker(ctx, docID)
	if err != nil {
		return err
	}

	// Build a parent → has-children map so we skip internal nodes (HyDE
	// targets leaf content, not abstract summaries).
	hasChildren := map[tree.SectionID]bool{}
	for _, s := range sections {
		if s.ParentID != "" {
			hasChildren[s.ParentID] = true
		}
	}

	var (
		mu   sync.Mutex
		errs []error
	)

	concurrency := p.HyDEConcurrency
	if concurrency <= 0 {
		concurrency = 4
	}
	sem := make(chan struct{}, concurrency)
	g, gctx := errgroup.WithContext(ctx)

	for _, s := range sections {
		if hasChildren[s.ID] {
			continue // internal nodes skip HyDE; only leaves get question lists
		}
		if len(s.CandidateQuestions) > 0 {
			continue // already populated (idempotent retry)
		}
		s := s
		g.Go(func() error {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-gctx.Done():
				return nil
			}

			questions, err := p.candidateQuestionsFor(gctx, s, profile)
			if err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("section %s: %w", s.ID, err))
				mu.Unlock()
				return nil // non-fatal — don't abort siblings
			}
			if len(questions) == 0 {
				// No usable questions (parse failure or empty list) — leave
				// candidate_questions NULL rather than store an empty array.
				return nil
			}
			if err := p.DB.UpdateSectionCandidateQuestions(gctx, s.ID, questions); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
			return nil
		})
	}

	_ = g.Wait() // errors collected in errs, not propagated
	return errors.Join(errs...)
}

// candidateQuestionsFor runs the HyDE LLM call for a single leaf section
// and returns the parsed question list. Empty list + nil error means
// "model produced something we can't parse — proceed without questions".
func (p *Pipeline) candidateQuestionsFor(ctx context.Context, s db.Section, profile string) ([]string, error) {
	body := ""
	if s.ContentRef != "" {
		rc, _, err := p.Storage.Get(ctx, s.ContentRef)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		raw, err := io.ReadAll(io.LimitReader(rc, int64(p.SummaryMaxChars)))
		if err != nil {
			return nil, err
		}
		body = cleanForLLM(string(raw))
	}

	n := p.HyDENumQuestions
	if n <= 0 {
		n = 5
	}

	model := p.HyDEModel
	if model == "" {
		model = p.SummaryModel
	}

	system := hydeSystemPrompt(profile)
	user := fmt.Sprintf(
		"Section titled %q.\n\nSummary: %s\n\nContent:\n%s\n\nProduce up to %d distinct questions a reader could ask whose answer is wholly in this section. Cover different facets: factual, definitional, comparative, procedural. Each question must be self-contained (no \"this section\" / \"the above\"). Return ONLY a JSON object: {\"questions\": [\"...\", \"...\"]}",
		cleanForLLM(s.Title), cleanForLLM(s.Summary), body, n,
	)

	req := llmgate.Request{
		Model:       model,
		Temperature: 0.2, // a smidgen of variety so questions don't collapse
		MaxTokens:   600,
		Messages: []llmgate.Message{
			{Role: llmgate.RoleSystem, Content: system},
			{Role: llmgate.RoleUser, Content: user},
		},
		JSONMode:   true,
		JSONSchema: []byte(hydeJSONSchema),
	}

	questions, err := runHyDEWithRetry(ctx, p.LLM, req, defaultHyDERetries)
	if err != nil {
		return nil, err
	}

	// Cap at the requested count + trim duplicates / blanks.
	return dedupeNonEmpty(questions, n), nil
}

// defaultHyDERetries mirrors the retrieval pattern: 1 initial attempt + N
// retries with a stricter JSON nudge.
const defaultHyDERetries = 2

// runHyDEWithRetry runs the HyDE LLM call and parses the response,
// retrying up to maxRetries additional times if parsing fails. Final
// parse failure returns an error so the caller can log it; transport
// errors propagate. ErrNotImplemented (stub LLM) degrades to "no
// questions" so test paths keep working.
func runHyDEWithRetry(ctx context.Context, client llmgate.Client, baseReq llmgate.Request, maxRetries int) ([]string, error) {
	if maxRetries < 0 {
		maxRetries = 0
	}
	var lastParseErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		req := baseReq
		if attempt > 0 {
			msgs := make([]llmgate.Message, len(baseReq.Messages))
			copy(msgs, baseReq.Messages)
			tail := len(msgs) - 1
			msgs[tail] = llmgate.Message{
				Role:    msgs[tail].Role,
				Content: msgs[tail].Content + "\n\nIMPORTANT: respond with ONLY a JSON object matching the schema {\"questions\": [\"...\", \"...\"]}. No prose, no markdown fences.",
			}
			req.Messages = msgs
		}
		resp, err := client.Complete(ctx, req)
		if err != nil {
			// Stub clients return ErrNotImplemented — treat as "no
			// questions" so the pipeline proceeds without LLM access
			// in test setups.
			if errors.Is(err, llmgate.ErrNotImplemented) {
				return nil, nil
			}
			return nil, err
		}
		questions, parseErr := parseHyDEResponse(resp.Content)
		if parseErr == nil {
			return questions, nil
		}
		lastParseErr = parseErr
	}
	return nil, fmt.Errorf("hyde: parse failed after %d attempts: %w", maxRetries+1, lastParseErr)
}

// parseHyDEResponse extracts the question list from an LLM JSON response.
// Tolerates code-fence wrappers and leading/trailing prose, matching the
// retrieval ParseSelection contract.
func parseHyDEResponse(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
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

	var payload struct {
		Questions []string `json:"questions"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil, fmt.Errorf("unmarshal hyde response: %w", err)
	}
	return payload.Questions, nil
}

// dedupeNonEmpty trims, drops blanks, dedupes (case-insensitive) and
// caps the slice at max entries. Preserves first-seen order.
func dedupeNonEmpty(in []string, max int) []string {
	if max <= 0 {
		max = len(in)
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, q := range in {
		q = strings.TrimSpace(q)
		if q == "" {
			continue
		}
		key := strings.ToLower(q)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, q)
		if len(out) >= max {
			break
		}
	}
	return out
}

const hydeJSONSchema = `{
  "type": "object",
  "properties": {
    "questions": {"type": "array", "items": {"type": "string"}}
  },
  "required": ["questions"]
}`

// hydeSystemPrompt returns a domain-aware system prompt for the HyDE
// candidate-question stage. The questions are retrieval helpers — they
// widen the lexical/semantic surface of a section so that a downstream
// retrieval engine matches it to user queries that don't echo the
// section's exact wording.
func hydeSystemPrompt(profile string) string {
	const rule = "Generate candidate questions whose answer is entirely contained in this section. Each question must be self-contained, specific, and use the section's own terminology where it is informative. Vary the questions so they cover different facets: factual lookup, definitional, comparative, procedural, and 'why/how' questions when applicable. Avoid yes/no questions when an open-ended phrasing carries more lexical signal. Do NOT invent facts that aren't supported by the section."
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "research":
		return "You generate retrieval-helper questions for sections of academic research papers. " + rule
	case "medical":
		return "You generate retrieval-helper questions for sections of clinical and medical documents. " + rule
	default:
		return "You generate retrieval-helper questions for sections of business, legal, and financial documents. " + rule
	}
}
