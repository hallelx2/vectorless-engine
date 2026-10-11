// Command pincitecheck builds a PDF's pincite layout, locates a quote on
// a page, renders that page and prints the regions — the manual check
// that a highlight lands on the words it claims.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/hallelx2/vectorless-engine/pkg/parser"
	"github.com/hallelx2/vectorless-engine/pkg/pincite"
)

func main() {
	pdfPath := flag.String("pdf", "", "PDF file")
	page := flag.Int("page", 1, "page to search and render")
	quote := flag.String("quote", "", "quote to locate")
	out := flag.String("out", "", "write the rendered page JPEG here")
	sentences := flag.Bool("sentences", false, "print the page's candidate sentences")
	flag.Parse()

	b, err := os.ReadFile(*pdfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	t0 := time.Now()
	l, err := parser.PDFLayout(b)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	raw, _ := json.Marshal(l)
	fmt.Printf("layout: %d pages, %d KB, %s\n", len(l.Pages), len(raw)/1024, time.Since(t0).Round(time.Millisecond))
	p := l.Page(*page)
	if *sentences {
		for i, s := range pincite.Sentences(p) {
			fmt.Printf("s%d: %s\n", i, s)
		}
	}
	if *quote != "" {
		m := pincite.Resolve(l, *quote, []int{*page})
		mj, _ := json.Marshal(m)
		fmt.Println(string(mj))
	}
	if *out != "" {
		err := pincite.Poppler{}.RenderPages(context.Background(), b, *page, *page, func(_ int, jpeg []byte) error {
			return os.WriteFile(*out, jpeg, 0o644)
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}
