package parser

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	pdflib "github.com/ledongthuc/pdf"
)

// PDF is a pragmatic first-pass PDF parser.
//
// PDF is a layout format, not a structured format — there are no real
// headings in the wire layer, just runs of glyphs with font sizes and
// positions. To recover structure we:
//
//  1. Extract text per page, row-by-row, with font-size information.
//  2. Compute the median font size across the whole document.
//  3. Treat any row whose font size exceeds a threshold (1.2× median)
//     AND that is short (<= 14 words) as a heading candidate.
//  4. Group headings into levels by font-size buckets (largest = level 1).
//  5. Everything else is body text for the most recent heading.
//
// This won't beat a PDF with a proper bookmark outline, but it recovers
// surprisingly usable structure from academic papers, whitepapers, and
// reports. A future parser can read the PDF's /Outlines dictionary
// directly for documents that have one.
//
// Encrypted PDFs, PDFs with non-standard fonts, and scanned PDFs (pure
// images) are not supported at this stage.
type PDF struct{}

// NewPDF returns a new PDF parser.
func NewPDF() *PDF { return &PDF{} }

// Name implements Parser.
func (*PDF) Name() string { return "pdf" }

// Accepts implements Parser.
func (*PDF) Accepts(contentType, filename string) bool {
	if contentType == "application/pdf" {
		return true
	}
	return HasExt(filename, ".pdf")
}

// Parse implements Parser.
func (*PDF) Parse(_ context.Context, r io.Reader) (*ParsedDoc, error) {
	buf, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	reader, err := pdflib.NewReader(bytes.NewReader(buf), int64(len(buf)))
	if err != nil {
		return nil, fmt.Errorf("pdf: open: %w", err)
	}

	rows, err := extractPDFRows(reader)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return &ParsedDoc{
			Title:    "",
			Sections: []Section{{Level: 1, Title: "Document", Content: ""}},
		}, nil
	}

	// Median font size — our reference for "normal body text".
	sizes := make([]float64, 0, len(rows))
	for _, r := range rows {
		if r.fontSize > 0 {
			sizes = append(sizes, r.fontSize)
		}
	}
	sort.Float64s(sizes)
	median := 0.0
	if n := len(sizes); n > 0 {
		median = sizes[n/2]
	}
	headingFloor := median * 1.2

	// Unique heading sizes, largest first. These define heading levels:
	// the largest bucket is level 1, next is level 2, etc. (capped at 6).
	levelForSize := buildHeadingLevelMap(rows, headingFloor)

	type flat struct {
		level int
		title string
		body  strings.Builder
	}
	flats := []*flat{{level: 0, title: ""}}
	current := flats[0]

	for _, row := range rows {
		text := strings.TrimSpace(row.text)
		if text == "" {
			continue
		}
		lvl, isHeading := levelForSize[roundSize(row.fontSize)]
		if isHeading && looksLikeHeading(text) {
			current = &flat{level: lvl, title: text}
			flats = append(flats, current)
			continue
		}
		if current.body.Len() > 0 {
			current.body.WriteString(" ")
		}
		current.body.WriteString(text)
	}

	if len(flats) > 1 && flats[0].level == 0 && strings.TrimSpace(flats[0].body.String()) == "" {
		flats = flats[1:]
	}

	var title string
	for _, f := range flats {
		if f.level == 1 {
			title = f.title
			break
		}
	}
	if title == "" && len(flats) > 0 {
		title = flats[0].title
	}

	// Build hierarchy via level stack.
	rootSec := &Section{Level: 0, Title: title}
	stack := []*Section{rootSec}
	for _, f := range flats {
		sec := Section{
			Level:   f.level,
			Title:   f.title,
			Content: strings.TrimSpace(f.body.String()),
		}
		if f.level == 0 {
			if sec.Content == "" {
				continue
			}
			sec.Level = 1
			sec.Title = "Introduction"
		}
		for len(stack) > 1 && stack[len(stack)-1].Level >= sec.Level {
			stack = stack[:len(stack)-1]
		}
		parent := stack[len(stack)-1]
		parent.Children = append(parent.Children, sec)
		tail := &parent.Children[len(parent.Children)-1]
		stack = append(stack, tail)
	}

	// No headings recovered? Fall back to one "Document" section.
	if len(rootSec.Children) == 0 {
		var all strings.Builder
		for _, f := range flats {
			if s := strings.TrimSpace(f.body.String()); s != "" {
				if all.Len() > 0 {
					all.WriteString(" ")
				}
				all.WriteString(s)
			}
		}
		rootSec.Children = []Section{{
			Level:   1,
			Title:   "Document",
			Content: all.String(),
		}}
	}

	return &ParsedDoc{
		Title:    title,
		Sections: rootSec.Children,
	}, nil
}

type pdfRow struct {
	page     int
	fontSize float64
	text     string
}

// extractPDFRows walks each page, grouping letters into rows by y-position
// and recording the dominant font size per row. ledongthuc/pdf's Content()
// returns individual glyphs; we reassemble them into lines.
func extractPDFRows(reader *pdflib.Reader) ([]pdfRow, error) {
	numPages := reader.NumPage()
	var out []pdfRow

	for pageNum := 1; pageNum <= numPages; pageNum++ {
		page := reader.Page(pageNum)
		if page.V.IsNull() {
			continue
		}
		content := page.Content()

		// Group letters by (approximate) baseline Y. Values within 2pt are
		// considered the same row — PDFs frequently jitter Y by a fraction.
		type rowBucket struct {
			y      float64
			maxFS  float64
			chars  []pdflib.Text
		}
		var buckets []*rowBucket
		find := func(y float64) *rowBucket {
			for _, b := range buckets {
				if abs(b.y-y) < 2.0 {
					return b
				}
			}
			b := &rowBucket{y: y}
			buckets = append(buckets, b)
			return b
		}
		for _, t := range content.Text {
			b := find(t.Y)
			b.chars = append(b.chars, t)
			if t.FontSize > b.maxFS {
				b.maxFS = t.FontSize
			}
		}
		// Sort rows top-to-bottom (higher Y = higher on page in PDF).
		sort.Slice(buckets, func(i, j int) bool { return buckets[i].y > buckets[j].y })

		for _, b := range buckets {
			sort.Slice(b.chars, func(i, j int) bool { return b.chars[i].X < b.chars[j].X })
			var sb strings.Builder
			var lastX float64
			for i, ch := range b.chars {
				// Insert a space if there's a visible gap between glyphs.
				if i > 0 && ch.X-lastX > ch.FontSize*0.3 {
					sb.WriteString(" ")
				}
				sb.WriteString(ch.S)
				lastX = ch.X + ch.W
			}
			text := strings.TrimSpace(sb.String())
			if text == "" {
				continue
			}
			out = append(out, pdfRow{
				page:     pageNum,
				fontSize: b.maxFS,
				text:     text,
			})
		}
	}
	return out, nil
}

// buildHeadingLevelMap returns a map from rounded-font-size → heading level
// (1 = largest = h1). Only sizes above headingFloor are considered.
// Levels are capped at 6.
func buildHeadingLevelMap(rows []pdfRow, floor float64) map[int]int {
	seen := map[int]bool{}
	for _, r := range rows {
		if r.fontSize > floor {
			seen[roundSize(r.fontSize)] = true
		}
	}
	var bigs []int
	for k := range seen {
		bigs = append(bigs, k)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(bigs)))

	out := make(map[int]int, len(bigs))
	for i, sz := range bigs {
		lvl := i + 1
		if lvl > 6 {
			lvl = 6
		}
		out[sz] = lvl
	}
	return out
}

// roundSize rounds a font size to the nearest 0.5pt, expressed as an int
// (×2) so it can key a map. Two glyphs with nominally the same font size
// often jitter by a fraction of a point.
func roundSize(s float64) int {
	return int(s*2 + 0.5)
}

func looksLikeHeading(s string) bool {
	// Headings are rarely > 14 words and never end with sentence punctuation
	// from the middle of a paragraph.
	words := strings.Fields(s)
	if len(words) == 0 || len(words) > 14 {
		return false
	}
	// Common body-text tells: trailing comma, trailing ellipsis.
	if strings.HasSuffix(s, ",") {
		return false
	}
	return true
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
