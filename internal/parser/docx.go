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
// Paragraphs with a style name like "Heading 1", "Heading 2", … become
// section boundaries. Paragraphs with no heading style are body text for
// the enclosing section.
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

	var body []byte
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			body, err = io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return nil, err
			}
			break
		}
	}
	if body == nil {
		return nil, fmt.Errorf("docx: missing word/document.xml")
	}

	paras, err := extractParagraphs(body)
	if err != nil {
		return nil, err
	}

	// Group paragraphs into sections. A heading paragraph opens a new
	// section; non-heading paragraphs append to the current section's body.
	type flat struct {
		level int
		title string
		body  []string
	}
	flats := []*flat{{level: 0, title: ""}}
	current := flats[0]
	for _, p := range paras {
		text := strings.TrimSpace(p.text)
		if text == "" {
			continue
		}
		if p.headingLevel > 0 {
			current = &flat{level: p.headingLevel, title: text}
			flats = append(flats, current)
			continue
		}
		current.body = append(current.body, text)
	}
	if len(flats) > 1 && flats[0].level == 0 && len(flats[0].body) == 0 {
		flats = flats[1:]
	}

	var title string
	for _, f := range flats {
		if f.level == 1 {
			title = f.title
			break
		}
	}
	if title == "" && len(flats) > 0 {
		title = flats[0].title
	}

	// Build hierarchy via level stack.
	rootSec := &Section{Level: 0, Title: title}
	stack := []*Section{rootSec}
	for _, f := range flats {
		sec := Section{
			Level:   f.level,
			Title:   f.title,
			Content: strings.Join(f.body, "\n\n"),
		}
		if f.level == 0 {
			if sec.Content == "" {
				continue
			}
			sec.Level = 1
			sec.Title = "Introduction"
		}
		for len(stack) > 1 && stack[len(stack)-1].Level >= sec.Level {
			stack = stack[:len(stack)-1]
		}
		parent := stack[len(stack)-1]
		parent.Children = append(parent.Children, sec)
		tail := &parent.Children[len(parent.Children)-1]
		stack = append(stack, tail)
	}

	return &ParsedDoc{
		Title:    title,
		Sections: rootSec.Children,
	}, nil
}

type docxPara struct {
	headingLevel int
	text         string
}

// extractParagraphs streams through document.xml and yields each <w:p>
// as a (headingLevel, text) pair.
//
// Heading detection: a paragraph is a heading if its <w:pStyle w:val>
// value is "Heading1".."Heading9" or "Heading 1".."Heading 9" (Word
// writes both spellings). Level is the trailing digit.
func extractParagraphs(body []byte) ([]docxPara, error) {
	dec := xml.NewDecoder(bytes.NewReader(body))
	var out []docxPara
	var (
		inPara    bool
		level     int
		textBuf   strings.Builder
		capturing bool // inside <w:t>
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
			case "p":
				if t.Name.Space == "" || strings.HasSuffix(t.Name.Space, "wordprocessingml/2006/main") {
					inPara = true
					level = 0
					textBuf.Reset()
				}
			case "pStyle":
				if inPara {
					for _, a := range t.Attr {
						if a.Name.Local == "val" {
							level = headingLevelFromStyle(a.Value)
						}
					}
				}
			case "t":
				if inPara {
					capturing = true
				}
			case "tab":
				if inPara {
					textBuf.WriteByte('\t')
				}
			case "br":
				if inPara {
					textBuf.WriteByte('\n')
				}
			}
		case xml.CharData:
			if capturing {
				textBuf.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				capturing = false
			case "p":
				if inPara {
					out = append(out, docxPara{
						headingLevel: level,
						text:         textBuf.String(),
					})
					inPara = false
				}
			}
		}
	}
	return out, nil
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
