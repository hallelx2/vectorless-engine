package retrieval

import (
	"context"
	"sort"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// ChunkedTree is a Strategy that scales tree reasoning to documents whose
// full tree view exceeds a single model's context budget.
//
// Pipeline:
//
//	Split(tree, budget)                      →  []Slice
//	for each slice in parallel: LLM.Select   →  []SectionID
//	Merge(results, policy)                   →  []SectionID
//
// The strategy therefore works with any LLM (large or small context) by
// trading context size for parallelism + merge.
type ChunkedTree struct {
	LLM      llmgate.Client
	Splitter Splitter

	// Merge determines how per-slice ID lists are combined. Defaults to
	// UnionMerge when nil: any ID picked by any slice is included.
	Merge MergePolicy
}

// NewChunkedTree constructs a ChunkedTree strategy with sensible defaults.
func NewChunkedTree(client llmgate.Client) *ChunkedTree {
	return &ChunkedTree{
		LLM:      client,
		Splitter: NewDefaultSplitter(),
		Merge:    UnionMerge{},
	}
}

// Name implements Strategy.
func (c *ChunkedTree) Name() string { return "chunked-tree" }

// Select implements Strategy.
func (c *ChunkedTree) Select(ctx context.Context, t *tree.Tree, query string, budget ContextBudget) ([]tree.SectionID, error) {
	r, err := c.SelectWithCost(ctx, t, query, budget)
	if err != nil {
		return nil, err
	}
	return r.SelectedIDs, nil
}

// SelectWithCost implements CostStrategy.
func (c *ChunkedTree) SelectWithCost(ctx context.Context, t *tree.Tree, query string, budget ContextBudget) (*Result, error) {
	if t == nil || t.Root == nil {
		return &Result{}, nil
	}
	tok := LLMTokenizer{C: c.LLM}
	slices, err := c.Splitter.Split(ctx, t, budget, tok)
	if err != nil {
		return nil, err
	}

	maxPar := budget.MaxParallelCalls
	if maxPar <= 0 {
		maxPar = 8
	}

	sem := make(chan struct{}, maxPar)
	type sliceResult struct {
		ids         []tree.SectionID
		confidences map[tree.SectionID]float64
		usage       Usage
	}
	results := make([]sliceResult, len(slices))

	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)

	for i, sl := range slices {
		i, sl := i, sl
		g.Go(func() error {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-gctx.Done():
				return gctx.Err()
			}

			ids, confidences, usage, err := c.reasonOverSliceWithCost(gctx, sl, query, budget)
			if err != nil {
				return err
			}
			mu.Lock()
			results[i] = sliceResult{ids: ids, confidences: confidences, usage: usage}
			mu.Unlock()
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Merge IDs and aggregate costs.
	allIDs := make([][]tree.SectionID, len(results))
	var totalUsage Usage
	// Union the per-slice confidence maps. When two slices both score
	// the same ID (rare but possible if the splitter overlaps), we
	// keep the higher confidence — the more confident slice has
	// better signal about that section.
	var mergedConfidences map[tree.SectionID]float64
	for i, r := range results {
		allIDs[i] = r.ids
		totalUsage.Add(r.usage)
		if len(r.confidences) > 0 {
			if mergedConfidences == nil {
				mergedConfidences = make(map[tree.SectionID]float64, len(r.confidences))
			}
			for id, conf := range r.confidences {
				if existing, ok := mergedConfidences[id]; !ok || conf > existing {
					mergedConfidences[id] = conf
				}
			}
		}
	}

	selected := c.Merge.Merge(allIDs)
	return &Result{
		SelectedIDs: selected,
		Confidences: filterConfidences(mergedConfidences, selected),
		Usage:       totalUsage,
		HopsTaken:   1,
		TraceToken:  ComputeTraceToken(t.DocumentID, traceDocVersionV1, budget.ModelName, selected),
	}, nil
}

// reasonOverSlice runs one LLM call for one slice and returns the IDs the
// model picked, filtered against sl.Sections so a model can never fabricate
// an ID that lives in a different slice.
func (c *ChunkedTree) reasonOverSlice(ctx context.Context, sl Slice, query string, budget ContextBudget) ([]tree.SectionID, error) {
	ids, _, _, err := c.reasonOverSliceWithCost(ctx, sl, query, budget)
	return ids, err
}

// reasonOverSliceWithCost is like reasonOverSlice but also returns the
// per-pick confidence map (nil when the model returned the legacy
// response shape) and the usage spent on the call.
func (c *ChunkedTree) reasonOverSliceWithCost(ctx context.Context, sl Slice, query string, budget ContextBudget) ([]tree.SectionID, map[tree.SectionID]float64, Usage, error) {
	prompt := BuildSelectionPrompt(sl.Breadcrumb, sl.Sections, sl.SiblingSummaries, query)

	req := llmgate.Request{
		Model: budget.ModelName,
		Messages: []llmgate.Message{
			{Role: llmgate.RoleSystem, Content: selectionSystemPrompt},
			{Role: llmgate.RoleUser, Content: prompt},
		},
		MaxTokens:   2048,
		Temperature: 0,
		JSONMode:    true,
		JSONSchema:  []byte(selectionJSONSchema),
	}

	ids, confidences, usage, err := runSelectionWithRetry(ctx, c.LLM, req, defaultSelectionRetries)
	if err != nil {
		return nil, nil, usage, err
	}
	filtered := FilterKnownIDs(ids, sl.Sections)
	return filtered, filterConfidences(confidences, filtered), usage, nil
}

// MergePolicy determines how per-slice ID lists are combined into a single
// final list.
type MergePolicy interface {
	Merge(perSlice [][]tree.SectionID) []tree.SectionID
}

// UnionMerge is the default: any ID selected by any slice is included.
// IDs are deduplicated and returned in a stable order.
type UnionMerge struct{}

// Merge implements MergePolicy.
func (UnionMerge) Merge(perSlice [][]tree.SectionID) []tree.SectionID {
	seen := map[tree.SectionID]struct{}{}
	var out []tree.SectionID
	for _, ids := range perSlice {
		for _, id := range ids {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
