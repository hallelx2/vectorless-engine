package parser

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// XLSX parses Microsoft Excel .xlsx workbooks. Each worksheet becomes a
// top-level section whose body is the sheet rendered as a Markdown grid, so
// every value keeps its row and column. Shared strings, inline strings, and
// numeric/boolean cells are all resolved; empty cells are preserved as gaps
// so columns stay aligned.
//
// Built on the standard library (archive/zip + encoding/xml) — no
// third-party dependency, matching the DOCX parser.
type XLSX struct {
	// MaxRowsPerSheet caps how many rows are emitted per sheet (0 = no cap).
	// Huge sheets can blow up section size; callers can bound it.
	MaxRowsPerSheet int
}

// NewXLSX returns an XLSX parser with no row cap.
func NewXLSX() *XLSX { return &XLSX{} }

// Name implements Parser.
func (*XLSX) Name() string { return "xlsx" }

// Accepts implements Parser.
func (*XLSX) Accepts(contentType, filename string) bool {
	if contentType == "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" {
		return true
	}
	return HasExt(filename, ".xlsx")
}

// Parse implements Parser.
func (x *XLSX) Parse(_ context.Context, r io.Reader) (*ParsedDoc, error) {
	buf, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(bytes.NewReader(buf), int64(len(buf)))
	if err != nil {
		return nil, fmt.Errorf("xlsx: not a valid zip: %w", err)
	}

	shared := parseSharedStrings(readZipFile(zr, "xl/sharedStrings.xml"))
	sheets := parseWorkbookSheets(readZipFile(zr, "xl/workbook.xml"))
	rels := parseWorkbookRels(readZipFile(zr, "xl/_rels/workbook.xml.rels"))

	var sections []Section
	for i, sh := range sheets {
		target := rels[sh.rID]
		if target == "" {
			continue
		}
		// Rel targets are relative to the xl/ directory.
		data := readZipFile(zr, "xl/"+strings.TrimPrefix(target, "/"))
		if data == nil {
			continue
		}
		rows := parseSheetRows(data, shared, x.MaxRowsPerSheet)
		title := sh.name
		if title == "" {
			title = fmt.Sprintf("Sheet %d", i+1)
		}
		content := renderTableMarkdown(rows)
		sections = append(sections, Section{Level: 1, Title: title, Content: content})
	}

	if len(sections) == 0 {
		return nil, fmt.Errorf("xlsx: no readable worksheets")
	}

	title := sections[0].Title
	return &ParsedDoc{
		Title:    title,
		Sections: sections,
		Metadata: map[string]string{"sheets": strconv.Itoa(len(sections))},
	}, nil
}

// parseSharedStrings reads xl/sharedStrings.xml into an indexed slice. Each
// <si> may hold a single <t> or several <r><t> runs that concatenate.
func parseSharedStrings(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	var out []string
	var (
		inSI      bool
		cur       strings.Builder
		capturing bool
	)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				inSI = true
				cur.Reset()
			case "t":
				if inSI {
					capturing = true
				}
			}
		case xml.CharData:
			if capturing {
				cur.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				capturing = false
			case "si":
				out = append(out, cur.String())
				inSI = false
			}
		}
	}
	return out
}

type xlsxSheet struct {
	name string
	rID  string
}

// parseWorkbookSheets reads the ordered <sheet name= r:id=> list.
func parseWorkbookSheets(data []byte) []xlsxSheet {
	if len(data) == 0 {
		return nil
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	var out []xlsxSheet
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "sheet" {
			var sh xlsxSheet
			for _, a := range se.Attr {
				switch a.Name.Local {
				case "name":
					sh.name = a.Value
				case "id": // r:id
					sh.rID = a.Value
				}
			}
			out = append(out, sh)
		}
	}
	return out
}

// parseWorkbookRels maps relationship IDs to worksheet targets.
func parseWorkbookRels(data []byte) map[string]string {
	out := map[string]string{}
	if len(data) == 0 {
		return out
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "Relationship" {
			var id, target string
			for _, a := range se.Attr {
				switch a.Name.Local {
				case "Id":
					id = a.Value
				case "Target":
					target = a.Value
				}
			}
			if id != "" {
				out[id] = target
			}
		}
	}
	return out
}

// parseSheetRows reads <sheetData> into a rows×cols grid, resolving cell
// types and honouring column references so gaps stay aligned.
func parseSheetRows(data []byte, shared []string, maxRows int) [][]string {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var rows [][]string

	var (
		inRow     bool
		cells     map[int]string // col index -> value for the current row
		maxCol    int
		cellType  string
		cellCol   int
		val       strings.Builder
		inlineStr strings.Builder
		capturing bool // inside <v>
		capInline bool // inside <is><t>
	)

	flushRow := func() {
		row := make([]string, maxCol+1)
		for c, v := range cells {
			if c >= 0 && c <= maxCol {
				row[c] = v
			}
		}
		rows = append(rows, row)
	}

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				inRow = true
				cells = map[int]string{}
				maxCol = -1
			case "c":
				cellType = ""
				cellCol = 0
				val.Reset()
				inlineStr.Reset()
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "t":
						cellType = a.Value
					case "r":
						cellCol = colIndexFromRef(a.Value)
					}
				}
			case "v":
				capturing = true
			case "t":
				// inline string text (t="inlineStr") lives in <is><t>
				if cellType == "inlineStr" {
					capInline = true
				}
			}
		case xml.CharData:
			if capturing {
				val.Write(t)
			} else if capInline {
				inlineStr.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v":
				capturing = false
			case "t":
				capInline = false
			case "c":
				if inRow {
					value := resolveCell(cellType, val.String(), inlineStr.String(), shared)
					cells[cellCol] = value
					if cellCol > maxCol {
						maxCol = cellCol
					}
				}
			case "row":
				if inRow {
					flushRow()
					inRow = false
					if maxRows > 0 && len(rows) >= maxRows {
						return rows
					}
				}
			}
		}
	}
	return rows
}

// resolveCell turns a cell's type + raw value into display text.
func resolveCell(cellType, v, inline string, shared []string) string {
	switch cellType {
	case "s": // shared string: v is an index
		if idx, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && idx >= 0 && idx < len(shared) {
			return shared[idx]
		}
		return ""
	case "inlineStr":
		return inline
	case "b": // boolean
		if strings.TrimSpace(v) == "1" {
			return "TRUE"
		}
		return "FALSE"
	default: // "n" (number), "str" (formula string), or untyped
		return v
	}
}

// colIndexFromRef turns a cell reference like "AB12" into a zero-based
// column index (A=0, B=1, … Z=25, AA=26, …). Returns 0 if no letters.
func colIndexFromRef(ref string) int {
	col := 0
	seen := false
	for _, r := range ref {
		switch {
		case r >= 'A' && r <= 'Z':
			col = col*26 + int(r-'A'+1)
			seen = true
		case r >= 'a' && r <= 'z':
			col = col*26 + int(r-'a'+1)
			seen = true
		default:
			if seen {
				return col - 1
			}
		}
	}
	if !seen {
		return 0
	}
	return col - 1
}
