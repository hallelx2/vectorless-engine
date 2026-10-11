package handler

import "testing"

func TestParsePageRange(t *testing.T) {
	for in, want := range map[string][2]int{"7": {7, 7}, "7-14": {7, 14}, " 3 - 4 ": {3, 4}} {
		a, b, err := parsePageRange(in)
		if err != nil || a != want[0] || b != want[1] {
			t.Errorf("%q = %d-%d, %v; want %d-%d", in, a, b, err, want[0], want[1])
		}
	}
	for _, bad := range []string{"", "0", "x", "9-3", "4-x", "-2"} {
		if _, _, err := parsePageRange(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}
