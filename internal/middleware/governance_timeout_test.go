package middleware

import "testing"

// Answers got the 30 s default deadline while queries got 120 s, so a
// slower answer model or the treewalk answer was cut off mid-generation
// (HAL-2846).
func TestIsQueryPath(t *testing.T) {
	cases := map[string]bool{
		"/v1/query":                     true,
		"/v1/query/stream":              true,
		"/v1/query/multi":               true,
		"/v1/answer":                    true,
		"/v1/answer/treewalk":           true,
		"/v1/answer/store":              true,
		"/vectorless.v1.QueryService/Q": true,
		"/v1/documents":                 false,
		"/v1/documents/doc_1/tree":      false,
		"/v1/health":                    false,
	}
	for path, want := range cases {
		if got := isQueryPath(path); got != want {
			t.Errorf("isQueryPath(%q) = %v, want %v", path, got, want)
		}
	}
}
