// Package calc does an answer's arithmetic in code.
//
// The answer model sets a calculation up from figures on the evidence pages
// and writes it as <<expression>>; Resolve evaluates every one and puts the
// value in its place, and Trace finds each operand on the evidence so the
// figure can be checked back to its page. A model that sets a calculation up
// correctly still gets the arithmetic wrong: on FinanceBench,
// gemini-2.5-flash-lite wrote 365 * 1,380.5 / 7,890 = 67.75, which is 63.86
// (HAL-3161).
//
// An expression holds numbers, + - * /, parentheses and signs, nothing
// else. Numbers are read the way a filing writes them: "$1,174" is 1174,
// "12.5%" is 0.125, "(1,234)" stays a parenthesised 1234.
package calc

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Instructions tell an answer model how to write a calculation so Resolve
// can do it. Every answer prompt that may derive a figure includes them.
const Instructions = "When the question asks for a figure the evidence lets you derive (a ratio, margin, growth rate, " +
	"change, average or sum), take each input from the evidence and set the calculation up, but do not work it out: " +
	"write it as <<expression>> using only numbers from the evidence, + - * / and parentheses, and it is computed " +
	"for you. Example: operating margin was <<11,512 / 86,392 * 100>>% [1]. Apply the question's own definitions " +
	"term by term. When the question asks for a judgement (is a business cyclical, what drove a change), answer it " +
	"from the facts the evidence states and cite them."

// ErrInvalid is any expression that is not plain arithmetic.
var ErrInvalid = errors.New("not an arithmetic expression")

// Operand is one number an expression used, as it was written.
type Operand struct {
	Text  string  `json:"text"`  // as written, e.g. "$11,512"
	Value float64 `json:"value"` // as read
	// Evidence is the 1-based evidence block the number was found in; 0
	// when it is on none of them.
	Evidence int `json:"evidence,omitempty"`
	Page     int `json:"page,omitempty"`
	// Found is false when the number appears on no evidence block: the
	// figure is the model's, not the document's.
	Found bool `json:"found"`
}

// Calculation is one evaluated <<expression>>.
type Calculation struct {
	Expression string    `json:"expression"`
	Value      *float64  `json:"value,omitempty"`
	Display    string    `json:"display,omitempty"`
	Error      string    `json:"error,omitempty"`
	Operands   []Operand `json:"operands,omitempty"`
}

// Resolve replaces every <<expression>> in text with its value followed by
// the expression, "63.8634 (= 365 * ...)", so a reader sees both. One that
// does not evaluate stays visible with a note, never a made-up number.
//
// A set-up may nest another (<<65,398 / <<(50,710 + 47,528) / 2>>>>): the
// innermost is worked out first and its value feeds the one around it
// (HAL-3245). Every level is recorded. An inner one that fails makes its
// outer one fail. A << with no >>, or a >> with no <<, is not computable,
// never a partial number.
func Resolve(text string) (string, []Calculation) {
	var (
		calcs []Calculation
		buf   strings.Builder
		opens []int // where each still-open << starts in buf
	)
	cut := func(start int) string {
		all := buf.String()
		buf.Reset()
		buf.WriteString(all[:start])
		return strings.TrimSpace(all[start:])
	}
	for i := 0; i < len(text); {
		switch {
		case strings.HasPrefix(text[i:], "<<"):
			opens = append(opens, buf.Len())
			i += 2
		case strings.HasPrefix(text[i:], ">>"):
			i += 2
			if len(opens) == 0 {
				calcs = append(calcs, Calculation{Expression: ">>", Error: "a >> with no <<"})
				continue
			}
			start := opens[len(opens)-1]
			opens = opens[:len(opens)-1]
			expr := cut(start)
			nested := len(opens) > 0
			c := evaluate(expr)
			calcs = append(calcs, c)
			switch {
			case c.Value == nil && nested:
				// the text stays, so the set-up around it fails too
				buf.WriteString("(" + expr + ")")
			case c.Value == nil:
				buf.WriteString(expr + " (could not be computed)")
			case nested:
				buf.WriteString(plain(*c.Value))
			default:
				buf.WriteString(c.Display + " (= " + expr + ")")
			}
		default:
			buf.WriteByte(text[i])
			i++
		}
	}
	for len(opens) > 0 { // a << never closed: everything after it is one failed set-up
		start := opens[len(opens)-1]
		opens = opens[:len(opens)-1]
		expr := cut(start)
		calcs = append(calcs, Calculation{Expression: expr, Error: "a << with no >>"})
		buf.WriteString(expr + " (could not be computed)")
	}
	return buf.String(), calcs
}

// evaluate is one set-up as a Calculation: its operands, and its value or
// why it has none.
func evaluate(expr string) Calculation {
	c := Calculation{Expression: expr}
	if ops, err := operands(expr); err == nil {
		c.Operands = ops
	}
	v, err := Evaluate(expr)
	if err != nil {
		c.Error = err.Error()
		return c
	}
	c.Value = &v
	c.Display = Format(v)
	return c
}

// plain writes an inner result back into its outer expression at full
// precision, in a form the number grammar reads (no exponent, no grouping).
func plain(v float64) string {
	s := strings.TrimRight(strings.TrimRight(strconv.FormatFloat(v, 'f', 12, 64), "0"), ".")
	if s == "" || s == "-0" {
		return "0"
	}
	return s
}

// Source is one evidence block an operand may come from.
type Source struct {
	Page int
	Text string
}

// Trace finds each operand of each calculation on the evidence, in order,
// and records the first block that states it.
func Trace(calcs []Calculation, sources []Source) {
	norm := make([]string, len(sources))
	for i, s := range sources {
		norm[i] = s.Text
	}
	for ci := range calcs {
		for oi := range calcs[ci].Operands {
			op := &calcs[ci].Operands[oi]
			for i := range sources {
				if containsNumber(norm[i], op) {
					op.Evidence, op.Page, op.Found = i+1, sources[i].Page, true
					break
				}
			}
		}
	}
}

// Format writes a value the way a reader would: no float noise, at most
// four decimals, thousands separated.
func Format(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return group(strconv.FormatInt(int64(v), 10))
	}
	s := strconv.FormatFloat(v, 'f', 4, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	intPart, frac, _ := strings.Cut(s, ".")
	out := group(intPart)
	if frac != "" {
		out += "." + frac
	}
	return out
}

func group(digits string) string {
	neg := strings.HasPrefix(digits, "-")
	digits = strings.TrimPrefix(digits, "-")
	var b strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// --- the evaluator: a recursive-descent parser over tokens ---

type tokKind int

const (
	tNum tokKind = iota
	tOp
	tLParen
	tRParen
)

type token struct {
	kind tokKind
	op   byte
	num  float64
	text string
}

// numberRE reads one number: an optional currency sign (a space may follow
// it), digits with thousands separators only in real groups ("1,174";
// "1,2" is not a number and fails rather than reading as 12), decimals and
// a percent sign.
var numberRE = regexp.MustCompile(`^(?:[$€£]\s?)?(?:(?:\d{1,3}(?:,\d{3})+|\d+)(?:\.\d+)?|\.\d+)%?`)

func tokenize(expr string) ([]token, error) {
	expr = strings.NewReplacer("×", "*", "÷", "/", "−", "-", "–", "-").Replace(expr)
	var toks []token
	for i := 0; i < len(expr); {
		c := expr[i]
		switch c {
		case ' ', '\t', '\n':
			i++
		case '+', '-', '*', '/':
			toks = append(toks, token{kind: tOp, op: c})
			i++
		case '(':
			toks = append(toks, token{kind: tLParen})
			i++
		case ')':
			toks = append(toks, token{kind: tRParen})
			i++
		default:
			m := numberRE.FindString(expr[i:])
			if m == "" {
				return nil, fmt.Errorf("%w: unexpected %q", ErrInvalid, expr[i:min(len(expr), i+12)])
			}
			v, err := readNumber(m)
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{kind: tNum, num: v, text: strings.TrimSpace(m)})
			i += len(m)
		}
	}
	if len(toks) == 0 {
		return nil, fmt.Errorf("%w: empty", ErrInvalid)
	}
	return toks, nil
}

func readNumber(s string) (float64, error) {
	pct := strings.HasSuffix(s, "%")
	clean := strings.Map(func(r rune) rune {
		if unicode.IsDigit(r) || r == '.' {
			return r
		}
		return -1
	}, s)
	v, err := strconv.ParseFloat(clean, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: number %q", ErrInvalid, s)
	}
	if pct {
		v /= 100
	}
	return v, nil
}

type parser struct {
	toks []token
	pos  int
}

func (p *parser) peek() *token {
	if p.pos < len(p.toks) {
		return &p.toks[p.pos]
	}
	return nil
}

// expr := term (("+"|"-") term)*
func (p *parser) expr() (float64, error) {
	v, err := p.term()
	if err != nil {
		return 0, err
	}
	for t := p.peek(); t != nil && t.kind == tOp && (t.op == '+' || t.op == '-'); t = p.peek() {
		p.pos++
		r, err := p.term()
		if err != nil {
			return 0, err
		}
		if t.op == '+' {
			v += r
		} else {
			v -= r
		}
	}
	return v, nil
}

// term := unary (("*"|"/") unary)*
func (p *parser) term() (float64, error) {
	v, err := p.unary()
	if err != nil {
		return 0, err
	}
	for t := p.peek(); t != nil && t.kind == tOp && (t.op == '*' || t.op == '/'); t = p.peek() {
		p.pos++
		r, err := p.unary()
		if err != nil {
			return 0, err
		}
		if t.op == '*' {
			v *= r
		} else {
			if r == 0 {
				return 0, fmt.Errorf("%w: division by zero", ErrInvalid)
			}
			v /= r
		}
	}
	return v, nil
}

// unary := ("+"|"-") unary | atom
func (p *parser) unary() (float64, error) {
	if t := p.peek(); t != nil && t.kind == tOp && (t.op == '+' || t.op == '-') {
		p.pos++
		v, err := p.unary()
		if t.op == '-' {
			v = -v
		}
		return v, err
	}
	return p.atom()
}

// atom := number | "(" expr ")"
func (p *parser) atom() (float64, error) {
	t := p.peek()
	if t == nil {
		return 0, fmt.Errorf("%w: ends early", ErrInvalid)
	}
	switch t.kind {
	case tNum:
		p.pos++
		return t.num, nil
	case tLParen:
		p.pos++
		v, err := p.expr()
		if err != nil {
			return 0, err
		}
		if c := p.peek(); c == nil || c.kind != tRParen {
			return 0, fmt.Errorf("%w: unclosed parenthesis", ErrInvalid)
		}
		p.pos++
		return v, nil
	}
	return 0, fmt.Errorf("%w: unexpected operator", ErrInvalid)
}

// Evaluate is the value of one arithmetic expression.
func Evaluate(expr string) (float64, error) {
	toks, err := tokenize(expr)
	if err != nil {
		return 0, err
	}
	p := &parser{toks: toks}
	v, err := p.expr()
	if err != nil {
		return 0, err
	}
	if p.pos != len(p.toks) {
		return 0, fmt.Errorf("%w: trailing input", ErrInvalid)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("%w: not a finite number", ErrInvalid)
	}
	return v, nil
}

// operands lists every number an expression uses, in order. Constants a
// formula brings (2 for an average, 100 for a percentage, 365 for days) are
// listed too; Trace marks which numbers the evidence states.
func operands(expr string) ([]Operand, error) {
	toks, err := tokenize(expr)
	if err != nil {
		return nil, err
	}
	var out []Operand
	for i, t := range toks {
		if t.kind != tNum {
			continue
		}
		// A unary sign belongs to the operand: "-12" is minus twelve, not
		// twelve. Trace still finds it by magnitude, because a filing
		// prints a negative as "(12)" as often as "-12".
		text, value := t.text, t.num
		for j := i - 1; unarySign(toks, j); j-- {
			text = string(toks[j].op) + text
			if toks[j].op == '-' {
				value = -value
			}
		}
		out = append(out, Operand{Text: text, Value: value})
	}
	return out, nil
}

// unarySign reports whether toks[i] is a sign on what follows it rather
// than a binary operator: it opens the expression or follows another
// operator or an opening parenthesis.
func unarySign(toks []token, i int) bool {
	if i < 0 || toks[i].kind != tOp || (toks[i].op != '+' && toks[i].op != '-') {
		return false
	}
	return i == 0 || toks[i-1].kind == tOp || toks[i-1].kind == tLParen
}

// containsNumber reports whether text states op's number: as written, or
// in the other common form (with or without thousands separators).
func containsNumber(text string, op *Operand) bool {
	digits := strings.Map(func(r rune) rune {
		if unicode.IsDigit(r) || r == '.' || r == ',' {
			return r
		}
		return -1
	}, op.Text)
	if digits == "" {
		return false
	}
	bare := strings.ReplaceAll(digits, ",", "")
	forms := []string{digits, bare}
	if intPart, frac, ok := strings.Cut(bare, "."); ok {
		forms = append(forms, group(intPart)+"."+frac)
	} else {
		forms = append(forms, group(bare))
	}
	for _, f := range forms {
		if f != "" && containsToken(text, f) {
			return true
		}
	}
	return false
}

// containsToken finds f in text as a whole number, not inside a longer one
// ("12" is not in "1,234" or "312").
func containsToken(text, f string) bool {
	for start := 0; ; {
		i := strings.Index(text[start:], f)
		if i < 0 {
			return false
		}
		i += start
		before := i == 0 || !isNumRune(text[i-1])
		end := i + len(f)
		after := end >= len(text) || !isNumRune(text[end]) ||
			// a sentence's full stop or a list comma after the number
			((text[end] == '.' || text[end] == ',') && (end+1 >= len(text) || !isDigit(text[end+1])))
		if before && after {
			return true
		}
		start = i + 1
	}
}

func isDigit(b byte) bool   { return b >= '0' && b <= '9' }
func isNumRune(b byte) bool { return isDigit(b) || b == '.' || b == ',' }
