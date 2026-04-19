package parser_test

import (
	"archive/zip"
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/hallelx2/vectorless-engine/internal/parser"
)

// findSection returns the first section (depth-first) whose title matches,
// or nil. Tests use this so they don't couple to exact tree shapes.
func findSection(doc *parser.ParsedDoc, title string) *parser.Section {
	for _, s := range doc.Flatten() {
		if s.Title == title {
			sc := s
			return &sc
		}
	}
	return nil
}

func TestMarkdownParser(t *testing.T) {
	const src = `# Project Atlas

Intro paragraph here.

## Setup

Install the thing.

### Dependencies

Needs Go 1.25+.

## Usage

Run ` + "`engine`" + ` and send it a query.
`
	p := parser.NewMarkdown()
	doc, err := p.Parse(context.Background(), strings.NewReader(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if doc.Title != "Project Atlas" {
		t.Errorf("title: got %q want %q", doc.Title, "Project Atlas")
	}
	if len(doc.Sections) != 1 {
		t.Fatalf("top-level sections: got %d want 1", len(doc.Sections))
	}
	top := doc.Sections[0]
	if top.Title != "Project Atlas" {
		t.Errorf("top title: got %q", top.Title)
	}
	if len(top.Children) != 2 {
		t.Fatalf("Project Atlas children: got %d want 2", len(top.Children))
	}

	setup := findSection(doc, "Setup")
	if setup == nil {
		t.Fatal(`missing "Setup" section`)
	}
	if len(setup.Children) != 1 || setup.Children[0].Title != "Dependencies" {
		t.Errorf("Setup children: %+v", setup.Children)
	}

	if deps := findSection(doc, "Dependencies"); deps == nil || !strings.Contains(deps.Content, "Go 1.25") {
		t.Errorf("Dependencies content missing expected text: %+v", deps)
	}
}

func TestMarkdownAccepts(t *testing.T) {
	p := parser.NewMarkdown()
	cases := []struct {
		ct, fn string
		want   bool
	}{
		{"text/markdown", "", true},
		{"", "README.md", true},
		{"", "notes.MARKDOWN", true},
		{"text/html", "", false},
		{"application/pdf", "paper.pdf", false},
	}
	for _, c := range cases {
		if got := p.Accepts(c.ct, c.fn); got != c.want {
			t.Errorf("Accepts(%q,%q)=%v want %v", c.ct, c.fn, got, c.want)
		}
	}
}

func TestHTMLParser(t *testing.T) {
	const src = `<!doctype html>
<html><head><title>Atlas</title></head>
<body>
  <nav>menu menu menu</nav>
  <main>
    <h1>Atlas</h1>
    <p>Intro paragraph.</p>
    <h2>Setup</h2>
    <p>Install the thing.</p>
    <h2>Usage</h2>
    <p>Run engine.</p>
  </main>
  <footer>© 2026</footer>
</body></html>`
	p := parser.NewHTML()
	doc, err := p.Parse(context.Background(), strings.NewReader(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if doc.Title != "Atlas" {
		t.Errorf("title: got %q", doc.Title)
	}
	if setup := findSection(doc, "Setup"); setup == nil {
		t.Fatal(`missing "Setup"`)
	}
	if usage := findSection(doc, "Usage"); usage == nil || !strings.Contains(usage.Content, "Run engine") {
		t.Errorf("Usage: %+v", usage)
	}
	// Chrome must be stripped.
	for _, s := range doc.Flatten() {
		if strings.Contains(s.Content, "menu menu menu") {
			t.Errorf("nav content leaked into section %q: %q", s.Title, s.Content)
		}
		if strings.Contains(s.Content, "© 2026") {
			t.Errorf("footer content leaked into section %q: %q", s.Title, s.Content)
		}
	}
}

func TestTextParser(t *testing.T) {
	p := parser.NewText()
	doc, err := p.Parse(context.Background(), strings.NewReader("Hello world.\nSecond line."))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if doc.Title != "Hello world." {
		t.Errorf("title: %q", doc.Title)
	}
	if len(doc.Sections) != 1 || !strings.Contains(doc.Sections[0].Content, "Second line") {
		t.Errorf("sections: %+v", doc.Sections)
	}
}

// TestDOCXParser assembles a minimal valid .docx in-memory and feeds it
// through the parser. Keeps the test self-contained (no binary fixtures).
func TestDOCXParser(t *testing.T) {
	const documentXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:body>
    <w:p>
      <w:pPr><w:pStyle w:val="Heading1"/></w:pPr>
      <w:r><w:t>Project Atlas</w:t></w:r>
    </w:p>
    <w:p>
      <w:r><w:t>Intro paragraph.</w:t></w:r>
    </w:p>
    <w:p>
      <w:pPr><w:pStyle w:val="Heading2"/></w:pPr>
      <w:r><w:t>Setup</w:t></w:r>
    </w:p>
    <w:p>
      <w:r><w:t>Install the thing.</w:t></w:r>
    </w:p>
    <w:p>
      <w:pPr><w:pStyle w:val="Heading 2"/></w:pPr>
      <w:r><w:t>Usage</w:t></w:r>
    </w:p>
    <w:p>
      <w:r><w:t>Run the engine.</w:t></w:r>
    </w:p>
  </w:body>
</w:document>`

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(documentXML)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	p := parser.NewDOCX()
	doc, err := p.Parse(context.Background(), bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if doc.Title != "Project Atlas" {
		t.Errorf("title: got %q", doc.Title)
	}
	setup := findSection(doc, "Setup")
	if setup == nil {
		t.Fatal(`missing "Setup"`)
	}
	if !strings.Contains(setup.Content, "Install the thing") {
		t.Errorf("Setup content: %q", setup.Content)
	}
	// Both "Heading2" (Word) and "Heading 2" (LibreOffice) spellings work.
	if usage := findSection(doc, "Usage"); usage == nil {
		t.Fatal(`missing "Usage" (spelled with space)`)
	}
}

func TestRegistryRouting(t *testing.T) {
	reg := parser.NewRegistry(parser.NewMarkdown(), parser.NewHTML(), parser.NewText())

	if p := reg.For("text/markdown", ""); p == nil || p.Name() != "markdown" {
		t.Errorf("markdown routing: %+v", p)
	}
	if p := reg.For("text/html", "index.html"); p == nil || p.Name() != "html" {
		t.Errorf("html routing: %+v", p)
	}
	if p := reg.For("", "notes.txt"); p == nil || p.Name() != "text" {
		t.Errorf("txt fallback: %+v", p)
	}
	if p := reg.For("application/octet-stream", "x.bin"); p != nil {
		t.Errorf("expected no match for unknown type, got %s", p.Name())
	}
}
