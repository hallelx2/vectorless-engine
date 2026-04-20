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
	results := make([][]tree.SectionID, len(slices))

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

			ids, err := c.reasonOverSlice(gctx, sl, query, budget)
			if err != nil {
				return err
			}
			mu.Lock()
			results[i] = ids
			mu.Unlock()
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}
	return c.Merge.Merge(results), nil
}

// reasonOverSlice runs one LLM call for one slice and returns the IDs the
// model picked, filtered against sl.Sections so a model can never fabricate
// an ID that lives in a different slice.
func (c *ChunkedTree) reasonOverSlice(ctx context.Context, sl Slice, query string, budget ContextBudget) ([]tree.SectionID, error) {
	prompt := BuildSelectionPrompt(sl.Breadcrumb, sl.Sections, sl.SiblingSummaries, query)

	resp, err := c.LLM.Complete(ctx, llmgate.Request{
		Model: budget.ModelName,
		Messages: []llmgate.Message{
			{Role: llmgate.RoleSystem, Content: selectionSystemPrompt},
			{Role: llmgate.RoleUser, Content: prompt},
		},
		MaxTokens:   2048,
		Temperature: 0,
		JSONMode:    true,
		JSONSchema:  []byte(selectionJSONSchema),
	})
	if err != nil {
		return nil, err
	}
	ids, err := ParseSelection(resp.Content)
	if err != nil {
		return nil, err
	}
	return FilterKnownIDs(ids, sl.Sections), nil
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
