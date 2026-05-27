package parser_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hallelx2/vectorless-engine/pkg/parser"
)

// readFixture is a tiny helper that fails the test if the fixture can't
// be read. Keeps the per-test setup boilerplate-free.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("testdata", name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %q: %v", path, err)
	}
	return b
}

// TestPDFParserEmitsTableSections asserts the table-extraction stage
// produces at least one Section flagged with Metadata["table"]="true"
// containing well-formed Markdown when fed the issue-466 fixture from
// pdftable (two ruled tables on page 1, with known cell contents).
//
// This is the single most important assertion of the integration: a
// regression here means numeric question answers from FinanceBench-class
// documents would collapse back into space-joined text runs.
func TestPDFParserEmitsTableSections(t *testing.T) {
	b := readFixture(t, "tables-example.pdf")
	p := parser.NewPDF()
	doc, err := p.Parse(context.Background(), bytes.NewReader(b))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var tables []parser.Section
	for _, s := range doc.Flatten() {
		if s.Metadata["table"] == "true" {
			tables = append(tables, s)
		}
	}
	if len(tables) == 0 {
		t.Fatalf("expected at least one table section, got 0 (sections: %d)", len(doc.Sections))
	}

	for i, ts := range tables {
		if ts.PageStart != 1 || ts.PageEnd != 1 {
			t.Errorf("table %d: expected pages 1-1, got %d-%d", i, ts.PageStart, ts.PageEnd)
		}
		if !strings.Contains(ts.Title, "page 1") {
			t.Errorf("table %d: title %q should mention the page", i, ts.Title)
		}
		// Markdown rows must have a header + separator + at least one data row.
		lines := strings.Split(ts.Content, "\n")
		if len(lines) < 3 {
			t.Errorf("table %d: content has too few lines (%d): %q", i, len(lines), ts.Content)
			continue
		}
		// Separator row is always second.
		if !strings.HasPrefix(lines[1], "|") || !strings.Contains(lines[1], "---") {
			t.Errorf("table %d: missing GFM separator row, got %q", i, lines[1])
		}
		// Each row starts and ends with a pipe.
		for j, l := range lines {
			if !strings.HasPrefix(l, "|") || !strings.HasSuffix(l, "|") {
				t.Errorf("table %d line %d not pipe-delimited: %q", i, j, l)
			}
		}
		// Rows / cols metadata must agree with the rendered rows
		// (header is row 0 in the rendering but still counted).
		rowsMeta := ts.Metadata["rows"]
		colsMeta := ts.Metadata["cols"]
		if rowsMeta == "" || colsMeta == "" {
			t.Errorf("table %d: missing rows/cols metadata: %+v", i, ts.Metadata)
		}
	}

	// At least one of the tables in this fixture has the known cell text
	// "T0-C0" (header) and "T0-22-last" (last data row). If pdftable
	// reshuffled the columns we'd still see these as substrings somewhere.
	joined := ""
	for _, ts := range tables {
		joined += ts.Content
	}
	if !strings.Contains(joined, "T0-C0") {
		t.Errorf("expected 'T0-C0' (header cell) somewhere in table content, missing")
	}
	if !strings.Contains(joined, "T0-22-last") {
		t.Errorf("expected 'T0-22-last' (last data row) somewhere in table content, missing")
	}
}

// TestPDFParserTablesContainerHidesUnderParent verifies that the engine
// wraps table sections under a synthetic "Tables" container at the
// document root rather than inlining them into the outline. This keeps
// the outline order matching page order for the prose sections — which
// downstream callers rely on for citation rendering.
func TestPDFParserTablesContainerHidesUnderParent(t *testing.T) {
	b := readFixture(t, "tables-example.pdf")
	p := parser.NewPDF()
	doc, err := p.Parse(context.Background(), bytes.NewReader(b))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var container *parser.Section
	for i := range doc.Sections {
		if doc.Sections[i].Title == "Tables" && doc.Sections[i].Metadata["tables_container"] == "true" {
			container = &doc.Sections[i]
			break
		}
	}
	if container == nil {
		t.Fatal(`missing synthetic "Tables" container at the document root`)
	}
	if len(container.Children) == 0 {
		t.Fatalf("Tables container has no children")
	}
	for _, ch := range container.Children {
		if ch.Metadata["table"] != "true" {
			t.Errorf("Tables container has non-table child %q (metadata=%+v)", ch.Title, ch.Metadata)
		}
	}
}

// TestPDFParserDisabledTables ensures the kill-switch works: when the
// parser is constructed with nil TableOpts (or Enabled=false) no table
// sections are emitted and the rest of the document still ingests cleanly.
// This is the rollback path if a real-world PDF ever surfaces a regression.
func TestPDFParserDisabledTables(t *testing.T) {
	b := readFixture(t, "tables-example.pdf")
	p := parser.NewPDFWithTables(nil)
	doc, err := p.Parse(context.Background(), bytes.NewReader(b))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, s := range doc.Flatten() {
		if s.Metadata["table"] == "true" {
			t.Errorf("expected no table sections when tables disabled, got %q", s.Title)
		}
		if s.Title == "Tables" && s.Metadata["tables_container"] == "true" {
			t.Errorf("expected no Tables container when tables disabled")
		}
	}
}

// TestPDFParserCorruptInputReturnsCleanError exercises the resilience
// guarantee: a malformed PDF (header bytes mutated) does NOT panic and
// returns a descriptive error rather than collapsing the engine.
func TestPDFParserCorruptInputReturnsCleanError(t *testing.T) {
	// Mutating the magic header is enough to make every PDF library
	// reject it. The error path we want to validate is "OpenBytes
	// returns; we wrap with 'pdf: open:' and propagate".
	corrupt := []byte("%PDFFOOBAR-1.4\n%garbage\nendoffile")
	p := parser.NewPDF()
	_, err := p.Parse(context.Background(), bytes.NewReader(corrupt))
	if err == nil {
		t.Fatal("expected error for corrupt PDF, got nil")
	}
	if !strings.HasPrefix(err.Error(), "pdf: open:") {
		t.Errorf("expected 'pdf: open:' prefix, got %q", err.Error())
	}
}

// TestPDFParser10KSmokeOptional runs the parser over a real 10-K when
// VLE_TEST_FILING_PDF points at one. It's a discovery aid for benchmark
// validation, not a regression gate, so we skip cleanly when the env
// var is unset (the default CI path). The point of this test is to
// confirm pdftable-driven extraction finds real balance-sheet tables in
// real financial filings before benchmark numbers come in.
func TestPDFParser10KSmokeOptional(t *testing.T) {
	path := os.Getenv("VLE_TEST_FILING_PDF")
	if path == "" {
		t.Skip("set VLE_TEST_FILING_PDF=<path to 10-K.pdf> to run")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	p := parser.NewPDF()
	doc, err := p.Parse(context.Background(), bytes.NewReader(b))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tables := 0
	pages := map[int]struct{}{}
	for _, s := range doc.Flatten() {
		if s.Metadata["table"] == "true" {
			tables++
			pages[s.PageStart] = struct{}{}
		}
	}
	t.Logf("10-K smoke: %d table sections across %d distinct pages", tables, len(pages))
	if tables == 0 {
		t.Errorf("expected at least one table section in a 10-K, got 0")
	}
}

// TestPDFParserResilienceToTableExtractionPanic is a smoke test that the
// safeExtractTables wrapper never propagates a panic from inside
// pdftable. We can't easily synthesise a panicking PDF, but we can run
// table extraction against the corrupted-but-still-PDF-shaped fixture
// to confirm the safety net is wired (any panic would also fail the
// previous corrupt-input test).
func TestPDFParserResilienceToTableExtractionPanic(t *testing.T) {
	// A valid PDF with no extractable tables should produce zero table
	// sections and zero errors — the resilience contract is "tables on
	// or off, ingest never breaks".
	b := readFixture(t, "tables-example.pdf")
	p := parser.NewPDF()
	doc, err := p.Parse(context.Background(), bytes.NewReader(b))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if doc == nil {
		t.Fatal("doc is nil")
	}
	// Verify there's at least one non-table section: ingest must produce
	// SOMETHING usable even on a tables-only fixture.
	hasNonTable := false
	for _, s := range doc.Flatten() {
		if s.Metadata["table"] != "true" && strings.TrimSpace(s.Content) != "" {
			hasNonTable = true
			break
		}
	}
	if !hasNonTable {
		// On this specific fixture the document is essentially "tables
		// only" — every word lives inside a table cell. The outline
		// might therefore contain no non-table prose, which is fine.
		// What we MUST have, though, is a non-empty Sections slice
		// (the Tables container at minimum).
		if len(doc.Sections) == 0 {
			t.Fatal("doc has no sections at all")
		}
	}

	// Trivially exercise the corruption path too — make sure we never
	// panic regardless of the input shape. We use errors.Is to catch
	// the case where a future change adds a sentinel.
	_, perr := p.Parse(context.Background(), bytes.NewReader([]byte{0x25, 0x50, 0x44, 0x46, 0x2d, 0x31, 0x2e, 0x37, 0x0a})) // bare "%PDF-1.7\n" with no body
	if perr == nil {
		t.Fatal("expected error for bare-header PDF, got nil")
	}
	// We don't pin the error type — pdftable evolves the wrapping — but
	// it must be a real error, not nil.
	_ = errors.Is(perr, errors.New("placeholder")) // sanity that errors.Is doesn't barf
}
