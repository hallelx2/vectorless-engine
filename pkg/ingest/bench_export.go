package ingest

import (
	"context"

	"github.com/hallelx2/vectorless-engine/pkg/parser"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// bench_export.go exposes the two Judge-backed phases, and the page
// assembly they consume, so cmd/jevbench can time them in isolation.
//
// They are separate from Build because the point of the measurement is
// to time ONE phase against the generative path it replaces. Timing the
// whole pipeline would fold in parsing, extraction and persistence and
// tell you much less.
//
// Exported here rather than in toc_builder.go to keep the public surface
// honest about why these exist: they are a measurement seam, not an API
// anyone should build on.

// BenchAssemblePages turns parsed sections into the per-page text the
// TOC builder consumes.
func BenchAssemblePages(secs []parser.Section) []PageText {
	return assemblePagesFromSections(secs)
}

// BenchDetectTOC runs the Judge-backed detection phase alone.
func BenchDetectTOC(ctx context.Context, b *TOCBuilder, pages []PageText, scan int) (found []int, usage Usage, handled bool) {
	found, handled = b.detectTOCPagesJudge(ctx, pages, scan, &usage)
	return found, usage, handled
}

// BenchVerifyTitles runs the Judge-backed verification phase alone.
func BenchVerifyTitles(ctx context.Context, b *TOCBuilder, nodes []tree.TOCNode, pages []PageText) (map[string]bool, Usage, bool) {
	var usage Usage
	v, handled := b.verifyTitlesJudge(ctx, nodes, pages, &usage)
	return v, usage, handled
}

// BenchDetectTOCGenerative runs the ORIGINAL sequential detection phase.
//
// Exported purely so a benchmark can time both arms through the same
// entry points. Without it the comparison would have to reimplement the
// generative loop, and would then be measuring the reimplementation.
func BenchDetectTOCGenerative(ctx context.Context, b *TOCBuilder, pages []PageText, scan int) ([]int, Usage) {
	var usage Usage
	found := b.detectTOCPages(ctx, pages, scan, &usage)
	return found, usage
}

// BenchDetectTOCFanout runs the speculative fan-out phase: both
// page-level questions in one request.
//
// Reported alongside the plain Judge path so the saving from batching
// across PHASES is separable from the saving from batching across pages.
// Rolling them into one number would make it impossible to tell which
// idea earned what.
func BenchDetectTOCFanout(ctx context.Context, b *TOCBuilder, pages []PageText, scan int) ([]int, Usage, bool) {
	var usage Usage
	j, handled := b.judgePagesFanout(ctx, pages, scan, &usage)
	if !handled {
		return nil, usage, false
	}
	return tocPagesFrom(j, b.judgeThreshold()), usage, true
}

// PrefilterAny exposes the skip decision for the benchmark's corpus
// validation, so the filter can be checked against every real TOC page
// before it is trusted to skip anything.
func PrefilterAny(text string) bool { return prefilterTOC(text).Any() }

// BenchResolvePages runs Judge-backed page resolution alone, over an
// already-extracted tree. Exists so the resolver can be validated on a
// real document without paying for extraction again.
func BenchResolvePages(ctx context.Context, b *TOCBuilder, nodes []tree.TOCNode, pages []PageText, exclude []int) (Usage, bool) {
	var usage Usage
	resolved, handled := b.resolvePagesJudge(ctx, nodes, pages, exclude, &usage)
	if handled {
		applyResolvedPages(nodes, resolved)
	}
	return usage, handled
}

// BenchFinalise derives end pages and stamps IDs, as the tail of Build
// does, so a resolved tree is comparable to a built one.
func BenchFinalise(nodes []tree.TOCNode, pages []PageText) {
	deriveEndPages(nodes, lastPage(pages))
	stampNodeIDs(nodes, "")
}

// BenchLikelyTOCPages returns pages the structural pre-filter rates as
// contents pages, for a caller that has a tree but did not run detection.
// The bar is high on purpose: a wrongly-excluded body page loses one
// candidate; a wrongly-included contents page wins every question.
func BenchLikelyTOCPages(pages []PageText) []int {
	var out []int
	for _, p := range pages {
		if s := prefilterTOC(p.Text); s.Keyword && (s.Items >= 3 || s.Entries >= 5) {
			out = append(out, p.PageNumber)
		}
	}
	return out
}

// BenchCandidate is one leaf's candidate set, for a command that wants to
// show why a leaf did or did not resolve.
type BenchCandidate struct {
	Title   string
	Claimed int
	Pages   []int
}

// BenchCandidates returns what the resolver would ask about, without
// asking. Pure code: no Judge, no cost.
func BenchCandidates(nodes []tree.TOCNode, pages []PageText, exclude []int) []BenchCandidate {
	var out []BenchCandidate
	for _, c := range collectResolveClaims(nodes, pages, exclude) {
		bc := BenchCandidate{Title: c.title, Claimed: c.claimed}
		for _, h := range c.candidates {
			bc.Pages = append(bc.Pages, h.page)
		}
		out = append(out, bc)
	}
	return out
}
