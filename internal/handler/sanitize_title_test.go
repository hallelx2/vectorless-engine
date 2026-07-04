package handler

import "testing"

func TestSanitizeTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Attention Is All You Need", "Attention Is All You Need"},
		{"  spaced   out  title ", "spaced out title"},
		{"Berkshire — 2023", "Berkshire — 2023"}, // valid UTF-8 em-dash is kept
		{"bad\xff\xfebyte", "badbyte"},            // invalid UTF-8 bytes dropped
		{"line\nbreak\ttab", "line break tab"},    // control chars → space, collapsed
		{"\xff\xfe", ""},                          // all-garbage collapses to empty
		{"", ""},
	}
	for _, c := range cases {
		if got := sanitizeTitle(c.in); got != c.want {
			t.Errorf("sanitizeTitle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
