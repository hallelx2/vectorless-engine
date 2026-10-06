package pincite

import (
	"sort"
	"strings"
	"unicode"
)

// maxCandidateChars caps one candidate. A run longer than this is a
// paragraph the splitter could not break, and highlighting all of it
// would claim more than the claim needs.
const maxCandidateChars = 420

// minCandidateTokens drops page furniture from the candidates.
const minCandidateTokens = 3

// Sentences splits a page into the units a pincite can point at:
// prose sentences, and table rows. A line that carries two or more
// numbers and no sentence ending is a table row (a 10-K's "Long-term
// debt ... 16,048 ... 15,233") and stands on its own, because joining
// it to the prose around it would make every figure on the page the
// same candidate.
func Sentences(p *Page) []string {
	if p == nil {
		return nil
	}
	var (
		out []string
		buf strings.Builder
	)
	flush := func() {
		s := strings.TrimSpace(buf.String())
		buf.Reset()
		if s == "" {
			return
		}
		out = append(out, splitLong(s)...)
	}
	for _, ln := range p.Lines {
		var lb strings.Builder
		for j, w := range ln.Words {
			if j > 0 {
				lb.WriteByte(' ')
			}
			lb.WriteString(w.Text)
		}
		line := strings.TrimSpace(lb.String())
		if line == "" {
			continue
		}
		if isPageNumber(line) {
			// Page furniture: it ends whatever run it interrupts and is
			// never a candidate itself.
			flush()
			continue
		}
		if isTableRow(line) || isHeading(line) {
			flush()
			out = append(out, line)
			continue
		}
		for line != "" {
			cut := sentenceEnd(line)
			if cut < 0 {
				if buf.Len() > 0 {
					buf.WriteByte(' ')
				}
				buf.WriteString(line)
				break
			}
			if buf.Len() > 0 {
				buf.WriteByte(' ')
			}
			buf.WriteString(line[:cut])
			flush()
			line = strings.TrimSpace(line[cut:])
		}
	}
	flush()
	return dedupe(out)
}

// sentenceEnd returns the byte index just past the first sentence
// terminator in s that is followed by a space and a capital, a digit or
// an opening quote, or -1. "U.S." and "approx. 5" stay whole because a
// terminator must also be preceded by a lower-case letter, a digit or a
// closing bracket.
func sentenceEnd(s string) int {
	rs := []rune(s)
	for i := 1; i < len(rs)-2; i++ {
		if rs[i] != '.' && rs[i] != '?' && rs[i] != '!' {
			continue
		}
		prev := rs[i-1]
		if !(unicode.IsLower(prev) || unicode.IsDigit(prev) || prev == ')' || prev == '%') {
			continue
		}
		if rs[i+1] != ' ' {
			continue
		}
		next := rs[i+2]
		if unicode.IsUpper(next) || unicode.IsDigit(next) || next == '"' || next == '“' || next == '(' {
			return len(string(rs[:i+1]))
		}
	}
	if n := len(rs); n > 0 && (rs[n-1] == '.' || rs[n-1] == '?' || rs[n-1] == '!') {
		return len(s)
	}
	return -1
}

func isTableRow(line string) bool {
	if strings.HasSuffix(line, ".") {
		return false
	}
	fields := strings.Fields(line)
	numeric := func(f string) bool {
		f = strings.Trim(f, "$()%,")
		if f == "" {
			return false
		}
		digits := 0
		for _, r := range f {
			if unicode.IsDigit(r) {
				digits++
			}
		}
		return digits > 0 && digits*2 >= len(f)
	}
	nums := 0
	for _, f := range fields {
		if numeric(f) {
			nums++
		}
	}
	if nums < 2 {
		return false
	}
	// A row label of any length followed by its figures: the line ends
	// in two numeric columns ("Total long-term debt, including current
	// portion $ 15,939 $ 17,347").
	var tail []string
	for i := len(fields) - 1; i >= 0 && len(tail) < 2; i-- {
		if fields[i] == "$" {
			continue
		}
		tail = append(tail, fields[i])
	}
	if len(tail) == 2 && numeric(tail[0]) && numeric(tail[1]) {
		return true
	}
	// Otherwise figures must be a real share of the line: prose that
	// cites "Item 1" and "Note 19" is still prose.
	return nums*3 >= len(fields)
}

// isPageNumber: a line that is only a number or two ("28", "F-12").
func isPageNumber(line string) bool {
	fields := strings.Fields(line)
	if len(fields) > 2 {
		return false
	}
	for _, f := range fields {
		for _, r := range f {
			if !unicode.IsDigit(r) && r != '-' && !unicode.IsUpper(r) {
				return false
			}
		}
		if !strings.ContainsFunc(f, unicode.IsDigit) {
			return false
		}
	}
	return len(fields) > 0
}

// isHeading: a short line with no lower-case letters ("PERFORMANCE BY
// BUSINESS SEGMENT") is a heading, never the start of the sentence on
// the next line.
func isHeading(line string) bool {
	if len(strings.Fields(line)) > 12 {
		return false
	}
	letters := 0
	for _, r := range line {
		if unicode.IsLower(r) {
			return false
		}
		if unicode.IsLetter(r) {
			letters++
		}
	}
	return letters >= 3
}

func splitLong(s string) []string {
	if len(s) <= maxCandidateChars {
		return []string{s}
	}
	var out []string
	words := strings.Fields(s)
	var b strings.Builder
	for _, w := range words {
		if b.Len()+len(w)+1 > maxCandidateChars && b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(w)
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		toks := Tokenize(s)
		// A page number or a one-word heading is not something a claim
		// can rest on.
		if len(toks) < minCandidateTokens {
			continue
		}
		k := strings.Join(toks, " ")
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, s)
	}
	return out
}

// Shortlist ranks candidates by how many of the claim's content tokens
// each contains and keeps the best k, in page order. It is a cheap,
// deterministic prefilter so the Judge chooses among a handful of
// plausible sentences rather than a whole page.
func Shortlist(claim string, candidates []string, k int) []int {
	ct := map[string]bool{}
	for _, t := range Tokenize(claim) {
		if !stopword[t] {
			ct[t] = true
		}
	}
	type scored struct {
		i     int
		score float64
	}
	var ss []scored
	for i, c := range candidates {
		seen := map[string]bool{}
		hits := 0.0
		for _, t := range Tokenize(c) {
			if ct[t] && !seen[t] {
				seen[t] = true
				// Figures carry the answer in a filing; a shared number
				// is far stronger evidence than a shared word.
				if isNumeric(t) {
					hits += 3
				} else {
					hits++
				}
			}
		}
		if hits > 0 {
			ss = append(ss, scored{i, hits})
		}
	}
	sort.SliceStable(ss, func(a, b int) bool { return ss[a].score > ss[b].score })
	if len(ss) > k {
		ss = ss[:k]
	}
	out := make([]int, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.i)
	}
	sort.Ints(out)
	return out
}

func isNumeric(t string) bool {
	for _, r := range t {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return t != ""
}

var stopword = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`a an and are as at be by for from has have in is it its of on or that the this to was were which with
		our we us their they he she not no than then so such per into over under also any all each other`) {
		m[w] = true
	}
	return m
}()
