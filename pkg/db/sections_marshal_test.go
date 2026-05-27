package db

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// TestMarshalCandidateQuestionsRoundTrip exercises the JSONB-marshal
// path that UpsertSection uses, so storing a list and reading it back
// reproduces the exact slice.
func TestMarshalCandidateQuestionsRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil → NULL → nil", nil, nil},
		{"empty → []", []string{}, []string{}},
		{"basic", []string{"Q1", "Q2", "Q3"}, []string{"Q1", "Q2", "Q3"}},
		{"unicode + punctuation",
			[]string{"What is § 12(b)?", "How do we use “smart” quotes?"},
			[]string{"What is § 12(b)?", "How do we use “smart” quotes?"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, err := marshalCandidateQuestions(c.in)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if c.in == nil {
				if raw != nil {
					t.Fatalf("nil input should produce nil (SQL NULL), got %v", raw)
				}
				// And unmarshaling NULL stays nil.
				if got := unmarshalCandidateQuestions(nil); got != nil {
					t.Errorf("unmarshal of NULL should be nil, got %v", got)
				}
				return
			}
			b, ok := raw.([]byte)
			if !ok {
				t.Fatalf("non-nil input should produce []byte, got %T", raw)
			}
			got := unmarshalCandidateQuestions(b)
			if len(got) != len(c.want) {
				t.Fatalf("len: got %v want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("idx %d: got %q want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

// TestUnmarshalCandidateQuestionsTolerant — non-JSON / garbled bytes
// should fall back to nil rather than panic. This guards against a
// future migration that backfills bad data; we'd rather lose the field
// silently than crash the whole listing endpoint.
func TestUnmarshalCandidateQuestionsTolerant(t *testing.T) {
	if got := unmarshalCandidateQuestions([]byte("not json")); got != nil {
		t.Errorf("garbled bytes should yield nil, got %v", got)
	}
}

func TestNullIfZero(t *testing.T) {
	if nullIfZero(0) != nil {
		t.Errorf("0 should be nil (SQL NULL)")
	}
	if nullIfZero(-3) != nil {
		t.Errorf("negative should be nil")
	}
	if v := nullIfZero(7); v != 7 {
		t.Errorf("non-zero should pass through, got %v", v)
	}
}

func TestScanNullableInt(t *testing.T) {
	if got := scanNullableInt(sql.NullInt64{Valid: false}); got != 0 {
		t.Errorf("NULL should scan to 0, got %d", got)
	}
	if got := scanNullableInt(sql.NullInt64{Valid: true, Int64: 42}); got != 42 {
		t.Errorf("got %d, want 42", got)
	}
}

// TestMarshalSummaryAxesRoundTrip exercises the JSONB-marshal path that
// UpsertSection + UpdateSectionSummaryAxes use, so storing a structured
// summary and reading it back reproduces the same object.
func TestMarshalSummaryAxesRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   *tree.SummaryAxes
	}{
		{"nil → NULL → nil", nil},
		{"empty object", &tree.SummaryAxes{}},
		{"only one_line", &tree.SummaryAxes{OneLine: "A short sentence."}},
		{"full shape", &tree.SummaryAxes{
			Topics:   []string{"debt", "long-term-obligations"},
			Entities: []string{"3M Company", "JPMorgan", "Q3 2024"},
			Numbers:  []string{"$4.2B", "2.8%"},
			OneLine:  "Long-term debt issued by 3M Company and serviced via JPMorgan.",
		}},
		{"unicode + punctuation", &tree.SummaryAxes{
			Topics:   []string{"§-12(b)", "côté"},
			Entities: []string{"Renée Müller"},
			Numbers:  []string{"€1,234"},
			OneLine:  "Une phrase en français — avec des “guillemets” et un § 12(b).",
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, err := marshalSummaryAxes(c.in)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if c.in == nil {
				if raw != nil {
					t.Fatalf("nil input should produce nil (SQL NULL), got %v", raw)
				}
				if got := unmarshalSummaryAxes(nil); got != nil {
					t.Errorf("unmarshal of NULL should be nil, got %v", got)
				}
				return
			}
			b, ok := raw.([]byte)
			if !ok {
				t.Fatalf("non-nil input should produce []byte, got %T", raw)
			}
			got := unmarshalSummaryAxes(b)
			if got == nil {
				t.Fatalf("roundtrip lost the value; raw=%s", string(b))
			}
			if got.OneLine != c.in.OneLine {
				t.Errorf("OneLine: got %q want %q", got.OneLine, c.in.OneLine)
			}
			assertStringSliceEq(t, "Topics", got.Topics, c.in.Topics)
			assertStringSliceEq(t, "Entities", got.Entities, c.in.Entities)
			assertStringSliceEq(t, "Numbers", got.Numbers, c.in.Numbers)
		})
	}
}

// TestUnmarshalSummaryAxesTolerant — non-JSON / garbled bytes should
// fall back to nil rather than panic. Guards listing endpoints against
// a stray bad row.
func TestUnmarshalSummaryAxesTolerant(t *testing.T) {
	if got := unmarshalSummaryAxes([]byte("not json")); got != nil {
		t.Errorf("garbled bytes should yield nil, got %v", got)
	}
}

// TestMarshalSummaryAxesEmptyObjectIsNotNull guards the contract that a
// non-nil pointer always serialises to JSONB (not SQL NULL). Lets the
// pipeline distinguish "we generated axes but every list is empty"
// from "axes were never generated".
func TestMarshalSummaryAxesEmptyObjectIsNotNull(t *testing.T) {
	raw, err := marshalSummaryAxes(&tree.SummaryAxes{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	b, ok := raw.([]byte)
	if !ok {
		t.Fatalf("empty axes should produce []byte, got %T", raw)
	}
	// Should be a JSON object — `{}` after omitempty stripping.
	var probe map[string]any
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("marshalled output is not a JSON object: %s", string(b))
	}
}

func assertStringSliceEq(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s len: got %v want %v", label, got, want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s[%d]: got %q want %q", label, i, got[i], want[i])
		}
	}
}
