package parser

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	pdflib "github.com/ledongthuc/pdf"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
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
		// ledongthuc/pdf has no encryption support — even PDFs that
		// open in any normal viewer (empty user password, owner-only
		// permissions like print/copy restrictions) get rejected with
		// a "256-bit encryption key" / "encrypted" error. Try to strip
		// the encryption layer with pdfcpu using the empty password,
		// then retry the parser on the cleaned bytes.
		if isEncryptedPDFError(err) {
			cleaned, decErr := decryptPDFWithEmptyPassword(buf)
			if decErr != nil {
				return nil, fmt.Errorf("pdf: open: encrypted and could not be unlocked with empty password: %w", decErr)
			}
			reader, err = pdflib.NewReader(bytes.NewReader(cleaned), int64(len(cleaned)))
		}
		if err != nil {
			return nil, fmt.Errorf("pdf: open: %w", err)
		}
	}

	rows, err := extractPDFRows(reader)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("pdf: parsed but no extractable text — the document may be a scanned image (OCR not yet supported) or use a font encoding the parser can't read")
	}
	// Sanity check on extracted content. PDFs with overlay watermarks
	// drawn on top of every page (the GINA-style "DO NOT COPY..." kind)
	// can produce rows that are mostly noise — extracted text consists
	// of doubled glyphs from the two layers being interleaved by Y.
	// Bail with a clear message instead of going "ready" on empty data.
	if !rowsLookLikeUsableText(rows) {
		return nil, fmt.Errorf("pdf: text extraction produced no usable content — the document may have an overlay watermark or use a non-standard font encoding")
	}

	// If the PDF ships with a real outline (bookmarks), use it as ground
	// truth for structure — beats any font-size heuristic. We still rely
	// on row extraction for section bodies by matching outline titles
	// against the first occurrence of that text in the row stream.
	if outline := reader.Outline(); len(outline.Child) > 0 {
		if doc, ok := parsePDFWithOutline(outline, rows); ok {
			return doc, nil
		}
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

	// Bold rows at (at least) body size are headings too. Filings bold their
	// section headers rather than enlarging them, so a size-only heuristic
	// collapses the whole body into one block. Bold-derived headings nest one
	// level below the smallest font-derived heading level.
	boldLevel := 1
	for _, lv := range levelForSize {
		if lv+1 > boldLevel {
			boldLevel = lv + 1
		}
	}
	if boldLevel > 6 {
		boldLevel = 6
	}

	type flat struct {
		level     int
		title     string
		body      strings.Builder
		pageStart int // min source page touched by this flat (0 = none seen yet)
		pageEnd   int // max source page touched by this flat
	}
	flats := []*flat{{level: 0, title: ""}}
	current := flats[0]

	// touch records that this flat consumed a row from the given page,
	// expanding pageStart/pageEnd. Pages on rows that aren't body text
	// (e.g. a heading row itself) are also counted: the heading lives on
	// that page, so the section visibly starts there.
	touch := func(f *flat, page int) {
		if page <= 0 {
			return
		}
		if f.pageStart == 0 || page < f.pageStart {
			f.pageStart = page
		}
		if page > f.pageEnd {
			f.pageEnd = page
		}
	}

	for _, row := range rows {
		text := strings.TrimSpace(row.text)
		if text == "" {
			continue
		}
		lvl, isHeading := levelForSize[roundSize(row.fontSize)]
		if !isHeading && row.bold && row.fontSize >= median && looksLikeHeading(text) {
			isHeading = true
			lvl = boldLevel
		}
		if isHeading && looksLikeHeading(text) {
			// A *sub-numbered* prefix ("3.1", "3.1.2") signals extra nesting
			// depth relative to the font-derived level. We only ever DEEPEN
			// (never change the base level), so top-level headings — numbered
			// "3" or not ("Abstract") — stay siblings at the same font level.
			if nd, ok := numberedHeadingDepth(text); ok && nd > 1 {
				lvl += nd - 1
			}
			current = &flat{level: lvl, title: text}
			touch(current, row.page)
			flats = append(flats, current)
			continue
		}
		if current.body.Len() > 0 {
			current.body.WriteString(" ")
		}
		current.body.WriteString(text)
		touch(current, row.page)
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
			Level:     f.level,
			Title:     f.title,
			Content:   strings.TrimSpace(f.body.String()),
			PageStart: f.pageStart,
			PageEnd:   f.pageEnd,
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

	// No headings recovered? Fall back to one "Document" section spanning
	// every page we saw.
	if len(rootSec.Children) == 0 {
		var all strings.Builder
		minPage, maxPage := 0, 0
		for _, f := range flats {
			if s := strings.TrimSpace(f.body.String()); s != "" {
				if all.Len() > 0 {
					all.WriteString(" ")
				}
				all.WriteString(s)
			}
			if f.pageStart > 0 && (minPage == 0 || f.pageStart < minPage) {
				minPage = f.pageStart
			}
			if f.pageEnd > maxPage {
				maxPage = f.pageEnd
			}
		}
		rootSec.Children = []Section{{
			Level:     1,
			Title:     "Document",
			Content:   all.String(),
			PageStart: minPage,
			PageEnd:   maxPage,
		}}
	}

	// Internal sections inherit the union of their children's page ranges
	// so callers reading the outline can still cite a page span.
	propagateSectionPages(rootSec.Children)

	return &ParsedDoc{
		Title:    title,
		Sections: chunkOversizedLeaves(rootSec.Children),
	}, nil
}

// propagateSectionPages fills internal-node PageStart/PageEnd from the union
// of descendant leaf ranges where the internal node didn't have its own
// (because its body was empty / hoisted into children). Leaves keep their
// own range untouched.
func propagateSectionPages(sections []Section) (minPage, maxPage int) {
	for i := range sections {
		s := &sections[i]
		childMin, childMax := propagateSectionPages(s.Children)
		// Fold the section's own range with its children's.
		if s.PageStart > 0 && (childMin == 0 || s.PageStart < childMin) {
			childMin = s.PageStart
		}
		if s.PageEnd > childMax {
			childMax = s.PageEnd
		}
		// Only widen the section — never shrink a populated range to 0.
		if childMin > 0 {
			s.PageStart = childMin
		}
		if childMax > 0 {
			s.PageEnd = childMax
		}
		if s.PageStart > 0 && (minPage == 0 || s.PageStart < minPage) {
			minPage = s.PageStart
		}
		if s.PageEnd > maxPage {
			maxPage = s.PageEnd
		}
	}
	return minPage, maxPage
}

// Filing cover pages (and any other long, mixed-topic leaf) often produce one
// 2-3k-char section under a generic title like "3M COMPANY", which mixes
// registration tables, addresses, IRS IDs and contact info. A single summary
// can't cover all those topics, so retrieval misses. Split such leaves into
// smaller sub-sections at word boundaries; each sub-section then gets its own
// title (from a natural colon-terminated header, e.g. "Securities registered
// pursuant to Section 12(b) of the Act", or the first few words) and its own
// summary downstream.
const (
	leafChunkThreshold = 2400 // chars; high enough to leave paper sub-sections alone
	leafChunkTarget    = 900  // chars per chunk, give or take
)

// chunkOversizedLeaves splits any LEAF section whose content exceeds
// leafChunkThreshold into smaller sub-sections. Internal nodes (sections with
// children) are recursed into but never split — they're already structured.
func chunkOversizedLeaves(sections []Section) []Section {
	out := make([]Section, 0, len(sections))
	for _, s := range sections {
		if len(s.Children) > 0 {
			s.Children = chunkOversizedLeaves(s.Children)
			out = append(out, s)
			continue
		}
		if len(s.Content) <= leafChunkThreshold {
			out = append(out, s)
			continue
		}
		pieces := splitContentByWords(s.Content, leafChunkTarget)
		if len(pieces) <= 1 {
			out = append(out, s)
			continue
		}
		parent := Section{Level: s.Level, Title: s.Title, PageStart: s.PageStart, PageEnd: s.PageEnd}
		for i, piece := range pieces {
			fallback := fmt.Sprintf("%s — part %d", s.Title, i+1)
			// We don't track per-chunk pages once content is byte-split — each
			// chunk inherits the parent's range (the leaf is the same source
			// material). Good-enough for retrieval citations.
			parent.Children = append(parent.Children, Section{
				Level:     s.Level + 1,
				Title:     deriveChunkTitle(piece, fallback),
				Content:   piece,
				PageStart: s.PageStart,
				PageEnd:   s.PageEnd,
			})
		}
		out = append(out, parent)
	}
	return out
}

// splitContentByWords breaks a long string into pieces near target size at
// word boundaries. The last piece may be smaller; pieces are never midword.
func splitContentByWords(s string, target int) []string {
	s = strings.TrimSpace(s)
	if target < 200 {
		target = 200
	}
	slack := target / 4
	if len(s) <= target+slack {
		return []string{s}
	}
	var chunks []string
	for len(s) > 0 {
		if len(s) <= target+slack {
			chunks = append(chunks, strings.TrimSpace(s))
			break
		}
		upper := target + slack
		if upper > len(s) {
			upper = len(s)
		}
		cut := strings.LastIndex(s[:upper], " ")
		if cut < target/2 {
			cut = upper // no good break: hard-cut at upper bound
		}
		chunks = append(chunks, strings.TrimSpace(s[:cut]))
		s = strings.TrimSpace(s[cut:])
	}
	return chunks
}

// deriveChunkTitle picks a readable label for a content chunk. Prefers a
// phrase ending in ":" within the first ~80 chars (filings use these as
// natural sub-headers, e.g. "Securities registered pursuant to Section 12(b)
// of the Act:"); otherwise takes the first ~60 chars trimmed at a word
// boundary. Falls back to the supplied default when degenerate.
func deriveChunkTitle(chunk, fallback string) string {
	s := strings.TrimSpace(chunk)
	if s == "" {
		return fallback
	}
	if i := strings.Index(s, ":"); i > 0 && i < 80 {
		candidate := strings.TrimSpace(s[:i])
		if len(strings.Fields(candidate)) >= 2 {
			return candidate
		}
	}
	if len(s) <= 60 {
		return strings.TrimRight(s, " ,;.:")
	}
	cut := strings.LastIndex(s[:60], " ")
	if cut < 30 {
		cut = 60
	}
	t := strings.TrimRight(strings.TrimSpace(s[:cut]), " ,;.:")
	if t == "" {
		return fallback
	}
	return t
}

type pdfRow struct {
	page     int
	fontSize float64
	bold     bool
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
			y     float64
			maxFS float64
			chars []pdflib.Text
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
			boldGlyphs, totalGlyphs := 0, 0
			for i, ch := range b.chars {
				// Insert a space when the gap between the previous
				// glyph's end and this glyph's start exceeds a fraction
				// of the font size. 0.20 was tuned against real PDFs
				// (arXiv papers): word-boundary gaps land around
				// 0.20-0.30·fontSize while intra-word kerning stays
				// well below. The old 0.30 threshold missed most word
				// boundaries, producing run-together text like
				// "implementingtensor2tensor".
				if i > 0 && ch.X-lastX > ch.FontSize*0.20 {
					sb.WriteString(" ")
				}
				sb.WriteString(ch.S)
				lastX = ch.X + ch.W
				if strings.TrimSpace(ch.S) != "" {
					totalGlyphs++
					if isBoldFont(ch.Font) {
						boldGlyphs++
					}
				}
			}
			// Wide letter-tracking — common on filing cover pages and
			// bold section headers — makes every glyph gap exceed the
			// space threshold, yielding "U N I T E D   S T A T E S".
			// Re-join those runs into real words.
			text := collapseLetterSpacing(strings.TrimSpace(sb.String()))
			if text == "" {
				continue
			}
			// Drop publisher/preprint boilerplate (e.g. the rotated
			// arXiv license stamp in the left margin). Left in, it
			// pollutes the structure with junk top-level "headings"
			// and the document title.
			if isBoilerplateLine(text) {
				continue
			}
			out = append(out, pdfRow{
				page:     pageNum,
				fontSize: b.maxFS,
				bold:     totalGlyphs > 0 && boldGlyphs*2 > totalGlyphs,
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

// parsePDFWithOutline builds a ParsedDoc using the PDF's /Outlines as
// the structural ground truth. For each outline entry (depth-first,
// pre-order) we scan forward through rows starting at the last match
// position and treat the first matching row as that heading. Content
// between one outline match and the next is the preceding heading's
// body.
//
// Returns ok=false if we can't match enough outline entries to rows —
// in which case the caller falls back to the font-size heuristic.
func parsePDFWithOutline(outline pdflib.Outline, rows []pdfRow) (*ParsedDoc, bool) {
	// Flatten outline to (level, title) pairs via depth-first walk.
	type entry struct {
		level int
		title string
	}
	var flat []entry
	var walk func(nodes []pdflib.Outline, depth int)
	walk = func(nodes []pdflib.Outline, depth int) {
		lvl := depth + 1
		if lvl > 6 {
			lvl = 6
		}
		for _, n := range nodes {
			t := strings.TrimSpace(n.Title)
			if t != "" {
				flat = append(flat, entry{level: lvl, title: t})
			}
			walk(n.Child, depth+1)
		}
	}
	walk(outline.Child, 0)
	if len(flat) == 0 {
		return nil, false
	}

	// Match each outline title to the first row at or after the cursor
	// whose normalized text begins with the normalized title. This is
	// forgiving of trailing page numbers, section numbering prefixes the
	// outline sometimes omits, etc.
	type matched struct {
		level  int
		title  string
		rowIdx int // index into rows where this heading starts
	}
	var chosen []matched
	cursor := 0
	for _, e := range flat {
		want := normalizeForMatch(e.title)
		found := -1
		for i := cursor; i < len(rows); i++ {
			if strings.HasPrefix(normalizeForMatch(rows[i].text), want) {
				found = i
				break
			}
		}
		if found < 0 {
			continue
		}
		chosen = append(chosen, matched{level: e.level, title: e.title, rowIdx: found})
		cursor = found + 1
	}
	// Require at least half the outline to match, otherwise the outline
	// likely doesn't describe the text we extracted (encrypted fonts,
	// weird glyph mappings) and we should fall back.
	if len(chosen)*2 < len(flat) {
		return nil, false
	}

	// Assemble sections: body text is the concatenation of rows between
	// one match and the next (exclusive). Page range = min/max page across
	// the heading row + body rows.
	rootSec := &Section{Level: 0}
	stack := []*Section{rootSec}
	for i, m := range chosen {
		end := len(rows)
		if i+1 < len(chosen) {
			end = chosen[i+1].rowIdx
		}
		var body strings.Builder
		minPage, maxPage := rows[m.rowIdx].page, rows[m.rowIdx].page
		for _, row := range rows[m.rowIdx+1 : end] {
			text := strings.TrimSpace(row.text)
			if text == "" {
				continue
			}
			if body.Len() > 0 {
				body.WriteByte(' ')
			}
			body.WriteString(text)
			if row.page > 0 && (minPage == 0 || row.page < minPage) {
				minPage = row.page
			}
			if row.page > maxPage {
				maxPage = row.page
			}
		}
		sec := Section{
			Level:     m.level,
			Title:     m.title,
			Content:   body.String(),
			PageStart: minPage,
			PageEnd:   maxPage,
		}
		for len(stack) > 1 && stack[len(stack)-1].Level >= sec.Level {
			stack = stack[:len(stack)-1]
		}
		parent := stack[len(stack)-1]
		parent.Children = append(parent.Children, sec)
		tail := &parent.Children[len(parent.Children)-1]
		stack = append(stack, tail)
	}

	title := ""
	if len(rootSec.Children) > 0 {
		title = rootSec.Children[0].Title
	}

	// Propagate page ranges so internal nodes span their children.
	propagateSectionPages(rootSec.Children)

	return &ParsedDoc{
		Title:    title,
		Sections: rootSec.Children,
	}, true
}

// normalizeForMatch lowercases, strips punctuation, and collapses
// whitespace so outline titles match row text despite cosmetic drift.
func normalizeForMatch(s string) string {
	var b strings.Builder
	prevSpace := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevSpace = false
		case r == ' ' || r == '\t' || r == '\n':
			if !prevSpace && b.Len() > 0 {
				b.WriteByte(' ')
				prevSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// boilerplateSignatures are substrings (lower-cased) that mark a line
// as publisher / preprint / license boilerplate rather than document
// content. Kept deliberately specific so we never drop real prose —
// each phrase is something that essentially only appears in a
// copyright/license stamp.
var boilerplateSignatures = []string{
	"hereby grants permission",
	"provided proper attribution is provided",
	"solely for use in journalistic",
	"all rights reserved",
	"is licensed under a creative commons",
	"licensed under cc by",
	"this work is licensed under",
	"permission to make digital or hard copies",
	"copyright held by the owner",
	"preprint. under review",
	"preprint submitted to",
}

// boilerplateFragments are short tails of a license sentence that the
// PDF splits onto their own row (e.g. "...journalistic or scholarly
// works." → a lone "scholarly works." row). These are too generic to
// match anywhere in a line, so we only drop them when the whole row
// is a short fragment (≤ 4 words) — real prose using the phrase lives
// inside a longer sentence.
var boilerplateFragments = []string{
	"scholarly works",
	"journalistic or",
	"or scholarly",
}

// isBoilerplateLine reports whether a row is publisher/license noise.
// Matches the curated signature list, the bare arXiv id stamp
// ("arXiv:2401.01234v2 [cs.CL] 5 Jan 2024"), and short license-tail
// fragments.
func isBoilerplateLine(s string) bool {
	low := strings.ToLower(strings.TrimSpace(s))
	for _, sig := range boilerplateSignatures {
		if strings.Contains(low, sig) {
			return true
		}
	}
	// arXiv margin stamp: starts with "arxiv:" followed by a digit.
	if strings.HasPrefix(low, "arxiv:") && len(low) > 6 && low[6] >= '0' && low[6] <= '9' {
		return true
	}
	// Short license-tail fragments.
	if len(strings.Fields(low)) <= 4 {
		for _, frag := range boilerplateFragments {
			if strings.Contains(low, frag) {
				return true
			}
		}
	}
	return false
}

// numberedHeadingRe matches a leading section number like "3", "3.1",
// "3.1.2", or "3." followed by whitespace and the heading text.
var numberedHeadingRe = regexp.MustCompile(`^(\d+(?:\.\d+)*)\.?\s+\S`)

// numberedHeadingDepth returns the nesting depth implied by a leading
// section number: "3 Foo" → 1, "3.1 Bar" → 2, "3.1.2 Baz" → 3. Returns
// ok=false when there is no numbered prefix. Depth is clamped to 6.
//
// Callers use depth>1 only to deepen a heading relative to its font-size
// level — never to set the absolute level — so numbered and unnumbered
// top-level headings remain siblings.
func numberedHeadingDepth(s string) (int, bool) {
	m := numberedHeadingRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	depth := strings.Count(m[1], ".") + 1
	if depth > 6 {
		depth = 6
	}
	return depth, true
}

func looksLikeHeading(s string) bool {
	// Headings are rarely > 25 words and never end with sentence punctuation
	// from the middle of a paragraph. (Filing headings like "Item 2.
	// Management's Discussion and Analysis of Financial Condition and Results
	// of Operations" run long, so the cap is generous.)
	words := strings.Fields(s)
	if len(words) == 0 || len(words) > 25 {
		return false
	}
	// Common body-text tells: trailing comma, trailing ellipsis.
	if strings.HasSuffix(s, ",") {
		return false
	}
	return true
}

var multiSpaceRe = regexp.MustCompile(`\s{2,}`)

// isBoldFont reports whether a PDF font name denotes a bold weight. SEC filing
// section headings are typically bold at body font size (not larger), so this is
// how we recover them — a size-only heuristic misses them entirely.
func isBoldFont(font string) bool {
	f := strings.ToLower(font)
	return strings.Contains(f, "bold") || strings.Contains(f, "-bd") || strings.Contains(f, ",bd")
}

// looksLetterSpaced reports whether a row is dominated by solitary-character
// tokens — the signature of wide letter-tracking ("U N I T E D   S T A T E S").
func looksLetterSpaced(s string) bool {
	toks := strings.Fields(s)
	if len(toks) < 4 {
		return false
	}
	single := 0
	for _, t := range toks {
		if len([]rune(t)) == 1 {
			single++
		}
	}
	return single*2 > len(toks)
}

// collapseLetterSpacing rejoins letter-tracked text. Word boundaries survive as
// runs of 2+ spaces; within each word the single spaces between solitary glyphs
// are removed ("F O R M   1 0 - Q" → "FORM 10-Q"). Rows that aren't
// letter-spaced are returned unchanged, so normal prose is never touched.
func collapseLetterSpacing(s string) string {
	if !looksLetterSpaced(s) {
		return s
	}
	words := multiSpaceRe.Split(s, -1)
	for i, w := range words {
		parts := strings.Fields(w)
		allSingle := len(parts) > 0
		for _, p := range parts {
			if len([]rune(p)) > 1 {
				allSingle = false
				break
			}
		}
		if allSingle {
			words[i] = strings.Join(parts, "")
		} else {
			words[i] = strings.Join(parts, " ")
		}
	}
	return strings.TrimSpace(strings.Join(words, " "))
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// rowsLookLikeUsableText is a coarse sanity check. PDFs with an
// overlay watermark drawn at the same Y coordinate as the real text
// produce extracted rows where chars from both layers are interleaved
// — the row text ends up with doubled glyphs ("GGlloobbaall") that
// look like text to len() but contain no actual words. The signal
// we look for is "are at least some rows of normal length and contain
// vowel + consonant patterns rather than runs of repeated chars".
func rowsLookLikeUsableText(rows []pdfRow) bool {
	usable := 0
	for _, r := range rows {
		t := strings.TrimSpace(r.text)
		if len(t) < 4 {
			continue
		}
		if hasRepeatedAdjacentChars(t) {
			continue
		}
		usable++
		if usable >= 5 {
			return true
		}
	}
	return false
}

// hasRepeatedAdjacentChars returns true if more than 30% of letter
// pairs in s are the same letter twice in a row (case-insensitive).
// That's the signature of "GGlloobbaall" interleaving.
func hasRepeatedAdjacentChars(s string) bool {
	letters := 0
	doubled := 0
	prev := rune(0)
	for _, r := range strings.ToLower(s) {
		if r < 'a' || r > 'z' {
			prev = 0
			continue
		}
		letters++
		if r == prev {
			doubled++
		}
		prev = r
	}
	if letters < 4 {
		return false
	}
	return doubled*100/letters > 30
}

// isEncryptedPDFError reports whether the given error from
// ledongthuc/pdf indicates the document is encrypted. The library
// has no proper error type for this, so we match on the message.
func isEncryptedPDFError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "encryption key") ||
		strings.Contains(msg, "encrypted") ||
		strings.Contains(msg, "/encrypt")
}

// decryptPDFWithEmptyPassword strips the encryption dict from a PDF
// using pdfcpu, assuming an empty user password (the common case for
// owner-password-only / "permissions" encryption). Returns the cleaned
// bytes that any unencrypted-PDF parser can consume.
func decryptPDFWithEmptyPassword(in []byte) ([]byte, error) {
	conf := model.NewDefaultConfiguration()
	conf.UserPW = ""
	conf.OwnerPW = ""
	// pdfcpu is conservative by default and won't strip encryption
	// without explicit owner permission acknowledgement when the doc
	// has restrictive perms. We're decrypting purely to extract text
	// for indexing — the user already uploaded the PDF intending it
	// to be searchable, so this is consistent with their intent.
	conf.OwnerPWNew = nil
	conf.UserPWNew = nil
	var out bytes.Buffer
	if err := api.Decrypt(bytes.NewReader(in), &out, conf); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
