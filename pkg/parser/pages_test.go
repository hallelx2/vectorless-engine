package parser

import (
	"context"
	"os"
	"testing"
)

// Pages come from rows grouped by physical page, never from sections.
func TestPagesFromRowsGroupsByPhysicalPage(t *testing.T) {
	rows := []pdfRow{
		{page: 1, text: "Cover"},
		{page: 2, text: "Item 1. Business"},
		{page: 2, text: "We make things."},
		{page: 3, text: "and keep making them."}, // same section, next page
		{page: 5, text: "Item 1A. Risk Factors"}, // page 4 is blank
	}
	got := pagesFromRows(rows)
	if len(got) != 4 {
		t.Fatalf("pages: %d want 4 (1,2,3,5)", len(got))
	}
	if got[2].Number != 3 || got[2].Text != "and keep making them." {
		t.Errorf("page 3 should hold only its own text: %+v", got[2])
	}
	if got[3].Number != 5 {
		t.Errorf("a blank page keeps its number for the next: %+v", got[3])
	}
	if got[1].Text != "Item 1. Business\nWe make things." {
		t.Errorf("page 2: %q", got[1].Text)
	}
}

func TestParsedPDFExposesPages(t *testing.T) {
	f, err := os.Open("testdata/tables-example.pdf")
	if err != nil {
		t.Skip("fixture missing")
	}
	defer f.Close()
	doc, err := NewPDF().Parse(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Pages) == 0 {
		t.Fatal("no pages on a parsed PDF")
	}
	for i := 1; i < len(doc.Pages); i++ {
		if doc.Pages[i].Number <= doc.Pages[i-1].Number {
			t.Errorf("pages out of order: %d after %d", doc.Pages[i].Number, doc.Pages[i-1].Number)
		}
	}
	if doc.Pages[0].Number != 1 {
		t.Errorf("first page is %d", doc.Pages[0].Number)
	}
}
