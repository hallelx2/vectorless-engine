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

// DOCX parses Microsoft Word .docx files.
//
// A .docx is a ZIP archive; the main payload lives at word/document.xml.
// Paragraphs with a heading style ("Heading 1"…) — or, failing that, an
// explicit outline level — become section boundaries. Non-heading
// paragraphs are body text for the enclosing section. Tables are preserved
// as Markdown grids so a cell's number stays bound to its row/column
// labels, and footnotes are recovered into a trailing section.
//
// We use encoding/xml directly (no third-party dependency) because the
// WordprocessingML subset we care about is small and stable.
type DOCX struct{}

// NewDOCX returns a new DOCX parser.
func NewDOCX() *DOCX { return &DOCX{} }

// Name implements Parser.
func (*DOCX) Name() string { return "docx" }

// Accepts implements Parser.
func (*DOCX) Accepts(contentType, filename string) bool {
	if contentType == "application/vnd.openxmlformats-officedocument.wordprocessingml.document" {
		return true
	}
	return HasExt(filename, ".docx")
}

// Parse implements Parser.
func (*DOCX) Parse(_ context.Context, r io.Reader) (*ParsedDoc, error) {
	// zip.Reader needs a ReaderAt + size; buffer into memory. DOCX files
	// are almost always small (< a few MB) in practice.
	buf, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(bytes.NewReader(buf), int64(len(buf)))
	if err != nil {
		return nil, fmt.Errorf("docx: not a valid zip: %w", err)
	}

	body := readZipFile(zr, "word/document.xml")
	if body == nil {
		return nil, fmt.Errorf("docx: missing word/document.xml")
	}

	blocks, err := extractBlocks(body)
	if err != nil {
		return nil, err
	}

	// Group blocks into sections. A heading paragraph opens a new section;
	// non-heading paragraphs and tables append to the current section body.
	type acc struct {
		level int
		title string
		body  []string
	}
	accs := []*acc{{level: 0}}
	current := accs[0]
	for _, blk := range blocks {
		switch blk.kind {
		case blockTable:
			if md := renderTableMarkdown(blk.rows); md != "" {
				current.body = append(current.body, md)
			}
		default: // blockPara
			text := strings.TrimSpace(blk.text)
			if text == "" {
				continue
			}
			if blk.headingLevel > 0 {
				current = &acc{level: blk.headingLevel, title: text}
				accs = append(accs, current)
				continue
			}
			current.body = append(current.body, text)
		}
	}

	flats := make([]flatSection, 0, len(accs))
	for _, a := range accs {
		flats = append(flats, flatSection{
			Level:   a.level,
			Title:   a.title,
			Content: strings.Join(a.body, "\n\n"),
		})
	}
	flats = dropEmptyPreamble(flats)

	// Derive the title from the document body (before footnotes, so a
	// "Footnotes" section can't be mistaken for the title).
	title := deriveTitle(flats)

	// Recover footnotes into a trailing section so their content isn't lost.
	if fn := extractFootnotes(zr); fn != "" {
		flats = append(flats, flatSection{Level: 1, Title: "Footnotes", Content: fn})
	}

	return &ParsedDoc{
		Title:    title,
		Sections: buildSections(flats),
	}, nil
}

// readZipFile returns the bytes of the named entry, or nil if absent.
func readZipFile(zr *zip.Reader, name string) []byte {
	for _, f := range zr.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				return nil
			}
			defer rc.Close()
			data, err := io.ReadAll(rc)
			if err != nil {
				return nil
			}
			return data
		}
	}
	return nil
}

type docxBlockKind int

const (
	blockPara docxBlockKind = iota
	blockTable
)

type docxBlock struct {
	kind         docxBlockKind
	headingLevel int        // blockPara: 0 = body, 1-6 = heading
	text         string     // blockPara
	rows         [][]string // blockTable
}

// isWordNS reports whether an XML namespace is the WordprocessingML main
// namespace (or empty, for decoders that drop the prefix).
func isWordNS(space string) bool {
	return space == "" || strings.HasSuffix(space, "wordprocessingml/2006/main")
}

// extractBlocks streams through document.xml and yields a document-order
// sequence of paragraph and table blocks. Heading level comes from the
// paragraph style ("Heading N"/"Title") and falls back to <w:outlineLvl>.
// Table cells (including the paragraphs inside them) are captured into a
// rows×cols grid; nested tables collapse into their enclosing cell's text.
func extractBlocks(body []byte) ([]docxBlock, error) {
	dec := xml.NewDecoder(bytes.NewReader(body))
	var out []docxBlock

	var (
		tblDepth        int
		rows            [][]string // current outermost table
		inRow           bool
		inCell          bool
		cellBuf         strings.Builder

		inPara          bool
		level           int
		hasStyleHeading bool
		paraBuf         strings.Builder
		capturing       bool // inside <w:t>
	)

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "tbl":
				if tblDepth == 0 {
					rows = nil
				}
				tblDepth++
			case "tr":
				if tblDepth == 1 {
					rows = append(rows, nil)
					inRow = true
				}
			case "tc":
				if tblDepth == 1 && inRow {
					inCell = true
					cellBuf.Reset()
				}
			case "p":
				if isWordNS(t.Name.Space) {
					inPara = true
					level = 0
					hasStyleHeading = false
					paraBuf.Reset()
				}
			case "pStyle":
				if inPara && tblDepth == 0 {
					for _, a := range t.Attr {
						if a.Name.Local == "val" {
							if lv := headingLevelFromStyle(a.Value); lv > 0 {
								level = lv
								hasStyleHeading = true
							}
						}
					}
				}
			case "outlineLvl":
				// Fallback heading signal for docs that use outline levels
				// instead of named heading styles. <w:val> is 0-based.
				if inPara && tblDepth == 0 && !hasStyleHeading {
					for _, a := range t.Attr {
						if a.Name.Local == "val" {
							if n, e := strconv.Atoi(strings.TrimSpace(a.Value)); e == nil {
								if lv := n + 1; lv >= 1 && lv <= 6 {
									level = lv
								}
							}
						}
					}
				}
			case "t":
				if inPara {
					capturing = true
				}
			case "tab":
				if inPara {
					paraBuf.WriteByte('\t')
				}
			case "br":
				if inPara {
					paraBuf.WriteByte('\n')
				}
			}
		case xml.CharData:
			if capturing {
				paraBuf.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				capturing = false
			case "p":
				if inPara {
					text := paraBuf.String()
					switch {
					case inCell:
						// Paragraph inside a table cell: fold into the cell.
						if cellBuf.Len() > 0 {
							cellBuf.WriteByte('\n')
						}
						cellBuf.WriteString(text)
					case tblDepth == 0:
						out = append(out, docxBlock{kind: blockPara, headingLevel: level, text: text})
					}
					inPara = false
				}
			case "tc":
				if tblDepth == 1 && inCell {
					if n := len(rows); n > 0 {
						rows[n-1] = append(rows[n-1], cellBuf.String())
					}
					inCell = false
				}
			case "tr":
				if tblDepth == 1 {
					inRow = false
				}
			case "tbl":
				if tblDepth == 1 {
					out = append(out, docxBlock{kind: blockTable, rows: rows})
					rows = nil
				}
				if tblDepth > 0 {
					tblDepth--
				}
			}
		}
	}
	return out, nil
}

// extractFootnotes pulls the text of every content footnote out of
// word/footnotes.xml, joined by blank lines. Structural footnotes (the
// separator / continuation entries, which carry a w:type attribute) are
// skipped. Returns "" when there are no footnotes.
func extractFootnotes(zr *zip.Reader) string {
	data := readZipFile(zr, "word/footnotes.xml")
	if len(data) == 0 {
		return ""
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	var notes []string
	var (
		inNote    bool
		skip      bool
		buf       strings.Builder
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
			case "footnote":
				inNote = true
				skip = false
				buf.Reset()
				for _, a := range t.Attr {
					if a.Name.Local == "type" {
						skip = true // separator / continuationSeparator
					}
				}
			case "t":
				if inNote && !skip {
					capturing = true
				}
			}
		case xml.CharData:
			if capturing {
				buf.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				capturing = false
			case "footnote":
				if inNote && !skip {
					if s := strings.TrimSpace(buf.String()); s != "" {
						notes = append(notes, s)
					}
				}
				inNote = false
			}
		}
	}
	return strings.Join(notes, "\n\n")
}

// renderTableMarkdown serialises a rows×cols grid as a GitHub-flavoured
// Markdown table. The first row is treated as the header. Cell newlines are
// flattened to spaces and pipes are escaped so the grid stays well-formed.
func renderTableMarkdown(rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	cols := 0
	for _, r := range rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	if cols == 0 {
		return ""
	}

	var b strings.Builder
	writeRow := func(cells []string) {
		b.WriteByte('|')
		for c := 0; c < cols; c++ {
			v := ""
			if c < len(cells) {
				v = strings.TrimSpace(strings.ReplaceAll(cells[c], "\n", " "))
				v = strings.ReplaceAll(v, "|", "\\|")
			}
			b.WriteByte(' ')
			b.WriteString(v)
			b.WriteString(" |")
		}
		b.WriteByte('\n')
	}

	writeRow(rows[0])
	b.WriteByte('|')
	for c := 0; c < cols; c++ {
		b.WriteString(" --- |")
	}
	b.WriteByte('\n')
	for _, r := range rows[1:] {
		writeRow(r)
	}
	return strings.TrimRight(b.String(), "\n")
}

// headingLevelFromStyle parses the pStyle @val attribute. Word writes:
//
//	"Heading1", "Heading2", ... (Word 2007+, most common)
//	"Heading 1", "Heading 2", ... (with space, some variants)
//	"heading 1" (LibreOffice)
//	"Title" — treat as level 1
//
// Returns 0 when the style is not a heading.
func headingLevelFromStyle(s string) int {
	v := strings.TrimSpace(strings.ToLower(s))
	if v == "title" {
		return 1
	}
	v = strings.TrimSpace(strings.TrimPrefix(v, "heading"))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	if n < 1 {
		return 0
	}
	if n > 6 {
		n = 6
	}
	return n
}
