package parser

import "strings"

// flatSection is the document-order (level, title, content) tuple every
// structured parser produces in its first pass, before the nested Section
// tree is assembled. Level 0 marks a preamble bucket (content that appears
// before the first heading).
type flatSection struct {
	Level   int
	Title   string
	Content string
}

// dropEmptyPreamble removes a leading empty level-0 bucket when at least
// one real section follows it. A preamble that actually has content is
// kept so buildSections can promote it to an "Introduction".
func dropEmptyPreamble(flats []flatSection) []flatSection {
	if len(flats) > 1 && flats[0].Level == 0 && strings.TrimSpace(flats[0].Content) == "" {
		return flats[1:]
	}
	return flats
}

// deriveTitle picks the document title from a flat section list: the first
// level-1 heading if present, otherwise the first bucket's title. Callers
// that have a better source (an HTML <title>, DOCX core properties) should
// prefer that and fall back to this.
func deriveTitle(flats []flatSection) string {
	for _, f := range flats {
		if f.Level == 1 {
			return f.Title
		}
	}
	if len(flats) > 0 {
		return flats[0].Title
	}
	return ""
}

// buildSections assembles a document-order flat list into a nested Section
// tree using a level stack: the stack top is always the most recent
// ancestor, and each new section is hung off the nearest strictly
// shallower ancestor. A level-0 bucket with content is promoted to a
// level-1 "Introduction"; an empty one is dropped.
//
// This is the single shared implementation behind the Markdown, HTML, and
// DOCX parsers — they differ only in how they extract text per format, not
// in how the outline is shaped.
func buildSections(flats []flatSection) []Section {
	root := &Section{Level: 0}
	stack := []*Section{root}

	for _, f := range flats {
		sec := Section{Level: f.Level, Title: f.Title, Content: f.Content}
		if f.Level == 0 {
			if strings.TrimSpace(sec.Content) == "" {
				continue
			}
			sec.Level = 1
			sec.Title = "Introduction"
		}
		// Pop until the stack top is strictly shallower than this section.
		for len(stack) > 1 && stack[len(stack)-1].Level >= sec.Level {
			stack = stack[:len(stack)-1]
		}
		parent := stack[len(stack)-1]
		parent.Children = append(parent.Children, sec)
		// The freshly appended child is addressable via the slice tail.
		stack = append(stack, &parent.Children[len(parent.Children)-1])
	}
	return root.Children
}
