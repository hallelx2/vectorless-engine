package retrieval

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// ContentFetcher reads the raw bytes for a content reference. The agentic
// strategy uses it to satisfy the `read` action, which materializes a
// section's full body into the conversation. Implementations should be
// safe for concurrent use; one tree may be traversed by many queries.
type ContentFetcher interface {
	Get(ctx context.Context, ref string) ([]byte, error)
}

// AgenticStrategy is a tool-using retrieval loop.
//
// Rather than feeding the whole tree to the model in one shot
// (single-pass) or splitting and reasoning over slices in parallel
// (chunked-tree), it lets the model explore the tree iteratively:
// outline → expand interesting branches → read promising sections →
// done. Each turn the model receives the observation from the previous
// action and emits the next action as a JSON object. The strategy
// dispatches the action, fetches the observation, and feeds it back as
// the next user message.
//
// This trades latency (N sequential LLM calls) for the ability to handle
// trees that don't fit in any single context window, with reading
// behaviour that adapts to each query.
//
// Protocol choice
//
// The strategy uses a JSON-action text protocol rather than llmgate's
// Tools field. The provider adapters in llmgate v0.2.0 declare
// ToolDef/ToolCall as scaffolding but do not yet populate ToolCalls on
// responses, so the only portable way to drive a multi-turn tool loop
// today is to ask the model to emit a JSON action each turn and parse
// it with ParseAction. The strategy is forward-compatible: when llmgate
// wires native tool calling, the actionable surface here is the same.
type AgenticStrategy struct {
	// LLM is the shared client used for every turn.
	LLM llmgate.Client

	// Fetcher reads section bodies for the `read` action.
	Fetcher ContentFetcher

	// MaxHops caps the number of LLM turns one Select consumes,
	// including the terminal "done" turn. Zero means use defaultMaxHops.
	MaxHops int

	// ModelOverride, if non-empty, replaces the budget's ModelName for
	// every turn. Useful for routing the navigation loop to a cheaper or
	// faster model than the rest of the engine.
	ModelOverride string
}

// defaultMaxHops bounds the agentic loop. Six turns is enough for
// outline → 2 expands → 2 reads → done while keeping latency and cost
// predictable. Bump this only when measurements show selection quality
// climbing with deeper traversal.
const defaultMaxHops = 6

// defaultOutlineLevel is the depth of the initial outline observation.
// One level usually surfaces enough structure for the model to choose
// where to expand next without burning a turn.
const defaultOutlineLevel = 1

// Compile-time interface checks.
var (
	_ Strategy     = (*AgenticStrategy)(nil)
	_ CostStrategy = (*AgenticStrategy)(nil)
)

// NewAgentic constructs an AgenticStrategy with sensible defaults.
func NewAgentic(client llmgate.Client, fetcher ContentFetcher) *AgenticStrategy {
	return &AgenticStrategy{
		LLM:     client,
		Fetcher: fetcher,
		MaxHops: defaultMaxHops,
	}
}

// Name implements Strategy.
func (a *AgenticStrategy) Name() string { return "agentic" }

// Select implements Strategy.
func (a *AgenticStrategy) Select(ctx context.Context, t *tree.Tree, query string, budget ContextBudget) ([]tree.SectionID, error) {
	r, err := a.SelectWithCost(ctx, t, query, budget)
	if err != nil {
		return nil, err
	}
	return r.SelectedIDs, nil
}

// SelectWithCost implements CostStrategy.
func (a *AgenticStrategy) SelectWithCost(ctx context.Context, t *tree.Tree, query string, budget ContextBudget) (*Result, error) {
	if t == nil || t.Root == nil {
		return &Result{}, nil
	}
	view := t.BuildView()
	bySectionID := indexSections(view.Sections)

	model := a.ModelOverride
	if model == "" {
		model = budget.ModelName
	}

	maxHops := a.MaxHops
	if maxHops <= 0 {
		maxHops = defaultMaxHops
	}

	// Conversation: system + initial user message (query + outline).
	msgs := []llmgate.Message{
		{Role: llmgate.RoleSystem, Content: agenticSystemPrompt},
		{Role: llmgate.RoleUser, Content: a.initialUserPrompt(view, query)},
	}

	var (
		totalUsage Usage
		hopsTaken  int
		finalIDs   []tree.SectionID
		reasoning  string
	)

	for hop := 0; hop < maxHops; hop++ {
		req := llmgate.Request{
			Model:       model,
			Messages:    msgs,
			MaxTokens:   1024,
			Temperature: 0,
		}
		resp, err := a.LLM.Complete(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("agentic hop %d: %w", hop+1, err)
		}
		hopsTaken++
		totalUsage.Add(Usage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
			TotalTokens:  resp.Usage.TotalTokens,
			CostUSD:      resp.Usage.CostUSD,
			LLMCalls:     1,
		})

		// Record the assistant turn before parsing so the next prompt has
		// the model's own context.
		msgs = append(msgs, llmgate.Message{
			Role:    llmgate.RoleAssistant,
			Content: resp.Content,
		})

		action, parseErr := ParseAction(resp.Content)
		if parseErr != nil {
			// Graceful degradation: a malformed action doesn't 500 the
			// query — we ask the model to retry once with a stronger
			// instruction. If the next turn still misparses, we abort
			// and return whatever the model has picked so far (often
			// nothing). Matches the runSelectionWithRetry pattern from
			// single_pass.go.
			log.Printf("retrieval: agentic hop %d action parse failed: %v", hop+1, parseErr)
			msgs = append(msgs, llmgate.Message{
				Role:    llmgate.RoleUser,
				Content: "Your last reply was not a valid JSON action. Reply with EXACTLY one JSON object: {\"action\":\"outline|expand|read|done\", ...}. No prose, no markdown fences.",
			})
			continue
		}

		switch action.Action {
		case actionDone:
			finalIDs = filterToTreeIDs(action.PickedIDs, bySectionID)
			reasoning = action.Reasoning
			return &Result{
				SelectedIDs: finalIDs,
				Reasoning:   reasoning,
				ModelUsed:   model,
				Usage:       totalUsage,
				HopsTaken:   hopsTaken,
				TraceToken:  ComputeTraceToken(t.DocumentID, traceDocVersionV1, model, finalIDs),
			}, nil

		case actionOutline:
			level := action.Level
			if level <= 0 {
				level = defaultOutlineLevel
			}
			obs := renderOutline(view, level)
			msgs = append(msgs, llmgate.Message{
				Role:    llmgate.RoleUser,
				Content: wrapObservation("outline", obs),
			})

		case actionExpand:
			obs, ok := renderExpand(bySectionID, action.SectionID)
			if !ok {
				msgs = append(msgs, llmgate.Message{
					Role:    llmgate.RoleUser,
					Content: wrapObservation("expand", fmt.Sprintf("unknown section_id %q. Use outline or pick an ID from a previous observation.", action.SectionID)),
				})
				continue
			}
			msgs = append(msgs, llmgate.Message{
				Role:    llmgate.RoleUser,
				Content: wrapObservation("expand", obs),
			})

		case actionRead:
			obs, ok := a.renderRead(ctx, t, tree.SectionID(action.SectionID))
			if !ok {
				msgs = append(msgs, llmgate.Message{
					Role:    llmgate.RoleUser,
					Content: wrapObservation("read", fmt.Sprintf("unknown section_id %q or no content available.", action.SectionID)),
				})
				continue
			}
			msgs = append(msgs, llmgate.Message{
				Role:    llmgate.RoleUser,
				Content: wrapObservation("read", obs),
			})

		default:
			msgs = append(msgs, llmgate.Message{
				Role:    llmgate.RoleUser,
				Content: wrapObservation(action.Action, fmt.Sprintf("unsupported action %q. Use one of: outline, expand, read, done.", action.Action)),
			})
		}
	}

	// Ran out of hops without a `done` action. Return whatever IDs the
	// model proposed in the last action (if any) plus the hop count so
	// the caller can see the cap was hit.
	log.Printf("retrieval: agentic strategy hit max_hops=%d without done; returning %d ids", maxHops, len(finalIDs))
	return &Result{
		SelectedIDs: finalIDs,
		Reasoning:   reasoning,
		ModelUsed:   model,
		Usage:       totalUsage,
		HopsTaken:   hopsTaken,
		TraceToken:  ComputeTraceToken(t.DocumentID, traceDocVersionV1, model, finalIDs),
	}, nil
}

// initialUserPrompt is the very first user turn: it explains the task,
// renders a shallow outline (default level=1) so the model has
// something to react to, and reminds the model of the action protocol.
func (a *AgenticStrategy) initialUserPrompt(view tree.View, query string) string {
	var b strings.Builder
	if view.Title != "" {
		b.WriteString("Document: ")
		b.WriteString(view.Title)
		b.WriteString("\n\n")
	}
	b.WriteString("Initial outline (depth=")
	fmt.Fprintf(&b, "%d", defaultOutlineLevel)
	b.WriteString("):\n")
	b.WriteString(renderOutline(view, defaultOutlineLevel))
	b.WriteString("\nUser query:\n")
	b.WriteString(query)
	b.WriteString("\n\nReply with a JSON action. The actions you may use are:\n")
	b.WriteString(actionProtocolHelp)
	return b.String()
}

// renderRead pulls a section's full content via the Fetcher. Returns
// (text, true) on success, or ("", false) when the section is unknown
// or has no ContentRef / no fetcher. Failures from the storage backend
// (e.g. transient network error) are returned to the model as the
// observation so it can recover with a different action.
func (a *AgenticStrategy) renderRead(ctx context.Context, t *tree.Tree, id tree.SectionID) (string, bool) {
	sec := t.FindByID(id)
	if sec == nil {
		return "", false
	}
	if a.Fetcher == nil || sec.ContentRef == "" {
		// Internal sections summarize their children — fall back to
		// the summary so the model still gets useful signal.
		if sec.Summary != "" {
			return fmt.Sprintf("section %s (%s) has no body content; summary:\n%s", sec.ID, sec.Title, sec.Summary), true
		}
		return "", false
	}
	data, err := a.Fetcher.Get(ctx, sec.ContentRef)
	if err != nil {
		return fmt.Sprintf("error reading section %s: %v", sec.ID, err), true
	}
	body := string(data)
	body = strings.TrimSpace(body)
	if body == "" && sec.Summary != "" {
		return fmt.Sprintf("section %s body was empty; summary:\n%s", sec.ID, sec.Summary), true
	}
	return fmt.Sprintf("section %s (%s):\n%s", sec.ID, sec.Title, body), true
}

// agenticSystemPrompt instructs the model on the navigation loop. It
// mirrors the language of the existing selection prompt so behaviour
// across strategies stays consistent: prefer leaves, pick few, never
// invent IDs.
const agenticSystemPrompt = `You are a navigation agent for a document tree. You explore a hierarchical outline of titles + short summaries + stable section IDs, then pick the leaf section IDs whose full content most directly answers the user's query.

Process:
- On each turn, reply with EXACTLY one JSON object describing the next action.
- Use 'outline' to refresh your view of the whole tree at a given depth.
- Use 'expand' to see a section's immediate children.
- Use 'read' to read a section's full body (use sparingly — bodies are large).
- Use 'done' to terminate with your final picks.

Rules:
- Prefer leaf sections. Include a parent only if its own body is directly relevant.
- Include as few sections as possible. Quality over quantity.
- Only return IDs you have seen in a prior observation. Do not invent IDs.
- If nothing in the document is relevant, return done with an empty picked_ids array.`

// actionProtocolHelp is the one-shot reminder appended to the initial
// user prompt so the model gets concrete examples of valid actions
// without us needing to maintain a separate few-shot block.
const actionProtocolHelp = `- {"action":"outline","level":2} — re-render the outline N levels deep
- {"action":"expand","section_id":"sec_x"} — list immediate children of sec_x
- {"action":"read","section_id":"sec_x"} — fetch the full body of sec_x
- {"action":"done","picked_ids":["sec_x","sec_y"],"reasoning":"why"} — finalize

Reply with ONLY the JSON object. No prose, no markdown fences.`

// Action describes the LLM-chosen next step in the agentic loop.
//
// The struct is exported so tests can construct expected actions
// without depending on internal JSON shapes. SectionID is a string
// rather than tree.SectionID because the model's value may not match
// any real section in the tree; we keep the raw input here and let the
// loop attribute "unknown section" errors back to the model.
type Action struct {
	// Action is the dispatch tag. One of: outline, expand, read, done.
	Action string `json:"action"`

	// Level is the depth requested by an outline action. Optional;
	// defaults to defaultOutlineLevel when zero/negative.
	Level int `json:"level,omitempty"`

	// SectionID is the target of expand and read actions.
	SectionID string `json:"section_id,omitempty"`

	// PickedIDs is the final selection for a done action.
	PickedIDs []string `json:"picked_ids,omitempty"`

	// Reasoning is an optional explanation accompanying done.
	Reasoning string `json:"reasoning,omitempty"`
}

// Action tag constants. Defined as untyped strings rather than a
// custom Go enum because the value lives in JSON and must round-trip
// without surprise.
const (
	actionOutline = "outline"
	actionExpand  = "expand"
	actionRead    = "read"
	actionDone    = "done"
)

// ParseAction is the tolerant JSON decoder for the agentic protocol.
// It mirrors ParseSelection: it strips code fences, peels prose
// wrappers, and isolates the first balanced JSON object. Returns an
// error only when no JSON object can be recovered.
func ParseAction(raw string) (Action, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Action{}, fmt.Errorf("empty response")
	}
	// Strip ```json ... ``` fences if present.
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

	var a Action
	dec := json.NewDecoder(strings.NewReader(raw))
	if err := dec.Decode(&a); err != nil && err != io.EOF {
		return Action{}, fmt.Errorf("decode action: %w", err)
	}
	a.Action = strings.ToLower(strings.TrimSpace(a.Action))
	if a.Action == "" {
		return Action{}, fmt.Errorf("missing 'action' field")
	}
	a.SectionID = strings.TrimSpace(a.SectionID)
	return a, nil
}

// renderOutline renders the section view down to the given depth.
// depth=1 shows the root and its immediate children; depth=2 also
// shows grandchildren; etc. The format is the same as the
// chunked-tree prompt so the model sees consistent structure across
// strategies.
func renderOutline(view tree.View, depth int) string {
	if depth <= 0 {
		depth = defaultOutlineLevel
	}
	var b strings.Builder
	for _, sv := range view.Sections {
		if sv.Depth > depth {
			continue
		}
		writeSectionLine(&b, sv)
	}
	return b.String()
}

// renderExpand returns a string with the immediate children of
// sectionID rendered as outline lines. Returns ("", false) when the
// ID is not in the tree. The fallback when a section has no children
// is to render the section itself with a note — that lets the model
// distinguish "unknown id" from "leaf, nothing to expand".
func renderExpand(bySectionID map[tree.SectionID]tree.SectionView, sectionID string) (string, bool) {
	sv, ok := bySectionID[tree.SectionID(sectionID)]
	if !ok {
		return "", false
	}
	if len(sv.Children) == 0 {
		return fmt.Sprintf("[%s] %s is a leaf (no children). Use read to fetch its body.", sv.ID, sv.Title), true
	}
	var b strings.Builder
	for _, cid := range sv.Children {
		child, ok := bySectionID[cid]
		if !ok {
			continue
		}
		writeSectionLine(&b, child)
	}
	return b.String(), true
}

// wrapObservation formats an action's result so the model can clearly
// see which action produced which observation.
func wrapObservation(action, body string) string {
	return fmt.Sprintf("Observation (%s):\n%s\n\nNext JSON action?", action, body)
}

// indexSections returns a flat map from SectionID to SectionView for
// O(1) lookup during the loop. The map is read-only after construction
// and so is safe for concurrent use — but each Select call builds its
// own anyway because tree views are cheap.
func indexSections(sections []tree.SectionView) map[tree.SectionID]tree.SectionView {
	out := make(map[tree.SectionID]tree.SectionView, len(sections))
	for _, sv := range sections {
		out[sv.ID] = sv
	}
	return out
}

// filterToTreeIDs drops IDs the model invented (those not in the
// tree's section index) and deduplicates. Preserves first-seen order.
func filterToTreeIDs(rawIDs []string, bySectionID map[tree.SectionID]tree.SectionView) []tree.SectionID {
	seen := map[tree.SectionID]struct{}{}
	out := make([]tree.SectionID, 0, len(rawIDs))
	for _, id := range rawIDs {
		sid := tree.SectionID(strings.TrimSpace(id))
		if sid == "" {
			continue
		}
		if _, ok := bySectionID[sid]; !ok {
			continue
		}
		if _, dup := seen[sid]; dup {
			continue
		}
		seen[sid] = struct{}{}
		out = append(out, sid)
	}
	return out
}
