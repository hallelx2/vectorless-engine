package pincite

import (
	"regexp"
	"strconv"
	"strings"
)

// markerRe matches an inline citation marker as the answer prompt asks
// for it: [3], or a group [1, 3] / [1][3].
var markerRe = regexp.MustCompile(`\[(\d+(?:\s*,\s*\d+)*)\]`)

// Marked is one marker in an answer: the evidence it cites and the
// claim it is attached to, which is the sentence it sits in with every
// marker removed.
type Marked struct {
	Evidence int    // 1-based evidence number the model cited
	Claim    string // the sentence the marker supports
	Ordinal  int    // position of the claim in the answer, for stable ids
}

// ParseMarkers validates the answer's markers against the evidence
// list, before the answer is returned. A marker pointing at nothing is
// worse than no marker, so every out-of-range number is stripped and
// counted. Markers inside one sentence that cite the same evidence twice
// collapse to one.
//
// It returns the marked claims in answer order and the number of
// markers stripped.
func ParseMarkers(answer string, evidence int) ([]Marked, int) {
	var (
		out      []Marked
		stripped int
		seen     = map[[2]int]bool{}
	)
	sentences := splitAnswer(answer)
	for si, s := range sentences {
		claim := tidy(strings.Join(strings.Fields(markerRe.ReplaceAllString(s, "")), " "))
		for _, m := range markerRe.FindAllStringSubmatch(s, -1) {
			for _, part := range strings.Split(m[1], ",") {
				n, err := strconv.Atoi(strings.TrimSpace(part))
				if err != nil || n < 1 || n > evidence {
					stripped++
					continue
				}
				k := [2]int{si, n}
				if seen[k] {
					continue
				}
				seen[k] = true
				out = append(out, Marked{Evidence: n, Claim: claim, Ordinal: si})
			}
		}
	}
	return out, stripped
}

// Renumber rewrites the answer's markers to pincite ids. ids maps a
// (sentence ordinal, evidence number) pair to the pincite id that pair
// became; a marker with no id — out of range, or a pincite that could
// not be built — is removed rather than left dangling.
func Renumber(answer string, ids map[[2]int]int) string {
	sentences := splitAnswer(answer)
	var b strings.Builder
	for si, s := range sentences {
		s = markerRe.ReplaceAllStringFunc(s, func(m string) string {
			inner := markerRe.FindStringSubmatch(m)[1]
			var keep []string
			used := map[int]bool{}
			for _, part := range strings.Split(inner, ",") {
				n, err := strconv.Atoi(strings.TrimSpace(part))
				if err != nil {
					continue
				}
				id, ok := ids[[2]int{si, n}]
				if !ok || used[id] {
					continue
				}
				used[id] = true
				keep = append(keep, "["+strconv.Itoa(id)+"]")
			}
			return strings.Join(keep, "")
		})
		b.WriteString(s)
	}
	return tidy(b.String())
}

// tidy closes the gap removing a marker leaves: "billion ." → "billion.".
func tidy(s string) string {
	for _, p := range []string{".", ",", ";", ":", "?", "!"} {
		s = strings.ReplaceAll(s, " "+p, p)
	}
	return s
}

// splitAnswer splits an answer into sentence-sized pieces that, joined,
// reproduce it exactly. A piece ends after a sentence terminator and
// any markers that follow it ("... $5.2 billion. [2] Next ..." keeps
// "[2]" with the sentence before it, because that is the sentence the
// model was citing), or at a newline.
func splitAnswer(s string) []string {
	var out []string
	start := 0
	rs := []rune(s)
	i := 0
	for i < len(rs) {
		r := rs[i]
		if r == '\n' {
			out = append(out, string(rs[start:i+1]))
			i++
			start = i
			continue
		}
		if (r == '.' || r == '?' || r == '!') && endsSentence(rs, i) {
			j := i + 1
			// Pull trailing markers (and the spaces between) into this piece.
			for {
				k := j
				for k < len(rs) && rs[k] == ' ' {
					k++
				}
				if loc := markerRe.FindStringIndex(string(rs[k:])); loc != nil && loc[0] == 0 {
					j = k + len([]rune(string(rs[k:])[:loc[1]]))
					continue
				}
				break
			}
			out = append(out, string(rs[start:j]))
			i = j
			start = j
			continue
		}
		i++
	}
	if start < len(rs) {
		out = append(out, string(rs[start:]))
	}
	return out
}

// endsSentence reports whether the terminator at i ends a sentence: it
// is followed by whitespace, a marker or the end, and is not the
// decimal point of a number ("$5.2").
func endsSentence(rs []rune, i int) bool {
	if i+1 >= len(rs) {
		return true
	}
	next := rs[i+1]
	if next >= '0' && next <= '9' {
		return false
	}
	return next == ' ' || next == '\n' || next == '['
}
