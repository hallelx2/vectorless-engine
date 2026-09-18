// Command pagedump prints a PDF's pages exactly as the ingest pipeline
// sees them, so a resolver miss can be traced to the text it searched.
//
//	go run ./cmd/pagedump doc.pdf | grep -n -i "item 2"
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/hallelx2/vectorless-engine/pkg/ingest"
	"github.com/hallelx2/vectorless-engine/pkg/parser"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: pagedump doc.pdf")
		os.Exit(2)
	}
	f, err := os.Open(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	doc, err := parser.NewPDF().Parse(context.Background(), f)
	if err != nil {
		fmt.Fprintln(os.Stderr, "parse:", err)
		os.Exit(1)
	}
	for _, p := range ingest.BenchAssemblePages(doc) {
		fmt.Printf("\n===== PAGE %d (%d chars) =====\n%s", p.PageNumber, len(p.Text), p.Text)
	}
}
