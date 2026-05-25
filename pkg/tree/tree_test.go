package tree

import (
	"testing"
)

// buildTestTree creates a test tree:
//
//	root (sec_root)
//	├── ch1 (sec_1) — leaf
//	├── ch2 (sec_2)
//	│   ├── ch2a (sec_2a) — leaf
//	│   └── ch2b (sec_2b) — leaf
//	└── ch3 (sec_3) — leaf
func buildTestTree() *Tree {
	return &Tree{
		DocumentID: "doc_test",
		Title:      "Test Document",
		Root: &Section{
			ID:    "sec_root",
			Title: "Root",
			Children: []*Section{
				{
					ID:         "sec_1",
					ParentID:   "sec_root",
					Ordinal:    0,
					Title:      "Chapter 1",
					Summary:    "intro stuff",
					ContentRef: "docs/sec_1.txt",
					TokenCount: 500,
				},
				{
					ID:       "sec_2",
					ParentID: "sec_root",
					Ordinal:  1,
					Title:    "Chapter 2",
					Summary:  "main body",
					Children: []*Section{
						{
							ID:         "sec_2a",
							ParentID:   "sec_2",
							Ordinal:    0,
							Title:      "Section 2A",
							Summary:    "first part",
							ContentRef: "docs/sec_2a.txt",
							TokenCount: 300,
						},
						{
							ID:         "sec_2b",
							ParentID:   "sec_2",
							Ordinal:    1,
							Title:      "Section 2B",
							Summary:    "second part",
							ContentRef: "docs/sec_2b.txt",
							TokenCount: 400,
							Metadata:   map[string]string{"type": "code"},
						},
					},
				},
				{
					ID:         "sec_3",
					ParentID:   "sec_root",
					Ordinal:    2,
					Title:      "Chapter 3",
					ContentRef: "docs/sec_3.txt",
					TokenCount: 200,
				},
			},
		},
	}
}

func TestSectionIsLeaf(t *testing.T) {
	t.Parallel()
	tr := buildTestTree()

	// Leaves.
	if !tr.FindByID("sec_1").IsLeaf() {
		t.Error("sec_1 should be a leaf")
	}
	if !tr.FindByID("sec_2a").IsLeaf() {
		t.Error("sec_2a should be a leaf")
	}

	// Non-leaves.
	if tr.Root.IsLeaf() {
		t.Error("root should not be a leaf")
	}
	if tr.FindByID("sec_2").IsLeaf() {
		t.Error("sec_2 should not be a leaf (has children)")
	}
}

func TestFindByID(t *testing.T) {
	t.Parallel()
	tr := buildTestTree()

	cases := []struct {
		id    SectionID
		title string
	}{
		{"sec_root", "Root"},
		{"sec_1", "Chapter 1"},
		{"sec_2", "Chapter 2"},
		{"sec_2a", "Section 2A"},
		{"sec_2b", "Section 2B"},
		{"sec_3", "Chapter 3"},
	}
	for _, c := range cases {
		s := tr.FindByID(c.id)
		if s == nil {
			t.Errorf("FindByID(%q): not found", c.id)
			continue
		}
		if s.Title != c.title {
			t.Errorf("FindByID(%q): got title %q, want %q", c.id, s.Title, c.title)
		}
	}
}

func TestFindByIDNotFound(t *testing.T) {
	t.Parallel()
	tr := buildTestTree()
	if s := tr.FindByID("nonexistent"); s != nil {
		t.Errorf("expected nil for nonexistent ID, got %v", s)
	}
}

func TestWalk(t *testing.T) {
	t.Parallel()
	tr := buildTestTree()

	var visited []SectionID
	tr.Root.Walk(func(s *Section) bool {
		visited = append(visited, s.ID)
		return true
	})

	// Depth-first pre-order: root, ch1, ch2, ch2a, ch2b, ch3.
	want := []SectionID{"sec_root", "sec_1", "sec_2", "sec_2a", "sec_2b", "sec_3"}
	if len(visited) != len(want) {
		t.Fatalf("visited %d sections, want %d: %v", len(visited), len(want), visited)
	}
	for i, id := range want {
		if visited[i] != id {
			t.Errorf("visited[%d] = %q, want %q", i, visited[i], id)
		}
	}
}

func TestWalkEarlyStop(t *testing.T) {
	t.Parallel()
	tr := buildTestTree()

	var visited []SectionID
	tr.Root.Walk(func(s *Section) bool {
		visited = append(visited, s.ID)
		return s.ID != "sec_2" // stop when we hit sec_2
	})

	// Walk should stop descending into sec_2's children, but continue
	// visiting sec_3 (siblings are still visited because Walk returns
	// false only for the current subtree).
	//
	// In this implementation, returning false stops the entire subtree
	// rooted at that node (children are not visited).
	if len(visited) < 2 {
		t.Fatalf("visited too few sections: %v", visited)
	}
	// sec_2a and sec_2b should NOT be visited.
	for _, id := range visited {
		if id == "sec_2a" || id == "sec_2b" {
			t.Errorf("should not have visited %q after stopping at sec_2", id)
		}
	}
}

func TestWalkNilSection(t *testing.T) {
	t.Parallel()
	// Walk on nil should not panic.
	var s *Section
	s.Walk(func(sec *Section) bool {
		t.Fatal("should not be called")
		return true
	})
}

func TestBuildView(t *testing.T) {
	t.Parallel()
	tr := buildTestTree()
	view := tr.BuildView()

	if view.DocumentID != "doc_test" {
		t.Errorf("DocumentID = %q, want doc_test", view.DocumentID)
	}
	if view.Title != "Test Document" {
		t.Errorf("Title = %q, want Test Document", view.Title)
	}

	// Should have 6 sections in depth-first order.
	if len(view.Sections) != 6 {
		t.Fatalf("want 6 sections, got %d", len(view.Sections))
	}

	// Check depth-first ordering.
	wantIDs := []SectionID{"sec_root", "sec_1", "sec_2", "sec_2a", "sec_2b", "sec_3"}
	for i, sv := range view.Sections {
		if sv.ID != wantIDs[i] {
			t.Errorf("section[%d].ID = %q, want %q", i, sv.ID, wantIDs[i])
		}
	}

	// Check root has correct children.
	root := view.Sections[0]
	if len(root.Children) != 3 {
		t.Errorf("root children count = %d, want 3", len(root.Children))
	}

	// Check depth values.
	wantDepths := []int{0, 1, 1, 2, 2, 1}
	for i, sv := range view.Sections {
		if sv.Depth != wantDepths[i] {
			t.Errorf("section[%d] (%s) depth = %d, want %d", i, sv.ID, sv.Depth, wantDepths[i])
		}
	}

	// Check tokens are preserved.
	sec1 := view.Sections[1]
	if sec1.Tokens != 500 {
		t.Errorf("sec_1 tokens = %d, want 500", sec1.Tokens)
	}
}

func TestBuildViewEmptyTree(t *testing.T) {
	t.Parallel()
	tr := &Tree{DocumentID: "empty", Title: "Empty"}
	view := tr.BuildView()
	if len(view.Sections) != 0 {
		t.Errorf("empty tree should have 0 sections, got %d", len(view.Sections))
	}
}

func TestSectionMetadata(t *testing.T) {
	t.Parallel()
	tr := buildTestTree()
	sec := tr.FindByID("sec_2b")
	if sec == nil {
		t.Fatal("sec_2b not found")
	}
	if sec.Metadata["type"] != "code" {
		t.Errorf("metadata[type] = %q, want 'code'", sec.Metadata["type"])
	}
}
