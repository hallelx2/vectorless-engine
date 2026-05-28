package parser

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	"github.com/hallelx2/pdftable"
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
//  1. Extract positioned WORDS per page (font name + size + bbox) using
//     pdftable's content-stream interpreter.
//  2. Compute the median font size across the whole document.
//  3. Treat any row whose font size exceeds a threshold (1.2× median)
//     AND that is short (<= 14 words) as a heading candidate.
//  4. Group headings into levels by font-size buckets (largest = level 1).
//  5. Everything else is body text for the most recent heading.
//  6. Run pdftable's table-finding pipeline over each page and emit one
//     extra Section per detected table whose Content is a GitHub-flavoured
//     Markdown rendering of the cells. Tables are flagged with
//     Metadata["table"]="true" so retrieval can lean on numeric content
//     that would otherwise collapse into a space-joined run.
//
// Encrypted PDFs are auto-decrypted with the empty password via pdfcpu.
// PDFs with non-standard fonts and scanned PDFs (pure images) are not
// supported at this stage.
type PDF struct {
	// Tables, when non-nil, overrides the default table-extraction
	// behaviour (enabled, lines/lines strategies, minima 2×2). Pass nil
	// to use the engine defaults; pass a zero value to disable tables
	// entirely.
	Tables *TableOpts

	// MaxSections caps the number of leaf sections the parser emits for a
	// single document. A pathological PDF — e.g. a 90-page filing whose
	// every bold statement title and repeated "<Company> and
	// Subsidiaries" line trips the heading detector, leaving a swarm of
	// empty/tiny heading-only leaves — can otherwise produce far more
	// leaves than the document has real sections. Each leaf later costs a
	// summarize + HyDE LLM call, so an uncapped count directly throttles
	// or stalls ingest.
	//
	// When the prose leaf count exceeds MaxSections, adjacent small leaf
	// siblings under a shared parent are merged (smallest first) until the
	// count is back under the cap. Table sections (which carry distinct
	// numeric content) are never merged.
	//
	// Zero selects defaultMaxLeafSections. A negative value disables the
	// cap entirely (escape hatch for callers that want the raw outline).
	MaxSections int
}

// TableOpts controls pdftable's table-finding stage. The zero value
// disables table extraction; use DefaultTableOpts() for the
// production-default knobs.
type TableOpts struct {
	// Enabled toggles table extraction. When false, the parser behaves
	// exactly like the pre-integration text-only flow.
	Enabled bool

	// VerticalStrategy is forwarded to pdftable as
	// TableSettings.VerticalStrategy. Empty falls back to "lines".
	VerticalStrategy string

	// HorizontalStrategy is forwarded to pdftable as
	// TableSettings.HorizontalStrategy. Empty falls back to "lines".
	HorizontalStrategy string

	// MinTableRows is the minimum row count for a candidate table to be
	// emitted as a Section. 0 means "no minimum"; recommend 2 in
	// production so trivial single-row matches don't leak into the
	// outline.
	MinTableRows int

	// MinTableCols is the minimum column count for a candidate table.
	// Same semantics as MinTableRows.
	MinTableCols int
}

// DefaultTableOpts returns the production defaults: tables on, both axes
// using the "lines" strategy, minima 2×2. These mirror pdftable's own
// DefaultTableSettings() and were tuned against the FinanceBench 10-K
// fixtures.
func DefaultTableOpts() *TableOpts {
	return &TableOpts{
		Enabled:            true,
		VerticalStrategy:   "lines",
		HorizontalStrategy: "lines",
		MinTableRows:       2,
		MinTableCols:       2,
	}
}

// NewPDF returns a new PDF parser with table extraction enabled at the
// production defaults and the default leaf-section cap. Pass
// NewPDFWithTables(nil) (or a zero TableOpts) to opt out of tables.
func NewPDF() *PDF { return &PDF{Tables: DefaultTableOpts()} }

// NewPDFWithTables returns a PDF parser using the supplied table-
// extraction options and the default leaf-section cap. Pass nil to
// disable table extraction.
func NewPDFWithTables(opts *TableOpts) *PDF { return &PDF{Tables: opts} }

// NewPDFWithOpts returns a PDF parser using the supplied table-extraction
// options and an explicit leaf-section cap. maxSections == 0 selects
// defaultMaxLeafSections; a negative value disables the cap. This is the
// constructor the engine wiring uses so the cap is operator-tunable via
// config (ingest.max_sections).
func NewPDFWithOpts(opts *TableOpts, maxSections int) *PDF {
	return &PDF{Tables: opts, MaxSections: maxSections}
}

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
func (p *PDF) Parse(_ context.Context, r io.Reader) (*ParsedDoc, error) {
	buf, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	// We run TWO PDF backends in parallel here:
	//
	//   - pdftable (the new primitive layer) extracts positioned WORDS
	//     (font name + size + bbox) directly. This is the input to the
	//     section-discovery heuristics and is also the only source for
	//     the table-finding pass. It is robust to letter-spaced glyphs
	//     and ships pdfplumber-parity word grouping out of the box.
	//
	//   - ledongthuc/pdf is retained solely for /Outlines (bookmark)
	//     access — pdftable does not expose the outline dictionary yet,
	//     and outlines are ground truth for SEC filings / academic papers
	//     that have one. Once pdftable surfaces outlines we can drop the
	//     dependency entirely.
	//
	// Both backends consume the same byte slice. If pdftable rejects the
	// document as encrypted we strip the encryption layer with pdfcpu
	// (empty password) and retry — this is the path that lets us index
	// "owner-password" PDFs whose only restriction is print/copy.
	docBytes := buf
	pdoc, err := pdftable.OpenBytes(docBytes)
	if err != nil {
		if isPdftableEncryptedErr(err) {
			cleaned, decErr := decryptPDFWithEmptyPassword(buf)
			if decErr != nil {
				return nil, fmt.Errorf("pdf: open: encrypted and could not be unlocked with empty password: %w", decErr)
			}
			docBytes = cleaned
			pdoc, err = pdftable.OpenBytes(docBytes)
		}
		if err != nil {
			return nil, fmt.Errorf("pdf: open: %w", err)
		}
	}
	defer pdoc.Close()

	reader, err := pdflib.NewReader(bytes.NewReader(docBytes), int64(len(docBytes)))
	if err != nil {
		// ledongthuc/pdf failed on a PDF pdftable accepted (some
		// xref-stream variants are pdftable-only). Without ledongthuc
		// we lose both outline access AND the primitive layer the
		// row extractor uses — so we bail with a clear message rather
		// than emit garbled text from pdftable.Words() (its word
		// grouping concatenates words on standard-14 fonts without
		// AFM metrics; see v0.4.x followup).
		return nil, fmt.Errorf("pdf: open: ledongthuc/pdf backend rejected the document: %w", err)
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

	// Run table extraction once, BEFORE we commit to either the outline
	// path or the heuristic path: both should be able to inherit the
	// same set of detected tables.
	tableSections := extractPDFTables(pdoc, p.Tables)

	// If the PDF ships with a real outline (bookmarks), use it as ground
	// truth for structure — beats any font-size heuristic. We still rely
	// on row extraction for section bodies by matching outline titles
	// against the first occurrence of that text in the row stream.
	if reader != nil {
		if outline := reader.Outline(); len(outline.Child) > 0 {
			if doc, ok := parsePDFWithOutline(outline, rows); ok {
				doc.Sections = capLeafSections(doc.Sections, p.resolvedMaxSections())
				attachTableSections(doc, tableSections)
				return doc, nil
			}
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

	out := &ParsedDoc{
		Title:    title,
		Sections: capLeafSections(chunkOversizedLeaves(rootSec.Children), p.resolvedMaxSections()),
	}
	attachTableSections(out, tableSections)
	return out, nil
}

// resolvedMaxSections turns the configured MaxSections into the value
// the cap actually uses: 0 selects defaultMaxLeafSections; a negative
// value disables the cap (returns a non-positive number capLeafSections
// treats as "off").
func (p *PDF) resolvedMaxSections() int {
	if p.MaxSections == 0 {
		return defaultMaxLeafSections
	}
	return p.MaxSections
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

// defaultMaxLeafSections is the ceiling NewPDF applies when MaxSections
// is left at zero. A 92-page 10-K whose "Notes to Financial Statements"
// section byte-splits into ~50 chunks (and whose body splits into
// hundreds more) was observed producing ~1500 leaves — each one of
// which then costs a summarize + HyDE + multi-axis LLM call at ingest,
// which is what stalled the pipeline. 400 keeps a filing richly
// structured while bounding ingest cost to something Gemini's
// free-tier RPM can clear.
const defaultMaxLeafSections = 400

// countLeafSections returns the number of leaf sections (no children)
// in the tree rooted at sections.
func countLeafSections(sections []Section) int {
	n := 0
	for i := range sections {
		if len(sections[i].Children) == 0 {
			n++
		} else {
			n += countLeafSections(sections[i].Children)
		}
	}
	return n
}

// capLeafSections enforces a ceiling on the total leaf-section count.
// While the count exceeds maxLeaves it repeatedly merges the two
// smallest ADJACENT leaf siblings under whichever parent currently has
// the most leaf children — so the runaway byte-split sections collapse
// back first, while genuinely distinct top-level sections are left
// alone. maxLeaves <= 0 disables the cap.
//
// Merged leaves concatenate their content (blank-line separated), keep
// the first sibling's title, and union their page ranges. Table
// sections are attached AFTER this pass (attachTableSections), so their
// numeric content is never merged away.
func capLeafSections(sections []Section, maxLeaves int) []Section {
	if maxLeaves <= 0 {
		return sections
	}
	// Guard against pathological loops: at most one merge per excess leaf.
	for guard := 0; countLeafSections(sections) > maxLeaves && guard < 100000; guard++ {
		if !mergeOneSmallestAdjacentLeafPair(sections) {
			break // no mergeable adjacent leaf pair anywhere
		}
	}
	return sections
}

// mergeOneSmallestAdjacentLeafPair finds the adjacent leaf-sibling pair
// with the smallest combined content length anywhere in the tree and
// merges it in place. Returns false when no sibling list has two
// adjacent leaves to merge.
func mergeOneSmallestAdjacentLeafPair(sections []Section) bool {
	bestList := (*[]Section)(nil)
	bestIdx := -1
	bestSize := -1

	var walk func(list *[]Section)
	walk = func(list *[]Section) {
		s := *list
		for i := 0; i+1 < len(s); i++ {
			if len(s[i].Children) == 0 && len(s[i+1].Children) == 0 {
				size := len(s[i].Content) + len(s[i+1].Content)
				if bestSize < 0 || size < bestSize {
					bestSize, bestList, bestIdx = size, list, i
				}
			}
		}
		for i := range s {
			if len(s[i].Children) > 0 {
				walk(&s[i].Children)
			}
		}
	}
	walk(&sections)

	if bestList == nil {
		return false
	}
	s := *bestList
	a, b := s[bestIdx], s[bestIdx+1]
	merged := a
	if strings.TrimSpace(a.Content) == "" {
		merged.Content = b.Content
	} else if strings.TrimSpace(b.Content) != "" {
		merged.Content = a.Content + "\n\n" + b.Content
	}
	merged.PageStart = minNonZero(a.PageStart, b.PageStart)
	if b.PageEnd > merged.PageEnd {
		merged.PageEnd = b.PageEnd
	}
	s[bestIdx] = merged
	*bestList = append(s[:bestIdx+1], s[bestIdx+2:]...)
	return true
}

// minNonZero returns the smaller of two page numbers, treating 0
// (unknown) as "no lower bound" so a known page always wins.
func minNonZero(a, b int) int {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

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

// extractPDFRows walks each page of doc, asks pdftable for positioned
// Words, and groups them into rows by visual top (Y1 in PDF user space).
// pdftable's Words() already takes care of intra-word glyph reassembly,
// letter-spacing collapse, and ligature expansion — so this layer just
// has to bucket words back into lines and tally the dominant font size
// + bold ratio per row.
//
// The bucket tolerance (Y1 within 2pt) matches what the previous
// ledongthuc-backed implementation used; word-level Y1 jitter is the
// same scale as the per-glyph jitter it replaced.
// extractPDFRows walks each page, grouping glyphs into rows by Y-position
// and recording the dominant font size + bold ratio per row.
//
// We use ledongthuc/pdf's Content() as the primitive source rather than
// pdftable.Words() because pdftable v0.3.0's word grouping silently
// concatenates adjacent words on the standard-14 fonts SEC filings use
// (no bundled AFM widths → glyph X-advance estimated wrong → word
// X-gaps collapse → "Currentmarketablesecurities"). The X-gap-into-spaces
// heuristic below is robust to that because we never trust pdftable's
// word boundaries — we re-derive them from raw glyph X positions.
//
// Once pdftable bundles standard-14 AFM metrics (v0.4.x goal) we can
// swap back to its Words() output.
func extractPDFRows(reader *pdflib.Reader) ([]pdfRow, error) {
	numPages := reader.NumPage()
	var out []pdfRow

	for pageNum := 1; pageNum <= numPages; pageNum++ {
		page := reader.Page(pageNum)
		if page.V.IsNull() {
			continue
		}
		content := page.Content()

		// Group letters by (approximate) baseline Y. Values within 2pt
		// are considered the same row — PDFs frequently jitter Y by a
		// fraction.
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
		sort.Slice(buckets, func(i, j int) bool { return buckets[i].y > buckets[j].y })

		for _, b := range buckets {
			sort.Slice(b.chars, func(i, j int) bool { return b.chars[i].X < b.chars[j].X })
			var sb strings.Builder
			var lastX float64
			boldGlyphs, totalGlyphs := 0, 0
			for i, ch := range b.chars {
				// Insert a space when the gap between the previous
				// glyph's end and this glyph's start exceeds 0.20 of
				// the font size. Tuned against real PDFs (arXiv +
				// SEC 10-Ks): word-boundary gaps land around
				// 0.20-0.30·fontSize; intra-word kerning stays well
				// below 0.10.
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

// multiSpaceRe matches two or more consecutive whitespace characters.
var multiSpaceRe = regexp.MustCompile(`\s{2,}`)

// looksLetterSpaced reports whether s appears to have been rendered with
// wide letter-tracking — a chain of single characters separated by
// spaces ("U N I T E D"). Used to detect cover-page / heading rows that
// the glyph-spacing heuristic over-split.
func looksLetterSpaced(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 5 {
		return false
	}
	parts := strings.Fields(s)
	if len(parts) < 4 {
		return false
	}
	singles := 0
	for _, p := range parts {
		if len(p) <= 1 {
			singles++
		}
	}
	// At least half of the parts must be single characters for us to
	// call the run letter-spaced.
	return singles*2 >= len(parts)
}

// collapseLetterSpacing rejoins runs of single-character "words"
// (the artifact of wide letter-tracking) into real words. Multi-space
// gaps between letter-spaced groups become regular word boundaries.
func collapseLetterSpacing(s string) string {
	if !looksLetterSpaced(s) {
		return s
	}
	// Split on runs of 2+ spaces to identify word groups, then collapse
	// each group's single-character chain.
	groups := multiSpaceRe.Split(s, -1)
	for i, g := range groups {
		parts := strings.Fields(g)
		// If every part is a single character, glue them.
		allSingles := true
		for _, p := range parts {
			if len(p) > 1 {
				allSingles = false
				break
			}
		}
		if allSingles {
			groups[i] = strings.Join(parts, "")
		}
	}
	return strings.Join(groups, " ")
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

// isBoldFont reports whether a PDF font name denotes a bold weight. SEC filing
// section headings are typically bold at body font size (not larger), so this is
// how we recover them — a size-only heuristic misses them entirely.
func isBoldFont(font string) bool {
	f := strings.ToLower(font)
	return strings.Contains(f, "bold") || strings.Contains(f, "-bd") || strings.Contains(f, ",bd")
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

// isPdftableEncryptedErr reports whether the given pdftable error is
// the sentinel for an encrypted PDF. pdftable surfaces ErrEncrypted via
// errors.Is, which is what we use here so we stay forward-compatible if
// the wrapping ever changes.
func isPdftableEncryptedErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, pdftable.ErrEncrypted) {
		return true
	}
	// Defensive fallback: even if the sentinel ever changes name we
	// still want to retry through pdfcpu rather than fail open.
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "encrypted") || strings.Contains(msg, "encryption")
}

// extractPDFTables runs pdftable's table-finding pipeline over every
// page of doc and returns one Section per detected table. Each
// returned section carries:
//
//   - Title: "Table (page N)" for callers/UIs that want a stable label.
//   - Content: a GitHub-flavoured Markdown rendering of the cells.
//   - PageStart/PageEnd: the page the table was found on (always equal
//     because pdftable does not yet cross-page-merge tables).
//   - Metadata["table"]="true": retrieval can branch on this to apply
//     numeric-content-aware ranking; the rows/cols entries surface the
//     shape for debugging and per-document analytics.
//
// Errors during table extraction are LOGGED and SWALLOWED — the engine's
// commitment is that bad PDFs never break ingest. A panic inside
// pdftable (defensive guard) is also caught.
//
// Pass opts=nil or opts.Enabled=false to short-circuit; the function
// then returns nil cheaply without walking the document.
func extractPDFTables(doc pdftable.Document, opts *TableOpts) []Section {
	if opts == nil || !opts.Enabled {
		return nil
	}
	settings := pdftable.DefaultTableSettings()
	if opts.VerticalStrategy != "" {
		settings.VerticalStrategy = pdftable.TableStrategy(opts.VerticalStrategy)
	}
	if opts.HorizontalStrategy != "" {
		settings.HorizontalStrategy = pdftable.TableStrategy(opts.HorizontalStrategy)
	}
	minRows := opts.MinTableRows
	minCols := opts.MinTableCols

	var sections []Section
	for n := 1; n <= doc.NumPages(); n++ {
		page, err := doc.Page(n)
		if err != nil {
			continue
		}
		tables := safeExtractTables(page, settings, n)
		for _, t := range tables {
			if t == nil {
				continue
			}
			rows := normaliseTableRows(t.Rows)
			if len(rows) < minRows {
				continue
			}
			cols := 0
			if len(rows) > 0 {
				cols = len(rows[0])
			}
			if cols < minCols {
				continue
			}
			md := tableToMarkdown(rows)
			if strings.TrimSpace(md) == "" {
				continue
			}
			sections = append(sections, Section{
				Level:     1,
				Title:     fmt.Sprintf("Table (page %d)", n),
				Content:   md,
				PageStart: n,
				PageEnd:   n,
				Metadata: map[string]string{
					"table": "true",
					"rows":  fmt.Sprintf("%d", len(rows)),
					"cols":  fmt.Sprintf("%d", cols),
				},
			})
		}
	}
	return sections
}

// safeExtractTables wraps page.ExtractTables in a recover() so a bug
// deep inside pdftable can never take down the engine's ingest
// pipeline. Errors and panics are logged at warn level (not error —
// the document still gets ingested, just without its tables).
func safeExtractTables(page pdftable.Page, settings pdftable.TableSettings, pageNum int) (tables []*pdftable.Table) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("pdf: table extraction panicked",
				"page", pageNum,
				"panic", fmt.Sprintf("%v", r))
			tables = nil
		}
	}()
	tables, err := page.ExtractTables(settings)
	if err != nil {
		slog.Warn("pdf: table extraction failed",
			"page", pageNum,
			"err", err)
		return nil
	}
	return tables
}

// normaliseTableRows trims whitespace per cell and pads short rows out
// to the table's max column count. pdftable can emit rows with fewer
// cells than the header when its cell detection finds a hole; we
// promote those to empty strings so Markdown rendering produces a
// well-formed grid (every row has the same column count).
func normaliseTableRows(rows [][]string) [][]string {
	maxCols := 0
	for _, r := range rows {
		if len(r) > maxCols {
			maxCols = len(r)
		}
	}
	if maxCols == 0 {
		return nil
	}
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		row := make([]string, maxCols)
		for i := 0; i < maxCols; i++ {
			if i < len(r) {
				row[i] = strings.TrimSpace(r[i])
			} else {
				row[i] = ""
			}
		}
		// Drop entirely blank rows — they're cell-detection artefacts
		// and contribute no information to retrieval.
		if !isAllBlank(row) {
			out = append(out, row)
		}
	}
	return out
}

// isAllBlank reports whether every cell in row is empty/whitespace.
func isAllBlank(row []string) bool {
	for _, c := range row {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}

// tableToMarkdown renders a normalised table-rows slice as a
// GitHub-flavoured Markdown table. The first row is treated as the
// header; if it is entirely blank, a row of empty header cells is
// emitted so the markdown stays well-formed.
//
// Cell content is escaped minimally: pipe characters inside a cell are
// replaced with the HTML entity so they don't terminate the cell. We
// don't escape backslashes or newlines — newlines inside a cell would
// break the GFM table syntax, so we collapse them to spaces here too.
func tableToMarkdown(rows [][]string) string {
	if len(rows) == 0 || len(rows[0]) == 0 {
		return ""
	}
	cols := len(rows[0])
	var sb strings.Builder

	emitRow := func(cells []string) {
		sb.WriteByte('|')
		for i := 0; i < cols; i++ {
			cell := ""
			if i < len(cells) {
				cell = escapeMarkdownCell(cells[i])
			}
			sb.WriteByte(' ')
			sb.WriteString(cell)
			sb.WriteByte(' ')
			sb.WriteByte('|')
		}
		sb.WriteByte('\n')
	}

	// Header row.
	header := rows[0]
	if isAllBlank(header) {
		header = make([]string, cols)
	}
	emitRow(header)

	// Separator row (GFM uses --- per column).
	sb.WriteByte('|')
	for i := 0; i < cols; i++ {
		sb.WriteString(" --- |")
	}
	sb.WriteByte('\n')

	// Data rows.
	for _, r := range rows[1:] {
		emitRow(r)
	}

	return strings.TrimRight(sb.String(), "\n")
}

// escapeMarkdownCell makes a cell safe for inclusion in a GFM table:
// pipes are entity-encoded (they would otherwise close the cell) and
// embedded newlines / tabs are collapsed to single spaces (GFM tables
// are single-line per cell). Runs of whitespace produced by the
// collapse are squashed to one space for readability.
func escapeMarkdownCell(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "|", "&#124;")
	// Newlines and tabs become spaces; multiple spaces collapse.
	repl := strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ")
	s = repl.Replace(s)
	// Squash runs of spaces.
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return strings.TrimSpace(s)
}

// attachTableSections appends every table section to doc.Sections at
// the document root, after a synthetic "Tables" parent — keeping
// retrieval able to find them but not interleaving them with the
// document outline (which would confuse callers that rely on outline
// order matching page order).
//
// We always create a single "Tables" parent so the top level of the
// outline doesn't balloon: a 10-K with 80 tables would otherwise dwarf
// the actual section list. The parent inherits the union of its
// children's page ranges.
func attachTableSections(doc *ParsedDoc, tables []Section) {
	if doc == nil || len(tables) == 0 {
		return
	}
	parent := Section{
		Level:    1,
		Title:    "Tables",
		Children: tables,
		Metadata: map[string]string{"tables_container": "true"},
	}
	// Compute the parent's page span as the union of children's.
	for _, t := range tables {
		if t.PageStart > 0 && (parent.PageStart == 0 || t.PageStart < parent.PageStart) {
			parent.PageStart = t.PageStart
		}
		if t.PageEnd > parent.PageEnd {
			parent.PageEnd = t.PageEnd
		}
	}
	doc.Sections = append(doc.Sections, parent)
}
