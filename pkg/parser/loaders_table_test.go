package parser_test

import (
	"archive/zip"
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/hallelx2/vectorless-engine/pkg/parser"
)

// zipBytes assembles an in-memory zip from name->content pairs.
func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const wNS = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"`

// TestDOCXTableFootnotesOutline: a heading via <w:outlineLvl> (no named
// style), a table preserved as a Markdown grid, and a content footnote
// recovered while the separator is skipped.
func TestDOCXTableFootnotesOutline(t *testing.T) {
	doc := `<w:document ` + wNS + `><w:body>
	  <w:p><w:pPr><w:outlineLvl w:val="0"/></w:pPr><w:r><w:t>Financials</w:t></w:r></w:p>
	  <w:tbl>
	    <w:tr><w:tc><w:p><w:r><w:t>Metric</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>FY22</w:t></w:r></w:p></w:tc></w:tr>
	    <w:tr><w:tc><w:p><w:r><w:t>Revenue</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>100</w:t></w:r></w:p></w:tc></w:tr>
	  </w:tbl>
	</w:body></w:document>`
	footnotes := `<w:footnotes ` + wNS + `>
	  <w:footnote w:type="separator" w:id="-1"><w:p><w:r><w:t>SEP</w:t></w:r></w:p></w:footnote>
	  <w:footnote w:id="1"><w:p><w:r><w:t>Revenue is net of returns.</w:t></w:r></w:p></w:footnote>
	</w:footnotes>`

	data := zipBytes(t, map[string]string{
		"word/document.xml":  doc,
		"word/footnotes.xml": footnotes,
	})
	parsed, err := parser.NewDOCX().Parse(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	fin := findSection(parsed, "Financials")
	if fin == nil {
		t.Fatal(`outlineLvl heading "Financials" not detected`)
	}
	for _, want := range []string{"Metric", "FY22", "Revenue", "100", "|"} {
		if !strings.Contains(fin.Content, want) {
			t.Errorf("table content missing %q; got:\n%s", want, fin.Content)
		}
	}
	fn := findSection(parsed, "Footnotes")
	if fn == nil || !strings.Contains(fn.Content, "Revenue is net of returns.") {
		t.Errorf("footnote not recovered: %+v", fn)
	}
	if fn != nil && strings.Contains(fn.Content, "SEP") {
		t.Errorf("separator footnote should be skipped: %q", fn.Content)
	}
}

// TestHTMLTableToMarkdown: a <table> renders as a grid, not flattened prose.
func TestHTMLTableToMarkdown(t *testing.T) {
	html := `<html><body>
	  <h1>Report</h1>
	  <table>
	    <thead><tr><th>Region</th><th>Revenue</th></tr></thead>
	    <tbody>
	      <tr><td>North</td><td>100</td></tr>
	      <tr><td>South</td><td>200</td></tr>
	    </tbody>
	  </table>
	</body></html>`
	parsed, err := parser.NewHTML().Parse(context.Background(), strings.NewReader(html))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sec := findSection(parsed, "Report")
	if sec == nil {
		t.Fatal(`missing "Report" section`)
	}
	for _, want := range []string{"| Region | Revenue |", "North", "200"} {
		if !strings.Contains(sec.Content, want) {
			t.Errorf("html table missing %q; got:\n%s", want, sec.Content)
		}
	}
}

// TestXLSXSheetsToTables: each worksheet becomes a section with a grid;
// shared strings and column refs resolve correctly.
func TestXLSXSheetsToTables(t *testing.T) {
	files := map[string]string{
		"xl/sharedStrings.xml": `<sst><si><t>Metric</t></si><si><t>Revenue</t></si></sst>`,
		"xl/workbook.xml": `<workbook xmlns:r="http://x"><sheets>` +
			`<sheet name="P&amp;L" sheetId="1" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships>` +
			`<Relationship Id="rId1" Target="worksheets/sheet1.xml"/></Relationships>`,
		"xl/worksheets/sheet1.xml": `<worksheet><sheetData>` +
			`<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1"><v>2022</v></c></row>` +
			`<row r="2"><c r="A2" t="s"><v>1</v></c><c r="B2"><v>100</v></c></row>` +
			`</sheetData></worksheet>`,
	}
	data := zipBytes(t, files)
	parsed, err := parser.NewXLSX().Parse(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sec := findSection(parsed, "P&L")
	if sec == nil {
		t.Fatalf("missing P&L sheet section; title=%q sections=%d", parsed.Title, len(parsed.Sections))
	}
	for _, want := range []string{"Metric", "Revenue", "2022", "100"} {
		if !strings.Contains(sec.Content, want) {
			t.Errorf("xlsx content missing %q; got:\n%s", want, sec.Content)
		}
	}
}

// TestCSVToTable: CSV becomes a single grid section.
func TestCSVToTable(t *testing.T) {
	csv := "Region,Revenue\nNorth,100\nSouth,200\n"
	parsed, err := parser.NewCSV().Parse(context.Background(), strings.NewReader(csv))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed.Sections) != 1 {
		t.Fatalf("expected 1 section, got %d", len(parsed.Sections))
	}
	c := parsed.Sections[0].Content
	for _, want := range []string{"| Region | Revenue |", "North", "200"} {
		if !strings.Contains(c, want) {
			t.Errorf("csv grid missing %q; got:\n%s", want, c)
		}
	}
}

// TestTextHeadingSplitting: ALL-CAPS and numbered headings split the doc.
func TestTextHeadingSplitting(t *testing.T) {
	txt := "Intro line here.\n\nOVERVIEW\nThis is the overview body.\n\n1. Background\nSome background text.\n"
	parsed, err := parser.NewText().Parse(context.Background(), strings.NewReader(txt))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if findSection(parsed, "OVERVIEW") == nil {
		t.Error(`ALL-CAPS heading "OVERVIEW" not split out`)
	}
	if findSection(parsed, "1. Background") == nil {
		t.Error(`numbered heading "1. Background" not split out`)
	}
	if intro := findSection(parsed, "Introduction"); intro == nil || !strings.Contains(intro.Content, "Intro line here.") {
		t.Errorf("preamble not preserved as Introduction: %+v", intro)
	}
}
