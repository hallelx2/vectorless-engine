package db

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// TestTOCTreeRoundTrip confirms a []tree.TOCNode marshals to JSON
// bytes that, when shoved through Document.TOCTree and pulled back
// out, decode to the same shape. The DB column stores the bytes
// verbatim so this is really a guard on the JSON tag contract —
// dropping a tag or renaming a field breaks downstream consumers
// that depend on the stable wire shape.
func TestTOCTreeRoundTrip(t *testing.T) {
	in := []tree.TOCNode{
		{
			NodeID:    "toc_1",
			Structure: "1",
			Title:     "Business",
			StartPage: 1,
			EndPage:   12,
			Nodes: []tree.TOCNode{
				{NodeID: "toc_1_1", Structure: "1.1", Title: "Overview", StartPage: 1, EndPage: 4},
				{NodeID: "toc_1_2", Structure: "1.2", Title: "Strategy", StartPage: 5, EndPage: 12},
			},
		},
		{
			NodeID:    "toc_2",
			Structure: "2",
			Title:     "Risk Factors",
			StartPage: 13,
			EndPage:   38,
		},
	}

	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var out []tree.TOCNode
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("top-level len: got %d want %d", len(out), len(in))
	}
	for i := range in {
		assertTOCNodeEq(t, &out[i], &in[i])
	}

	// Re-marshal and check byte-stable form so persisting and
	// re-reading never quietly changes content. JSON encoding is
	// deterministic for a fixed key order; our struct tags fix that.
	raw2, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !bytes.Equal(raw, raw2) {
		t.Errorf("round-trip changed bytes\n  first:  %s\n  second: %s", raw, raw2)
	}
}

// TestTOCTreeOmitsZeroFields guards the wire contract: optional
// fields (EndPage, Summary, Nodes) drop out of the serialised form
// when zero, so the persisted blob stays small and free of noise.
func TestTOCTreeOmitsZeroFields(t *testing.T) {
	in := []tree.TOCNode{{NodeID: "toc_x", Structure: "1", Title: "Stub", StartPage: 7}}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(raw)
	for _, banned := range []string{"end_page", "summary", "nodes"} {
		if bytes.Contains(raw, []byte(banned)) {
			t.Errorf("expected %q to be omitted, got %s", banned, s)
		}
	}
}

func assertTOCNodeEq(t *testing.T, got, want *tree.TOCNode) {
	t.Helper()
	if got.NodeID != want.NodeID {
		t.Errorf("NodeID: got %q want %q", got.NodeID, want.NodeID)
	}
	if got.Structure != want.Structure {
		t.Errorf("Structure: got %q want %q", got.Structure, want.Structure)
	}
	if got.Title != want.Title {
		t.Errorf("Title: got %q want %q", got.Title, want.Title)
	}
	if got.StartPage != want.StartPage {
		t.Errorf("StartPage: got %d want %d", got.StartPage, want.StartPage)
	}
	if got.EndPage != want.EndPage {
		t.Errorf("EndPage: got %d want %d", got.EndPage, want.EndPage)
	}
	if got.Summary != want.Summary {
		t.Errorf("Summary: got %q want %q", got.Summary, want.Summary)
	}
	if len(got.Nodes) != len(want.Nodes) {
		t.Errorf("Nodes len: got %d want %d", len(got.Nodes), len(want.Nodes))
		return
	}
	for i := range want.Nodes {
		assertTOCNodeEq(t, &got.Nodes[i], &want.Nodes[i])
	}
}
