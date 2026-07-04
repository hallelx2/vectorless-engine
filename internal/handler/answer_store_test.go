package handler

import (
	"reflect"
	"testing"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

func TestParseStoreAnswer(t *testing.T) {
	t.Run("clean JSON", func(t *testing.T) {
		ans, cited := parseStoreAnswer(`{"answer":"Consent must be freely given [1] and specific [2].","cited":[1,2]}`, 3)
		if ans != "Consent must be freely given [1] and specific [2]." {
			t.Errorf("answer = %q", ans)
		}
		if !reflect.DeepEqual(cited, []int{1, 2}) {
			t.Errorf("cited = %v", cited)
		}
	})

	t.Run("JSON wrapped in code fence + prose", func(t *testing.T) {
		raw := "Here you go:\n```json\n{\"answer\":\"See [2].\",\"cited\":[2]}\n```"
		ans, cited := parseStoreAnswer(raw, 3)
		if ans != "See [2]." || !reflect.DeepEqual(cited, []int{2}) {
			t.Errorf("ans=%q cited=%v", ans, cited)
		}
	})

	t.Run("cited indices deduped + range-clamped", func(t *testing.T) {
		_, cited := parseStoreAnswer(`{"answer":"x [1]","cited":[1,1,2,9,0,-3]}`, 2)
		if !reflect.DeepEqual(cited, []int{1, 2}) {
			t.Errorf("cited = %v, want [1 2]", cited)
		}
	})

	t.Run("non-JSON falls back to raw text + scraped markers", func(t *testing.T) {
		ans, cited := parseStoreAnswer("The answer is grounded on [1] and [3].", 3)
		if ans != "The answer is grounded on [1] and [3]." {
			t.Errorf("answer = %q", ans)
		}
		if !reflect.DeepEqual(cited, []int{1, 3}) {
			t.Errorf("cited = %v, want [1 3]", cited)
		}
	})
}

func TestBuildStoreCitations(t *testing.T) {
	secs := []relevantSection{
		{index: 1, docID: "doc_a", docTitle: "GDPR", sectionID: "sec_1", sectionTitle: "Consent", startPage: 32, endPage: 33, content: "Consent must be freely given, specific, informed and unambiguous."},
		{index: 2, docID: "doc_b", docTitle: "PRISMA", sectionID: "sec_2", sectionTitle: "Reporting", startPage: 4, endPage: 4, content: "Report the review using the checklist."},
	}

	t.Run("cited subset in order with metadata", func(t *testing.T) {
		cits := buildStoreCitations(secs, []int{2})
		if len(cits) != 1 {
			t.Fatalf("want 1 citation, got %d", len(cits))
		}
		if cits[0]["document_id"] != tree.DocumentID("doc_b") || cits[0]["document_title"] != "PRISMA" {
			t.Errorf("wrong citation: %v", cits[0])
		}
		if cits[0]["start_page"] != 4 {
			t.Errorf("start_page = %v", cits[0]["start_page"])
		}
	})

	t.Run("empty cited falls back to all sections", func(t *testing.T) {
		cits := buildStoreCitations(secs, nil)
		if len(cits) != 2 {
			t.Fatalf("want 2 (fallback to all), got %d", len(cits))
		}
	})
}

func TestParseRelevant(t *testing.T) {
	got := parseRelevant(`{"relevant":[2,5,2,99,0]}`, 6)
	if len(got) != 2 || got[0] != 2 || got[1] != 5 {
		t.Errorf("parseRelevant = %v, want [2 5]", got)
	}
	if len(parseRelevant("nonsense no json", 6)) != 0 {
		t.Errorf("expected empty on non-JSON with no markers")
	}
}

func TestFormatPageRange(t *testing.T) {
	cases := []struct {
		s, e int
		want string
	}{
		{0, 0, ""},
		{5, 5, "p.5"},
		{5, 0, "p.5"},
		{32, 33, "pp.32-33"},
	}
	for _, c := range cases {
		if got := formatPageRange(c.s, c.e); got != c.want {
			t.Errorf("formatPageRange(%d,%d) = %q, want %q", c.s, c.e, got, c.want)
		}
	}
}
