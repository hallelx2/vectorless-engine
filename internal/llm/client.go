// Package llm is the provider-agnostic LLM client interface used by the
// retrieval strategies and ingest summarization steps.
//
// Ships with stubs for Anthropic, OpenAI, and Gemini. Real implementations
// will use each vendor's official Go SDK (or HTTP directly) and honor
// provider-specific features where they matter (Anthropic prompt caching,
// OpenAI structured outputs, Gemini long context).
package llm

import (
	"context"
	"errors"
)

// Role identifies the speaker of a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is a single chat turn.
type Message struct {
	Role    Role
	Content string
}

// Request is a single completion request.
type Request struct {
	Model       string
	Messages    []Message
	MaxTokens   int
	Temperature float64

	// JSONMode asks the provider to return a JSON object that conforms to
	// JSONSchema. Providers that don't support structured outputs natively
	// should fall back to prompt instruction.
	JSONMode   bool
	JSONSchema []byte
}

// Response is the model's reply.
type Response struct {
	Content      string
	InputTokens  int
	OutputTokens int
	Model        string
	FinishReason string
}

// Client is the provider-agnostic contract.
type Client interface {
	// Complete runs a single completion.
	Complete(ctx context.Context, req Request) (*Response, error)

	// CountTokens returns an approximate token count for text under this
	// client's model. Implementations may use a local tokenizer or the
	// provider's counting endpoint.
	CountTokens(ctx context.Context, text string) (int, error)
}

// Provider identifies an LLM vendor.
type Provider string

const (
	ProviderAnthropic Provider = "anthropic"
	ProviderOpenAI    Provider = "openai"
	ProviderGemini    Provider = "gemini"
)

// ErrNotImplemented is returned by stub providers during scaffolding.
var ErrNotImplemented = errors.New("llm: provider not yet implemented")
