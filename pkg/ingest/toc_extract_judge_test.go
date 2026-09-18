package ingest

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// Contents pages exactly as the parser hands them to the builder.
const adobeContents = `ADOBE INC. FORM 10-K TABLE OF CONTENTS
Page No.

PART I
Item 1. Business 3 Item 1A. Risk Factors 20 Item 1B. Unresolved Staff Comments 34 Item 2. Properties 34 Item 3. Legal Proceedings 34 Item 4. Mine Safety Disclosures 34

PART II
Item 5. Market for Registrant's Common Equity, Related Stockholder Matters and Issuer Purchases of Equity Securities 35 Item 6 [Reserved] 35 Item 7. Management's Discussion and Analysis of Financial Condition and Results of Operations 36 Item 7A. Quantitative and Qualitative Disclosures About Market Risk 50 Item 8. Financial Statements and Supplementary Data 52 Item 9. Changes in and Disagreements with Accountants on Accounting and Financial Disclosure 92 Item 9A. Controls and Procedures 92 Item 9B. Other Information 92 Item 9C. Disclosure Regarding Foreign Jurisdictions That Prevent Inspections 92

PART III
Item 10. Directors, Executive Officers and Corporate Governance 93 Item 11. Executive Compensation 93 Item 12. Security Ownership of Certain Beneficial Owners and Management and Related Stockholder Matters 93 Item 13. Certain Relationships and Related Transactions, and Director Independence 93 Item 14. Principal Accounting Fees and Services 93

PART IV
Item 15. Exhibits, Financial Statement Schedules 94 Item 16. Form 10-K Summary 96 Signatures 97 Summary of Trademarks 99 2 Table of Contents
`

const amcorContents = `DOCUMENTS INCORPORATED BY REFERENCE
Certain information required for Part III of this Annual Report on Form 10-K is incorporated by reference to the Amcor plc definitive Proxy Statement.

**Amcor plc Annual Report on Form 10-K Table of Contents Part I**

Item 1. Business 6 Item 1A. Risk Factors 10 Item 1B. Unresolved Staff Comments 20 Item 2. Properties 20 Item 3. Legal Proceedings 20 Item 4. Mine Safety Disclosures 20

**Part II**

Item 5. Market For Registrant’s Common Equity, Related Shareholder Matters and Issuer Purchases of Equity Securities 21 Item 6. Selected Financial Data 23 Item 7. Management’s Discussion and Analysis of Financial Condition and Results of Operations 24

**Part IV**

Item 15. Exhibits and Financial Statement Schedules 116 Exhibit Index 116 Item 16. Form 10-K Summary (optional) 120 Signatures 121 3
`

const boeingContents = `THE BOEING COMPANY Index to the Form 10-K For the Fiscal Year Ended December 31, 2022 PART I Page
Item 1. Business 1 Item 1A. Risk Factors 6 Item 1B. Unresolved Staff Comments 17 Item 2. Properties 18 Item 3. Legal Proceedings 18 Item 4. Mine Safety Disclosures 18

**PART II**

Item 5. Market for Registrant’s Common Equity, Related Stockholder Matters and Issuer Purchases of Equity Securities 19 Item 6. [Reserved] 19 Item 7. Management’s Discussion and Analysis of Financial Condition and Results of Operations 20

**PART IV**

Item 15. Exhibits, Financial Statement Schedules 128 Item 16. Form 10-K Summary 131 Signatures 132 Table of Contents

**PART I Item 1. Business**

The Boeing Company, together with its subsidiaries, is one of the world’s major aerospace firms.
`

func titlesOf(es []contentsEntry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Title)
	}
	return out
}

func hasTitle(es []contentsEntry, t string) *contentsEntry {
	for i := range es {
		if es[i].Title == t {
			return &es[i]
		}
	}
	return nil
}

func TestParseContentsAdobe(t *testing.T) {
	es := parseContentsEntries(adobeContents)
	want := map[string]int{
		"Item 1. Business": 3, "Item 1A. Risk Factors": 20, "Item 1B. Unresolved Staff Comments": 34,
		"Item 2. Properties": 34, "Item 4. Mine Safety Disclosures": 34,
		"Item 5. Market for Registrant's Common Equity, Related Stockholder Matters and Issuer Purchases of Equity Securities": 35,
		"Item 6 [Reserved]": 35, "Item 7A. Quantitative and Qualitative Disclosures About Market Risk": 50,
		"Item 9C. Disclosure Regarding Foreign Jurisdictions That Prevent Inspections": 92,
		"Item 14. Principal Accounting Fees and Services":                              93,
		"Item 15. Exhibits, Financial Statement Schedules":                             94, "Item 16. Form 10-K Summary": 96,
		"Signatures": 97, "Summary of Trademarks": 99,
	}
	for title, page := range want {
		e := hasTitle(es, title)
		if e == nil {
			t.Errorf("missing %q in %v", title, titlesOf(es))
			continue
		}
		if e.Printed != page {
			t.Errorf("%q printed page: got %d want %d", title, e.Printed, page)
		}
		if e.Depth != 2 {
			t.Errorf("%q depth: got %d want 2 (under a part)", title, e.Depth)
		}
	}
	if countRealEntries(es) != 24 {
		t.Errorf("real entries: got %d want 24: %v", countRealEntries(es), titlesOf(es))
	}
	parts := 0
	for _, e := range es {
		if e.Container {
			parts++
		}
	}
	if parts != 4 {
		t.Errorf("parts: got %d want 4", parts)
	}
	for _, junk := range []string{"Page No.", "Table of Contents", "2 Table of Contents", "ADOBE INC. FORM 10-K TABLE OF CONTENTS"} {
		if hasTitle(es, junk) != nil {
			t.Errorf("junk entry admitted: %q", junk)
		}
	}
}

func TestParseContentsAmcorPartAtEndOfTitleLine(t *testing.T) {
	es := parseContentsEntries(amcorContents)
	if e := hasTitle(es, "PART I"); e == nil || !e.Container {
		t.Fatalf("PART I closing the table's title line should be a container: %v", titlesOf(es))
	}
	if e := hasTitle(es, "Item 6. Selected Financial Data"); e == nil || e.Printed != 23 {
		t.Errorf("Item 6: %+v", e)
	}
	if e := hasTitle(es, "Item 16. Form 10-K Summary (optional)"); e == nil || e.Printed != 120 {
		t.Errorf("Item 16 with a parenthetical: %+v", e)
	}
	if e := hasTitle(es, "Exhibit Index"); e == nil || e.Printed != 116 {
		t.Errorf("Exhibit Index: %+v", e)
	}
	// The prose paragraph has no page numbers and yields nothing.
	for _, e := range es {
		if strings.HasPrefix(e.Title, "Certain information") {
			t.Errorf("prose admitted as an entry: %q", e.Title)
		}
	}
}

func TestParseContentsBoeingBodyBelowTheList(t *testing.T) {
	es := parseContentsEntries(boeingContents)
	if e := hasTitle(es, "Item 6. [Reserved]"); e == nil || e.Printed != 19 {
		t.Errorf("Item 6. [Reserved]: %+v", e)
	}
	if e := hasTitle(es, "Signatures"); e == nil || e.Printed != 132 {
		t.Errorf("Signatures: %+v", e)
	}
	// "PART I Item 1. Business" is the body heading on the same page;
	// it has no page number and is not an entry. The Judge would reject
	// a fragment; the parser must not even produce one.
	for _, e := range es {
		if strings.HasPrefix(e.Title, "The Boeing Company, together") {
			t.Errorf("body prose admitted: %q", e.Title)
		}
	}
}

func TestParseContentsDottedNumbering(t *testing.T) {
	es := parseContentsEntries("1 Introduction 3\n1.1 Background 4\n1.2 Scope 6\n2 Methods 9\n2.1.1 Sampling 10")
	depths := map[string]int{"1 Introduction": 1, "1.1 Background": 2, "1.2 Scope": 2, "2 Methods": 1, "2.1.1 Sampling": 3}
	for title, d := range depths {
		e := hasTitle(es, title)
		if e == nil {
			t.Errorf("missing %q: %v", title, titlesOf(es))
			continue
		}
		if e.Depth != d {
			t.Errorf("%q depth got %d want %d", title, e.Depth, d)
		}
	}
}

func TestCalibrateFromPrinted(t *testing.T) {
	nodes := []tree.TOCNode{{Title: "PART I", Nodes: []tree.TOCNode{
		{Title: "Item 1. Business", StartPage: 3},
		{Title: "Item 1A. Risk Factors", StartPage: 20},
		{Title: "Item 1B. Unresolved Staff Comments"}, // unplaced
		{Title: "Item 2. Properties"},                 // unplaced
	}}}
	printed := map[string]int{
		"r_" + slugForKey("Item 1. Business"): 1, "r_" + slugForKey("Item 1A. Risk Factors"): 18,
		"r_" + slugForKey("Item 1B. Unresolved Staff Comments"): 32, "r_" + slugForKey("Item 2. Properties"): 32,
	}
	n := calibrateFromPrinted(nodes, printed, 99)
	if n != 2 {
		t.Fatalf("placed %d want 2", n)
	}
	if got := nodes[0].Nodes[2].StartPage; got != 34 {
		t.Errorf("1B: got %d want 34 (printed 32 + offset 2)", got)
	}
	if got := nodes[0].Nodes[3].StartPage; got != 34 {
		t.Errorf("Item 2: got %d want 34", got)
	}
	// One resolved leaf is not enough to trust an offset.
	single := []tree.TOCNode{{Title: "A", StartPage: 5}, {Title: "B"}}
	if calibrateFromPrinted(single, map[string]int{"r_" + slugForKey("A"): 3, "r_" + slugForKey("B"): 7}, 99) != 0 {
		t.Errorf("a single offset should not be trusted")
	}
}

// End to end on a Judge: detection says the contents page is one, the
// entries are parsed and confirmed, the resolver places them — and the
// generative extractor is never called.
func TestBuildExtractsOnTheJudgeWithNoGenerativeCall(t *testing.T) {
	llm := &scriptedLLM{}
	var generativeExtract atomic.Int32
	llm.route = func(prompt string) string {
		if strings.Contains(prompt, "hierarchical tree structure") {
			generativeExtract.Add(1)
		}
		return `{"toc_detected":"no"}`
	}
	judge := &llmgate.MockJudge{Respond: func(_ context.Context, req llmgate.JudgeRequest) (*llmgate.Judgment, error) {
		st, _ := req.State.(map[string]any)
		ans := map[string]llmgate.Answer{}
		for id := range req.Questions {
			p := 0.0
			switch {
			case strings.HasPrefix(id, "e_"):
				// Confirm every entry except one planted piece of furniture.
				item := st[id].(map[string]any)
				if item["title"] != "Page No." {
					p = 0.9
				}
			case strings.HasPrefix(id, "r_"):
				item := st[id].(map[string]any)
				if strings.HasPrefix(item["excerpt"].(string), item["title"].(string)) {
					p = 0.9
				}
			default:
				// Detection: the page that says Table of Contents.
				if item, ok := st[id].(map[string]any); ok {
					for _, v := range item {
						if s, ok := v.(string); ok && strings.Contains(s, "TABLE OF CONTENTS") {
							p = 0.9
						}
					}
				} else if s, ok := st[id].(string); ok && strings.Contains(s, "TABLE OF CONTENTS") {
					p = 0.9
				}
			}
			ans[id] = llmgate.NoulAnswer{Noul: p}
		}
		return &llmgate.Judgment{Model: "mock", Answers: ans, Usage: llmgate.Usage{InputTokens: 10, TotalTokens: 10, TokensReported: true}}, nil
	}}
	pages := []PageText{
		{PageNumber: 1, Text: "Cover"},
		{PageNumber: 2, Text: "ACME FORM 10-K TABLE OF CONTENTS\nPage No.\nPART I\nItem 1. Business 1 Item 1A. Risk Factors 4 Item 2. Properties 9\nPART II\nItem 5. Market 11 Signatures 12"},
		{PageNumber: 3, Text: "PART I ITEM 1. BUSINESS\nWe make things."},
		{PageNumber: 6, Text: "Item 1A. Risk Factors\nThings go wrong."},
		{PageNumber: 11, Text: "Item 2. Properties\nWe own a shed."},
		{PageNumber: 13, Text: "PART II Item 5. Market\nShares trade."},
		{PageNumber: 14, Text: "Signatures\nSigned."},
	}
	b := &TOCBuilder{LLM: llm, Judge: judge, TOCCheckPages: 3, Concurrency: 2, MinimalContext: true}
	nodes, usage, err := b.Build(context.Background(), pages)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if generativeExtract.Load() != 0 || usage.GenerativeCalls != 0 {
		t.Errorf("generative extractor ran: route=%d usage=%d", generativeExtract.Load(), usage.GenerativeCalls)
	}
	if len(usage.Degraded) != 0 {
		t.Errorf("unexpected degradation: %v", usage.Degraded)
	}
	if len(nodes) != 2 || nodes[0].Title != "PART I" || nodes[1].Title != "PART II" {
		t.Fatalf("top level: %+v", nodes)
	}
	got := map[string]int{}
	for _, part := range nodes {
		for _, n := range part.Nodes {
			got[n.Title] = n.StartPage
		}
	}
	want := map[string]int{"Item 1. Business": 3, "Item 1A. Risk Factors": 6, "Item 2. Properties": 11, "Item 5. Market": 13, "Signatures": 14}
	for title, page := range want {
		if got[title] != page {
			t.Errorf("%q: got page %d want %d (all: %v)", title, got[title], page, got)
		}
	}
	if _, ok := got["Page No."]; ok {
		t.Errorf("furniture the Judge rejected was kept")
	}
	if nodes[0].Nodes[0].Structure != "1.1" || nodes[1].Nodes[0].Structure != "2.1" {
		t.Errorf("structure: %q %q", nodes[0].Nodes[0].Structure, nodes[1].Nodes[0].Structure)
	}
}

// A failed Judge extraction is a degradation, not a silent fallback.
func TestBuildRecordsAFailedJudgeExtraction(t *testing.T) {
	llm := &scriptedLLM{}
	llm.route = func(prompt string) string {
		if strings.Contains(prompt, "hierarchical tree structure") {
			return `{"nodes":[{"structure":"1","title":"Item 1. Business","physical_index":"<physical_index_3>"}]}`
		}
		return `{"toc_detected":"no"}`
	}
	judge := &llmgate.MockJudge{Respond: func(_ context.Context, req llmgate.JudgeRequest) (*llmgate.Judgment, error) {
		for id := range req.Questions {
			if strings.HasPrefix(id, "e_") {
				return nil, errors.New("typesafe: request failed")
			}
		}
		ans := map[string]llmgate.Answer{}
		for id := range req.Questions {
			ans[id] = llmgate.NoulAnswer{Noul: 0.9}
		}
		return &llmgate.Judgment{Model: "mock", Answers: ans, Usage: llmgate.Usage{TokensReported: true}}, nil
	}}
	pages := []PageText{
		{PageNumber: 2, Text: "TABLE OF CONTENTS\nItem 1. Business 1 Item 1A. Risk Factors 4 Item 2. Properties 9"},
		{PageNumber: 3, Text: "Item 1. Business\nWe make things."},
	}
	b := &TOCBuilder{LLM: llm, Judge: judge, TOCCheckPages: 2, MinimalContext: true}
	nodes, usage, err := b.Build(context.Background(), pages)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if usage.GenerativeCalls == 0 {
		t.Errorf("the generative extractor should have run as the fallback")
	}
	if len(usage.Degraded) != 1 || !strings.Contains(usage.Degraded[0], "extraction") {
		t.Errorf("degradation not recorded: %v", usage.Degraded)
	}
	if len(nodes) != 1 || nodes[0].Title != "Item 1. Business" {
		t.Errorf("fallback tree: %+v", nodes)
	}
}

func TestParseContentsShapesFromTheCorpus(t *testing.T) {
	t.Run("walmart: glued item numbers and an inline part after a column label", func(t *testing.T) {
		es := parseContentsEntries("Page Part I Item 1Business 7 Item 1ARisk Factors 14 Item 2Properties 24 Part II Item 5Market for Registrant's Common Equity 29")
		if e := hasTitle(es, "Item 1 Business"); e == nil || e.Printed != 7 {
			t.Errorf("Item 1: %+v in %v", e, titlesOf(es))
		}
		if e := hasTitle(es, "Item 1A Risk Factors"); e == nil || e.Printed != 14 {
			t.Errorf("Item 1A: %+v", e)
		}
		if e := hasTitle(es, "PART II"); e == nil || !e.Container {
			t.Errorf("inline PART II not a container: %v", titlesOf(es))
		}
		if e := hasTitle(es, "Item 5 Market for Registrant's Common Equity"); e == nil || e.Depth != 2 {
			t.Errorf("Item 5 under Part II: %+v", e)
		}
		if hasTitle(es, "Page") != nil || hasTitle(es, "Page Part I Item 1 Business") != nil {
			t.Errorf("column label leaked: %v", titlesOf(es))
		}
	})
	t.Run("verizon: item without a page, page number inside a wrapped title", func(t *testing.T) {
		es := parseContentsEntries("PART I\nItem 1. Business\nItem 1A. Risk Factors 14 PART II Item 5. Market for Registrant’s Common Equity, Related Stockholder Matters and Issuer Purchases of 20 Equity Securities Item 6. [Reserved] 21")
		if e := hasTitle(es, "Item 1. Business"); e == nil || e.Printed != 0 {
			t.Errorf("Item 1 without a page should still be an entry: %+v %v", e, titlesOf(es))
		}
		if e := hasTitle(es, "Item 5. Market for Registrant’s Common Equity, Related Stockholder Matters and Issuer Purchases of Equity Securities"); e == nil || e.Printed != 20 {
			t.Errorf("wrapped tail not re-attached: %v", titlesOf(es))
		}
		if e := hasTitle(es, "Item 6. [Reserved]"); e == nil || e.Printed != 21 {
			t.Errorf("Item 6: %+v", e)
		}
		if hasTitle(es, "Equity Securities Item 6. [Reserved]") != nil {
			t.Errorf("tail glued to the next entry: %v", titlesOf(es))
		}
	})
	t.Run("oracle: part with a trailing dot inline", func(t *testing.T) {
		es := parseContentsEntries("Page PART I. Item 1. Business 3 PART II. Item 5. Market 21")
		if e := hasTitle(es, "PART I"); e == nil || !e.Container {
			t.Errorf("PART I.: %v", titlesOf(es))
		}
		if e := hasTitle(es, "Item 1. Business"); e == nil || e.Printed != 3 || e.Depth != 2 {
			t.Errorf("Item 1: %+v", e)
		}
	})
	t.Run("pfizer: parenthesised sub-entries", func(t *testing.T) {
		es := parseContentsEntries("ITEM 15. EXHIBITS, FINANCIAL STATEMENT SCHEDULES 111 15(a)(1) Financial Statements 111 15(a)(2) Financial Statement Schedules 111 15(a)(3) Exhibits 111 ITEM 16. FORM 10-K SUMMARY 116")
		if e := hasTitle(es, "ITEM 15. EXHIBITS, FINANCIAL STATEMENT SCHEDULES"); e == nil || e.Printed != 111 {
			t.Errorf("Item 15 did not close at its page: %v", titlesOf(es))
		}
		if e := hasTitle(es, "15(a)(2) Financial Statement Schedules"); e == nil || e.Printed != 111 {
			t.Errorf("sub-entry: %v", titlesOf(es))
		}
	})
	t.Run("3m: bold-fold duplicates and a truncated copy", func(t *testing.T) {
		es := parseContentsEntries("**Geographic Area 38 Critical Accounting Estimates 39 New**\nGeographic Area 38 Critical Accounting Estimates 39 New Accounting Pronouncements 42 Financial Condition and Liquidity 43")
		n := 0
		for _, e := range es {
			if e.Title == "Geographic Area" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("duplicate entry kept %d times: %v", n, titlesOf(es))
		}
		if hasTitle(es, "New") != nil {
			t.Errorf("truncated fragment 'New' kept: %v", titlesOf(es))
		}
		if e := hasTitle(es, "New Accounting Pronouncements"); e == nil || e.Printed != 42 {
			t.Errorf("full entry missing: %v", titlesOf(es))
		}
	})
}
