package db

import (
	"database/sql"
	"testing"
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
