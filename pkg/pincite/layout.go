// Package pincite binds a verbatim quote to the exact region of the
// source page it came from (HAL-832).
//
// A pincite is one claim in an answer bound to an exact region of the
// source. The engine resolves it deterministically: a model chooses
// which sentence supports a claim, but no model places a highlight.
// The quote is located in the page's own words, as the PDF parser saw
// them, and the rectangles come from those words' positions.
//
// Geometry is normalised to 0..1 fractions of the page with the origin
// at the top-left, so a rectangle stays correct at any zoom and in any
// container width. Absolute PDF points are never stored.
package pincite

// LayoutVersion is bumped whenever the stored layout's shape or the
// line-building rules change, so a stale cached layout is rebuilt
// rather than misread.
const LayoutVersion = 1

// Precision says how exactly a citation is located. A line-level hit
// and a page-level fallback must never look the same to a reader: a
// fallback that renders like a hit claims accuracy the system does not
// have.
type Precision string

const (
	// PrecisionLine: the quote was found in the page's words; regions
	// cover exactly the words it spans, one rectangle per line.
	PrecisionLine Precision = "line"
	// PrecisionPage: the page is known but the quote could not be
	// located on it. No regions.
	PrecisionPage Precision = "page"
	// PrecisionSection: no page geometry exists (a non-PDF source).
	// No regions.
	PrecisionSection Precision = "section"
)

// Region is a rectangle on a page, in 0..1 fractions of the page's
// width and height, origin top-left.
type Region struct {
	Left   float64 `json:"left"`
	Top    float64 `json:"top"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// Word is one word on a line: its text and its horizontal extent, in
// fractions of the page width. The vertical extent is the line's.
type Word struct {
	Text string  `json:"t"`
	X0   float64 `json:"x0"`
	X1   float64 `json:"x1"`
}

// Line is a run of words that sit on the same baseline in the same
// column. Lines are the granularity the layout is stored at: fine
// enough to highlight a sentence honestly, coarse enough that a long
// filing stays small. Word extents trim the first and last line of a
// highlight to the words the quote actually covers.
type Line struct {
	Top    float64 `json:"top"`
	Height float64 `json:"height"`
	Words  []Word  `json:"words"`
}

// Page is one page's size in PDF points and its lines in reading
// order.
type Page struct {
	Number int     `json:"page"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
	Lines  []Line  `json:"lines"`
}

// Layout is a document's page geometry: what the span index of
// HAL-833 resolves against. It is stored beside the document, never
// inline in the section tree, because retrieval reads the tree
// constantly and must not pay for data only a reader's highlight uses.
type Layout struct {
	Version int    `json:"version"`
	Pages   []Page `json:"pages"`
}

// Page returns page n (1-based), or nil.
func (l *Layout) Page(n int) *Page {
	if l == nil || n < 1 {
		return nil
	}
	// Pages are stored in order and normally dense; check the direct
	// slot first, then scan in case a page failed to parse.
	if n-1 < len(l.Pages) && l.Pages[n-1].Number == n {
		return &l.Pages[n-1]
	}
	for i := range l.Pages {
		if l.Pages[i].Number == n {
			return &l.Pages[i]
		}
	}
	return nil
}

// PageSize is one page's dimensions, as the document viewer needs them.
type PageSize struct {
	Number int     `json:"page"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// Sizes lists every page's dimensions.
func (l *Layout) Sizes() []PageSize {
	if l == nil {
		return nil
	}
	out := make([]PageSize, 0, len(l.Pages))
	for _, p := range l.Pages {
		out = append(out, PageSize{Number: p.Number, Width: p.Width, Height: p.Height})
	}
	return out
}

// Text returns the page's text, one line per line, as the resolver
// sees it. Used to offer a model the page's sentences.
func (p *Page) Text() string {
	if p == nil {
		return ""
	}
	var n int
	for _, ln := range p.Lines {
		for _, w := range ln.Words {
			n += len(w.Text) + 1
		}
	}
	b := make([]byte, 0, n)
	for i, ln := range p.Lines {
		if i > 0 {
			b = append(b, '\n')
		}
		for j, w := range ln.Words {
			if j > 0 {
				b = append(b, ' ')
			}
			b = append(b, w.Text...)
		}
	}
	return string(b)
}
