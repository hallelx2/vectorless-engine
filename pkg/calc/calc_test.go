package calc

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestEvaluate(t *testing.T) {
	cases := []struct {
		expr string
		want float64
	}{
		{"2112 / 14082 * 100", 14.99787},
		// FinanceBench Amazon FY2017 DPO, set up as the question defines it
		{"365 * ((34,616 + 25,309) / 2) / (111,934 + (16,047 - 11,461))", 93.8578},
		// Corning FY2020 DPO: the model wrote 67.75 for this
		{"365 * ((1,174 + 1,587) / 2) / (7,772 + (2,438 - 2,320))", 63.8634},
		{"$1,174 + $1,587", 2761},
		{"12.5% * 200", 25},
		{"-3 + 5", 2},
		{"-(2 - 5) * 2", 6},
		{"6 × 7 ÷ 2 − 1", 20},
		{".5 + 1", 1.5},
		{"10 - 2 - 3", 5},
		{"64 / 4 / 2", 8},
		{"$ 1,587 - 12,345.5", -10758.5},
		{"1174", 1174},
	}
	for _, c := range cases {
		got, err := Evaluate(c.expr)
		if err != nil {
			t.Errorf("%q: %v", c.expr, err)
			continue
		}
		if math.Abs(got-c.want) > 1e-3*math.Max(1, math.Abs(c.want)) {
			t.Errorf("%q = %v, want %v", c.expr, got, c.want)
		}
	}
}

func TestOnlyArithmeticEvaluates(t *testing.T) {
	for _, expr := range []string{"", "1 +", "2 ** 3", "abs(-1)", "x + 1", "1 / 0", "(1 + 2", "1 + 2)", "os.exit(1)", "1 2", "5 3", "1, 2", "1,2", "1,174,5"} {
		if _, err := Evaluate(expr); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: err = %v, want ErrInvalid", expr, err)
		}
	}
}

func TestResolveShowsValueAndSetUp(t *testing.T) {
	out, calcs := Resolve("Margin was <<11,512 / 86,392 * 100>>% [1], growth <<(86,392 - 79,474) / 79,474 * 100>>%, and <<abs(1)>>.")
	want := "Margin was 13.3253 (= 11,512 / 86,392 * 100)% [1], growth 8.7047 (= (86,392 - 79,474) / 79,474 * 100)%, and abs(1) (could not be computed)."
	if out != want {
		t.Fatalf("out =\n%s\nwant\n%s", out, want)
	}
	if len(calcs) != 3 || calcs[0].Value == nil || calcs[2].Value != nil || calcs[2].Error == "" {
		t.Fatalf("calcs = %+v", calcs)
	}
	if n := len(calcs[0].Operands); n != 3 || calcs[0].Operands[0].Text != "11,512" || calcs[0].Operands[2].Value != 100 {
		t.Errorf("operands = %+v", calcs[0].Operands)
	}
}

func TestTraceFindsEachOperandOnItsPage(t *testing.T) {
	_, calcs := Resolve("<<(11,512 + 2763) / 86392 * 100>>")
	Trace(calcs, []Source{
		{Page: 62, Text: "Net revenue 86,392 79,474. Operating profit 11,512 11,162."},
		{Page: 64, Text: "Depreciation and other amortization 2,763 2,710"},
	})
	ops := calcs[0].Operands
	if !ops[0].Found || ops[0].Page != 62 || ops[0].Evidence != 1 {
		t.Errorf("11,512: %+v", ops[0])
	}
	if !ops[1].Found || ops[1].Page != 64 {
		t.Errorf("2763 written without a separator, stated as 2,763: %+v", ops[1])
	}
	if !ops[2].Found || ops[2].Page != 62 {
		t.Errorf("86392 stated as 86,392: %+v", ops[2])
	}
	if ops[3].Found {
		t.Errorf("100 is the formula's, on no page: %+v", ops[3])
	}
}

func TestTraceMatchesWholeNumbersOnly(t *testing.T) {
	_, calcs := Resolve("<<12 + 3.5>>")
	Trace(calcs, []Source{{Page: 1, Text: "Revenue 1,234 and 312; rate 13.55. Units 12. Share 3.5."}})
	if !calcs[0].Operands[0].Found {
		t.Error("12 is stated (\"Units 12.\")")
	}
	if !calcs[0].Operands[1].Found {
		t.Error("3.5 is stated (\"Share 3.5.\")")
	}
	_, miss := Resolve("<<12>>")
	Trace(miss, []Source{{Page: 1, Text: "Revenue 1,234 and 312; 120 units."}})
	if miss[0].Operands[0].Found {
		t.Error("12 matched inside a longer number")
	}
}

func TestFormat(t *testing.T) {
	for v, want := range map[float64]string{1234: "1,234", 0.1 + 0.2: "0.3", -3.70004: "-3.7", 1234567.891: "1,234,567.891", -1500: "-1,500", 63.86343: "63.8634"} {
		if got := Format(v); got != want {
			t.Errorf("Format(%v) = %q, want %q", v, got, want)
		}
	}
}

func TestInstructionsShowTheNotation(t *testing.T) {
	if !strings.Contains(Instructions, "<<expression>>") || !strings.Contains(Instructions, "<<11,512 / 86,392 * 100>>") {
		t.Error("the instructions must show the model the notation Resolve reads")
	}
}

func TestOperandsKeepTheirSign(t *testing.T) {
	_, calcs := Resolve("<<-1,234 + (-(5)) - 3 * -2>>")
	got := []string{}
	for _, o := range calcs[0].Operands {
		got = append(got, fmt.Sprintf("%s=%v", o.Text, o.Value))
	}
	if strings.Join(got, " ") != "-1,234=-1234 5=5 3=3 -2=-2" {
		t.Errorf("operands = %v", got)
	}
	// Found by magnitude: the page prints the loss in parentheses.
	Trace(calcs, []Source{{Page: 9, Text: "Net loss (1,234) for the year."}})
	if !calcs[0].Operands[0].Found || calcs[0].Operands[0].Page != 9 {
		t.Errorf("-1,234 not found as (1,234): %+v", calcs[0].Operands[0])
	}
}

// FinanceBench 04412 (Lockheed Martin), as the model wrote it (HAL-3245).
func TestNestedSetUpIsWorkedOutInsideOut(t *testing.T) {
	out, calcs := Resolve("Ratio = <<65,398 / <<(50,710 + 47,528) / 2>>>>")
	if out != "Ratio = 1.3314 (= 65,398 / 49119)" {
		t.Fatalf("out = %q", out)
	}
	if len(calcs) != 2 || calcs[0].Expression != "(50,710 + 47,528) / 2" || calcs[1].Expression != "65,398 / 49119" {
		t.Fatalf("calcs = %+v", calcs)
	}
	if math.Abs(*calcs[1].Value-65398.0/49119.0) > 1e-9 {
		t.Errorf("value = %v", *calcs[1].Value)
	}
}

func TestTwoLevelsAndSiblings(t *testing.T) {
	out, calcs := Resolve("x <<<<1 + 1>> * <<2 + 3>> + <<<<4>> / 2>>>> y")
	if out != "x 12 (= 2 * 5 + 2) y" || len(calcs) != 5 {
		t.Errorf("out = %q, %d calcs", out, len(calcs))
	}
}

func TestFailedInnerSetUpFailsTheOuter(t *testing.T) {
	out, calcs := Resolve("<<10 / <<abs(1)>>>>")
	if out != "10 / (abs(1)) (could not be computed)" {
		t.Errorf("out = %q", out)
	}
	for _, c := range calcs {
		if c.Value != nil {
			t.Errorf("a level has a value: %+v", c)
		}
	}
}

func TestUnbalancedMarkerIsNeverANumber(t *testing.T) {
	for raw, want := range map[string]string{
		"margin <<11,512 / 86,392 * 100":   "margin 11,512 / 86,392 * 100 (could not be computed)",
		"margin 11,512 / 86,392 * 100>> %": "margin 11,512 / 86,392 * 100 %",
	} {
		out, calcs := Resolve(raw)
		if out != want || len(calcs) == 0 || calcs[len(calcs)-1].Value != nil {
			t.Errorf("%q -> %q, %+v", raw, out, calcs)
		}
	}
}

func TestInnerResultKeepsItsPrecision(t *testing.T) {
	if out, _ := Resolve("<<<<1 / 3>> * 3>>"); out != "1 (= 0.3333333333333333 * 3)" {
		t.Errorf("out = %q", out)
	}
}

// An inner result written into its outer set-up is marked derived and not
// looked for on the pages; a literal of the same value stays a literal.
func TestInnerResultIsDerivedNotModelSupplied(t *testing.T) {
	_, calcs := Resolve("<<<<1 + 1>> + 2>>")
	outer := calcs[1]
	if outer.Expression != "2 + 2" || len(outer.Operands) != 2 {
		t.Fatalf("outer = %+v", outer)
	}
	if !outer.Operands[0].Derived || outer.Operands[1].Derived {
		t.Errorf("want the first 2 derived and the literal 2 not: %+v", outer.Operands)
	}
	Trace(calcs, []Source{{Page: 3, Text: "two items, 2 in all"}})
	if outer.Operands[0].Found || !calcs[1].Operands[1].Found {
		t.Errorf("trace: %+v", calcs[1].Operands)
	}
	// a negative inner result, behind a 3-byte minus sign in the outer text
	_, neg := Resolve("<<10 − <<2 - 5>> × 2>>")
	ops := neg[1].Operands
	if len(ops) != 3 || ops[0].Derived || !ops[1].Derived || ops[1].Value != -3 || ops[2].Derived {
		t.Errorf("operands = %+v", ops)
	}
	if *neg[1].Value != 16 {
		t.Errorf("value = %v", *neg[1].Value)
	}
}

func TestInnerResultKeepsFullPrecision(t *testing.T) {
	for expr, want := range map[string]float64{
		"<<<<1 / 3>> * 1000000000000>>":              1.0 / 3 * 1e12,
		"<<1 / <<0.0000000000001 * 1>>>>":            1e13,
		"<<<<1.234567 / 1000000000>> * 1000000000>>": 1.234567,
	} {
		_, calcs := Resolve(expr)
		last := calcs[len(calcs)-1]
		if last.Value == nil || math.Abs(*last.Value-want) > 1e-9*math.Max(1, math.Abs(want)) {
			t.Errorf("%s = %+v, want %v", expr, last, want)
		}
	}
}
