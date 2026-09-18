package ingest

import "testing"

// A real 10-K TOC as the parser actually delivers it: flattened, no line
// breaks. Taken from 3M_2018_10K page 2.
const tocPageFlattened = `3M COMPANY FORM 10-K For the Year Ended December 31, 2018
Pursuant to Part IV, Item 16, a summary of Form 10-K content follows, including hyperlinked cross-references (in the EDGAR filing). Beginning Page PART I ITEM 1 Business 4 ITEM 1A Risk Factors 10 ITEM 1B Unresolved Staff Comments 12 ITEM 2 Properties 12 ITEM 3 Legal Proceedings 12 ITEM 4 Mine Safety Disclosures 12 PART II ITEM 5 Market for Registrant's Common Equity 13 ITEM 6 Selected Financial Data 15 ITEM 7 Management's Discussion and Analysis 16 ITEM 8 Financial Statements and Supplementary Data 52`

// A generic (non-SEC) contents page — no ITEM/PART markers at all, so
// only the entry shape can save it.
const tocPageGeneric = `Contents Introduction 1 Background and Motivation 3 Related Work 7 Method 12 Experiments 19 Results 24 Discussion 31 Conclusion 35 References 37 Appendix A 41`

// Body prose with the accidental single matches prose always has.
const prosePage = `The Company's results in 2018 reflected continued growth across its four business groups. As described in Section 4 of the agreement, the parties agreed to a term of 5 years. Revenue increased 3 percent to $32.8 billion, and operating income margins were 22.0 percent. Management believes that the underlying demand in 2019 will remain consistent with the trends observed during the fourth quarter, subject to the risks described elsewhere in this report.`

// A cover page: dense with numbers (commission file numbers, IRS ids,
// share counts) but none of them are page numbers attached to titles.
const coverPage = `UNITED STATES SECURITIES AND EXCHANGE COMMISSION Washington, D.C. 20549 FORM 10-K ANNUAL REPORT PURSUANT TO SECTION 13 OR 15(d) Commission file number: 001-01185 GENERAL MILLS, INC. Delaware 41-0274440 Number One General Mills Boulevard Minneapolis, Minnesota 55426 (763) 764-7600 Securities registered pursuant to Section 12(b) of the Act: Common Stock, $.10 par value GIS New York Stock Exchange`

func TestPrefilterKeepsA10KTOC(t *testing.T) {
	s := prefilterTOC(tocPageFlattened)
	if !s.Any() {
		t.Fatalf("a real 10-K TOC page scored zero signal: %+v", s)
	}
	if s.Items < 5 {
		t.Errorf("Items = %d, want the ITEM markers counted", s.Items)
	}
	if s.Parts < 2 {
		t.Errorf("Parts = %d, want PART I and PART II counted", s.Parts)
	}
}

// The generic case is the one that matters for anything that is not an
// SEC filing. It has no ITEM/PART markers, so this is a test of the
// entry-shape signal alone.
func TestPrefilterKeepsAGenericContentsPage(t *testing.T) {
	s := prefilterTOC(tocPageGeneric)
	if !s.Any() {
		t.Fatalf("a generic contents page scored zero signal: %+v", s)
	}
	if s.DenseRun < denseMin {
		t.Errorf("DenseRun = %d, want at least %d entries packed together", s.DenseRun, denseMin)
	}
	if !s.Keyword {
		t.Error("Keyword = false; the page says 'Contents'")
	}
}

// The property the whole filter rests on: prose must score ZERO, not
// low. A prose page that squeaks through costs tokens; a TOC page that
// gets dropped costs the document its tree. The asymmetry is why the
// bar for "signal" is set where it is.
func TestPrefilterSkipsProse(t *testing.T) {
	s := prefilterTOC(prosePage)
	if s.Any() {
		t.Errorf("body prose showed signal and would be sent: %+v", s)
	}
}

// Cover pages are number-dense — file numbers, tax ids, phone numbers —
// and must not be mistaken for a page-numbered list.
func TestPrefilterSkipsACoverPage(t *testing.T) {
	s := prefilterTOC(coverPage)
	if s.Any() {
		t.Errorf("a cover page showed signal and would be sent: %+v", s)
	}
}

func TestPrefilterEmptyPage(t *testing.T) {
	if s := prefilterTOC("   \n  "); s.Any() {
		t.Errorf("empty page showed signal: %+v", s)
	}
}

// Score must rank a page that names itself above one that merely has
// the shape, so a two-stage scan sends the likeliest page first.
func TestPrefilterScoreOrdersByEvidence(t *testing.T) {
	sec := prefilterTOC(tocPageFlattened).Score()
	gen := prefilterTOC(tocPageGeneric).Score()
	pro := prefilterTOC(prosePage).Score()
	if !(sec > pro && gen > pro) {
		t.Errorf("scores: sec=%d gen=%d prose=%d; both TOCs must outrank prose", sec, gen, pro)
	}
}

// Scattered accidental "word number" matches are not a run. This is the
// reason the rule is density, not a count.
func TestPrefilterOneAccidentalMatchIsNotSignal(t *testing.T) {
	s := prefilterTOC("The board met in 2018 and again in March 2019 to approve the plan.")
	if s.Any() {
		t.Errorf("one or two accidental matches counted as signal: %+v", s)
	}
}
