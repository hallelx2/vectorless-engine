package tree

import (
	"reflect"
	"testing"
)

// sec is a tiny helper for building a parser section with a page range.
func sec(id string, start, end int, children ...*Section) *Section {
	return &Section{
		ID:        SectionID(id),
		Title:     id, // parser title is deliberately non-semantic here
		PageStart: start,
		PageEnd:   end,
		Children:  children,
	}
}

// financialTOC mirrors the shape an SEC-filing TOC builder produces:
// Part II → Item 8 → the individual statements, with the deepest nodes
// carrying the headings the gold anchors are written against.
func financialTOC() []TOCNode {
	return []TOCNode{
		{
			Structure: "1", Title: "Part I", StartPage: 1, EndPage: 9,
		},
		{
			Structure: "2", Title: "Part II", StartPage: 10, EndPage: 60,
			Nodes: []TOCNode{
				{
					Structure: "2.1", Title: "Item 7 — MD&A", StartPage: 10, EndPage: 39,
				},
				{
					Structure: "2.2", Title: "Item 8 — Financial Statements", StartPage: 40, EndPage: 60,
					Nodes: []TOCNode{
						{Structure: "2.2.1", Title: "Balance Sheet", StartPage: 41, EndPage: 42},
						{Structure: "2.2.2", Title: "Statements of Operations", StartPage: 43, EndPage: 45},
						// Open-ended last child: EndPage 0 must resolve to the
						// parent's end (60), not leak past it.
						{Structure: "2.2.3", Title: "Notes to Financial Statements", StartPage: 46, EndPage: 0},
					},
				},
			},
		},
	}
}

func TestBuildHeadingPaths_DeepestContainingWins(t *testing.T) {
	// A content leaf sitting inside the Balance Sheet pages must map to
	// the full logical path ending at the most specific heading.
	root := sec("root", 0, 0,
		sec("sec_balance", 41, 41),
		sec("sec_ops", 44, 44),
	)
	got := BuildHeadingPaths(root, financialTOC())

	want := map[SectionID][]string{
		"sec_balance": {"Part II", "Item 8 — Financial Statements", "Balance Sheet"},
		"sec_ops":     {"Part II", "Item 8 — Financial Statements", "Statements of Operations"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("heading paths mismatch:\n got=%v\nwant=%v", got, want)
	}
}

func TestBuildHeadingPaths_OpenEndedLastChildBoundedByParent(t *testing.T) {
	// pages 50 fall under the open-ended "Notes" node, which must resolve
	// its end from the parent's end page (60).
	root := sec("root", 0, 0, sec("sec_notes", 50, 50))
	got := BuildHeadingPaths(root, financialTOC())

	want := []string{"Part II", "Item 8 — Financial Statements", "Notes to Financial Statements"}
	if !reflect.DeepEqual(got["sec_notes"], want) {
		t.Fatalf("open-ended child path:\n got=%v\nwant=%v", got["sec_notes"], want)
	}
}

func TestBuildHeadingPaths_SectionUnderTopLevelOnly(t *testing.T) {
	// A section in Part I (which has no children) maps to just that node.
	root := sec("root", 0, 0, sec("sec_intro", 3, 4))
	got := BuildHeadingPaths(root, financialTOC())
	if want := []string{"Part I"}; !reflect.DeepEqual(got["sec_intro"], want) {
		t.Fatalf("top-level-only path: got=%v want=%v", got["sec_intro"], want)
	}
}

func TestBuildHeadingPaths_StraddlingSectionPicksBestOverlap(t *testing.T) {
	// A coarse section spanning 41-44 isn't fully contained by any single
	// statement node; it overlaps Balance Sheet (41-42, 2 pages) and
	// Statements of Operations (43-45, 2 pages). With equal overlap the
	// container check fails for both, so the deeper tie resolves to the
	// parent that DOES contain it: Item 8.
	root := sec("root", 0, 0, sec("sec_wide", 41, 44))
	got := BuildHeadingPaths(root, financialTOC())

	want := []string{"Part II", "Item 8 — Financial Statements"}
	if !reflect.DeepEqual(got["sec_wide"], want) {
		t.Fatalf("straddling section path:\n got=%v\nwant=%v", got["sec_wide"], want)
	}
}

func TestBuildHeadingPaths_NoPageRangeSkipped(t *testing.T) {
	// Non-paginated sections (PageStart/End 0) must not appear in the map.
	root := sec("root", 0, 0,
		&Section{ID: "sec_nopages", Title: "Intro"}, // no pages
		sec("sec_p", 41, 41),
	)
	got := BuildHeadingPaths(root, financialTOC())
	if _, ok := got["sec_nopages"]; ok {
		t.Fatalf("section without a page range should be absent, got %v", got["sec_nopages"])
	}
	if _, ok := got["sec_p"]; !ok {
		t.Fatalf("paginated section should be present")
	}
}

func TestBuildHeadingPaths_EmptyTOCDegrades(t *testing.T) {
	root := sec("root", 0, 0, sec("sec_a", 1, 2))
	if got := BuildHeadingPaths(root, nil); len(got) != 0 {
		t.Fatalf("nil TOC must yield an empty map, got %v", got)
	}
	if got := BuildHeadingPaths(root, []TOCNode{}); got == nil || len(got) != 0 {
		t.Fatalf("empty TOC must yield a non-nil empty map, got %v", got)
	}
}

func TestBuildHeadingPaths_NilRootSafe(t *testing.T) {
	if got := BuildHeadingPaths(nil, financialTOC()); got == nil || len(got) != 0 {
		t.Fatalf("nil root must yield a non-nil empty map, got %v", got)
	}
}

func TestBuildHeadingPaths_OutsideTOCRangeAbsent(t *testing.T) {
	// A section on a page beyond every TOC node's reach maps to nothing
	// rather than guessing.
	root := sec("root", 0, 0, sec("sec_far", 999, 1000))
	got := BuildHeadingPaths(root, financialTOC())
	if _, ok := got["sec_far"]; ok {
		t.Fatalf("section outside all TOC ranges should be absent, got %v", got["sec_far"])
	}
}

func TestBuildHeadingPaths_EmptyTitleNodeSkippedInPath(t *testing.T) {
	// A structural wrapper node with no title must not inject a blank
	// segment, but its children still inherit correct ancestry.
	toc := []TOCNode{
		{
			Structure: "", Title: "", StartPage: 1, EndPage: 20,
			Nodes: []TOCNode{
				{Structure: "1", Title: "Overview", StartPage: 1, EndPage: 20},
			},
		},
	}
	root := sec("root", 0, 0, sec("sec_x", 5, 6))
	got := BuildHeadingPaths(root, toc)
	if want := []string{"Overview"}; !reflect.DeepEqual(got["sec_x"], want) {
		t.Fatalf("empty-title wrapper should be skipped: got=%v want=%v", got["sec_x"], want)
	}
}

func TestBuildHeadingPaths_ResultIsDefensivelyCopied(t *testing.T) {
	root := sec("root", 0, 0, sec("sec_balance", 41, 41))
	got := BuildHeadingPaths(root, financialTOC())
	got["sec_balance"][0] = "MUTATED"
	// A second call must be unaffected by a caller mutating the first.
	again := BuildHeadingPaths(root, financialTOC())
	if again["sec_balance"][0] != "Part II" {
		t.Fatalf("returned slices must not alias internal state; got %v", again["sec_balance"])
	}
}
