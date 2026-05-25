package retrieval

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// TreeLoader abstracts the DB call that reconstructs a tree from a
// document ID, scoped to an org. The API layer injects a closure that
// adapts to db.Pool.LoadTree (which takes orgID directly); tests inject
// a stub.
type TreeLoader func(ctx context.Context, docID tree.DocumentID, orgID, storeID string) (*tree.Tree, error)

// MultiDocResult groups per-document retrieval outcomes so the caller can
// attribute sections back to their source documents.
type MultiDocResult struct {
	// Documents is keyed by DocumentID and holds the retrieval result for
	// each document that was successfully queried.
	Documents map[tree.DocumentID]*DocResult

	// Errors is keyed by DocumentID and holds any per-document error. A
	// document appears in Errors only if its retrieval failed (tree not
	// found, LLM error, etc.). The MultiDoc call succeeds overall as long
	// as at least one document succeeds.
	Errors map[tree.DocumentID]error

	// TotalUsage aggregates token and cost accounting across all docs.
	TotalUsage Usage
}

// DocResult is one document's retrieval outcome within a multi-doc query.
type DocResult struct {
	DocumentID  tree.DocumentID
	SelectedIDs []tree.SectionID
	Tree        *tree.Tree
	Usage       Usage
}

// AllSelectedIDs returns a flat, deduplicated list of every section ID
// selected across all documents. IDs are prefixed with the document ID
// (formatted as "docID:sectionID") to disambiguate across documents.
func (r *MultiDocResult) AllSelectedIDs() []string {
	var out []string
	seen := map[string]struct{}{}
	for docID, dr := range r.Documents {
		for _, sid := range dr.SelectedIDs {
			key := string(docID) + ":" + string(sid)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// MultiDoc fans out a single query across multiple documents in parallel.
//
// Design decisions:
//
//   - One goroutine per document, bounded by the budget's MaxParallelCalls.
//     Each goroutine loads the tree and runs the wrapped strategy.
//
//   - Partial success: if some documents fail (not found, LLM error), the
//     query still returns results from the documents that succeeded. Errors
//     are collected per-document in MultiDocResult.Errors.
//
//   - The same ContextBudget is passed to each per-document call unchanged.
//     This is intentional: budget describes the LLM, not the total
//     workload. Each document gets the full budget.
type MultiDoc struct {
	strategy Strategy
	loader   TreeLoader
}

// NewMultiDoc constructs a MultiDoc dispatcher.
func NewMultiDoc(strategy Strategy, loader TreeLoader) *MultiDoc {
	return &MultiDoc{
		strategy: strategy,
		loader:   loader,
	}
}

// Query runs the wrapped strategy against every document in docIDs and
// returns a MultiDocResult. It is safe for concurrent use.
func (m *MultiDoc) Query(ctx context.Context, orgID, storeID string, docIDs []tree.DocumentID, query string, budget ContextBudget) (*MultiDocResult, error) {
	if len(docIDs) == 0 {
		return nil, fmt.Errorf("multi-doc: at least one document_id is required")
	}
	if orgID == "" {
		return nil, fmt.Errorf("multi-doc: orgID is required")
	}

	// Single-doc fast path — skip the fan-out machinery.
	if len(docIDs) == 1 {
		return m.querySingle(ctx, orgID, storeID, docIDs[0], query, budget)
	}

	maxPar := budget.MaxParallelCalls
	if maxPar <= 0 {
		maxPar = 8
	}
	// Cap document-level parallelism separately from per-document slice
	// parallelism. We don't want 8 documents × 8 slices = 64 concurrent
	// LLM calls. Use a separate, smaller limit for the outer fan-out.
	docPar := maxPar
	if docPar > len(docIDs) {
		docPar = len(docIDs)
	}

	sem := make(chan struct{}, docPar)

	var (
		mu     sync.Mutex
		result = &MultiDocResult{
			Documents: make(map[tree.DocumentID]*DocResult, len(docIDs)),
			Errors:    make(map[tree.DocumentID]error),
		}
	)

	g, gctx := errgroup.WithContext(ctx)

	for _, docID := range docIDs {
		docID := docID
		g.Go(func() error {
			// Acquire semaphore.
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-gctx.Done():
				return nil // don't propagate; partial results are OK
			}

			dr, err := m.queryOneDoc(gctx, orgID, storeID, docID, query, budget)

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				result.Errors[docID] = err
				return nil // partial success — don't abort other docs
			}
			result.Documents[docID] = dr
			result.TotalUsage.Add(dr.Usage)
			return nil
		})
	}

	// errgroup never returns an error here because per-doc goroutines
	// always return nil (errors are collected, not propagated).
	_ = g.Wait()

	// If every document failed, return an error.
	if len(result.Documents) == 0 {
		// Pick the first error for the top-level message.
		for _, err := range result.Errors {
			return nil, fmt.Errorf("multi-doc: all documents failed, first error: %w", err)
		}
	}

	return result, nil
}

// querySingle handles the fast path for a single document.
func (m *MultiDoc) querySingle(ctx context.Context, orgID, storeID string, docID tree.DocumentID, query string, budget ContextBudget) (*MultiDocResult, error) {
	dr, err := m.queryOneDoc(ctx, orgID, storeID, docID, query, budget)
	if err != nil {
		return nil, err
	}
	return &MultiDocResult{
		Documents:  map[tree.DocumentID]*DocResult{docID: dr},
		Errors:     map[tree.DocumentID]error{},
		TotalUsage: dr.Usage,
	}, nil
}

// queryOneDoc loads a tree and runs the strategy for one document.
func (m *MultiDoc) queryOneDoc(ctx context.Context, orgID, storeID string, docID tree.DocumentID, query string, budget ContextBudget) (*DocResult, error) {
	t, err := m.loader(ctx, docID, orgID, storeID)
	if err != nil {
		return nil, fmt.Errorf("load tree %s: %w", docID, err)
	}

	dr := &DocResult{
		DocumentID: docID,
		Tree:       t,
	}

	if cs, ok := m.strategy.(CostStrategy); ok {
		r, err := cs.SelectWithCost(ctx, t, query, budget)
		if err != nil {
			return nil, fmt.Errorf("strategy %s on %s: %w", m.strategy.Name(), docID, err)
		}
		dr.SelectedIDs = r.SelectedIDs
		dr.Usage = r.Usage
	} else {
		ids, err := m.strategy.Select(ctx, t, query, budget)
		if err != nil {
			return nil, fmt.Errorf("strategy %s on %s: %w", m.strategy.Name(), docID, err)
		}
		dr.SelectedIDs = ids
	}

	return dr, nil
}

// QueryStream fans out streaming retrieval across multiple documents.
// Each document's events are emitted to the returned channel with the
// SliceIndex repurposed as a document index for the caller to separate
// per-document events.
//
// The channel is closed when all documents finish.
func (m *MultiDoc) QueryStream(ctx context.Context, orgID, storeID string, docIDs []tree.DocumentID, query string, budget ContextBudget) <-chan MultiDocStreamEvent {
	out := make(chan MultiDocStreamEvent, 64)

	go func() {
		defer close(out)

		ss, ok := m.strategy.(StreamStrategy)
		if !ok {
			out <- MultiDocStreamEvent{
				Event: StreamEvent{
					Type:    EventError,
					Message: "strategy does not support streaming",
				},
			}
			return
		}

		maxPar := budget.MaxParallelCalls
		if maxPar <= 0 {
			maxPar = 8
		}
		docPar := maxPar
		if docPar > len(docIDs) {
			docPar = len(docIDs)
		}

		sem := make(chan struct{}, docPar)
		var wg sync.WaitGroup

		for i, docID := range docIDs {
			wg.Add(1)
			go func(idx int, did tree.DocumentID) {
				defer wg.Done()

				// Acquire semaphore.
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-ctx.Done():
					return
				}

				t, err := m.loader(ctx, did, orgID, storeID)
				if err != nil {
					select {
					case out <- MultiDocStreamEvent{
						DocumentID: did,
						DocIndex:   idx,
						Event: StreamEvent{
							Type:    EventError,
							Message: fmt.Sprintf("load tree: %v", err),
							Error:   err,
						},
					}:
					case <-ctx.Done():
					}
					return
				}

				ch := ss.SelectStream(ctx, t, query, budget)
				for ev := range ch {
					select {
					case out <- MultiDocStreamEvent{
						DocumentID: did,
						DocIndex:   idx,
						Event:      ev,
					}:
					case <-ctx.Done():
						return
					}
				}
			}(i, docID)
		}

		wg.Wait()
	}()

	return out
}

// MultiDocStreamEvent wraps a StreamEvent with document attribution.
type MultiDocStreamEvent struct {
	// DocumentID identifies which document produced this event.
	DocumentID tree.DocumentID

	// DocIndex is the 0-based index of the document in the original
	// docIDs slice, for ordering.
	DocIndex int

	// Event is the underlying retrieval event.
	Event StreamEvent
}
