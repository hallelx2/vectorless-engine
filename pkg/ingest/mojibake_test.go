package ingest

import "testing"

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
