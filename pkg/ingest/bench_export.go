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
