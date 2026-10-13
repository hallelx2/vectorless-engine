package handler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
)

// A reasoning model that spends its budget thinking returns no text. That
// used to reach the caller as a 200 with a blank answer (HAL-2846); it is
// an error now, and the error says what to change.
func TestSynthesiseFromPagesRejectsEmptyAnswer(t *testing.T) {
	evidence := []retrieval.EvidencePage{{Page: 39, Text: "Total Company 1,577 1,373"}}

	thinking := &llmgate.Mock{Respond: func(context.Context, llmgate.Request) (*llmgate.Response, error) {
		return &llmgate.Response{Content: "  \n", ReasoningContent: "let me think", FinishReason: "max_tokens",
			Usage: llmgate.Usage{OutputTokens: 1200}}, nil
	}}
	_, _, err := synthesiseFromPages(context.Background(), thinking, "glm-4.6", "capex?", evidence, 1200)
	if !errors.Is(err, errEmptyAnswer) {
		t.Fatalf("empty answer: err = %v, want errEmptyAnswer", err)
	}
	if !strings.Contains(err.Error(), "max_answer_tokens") {
		t.Errorf("reasoning case should say how to fix it: %v", err)
	}

	silent := &llmgate.Mock{Reply: ""}
	if _, _, err := synthesiseFromPages(context.Background(), silent, "m", "q", evidence, 100); !errors.Is(err, errEmptyAnswer) {
		t.Fatalf("blank reply: err = %v, want errEmptyAnswer", err)
	}

	ok := &llmgate.Mock{Reply: "3M spent $1,577 million [1]."}
	got, _, err := synthesiseFromPages(context.Background(), ok, "m", "q", evidence, 100)
	if err != nil || got != "3M spent $1,577 million [1]." {
		t.Fatalf("normal answer: got %q, %v", got, err)
	}
}
