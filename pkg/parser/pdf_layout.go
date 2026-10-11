package parser

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/hallelx2/pdftable"

	"github.com/hallelx2/vectorless-engine/pkg/pincite"
)

// PDFLayout extracts every page's line geometry for pincites (HAL-833):
// the words on each line and where they sit, normalised to 0..1 of the
// page with the origin at the top-left.
//
// Lines are built exactly the way extractPDFRows builds the rows the
// page text comes from — the same word options, the same 2pt baseline
// bucketing, the same rotated-run filter — so the words a highlight is
// placed on are the words retrieval read. Two different notions of a
// line would let a quote match the text and miss the geometry.
func PDFLayout(b []byte) (*pincite.Layout, error) {
	doc, err := openPDFBytes(b)
	if err != nil {
		return nil, fmt.Errorf("pdf layout: open: %w", err)
	}

	// Words() shares pdftable's package-level state with OpenBytes; see
	// pdftableOpenMu.
	pdftableOpenMu.Lock()
	defer pdftableOpenMu.Unlock()

	wopts := pdftable.WordOpts{
		XTolerance:        3,
		XToleranceRatio:   0.15,
		YTolerance:        3,
		HorizontalLTR:     true,
		VerticalTTB:       true,
		Expand:            true,
		UseExplicitSpaces: true,
	}

	out := &pincite.Layout{Version: pincite.LayoutVersion}
	for n := 1; n <= doc.NumPages(); n++ {
		page, perr := doc.Page(n)
		if perr != nil {
			continue
		}
		pw, ph := page.Width(), page.Height()
		if pw <= 0 || ph <= 0 {
			continue
		}
		pg := pincite.Page{Number: n, Width: round2(pw), Height: round2(ph)}
		words, werr := page.Words(wopts)
		if werr != nil {
			out.Pages = append(out.Pages, pg)
			continue
		}
		pg.Lines = layoutLines(words, pw, ph)
		out.Pages = append(out.Pages, pg)
	}
	if len(out.Pages) == 0 {
		return nil, fmt.Errorf("pdf layout: no readable pages")
	}
	return out, nil
}

func layoutLines(words []pdftable.Word, pw, ph float64) []pincite.Line {
	type bucket struct {
		y     float64
		words []pdftable.Word
	}
	var buckets []*bucket
	find := func(y float64) *bucket {
		for _, b := range buckets {
			if abs(b.y-y) < 2.0 {
				return b
			}
		}
		b := &bucket{y: y}
		buckets = append(buckets, b)
		return b
	}
	for _, w := range words {
		if isRotatedRun(w.Upright, w.Direction) || strings.TrimSpace(w.Text) == "" {
			continue
		}
		b := find(w.Y1)
		b.words = append(b.words, w)
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].y > buckets[j].y })

	lines := make([]pincite.Line, 0, len(buckets))
	for _, b := range buckets {
		sort.Slice(b.words, func(i, j int) bool { return b.words[i].X0 < b.words[j].X0 })
		top, bottom := math.Inf(1), math.Inf(-1)
		ln := pincite.Line{Words: make([]pincite.Word, 0, len(b.words))}
		for _, w := range b.words {
			top = math.Min(top, ph-w.Y1)
			bottom = math.Max(bottom, ph-w.Y0)
			ln.Words = append(ln.Words, pincite.Word{
				Text: w.Text,
				X0:   clamp01(w.X0 / pw),
				X1:   clamp01(w.X1 / pw),
			})
		}
		ln.Top = clamp01(top / ph)
		ln.Height = clamp01((bottom - top) / ph)
		lines = append(lines, ln)
	}
	return lines
}

func clamp01(f float64) float64 {
	switch {
	case f < 0:
		return 0
	case f > 1:
		return 1
	}
	// Four decimals is a tenth of a point on a letter page: well below
	// anything a reader sees, and it keeps the stored layout small.
	return math.Round(f*10000) / 10000
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
