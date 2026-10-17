package handler

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/calc"
	enginecfg "github.com/hallelx2/vectorless-engine/pkg/config"
	"github.com/hallelx2/vectorless-engine/pkg/pincite"
	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
)

// The answer model sets a calculation up; the engine works it out and
// finds each input on the page that states it (HAL-3161). FinanceBench's
// PepsiCo FY2022 EBITDA margin, from the income statement and the cash
// flow statement.
func TestPageAnswerComputesItsCalculations(t *testing.T) {
	var prompt string
	llm := &llmgate.Mock{Respond: func(_ context.Context, req llmgate.Request) (*llmgate.Response, error) {
		prompt = req.Messages[len(req.Messages)-1].Content
		return &llmgate.Response{Content: "Unadjusted EBITDA margin was <<(11,512 + 2,763) / 86,392 * 100>>% [1][2]."}, nil
	}}
	h := NewAnswerHandler(slog.Default(), nil, nil, evidenceStrategy{}, llm, "m",
		enginecfg.AnswerSpanBlock{}, enginecfg.AnswerBlock{}, nil)
	res := &retrieval.Result{EvidencePages: []retrieval.EvidencePage{
		{Page: 62, Title: "Income statement", Text: "Net Revenue $ 86,392 $ 79,474. Operating Profit 11,512 11,162.", Confidence: 0.9},
		{Page: 64, Title: "Cash flows", Text: "Depreciation and amortization 2,763 2,710.", Confidence: 0.8},
	}}
	resp, _, _, _, err := h.answerFromPages(context.Background(), queryStrategyTestTree(), pincite.Source{},
		answerRequest{DocumentID: "doc_x", Query: "PepsiCo FY2022 unadjusted EBITDA % margin?"}, res, time.Now(), func(string, any) {})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "<<expression>>") {
		t.Error("the answer prompt does not tell the model how to set a calculation up")
	}
	answer := resp["answer"].(string)
	if !strings.Contains(answer, "16.5235 (= (11,512 + 2,763) / 86,392 * 100)%") {
		t.Errorf("answer = %q, want the computed value beside its set-up", answer)
	}
	calcs := resp["calculations"].([]calc.Calculation)
	if len(calcs) != 1 || calcs[0].Value == nil {
		t.Fatalf("calculations = %+v", calcs)
	}
	ops := calcs[0].Operands
	if !ops[0].Found || ops[0].Page != 62 || !ops[1].Found || ops[1].Page != 64 || !ops[2].Found || ops[2].Page != 62 {
		t.Errorf("operands not traced to their pages: %+v", ops)
	}
	if ops[3].Found {
		t.Errorf("100 is the formula's own constant, on no page: %+v", ops[3])
	}
}

func TestPageAnswerWithoutCalculationsSaysSo(t *testing.T) {
	llm := &llmgate.Mock{Reply: "3M spent $1,577 million [1]."}
	h := NewAnswerHandler(slog.Default(), nil, nil, evidenceStrategy{}, llm, "m",
		enginecfg.AnswerSpanBlock{}, enginecfg.AnswerBlock{}, nil)
	res := &retrieval.Result{EvidencePages: []retrieval.EvidencePage{{Page: 39, Text: "Total Company 1,577 1,373"}}}
	resp, _, _, _, err := h.answerFromPages(context.Background(), queryStrategyTestTree(), pincite.Source{},
		answerRequest{DocumentID: "doc_x", Query: "capex?"}, res, time.Now(), func(string, any) {})
	if err != nil {
		t.Fatal(err)
	}
	if c := resp["calculations"].([]calc.Calculation); c == nil || len(c) != 0 {
		t.Errorf("calculations = %#v, want an empty array", c)
	}
}
