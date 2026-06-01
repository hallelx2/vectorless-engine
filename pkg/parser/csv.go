package parser

import (
	"context"
	"encoding/csv"
	"io"
	"strings"
)

// CSV parses comma-separated values into a single section rendered as a
// Markdown grid, so the table structure (and each value's column) is
// preserved for the LLM rather than collapsed into prose. The first row is
// treated as the header.
type CSV struct{}

// NewCSV returns a new CSV parser.
func NewCSV() *CSV { return &CSV{} }

// Name implements Parser.
func (*CSV) Name() string { return "csv" }

// Accepts implements Parser.
func (*CSV) Accepts(contentType, filename string) bool {
	switch contentType {
	case "text/csv", "application/csv":
		return true
	}
	return HasExt(filename, ".csv")
}

// Parse implements Parser.
func (*CSV) Parse(_ context.Context, r io.Reader) (*ParsedDoc, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1 // tolerate ragged rows
	cr.LazyQuotes = true

	records, err := cr.ReadAll()
	if err != nil {
		return nil, err
	}

	title := "Data"
	if len(records) > 0 && len(records[0]) > 0 {
		// Use the first header cell as a hint if it reads like a label.
		if h := strings.TrimSpace(records[0][0]); h != "" && len(h) <= 60 {
			title = h
		}
	}

	content := renderTableMarkdown(records)
	return &ParsedDoc{
		Title: title,
		Sections: []Section{{
			Level:   1,
			Title:   "Data",
			Content: content,
		}},
	}, nil
}
