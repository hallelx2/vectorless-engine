package tree

// Compact produces a reduced copy of the tree by pruning sections whose
// token count is below a threshold and merging single-child chains.
//
// Compaction is useful for large documents where the tree view itself is
// too big for the LLM context window. By removing very small sections
// (which rarely contain useful answers on their own) and collapsing
// pass-through nodes, the tree view shrinks without losing structural
// signal.
//
// Compact does NOT modify the original tree — it returns a new tree with
// copied sections. ContentRefs, Metadata, and other fields are preserved
// on surviving nodes.
type CompactOpts struct {
	// MinTokens is the minimum token count for a leaf section to survive.
	// Leaves below this threshold are pruned. Set to 0 to skip pruning.
	// Default: 50.
	MinTokens int

	// MergeSingleChild controls whether internal nodes with exactly one
	// child are collapsed into the child. The parent's title is prepended
	// to the child's title with " > " separator. Default: true.
	MergeSingleChild bool

	// MaxDepth limits the tree depth. Sections deeper than MaxDepth are
	// promoted to their closest surviving ancestor. 0 means no limit.
	MaxDepth int
}

// Compact returns a compacted copy of the tree.
func (t *Tree) Compact(opts CompactOpts) *Tree {
	if t == nil || t.Root == nil {
		return t
	}
	if opts.MinTokens == 0 {
		opts.MinTokens = 50
	}

	newRoot := compactSection(t.Root, opts, 0)
	if newRoot == nil {
		// Everything was pruned — keep at least the root shell.
		newRoot = &Section{
			ID:    t.Root.ID,
			Title: t.Root.Title,
		}
	}

	if opts.MergeSingleChild {
		newRoot = mergeSingleChildren(newRoot)
	}

	return &Tree{
		DocumentID: t.DocumentID,
		Title:      t.Title,
		Root:       newRoot,
		CreatedAt:  t.CreatedAt,
		UpdatedAt:  t.UpdatedAt,
	}
}

// compactSection recursively copies a section, pruning small leaves.
func compactSection(s *Section, opts CompactOpts, depth int) *Section {
	if s == nil {
		return nil
	}

	// Depth limit: if this node's children would exceed MaxDepth,
	// absorb all descendant tokens into this node and emit it as a leaf.
	// MaxDepth=2 means depths 0 and 1 survive; depth 2+ is flattened.
	shouldFlatten := opts.MaxDepth > 0 && len(s.Children) > 0 && (depth+1) >= opts.MaxDepth

	var children []*Section
	if shouldFlatten {
		// Absorb all descendant tokens into this node.
		totalTokens := s.TokenCount
		for _, c := range s.Children {
			c.Walk(func(desc *Section) bool {
				totalTokens += desc.TokenCount
				return true
			})
		}
		return &Section{
			ID:         s.ID,
			ParentID:   s.ParentID,
			Ordinal:    s.Ordinal,
			Title:      s.Title,
			Summary:    s.Summary,
			ContentRef: s.ContentRef,
			TokenCount: totalTokens,
			Metadata:   s.Metadata,
			// No children — flattened.
		}
	}

	// Recurse into children normally.
	for _, c := range s.Children {
		compacted := compactSection(c, opts, depth+1)
		if compacted != nil {
			children = append(children, compacted)
		}
	}

	// Prune leaf sections below token threshold.
	if len(children) == 0 && len(s.Children) == 0 {
		// Originally a leaf.
		if s.TokenCount < opts.MinTokens && s.ID != "" {
			return nil // pruned
		}
	}

	return &Section{
		ID:         s.ID,
		ParentID:   s.ParentID,
		Ordinal:    s.Ordinal,
		Title:      s.Title,
		Summary:    s.Summary,
		ContentRef: s.ContentRef,
		TokenCount: s.TokenCount,
		Metadata:   s.Metadata,
		Children:   children,
	}
}

// mergeSingleChildren collapses single-child chains. If a parent has
// exactly one child, the child absorbs the parent's title prefix.
func mergeSingleChildren(s *Section) *Section {
	if s == nil {
		return nil
	}

	// Recurse first.
	for i, c := range s.Children {
		s.Children[i] = mergeSingleChildren(c)
	}

	// Merge if single child.
	if len(s.Children) == 1 {
		child := s.Children[0]
		merged := &Section{
			ID:         child.ID,
			ParentID:   s.ParentID,
			Ordinal:    s.Ordinal,
			Title:      s.Title + " > " + child.Title,
			Summary:    child.Summary,
			ContentRef: child.ContentRef,
			TokenCount: child.TokenCount,
			Metadata:   child.Metadata,
			Children:   child.Children,
		}
		// If parent had content too, sum tokens.
		if s.TokenCount > 0 {
			merged.TokenCount += s.TokenCount
		}
		return merged
	}

	return s
}

// SectionCount returns the total number of sections in the tree.
func (t *Tree) SectionCount() int {
	if t == nil || t.Root == nil {
		return 0
	}
	count := 0
	t.Root.Walk(func(s *Section) bool {
		count++
		return true
	})
	return count
}

// TotalTokens returns the sum of all section token counts.
func (t *Tree) TotalTokens() int {
	if t == nil || t.Root == nil {
		return 0
	}
	total := 0
	t.Root.Walk(func(s *Section) bool {
		total += s.TokenCount
		return true
	})
	return total
}
