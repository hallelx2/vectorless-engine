package parser

import (
	"os"
	"testing"

	"github.com/hallelx2/vectorless-engine/pkg/pincite"
)

func TestPDFLayoutIsNormalisedAndReadable(t *testing.T) {
	b, err := os.ReadFile("testdata/tables-example.pdf")
	if err != nil {
		t.Fatal(err)
	}
	l, err := PDFLayout(b)
	if err != nil {
		t.Fatal(err)
	}
	if l.Version != pincite.LayoutVersion || len(l.Pages) == 0 {
		t.Fatalf("layout = v%d, %d pages", l.Version, len(l.Pages))
	}
	words := 0
	for _, p := range l.Pages {
		if p.Width <= 0 || p.Height <= 0 {
			t.Fatalf("page %d has no size", p.Number)
		}
		prevTop := -1.0
		for _, ln := range p.Lines {
			if ln.Top < 0 || ln.Top > 1 || ln.Height <= 0 || ln.Top+ln.Height > 1.0001 {
				t.Fatalf("page %d line out of bounds: top=%v h=%v", p.Number, ln.Top, ln.Height)
			}
			if ln.Top < prevTop-0.005 {
				t.Fatalf("page %d lines not top-to-bottom: %v after %v", p.Number, ln.Top, prevTop)
			}
			prevTop = ln.Top
			for i, w := range ln.Words {
				words++
				if w.X0 < 0 || w.X1 > 1 || w.X1 < w.X0 {
					t.Fatalf("word %q out of bounds: %v..%v", w.Text, w.X0, w.X1)
				}
				if i > 0 && w.X0 < ln.Words[i-1].X0 {
					t.Fatalf("words not left-to-right on page %d", p.Number)
				}
			}
		}
	}
	if words == 0 {
		t.Fatal("no words extracted")
	}
	// Every line can be found again by its own text: the resolver and
	// the layout agree on tokenisation.
	p := l.Page(1)
	for _, s := range pincite.Sentences(p) {
		if len(pincite.Tokenize(s)) < 4 {
			continue
		}
		if _, ok := pincite.Locate(p, s); !ok {
			t.Fatalf("a sentence taken from the page could not be located on it: %q", s)
		}
	}
}
