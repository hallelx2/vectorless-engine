package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
)

// The answer step: one generative call over the evidence pages, and the
// only generation in the pipeline. The prompt is the engine's
// synthesiseAnswer (internal/handler/answer.go) with evidence pages in
// place of sections.
//
// It talks to an OpenAI-compatible endpoint directly rather than through
// llmgate, because llmgate cannot yet turn a reasoning model's thinking
// off, and GLM-4.6 thinks by default: measured 2026-09-28, 127 output
// tokens to answer "17*23" against 3 with thinking disabled. Answering
// from evidence that is already selected needs no deliberation.

type answerer struct {
	baseURL, key, model string
	thinking            bool
	maxPages            int
	client              *http.Client
}

func newAnswerer(thinking bool, maxPages int) (*answerer, error) {
	key := os.Getenv("ANSWER_API_KEY")
	if key == "" {
		key = os.Getenv("VLE_LLM_ANTHROPIC_API_KEY")
	}
	if key == "" {
		return nil, fmt.Errorf("answer: set ANSWER_API_KEY (or VLE_LLM_ANTHROPIC_API_KEY for GLM)")
	}
	base := os.Getenv("ANSWER_BASE_URL")
	if base == "" {
		base = "https://api.z.ai/api/paas/v4"
	}
	model := os.Getenv("ANSWER_MODEL")
	if model == "" {
		model = "glm-4.6"
	}
	return &answerer{baseURL: strings.TrimRight(base, "/"), key: key, model: model, thinking: thinking,
		maxPages: maxPages, client: &http.Client{Timeout: 3 * time.Minute}}, nil
}

type answerOut struct {
	Text      string
	Seconds   float64
	InTokens  int
	OutTokens int
}

func (a *answerer) answer(ctx context.Context, query string, evidence []retrieval.PageScore) (answerOut, error) {
	var b strings.Builder
	b.WriteString("You are answering a user's question using ONLY the evidence below.\n\n")
	b.WriteString("User query:\n")
	b.WriteString(query)
	b.WriteString("\n\nRetrieved evidence (each block is a page of the document):\n")
	for i, ev := range evidence {
		if i >= a.maxPages {
			break
		}
		fmt.Fprintf(&b, "\n[%d] page=%d\nPage content:\n%s\n", i+1, ev.Page.Number, ev.Page.Text)
	}
	b.WriteString("\nWrite a concise answer to the user's query. ")
	b.WriteString("If the evidence does not contain an answer, say so. ")
	b.WriteString("Inline citations should reference the page numbers shown above. ")
	b.WriteString("Output plain prose; no JSON.")

	body := map[string]any{
		"model": a.model,
		"messages": []map[string]string{
			{"role": "system", "content": "You synthesise grounded answers from retrieved document pages. Never invent facts; only cite what the evidence shows."},
			{"role": "user", "content": b.String()},
		},
		"max_tokens":  1024,
		"temperature": 0,
	}
	if !a.thinking {
		body["thinking"] = map[string]string{"type": "disabled"}
	}
	raw, _ := json.Marshal(body)
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return answerOut{}, err
	}
	req.Header.Set("Authorization", "Bearer "+a.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return answerOut{}, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return answerOut{}, fmt.Errorf("answer: %d %s", resp.StatusCode, strings.TrimSpace(string(data[:min(len(data), 300)])))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &out); err != nil || len(out.Choices) == 0 {
		return answerOut{}, fmt.Errorf("answer: unreadable response: %v", err)
	}
	return answerOut{
		Text:      strings.TrimSpace(out.Choices[0].Message.Content),
		Seconds:   time.Since(start).Seconds(),
		InTokens:  out.Usage.PromptTokens,
		OutTokens: out.Usage.CompletionTokens,
	}, nil
}
