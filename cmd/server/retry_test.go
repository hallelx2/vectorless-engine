package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hallelx2/llmgate"
	"github.com/hallelx2/llmgate/middleware/retry"
)

// A provider that refuses twice and then answers yields the answer: the
// server's model client retries 429s (HAL-3206).
func TestModelClientRidesOutRefusals(t *testing.T) {
	var calls atomic.Int32
	inner := &llmgate.Mock{Respond: func(context.Context, llmgate.Request) (*llmgate.Response, error) {
		if calls.Add(1) <= 2 {
			return nil, llmgate.NewLLMError("gemini", 429, "vertex: Resource exhausted. Please try again later.", nil)
		}
		return &llmgate.Response{Content: "capex was $1,577 million [1]"}, nil
	}}
	fast := retry.Config{MaxRetries: modelRetry.MaxRetries, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}
	resp, err := withModelRetry(inner, fast).Complete(context.Background(), llmgate.Request{Model: "m"})
	if err != nil {
		t.Fatalf("err = %v after %d calls", err, calls.Load())
	}
	if resp.Content != "capex was $1,577 million [1]" || calls.Load() != 3 {
		t.Errorf("content %q after %d calls", resp.Content, calls.Load())
	}
}

// A request the provider rejects as malformed is not retried.
func TestModelClientDoesNotRetryABadRequest(t *testing.T) {
	var calls atomic.Int32
	inner := &llmgate.Mock{Respond: func(context.Context, llmgate.Request) (*llmgate.Response, error) {
		calls.Add(1)
		return nil, llmgate.NewLLMError("gemini", 400, "invalid argument", nil)
	}}
	fast := retry.Config{MaxRetries: 5, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}
	if _, err := withModelRetry(inner, fast).Complete(context.Background(), llmgate.Request{}); err == nil {
		t.Fatal("want the 400 back")
	}
	if calls.Load() != 1 {
		t.Errorf("a 400 was tried %d times", calls.Load())
	}
}

func TestModelRetryIsConfigured(t *testing.T) {
	if modelRetry.MaxRetries < 3 || modelRetry.MaxDelay < 10*time.Second {
		t.Errorf("modelRetry = %+v: too thin to ride out a shared-quota burst", modelRetry)
	}
	if withModelRetry(nil, modelRetry) != nil {
		t.Error("a nil client must stay nil, so the endpoint can report no model")
	}
}
