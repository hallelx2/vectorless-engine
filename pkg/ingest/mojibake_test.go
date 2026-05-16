package ingest

import (
	"testing"
	"unicode/utf8"
)

func TestCleanForLLM(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// Valid text passes through unchanged.
		{"Hello world", "Hello world"},
		{"Section title — with em dash", "Section title — with em dash"},
		{"Multi-line\nbody\nstays intact", "Multi-line\nbody\nstays intact"},
		// Invalid bytes get replaced with U+FFFD.
		{"bad \xff byte", "bad � byte"},
		// NUL + most C0 controls get dropped; tab/newline/CR are kept.
		{"keep\ttabs\nand\rcrs but drop\x00nul", "keep\ttabs\nand\rcrs but dropnul"},
		{"text with bell\x07char", "text with bellchar"},
		// Empty input survives.
		{"", ""},
	}
	for _, tc := range cases {
		got := cleanForLLM(tc.in)
		if got != tc.want {
			t.Errorf("cleanForLLM(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("cleanForLLM(%q) returned invalid UTF-8", tc.in)
		}
	}
}

func TestIsLikelyMojibakeTitle(t *testing.T) {
	cases := []struct {
		in      string
		mojiBad bool
	}{
		// Real titles that must NOT be flagged.
		{"Global Strategy for Asthma Management and Prevention", false},
		{"Attention Is All You Need", false},
		{"The Pragmatic Programmer", false},
		{"Q1 2026 Financial Report", false},
		{"Annual Review 2025", false},
		{"a", true}, // too few letters
		{"PDF", true},
		{"GINA-2025-Update-25_11_08-WMS.pdf", false}, // filenames are valid
		{"book", false},                              // exactly 4 letters
		// The GINA watermark interleaving cases we actually saw.
		{"GGlloobbaall  SSttrraatteeggyy  ffoorr", true},
		{"AAsstthhmmaa  MMaannaaggeemmeenntt", true},
		{"aanndd  PPrreevveennttiioonn", true},
		// Edge cases.
		{"", true},
		{"   ", true},
		{"Hello", false},
		{"HHHHHHHHH", true}, // pure repetition
		// Some abbreviation-heavy titles (common in academic papers).
		{"BERT: Pre-training of Deep Bidirectional Transformers", false},
	}
	for _, tc := range cases {
		got := isLikelyMojibakeTitle(tc.in)
		if got != tc.mojiBad {
			t.Errorf("isLikelyMojibakeTitle(%q) = %v, want %v", tc.in, got, tc.mojiBad)
		}
	}
}
