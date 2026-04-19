package retrieval

import (
	"context"
	"fmt"
	"strings"

	"github.com/hallelx2/llmgate"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// Slice is a portion of a tree view small enough to fit in one LLM call.
//
// Every Slice carries a Breadcrumb that tells the model where in the full
// document this slice lives — essential when the full tree has been split
// across multiple parallel reasoning calls.
type Slice struct {
	// Breadcrumb is a short, human-readable path into the document, e.g.
	//   "Document: Annual Report 2025 → Part II → (sections II.3–II.7 of 12)"
	// The model uses this to orient itself relative to unseen siblings.
	Breadcrumb string

	// Sections is the compact section view included in this slice.
	Sections []tree.SectionView

	// SiblingSummaries optionally includes short summaries of sibling
	// subtrees not fully expanded in this slice — useful as "peripheral
	// vision" for the model's relevance judgments.
	SiblingSummaries []tree.SectionView
}

// Splitter breaks a Tree view into slices that each fit a ContextBudget.
type Splitter interface {
	Split(ctx context.Context, t *tree.Tree, budget ContextBudget, tokenizer Tokenizer) ([]Slice, error)
}

// Tokenizer counts tokens for a given string under the target model.
//
// Strategies accept an llmgate.Client and wrap it in a Tokenizer so the splitter
// does not need to know about LLM providers directly.
type Tokenizer interface {
	Count(ctx context.Context, text string) (int, error)
}

// LLMTokenizer adapts an llmgate.Client to the Tokenizer interface.
type LLMTokenizer struct{ C llmgate.Client }

func (t LLMTokenizer) Count(ctx context.Context, s string) (int, error) {
	return t.C.CountTokens(ctx, s)
}

// DefaultSplitter is a structure-aware splitter that respects the tree's
// hierarchy. It tries to keep sibling groups together and preserves a
// breadcrumb path into each slice so the model has orientation.
//
// Algorithm:
//
//  1. If the full tree view fits the budget, return one slice.
//  2. Otherwise, walk the root's children. Greedily bin-pack sibling subtrees
//     into slices that fit the budget. Each child's subtree is fully included
//     when it fits whole.
//  3. If a single child's subtree is itself larger than the budget, recurse
//     into that subtree with the same algorithm and prepend the parent
//     breadcrumb.
//
// The splitter also enriches each slice with sibling summaries (shallow
// titles + summaries of sibling subtrees not included in full) so the model
// has peripheral context.
type DefaultSplitter struct {
	// IncludeSiblingSummaries, when true, attaches 1-line summaries of
	// sibling subtrees to each slice. Default true.
	IncludeSiblingSummaries bool
}

// NewDefaultSplitter returns the structure-aware splitter used by the
// chunked-tree strategy.
func NewDefaultSplitter() *DefaultSplitter {
	return &DefaultSplitter{IncludeSiblingSummaries: true}
}

// Split implements Splitter.
func (d *DefaultSplitter) Split(ctx context.Context, t *tree.Tree, budget ContextBudget, tok Tokenizer) ([]Slice, error) {
	if t == nil || t.Root == nil {
		return nil, nil
	}
	avail := budget.Available()
	if avail <= 0 {
		avail = budget.MaxTokens
	}
	if avail <= 0 {
		// No budget info — fall back to a single slice. Lets callers run
		// ChunkedTree against tiny test trees without configuring the budget.
		view := t.BuildView()
		return []Slice{{
			Breadcrumb: "Document: " + view.Title,
			Sections:   view.Sections,
		}}, nil
	}

	// Fast path: whole tree fits.
	full := flatten(t.Root, 0)
	fullCost, err := measure(ctx, tok, full)
	if err != nil {
		return nil, err
	}
	if fullCost <= avail {
		return []Slice{{
			Breadcrumb: "Document: " + t.Title,
			Sections:   full,
		}}, nil
	}

	// Recurse.
	breadcrumb := "Document: " + t.Title
	return d.splitNode(ctx, t.Root, breadcrumb, 0, avail, tok)
}

// splitNode bin-packs the children of node into slices fitting avail tokens.
// baseDepth is the depth of node in the original tree (used so rendered
// section lines keep coherent indentation inside a slice).
func (d *DefaultSplitter) splitNode(ctx context.Context, node *tree.Section, breadcrumb string, baseDepth, avail int, tok Tokenizer) ([]Slice, error) {
	var out []Slice

	// Siblings context we'll include on every slice coming from this level.
	var siblingCtx []tree.SectionView
	if d.IncludeSiblingSummaries {
		siblingCtx = shallowSiblings(node, baseDepth)
	}

	// Try greedy bin-packing.
	var cur []tree.SectionView
	curCost := 0

	flushCurrent := func() {
		if len(cur) == 0 {
			return
		}
		sl := Slice{
			Breadcrumb:       breadcrumb,
			Sections:         cur,
			SiblingSummaries: siblingCtx,
		}
		out = append(out, sl)
		cur = nil
		curCost = 0
	}

	// Always include a header for `node` itself so the model has the parent
	// title in-slice. Flatten starting at depth baseDepth.
	header := flatten(node, baseDepth)
	if len(header) > 0 {
		// just the self-view (children handled per-child below)
		selfView := header[0]
		selfView.Children = nil
		selfCost, err := measureOne(ctx, tok, selfView)
		if err != nil {
			return nil, err
		}
		cur = append(cur, selfView)
		curCost += selfCost
	}

	for _, child := range node.Children {
		subFlat := flatten(child, baseDepth+1)
		cost, err := measure(ctx, tok, subFlat)
		if err != nil {
			return nil, err
		}

		// Single child bigger than budget → recurse.
		if cost > avail {
			flushCurrent()
			childBread := breadcrumb
			if node.Title != "" {
				childBread = breadcrumb + " → " + node.Title
			}
			sub, err := d.splitNode(ctx, child, childBread, baseDepth+1, avail, tok)
			if err != nil {
				return nil, err
			}
			out = append(out, sub...)
			continue
		}

		// Fits whole. Bin-pack.
		if curCost+cost > avail && len(cur) > 0 {
			flushCurrent()
		}
		cur = append(cur, subFlat...)
		curCost += cost
	}
	flushCurrent()

	// If after all that we ended up with zero slices (e.g. single root with
	// no content), fall back to one slice of the header.
	if len(out) == 0 {
		out = append(out, Slice{
			Breadcrumb:       breadcrumb,
			Sections:         header,
			SiblingSummaries: siblingCtx,
		})
	}

	// Decorate breadcrumbs with a "(slice i of N)" suffix when we split.
	if len(out) > 1 {
		for i := range out {
			out[i].Breadcrumb = fmt.Sprintf("%s (slice %d of %d)", out[i].Breadcrumb, i+1, len(out))
		}
	}
	return out, nil
}

// flatten renders a section subtree as a depth-first list of SectionView
// records. The root section's depth is startDepth.
func flatten(s *tree.Section, startDepth int) []tree.SectionView {
	if s == nil {
		return nil
	}
	var out []tree.SectionView
	var walk func(s *tree.Section, depth int)
	walk = func(s *tree.Section, depth int) {
		sv := tree.SectionView{
			ID:       s.ID,
			ParentID: s.ParentID,
			Depth:    depth,
			Title:    s.Title,
			Summary:  s.Summary,
			Tokens:   s.TokenCount,
		}
		for _, c := range s.Children {
			sv.Children = append(sv.Children, c.ID)
		}
		out = append(out, sv)
		for _, c := range s.Children {
			walk(c, depth+1)
		}
	}
	walk(s, startDepth)
	return out
}

// shallowSiblings returns 1-line views for each child of node, representing
// the subtrees at this level without expanding them. Used as peripheral
// context attached to each slice.
func shallowSiblings(node *tree.Section, baseDepth int) []tree.SectionView {
	if node == nil || len(node.Children) == 0 {
		return nil
	}
	out := make([]tree.SectionView, 0, len(node.Children))
	for _, c := range node.Children {
		out = append(out, tree.SectionView{
			ID:      c.ID,
			Depth:   baseDepth + 1,
			Title:   c.Title,
			Summary: c.Summary,
		})
	}
	return out
}

// measure sums the tokenizer cost of a slice of section views rendered as
// outline lines.
func measure(ctx context.Context, tok Tokenizer, svs []tree.SectionView) (int, error) {
	if tok == nil {
		return renderCostFallback(svs), nil
	}
	var b strings.Builder
	for _, sv := range svs {
		writeSectionLine(&b, sv)
	}
	return tok.Count(ctx, b.String())
}

func measureOne(ctx context.Context, tok Tokenizer, sv tree.SectionView) (int, error) {
	return measure(ctx, tok, []tree.SectionView{sv})
}

// renderCostFallback estimates tokens as ~4 chars/token when no tokenizer
// is available (mostly for tests).
func renderCostFallback(svs []tree.SectionView) int {
	n := 0
	for _, sv := range svs {
		n += len(sv.Title) + len(sv.Summary) + len(sv.ID) + 8
	}
	return n / 4
}
