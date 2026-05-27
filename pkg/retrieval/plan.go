package retrieval

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/cache"
)

// Plan is the structured understanding the planner extracts from a user
// query before retrieval runs. It is a deliberately small, model-agnostic
// shape: enough to inform the downstream retrieval + synthesis steps
// without trying to encode the full query semantics.
//
// Fields:
//
//   - Intent labels the query so the synthesis prompt can adapt tone
//     (factual lookup vs. comparison vs. summary, etc.). Values are
//     free-form strings rather than an enum because the set is open and
//     downstream consumers are tolerant.
//
//   - Entities are the salient proper nouns, dates, or numbers the model
//     judged load-bearing. Synthesis surfaces these to the answer model;
//     they are not (yet) fed into the selection prompt.
//
//   - ExpectedDocAreas are coarse hints about where in a document the
//     answer is likely to live (e.g. "balance sheet", "risk factors").
//     The planner is allowed to leave this empty when no strong prior
//     exists.
//
//   - IsMultiHop signals that the query benefits from decomposition.
//     When true SubQuestions holds the individual focused questions the
//     decomposer dispatches one-at-a-time. When false SubQuestions is
//     empty.
type Plan struct {
	Intent           string   `json:"intent"`
	Entities         []string `json:"entities"`
	ExpectedDocAreas []string `json:"expected_doc_areas"`
	IsMultiHop       bool     `json:"is_multi_hop"`
	SubQuestions     []string `json:"sub_questions"`
}

// Planner runs one LLM call before retrieval to produce a Plan. The result
// is cached in an LRU keyed on the raw query text so repeated questions
// don't burn extra LLM budget.
//
// The planner is deliberately defensive about both LLM blips (parse
// failures fall back to a no-plan result so the engine continues with the
// original query) and cache pressure (cache misses always fall through to
// the LLM call; a cache eviction loop in the background must never block
// query latency).
type Planner struct {
	// LLM is the client used for the planning call.
	LLM llmgate.Client

	// Model is the model name passed to the LLM. The caller is expected
	// to point this at a small/fast model — planning is a short prompt
	// that should not run on the same flagship model used for synthesis.
	Model string

	// MaxRetries bounds the JSON-parse retries on a single planning call.
	// Zero defaults to defaultPlanningRetries.
	MaxRetries int

	cache cache.Cache

	// mu serialises cache writes for the same key so that two
	// concurrent queries don't race to populate the same entry. The
	// underlying cache.LRU is already mutex-guarded for Get/Set
	// atomicity; this is purely about avoiding redundant LLM calls.
	mu sync.Mutex
}

// defaultPlanningCacheSize is the planner cache capacity when the caller
// does not specify one. 128 distinct queries is comfortable for typical
// per-tenant repeat-question workloads while keeping resident memory
// trivial.
const defaultPlanningCacheSize = 128

// defaultPlanningRetries is the number of additional attempts the
// planning LLM call gets when the response fails to parse as JSON. The
// same Gemini JSON-mode blip that single_pass.go guards against can hit
// here too.
const defaultPlanningRetries = 2

// planningCacheTTL is the cache lifetime for planning results. Plans are
// a property of the query text, not the document, so a long TTL is safe;
// the only reason to expire is to bound stale-prompt drift across model
// upgrades.
const planningCacheTTL = 6 * time.Hour

// NewPlanner constructs a Planner backed by client. Pass cacheSize=0 to
// accept the default (128).
func NewPlanner(client llmgate.Client, model string) *Planner {
	return NewPlannerWithCacheSize(client, model, defaultPlanningCacheSize)
}

// NewPlannerWithCacheSize is the explicit-capacity constructor. Mostly
// useful in tests; production callers should prefer NewPlanner.
func NewPlannerWithCacheSize(client llmgate.Client, model string, cacheSize int) *Planner {
	if cacheSize <= 0 {
		cacheSize = defaultPlanningCacheSize
	}
	return &Planner{
		LLM:        client,
		Model:      model,
		MaxRetries: defaultPlanningRetries,
		cache:      cache.NewLRU(cacheSize),
	}
}

// Plan returns the planner's understanding of query. On cache hit Usage
// is the zero value (no LLM call was made). Errors are returned only on
// transport failures from the LLM client; persistent JSON-parse failures
// fall back to a nil Plan with a non-nil error so the caller can decide
// whether to ignore the planner for this request — handleQuery treats
// that as "no plan, no decomposition".
//
// A nil *Planner is treated as "planning disabled": Plan returns
// (nil, Usage{}, nil) so callers can wire a planner unconditionally
// without nil checks.
func (p *Planner) Plan(ctx context.Context, query string) (*Plan, Usage, error) {
	if p == nil || p.LLM == nil {
		return nil, Usage{}, nil
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, Usage{}, nil
	}

	key := planningCacheKey(query, p.Model)

	if v, ok := p.cache.Get(key); ok {
		if pl, ok := v.(*Plan); ok && pl != nil {
			// Return a defensive copy so callers can't mutate the
			// cached entry.
			return clonePlan(pl), Usage{}, nil
		}
	}

	// Serialise cache writes for this key. The cache itself is
	// thread-safe; this lock only prevents two concurrent identical
	// queries from each issuing the same LLM call.
	p.mu.Lock()
	defer p.mu.Unlock()
	// Re-check after acquiring the lock — another goroutine may have
	// just populated the entry while we were waiting.
	if v, ok := p.cache.Get(key); ok {
		if pl, ok := v.(*Plan); ok && pl != nil {
			return clonePlan(pl), Usage{}, nil
		}
	}

	maxRetries := p.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}
	if maxRetries == 0 {
		maxRetries = defaultPlanningRetries
	}

	plan, usage, err := runPlanningWithRetry(ctx, p.LLM, p.Model, query, maxRetries)
	if err != nil {
		return nil, usage, err
	}
	if plan == nil {
		return nil, usage, nil
	}

	// Cache write — failure to store (e.g. zero-capacity LRU) must
	// never propagate; the next call simply re-issues the LLM call.
	p.cache.Set(key, plan, planningCacheTTL)

	return clonePlan(plan), usage, nil
}

// CacheStats exposes the underlying cache metrics. Useful in tests + ops.
func (p *Planner) CacheStats() cache.Stats {
	if p == nil || p.cache == nil {
		return cache.Stats{}
	}
	return p.cache.Stats()
}

// planningCacheKey hashes the query + model into a stable cache key.
// Two callers pinning the same query at the same model share the cache
// entry; switching the model invalidates it.
func planningCacheKey(query, model string) string {
	return cache.Key("planner", query, "plan", model)
}

// clonePlan returns a defensive copy of p. The planner caches plans by
// pointer; without copying, a caller could mutate Entities/SubQuestions
// and corrupt the cache.
func clonePlan(p *Plan) *Plan {
	if p == nil {
		return nil
	}
	out := &Plan{
		Intent:     p.Intent,
		IsMultiHop: p.IsMultiHop,
	}
	if len(p.Entities) > 0 {
		out.Entities = append([]string(nil), p.Entities...)
	}
	if len(p.ExpectedDocAreas) > 0 {
		out.ExpectedDocAreas = append([]string(nil), p.ExpectedDocAreas...)
	}
	if len(p.SubQuestions) > 0 {
		out.SubQuestions = append([]string(nil), p.SubQuestions...)
	}
	return out
}

// --- prompt + parse ---

// planningSystemPrompt is intentionally conservative about IsMultiHop:
// the wording asks the model to mark a query multi-hop ONLY when distinct
// sub-questions are necessary, not whenever a query mentions two things.
// Over-firing here forces extra LLM calls in the decomposer without
// quality wins, so the prompt biases toward false rather than true.
const planningSystemPrompt = `You are a query planner for a document-retrieval engine. Given a user's query you return a small JSON object describing the query.

Rules:
- "intent": one short snake_case label. Examples: "factual_lookup", "comparison", "summary", "definition", "list", "calculation". Pick the closest fit.
- "entities": proper nouns, dates, numbers, or specific terms the query hinges on. Skip filler words.
- "expected_doc_areas": short hints about WHERE in a document the answer is likely (e.g. "balance sheet", "risk factors", "methodology", "conclusion"). Leave empty if unsure.
- "is_multi_hop": true ONLY when the query requires answering distinct sub-questions whose answers must be combined. A single question that mentions two things is NOT multi-hop. A compound question that genuinely needs two retrieval passes (e.g. "compare X's revenue with Y's revenue") IS multi-hop. When in doubt, return false.
- "sub_questions": when is_multi_hop is true, list the focused sub-questions (each one a standalone retrieval target). Empty when is_multi_hop is false.

Return only the JSON object. No prose, no markdown.`

const planningJSONSchema = `{
  "type": "object",
  "properties": {
    "intent": {"type": "string"},
    "entities": {"type": "array", "items": {"type": "string"}},
    "expected_doc_areas": {"type": "array", "items": {"type": "string"}},
    "is_multi_hop": {"type": "boolean"},
    "sub_questions": {"type": "array", "items": {"type": "string"}}
  },
  "required": ["intent", "is_multi_hop"]
}`

// buildPlanningPrompt returns the user message for the planning call.
func buildPlanningPrompt(query string) string {
	var b strings.Builder
	b.WriteString("User query:\n")
	b.WriteString(query)
	b.WriteString("\n\nReturn a JSON object with fields: intent (string), entities (array of strings), expected_doc_areas (array of strings), is_multi_hop (boolean), sub_questions (array of strings).")
	return b.String()
}

// runPlanningWithRetry issues the planning LLM call, retrying up to
// maxRetries additional times when the response does not parse. Mirrors
// the shape of runSelectionWithRetry but specialised to the Plan payload.
// Returns (nil, usage, nil) when the planner exhausts retries; the
// caller treats that as a no-plan request.
func runPlanningWithRetry(ctx context.Context, client llmgate.Client, model, query string, maxRetries int) (*Plan, Usage, error) {
	user := buildPlanningPrompt(query)
	baseReq := llmgate.Request{
		Model: model,
		Messages: []llmgate.Message{
			{Role: llmgate.RoleSystem, Content: planningSystemPrompt},
			{Role: llmgate.RoleUser, Content: user},
		},
		MaxTokens:   512,
		Temperature: 0,
		JSONMode:    true,
		JSONSchema:  []byte(planningJSONSchema),
	}

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
			return nil, totalUsage, fmt.Errorf("planner llm call: %w", err)
		}
		totalUsage.Add(Usage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
			TotalTokens:  resp.Usage.TotalTokens,
			CostUSD:      resp.Usage.CostUSD,
			LLMCalls:     1,
		})
		plan, parseErr := ParsePlan(resp.Content)
		if parseErr == nil {
			return plan, totalUsage, nil
		}
		lastParseErr = parseErr
	}
	log.Printf("retrieval: planner parse failed after %d attempts (%v); continuing without plan", maxRetries+1, lastParseErr)
	return nil, totalUsage, nil
}

// ParsePlan extracts a Plan from an LLM JSON response. Tolerates code
// fences and leading/trailing prose, the same as ParseSelection. Returns
// a sanitised Plan: trimmed strings, empty slices instead of nil for
// stable JSON output, and IsMultiHop forced to false when SubQuestions
// is empty (a multi-hop flag with no decomposition is a model glitch we
// can correct locally rather than letting bad data flow downstream).
func ParsePlan(raw string) (*Plan, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty planner response")
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

	var p Plan
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, fmt.Errorf("unmarshal plan: %w", err)
	}

	p.Intent = strings.TrimSpace(p.Intent)
	p.Entities = trimStrings(p.Entities)
	p.ExpectedDocAreas = trimStrings(p.ExpectedDocAreas)
	p.SubQuestions = trimStrings(p.SubQuestions)

	// Self-correct: a multi-hop flag without sub-questions is useless to
	// the decomposer. Clear the flag rather than raise an error so the
	// pipeline keeps making progress.
	if p.IsMultiHop && len(p.SubQuestions) == 0 {
		p.IsMultiHop = false
	}
	// And vice versa: if sub-questions came back but the flag wasn't
	// set, leave both as the model returned them. Decomposer's
	// fall-through path treats !IsMultiHop as "ignore sub-questions",
	// which is the safer default.

	return &p, nil
}

// trimStrings returns a new slice with each element trimmed and empty
// entries removed.
func trimStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}
