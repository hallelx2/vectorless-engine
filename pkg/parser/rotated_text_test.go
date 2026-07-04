package parser

import "testing"

// TestIsRotatedRun locks the orientation filter that keeps rotated /
// vertical page furniture (arXiv margin stamps, watermarks) out of the
// extracted row stream while preserving upright prose in any script.
func TestIsRotatedRun(t *testing.T) {
	cases := []struct {
		name      string
		upright   bool
		direction string
		want      bool
	}{
		{"upright ltr prose is kept", true, "ltr", false},
		{"upright rtl prose is kept", true, "rtl", false},
		{"upright with empty direction is kept", true, "", false},
		{"not upright is dropped", false, "ltr", true},
		{"vertical top-to-bottom is dropped", true, "ttb", true},
		{"vertical bottom-to-top is dropped", true, "btt", true},
		{"vertical, case-insensitive", true, "TTB", true},
		{"not upright AND vertical is dropped", false, "btt", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRotatedRun(tc.upright, tc.direction); got != tc.want {
				t.Errorf("isRotatedRun(%v, %q) = %v, want %v", tc.upright, tc.direction, got, tc.want)
			}
		})
	}
}
