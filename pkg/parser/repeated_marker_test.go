package parser

import "testing"

// TestIsRepeatedMarkerLine locks the filter that drops icon-glyph marker
// rows (e.g. an ORCID "iD" repeated once per author) while never touching
// real prose or short headings.
func TestIsRepeatedMarkerLine(t *testing.T) {
	drop := []string{
		"id id id id",        // 4/4 = 100%
		"iD iD iD iD",        // case-insensitive caller lower-cases first; test raw too
		"id id id",           // 3/3
		"id id id id author", // 4/5 = 80% ≥ 60%
	}
	keep := []string{
		"the right to erasure is established in article 17",
		"multi-head attention",                     // 2 tokens, below the 3-token floor
		"introduction",                             // single token
		"id id id penny whiting17, david moher 22", // 3 of 9 ≈ 33% → real author line, kept
		"id id elie a. akl 8, sue e. brennan",      // 2 of 8 → kept
	}
	for _, s := range drop {
		if !isRepeatedMarkerLine(s) {
			t.Errorf("expected DROP for %q", s)
		}
	}
	for _, s := range keep {
		if isRepeatedMarkerLine(s) {
			t.Errorf("expected KEEP for %q", s)
		}
	}
}
