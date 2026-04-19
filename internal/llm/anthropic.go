package llm

import "context"

// AnthropicConfig configures the Anthropic client.
type AnthropicConfig struct {
	APIKey          string
	Model           string
	ReasoningModel  string
	EnablePromptCache bool
}

// Anthropic is a Claude-backed Client.
type Anthropic struct {
	cfg AnthropicConfig
}

// NewAnthropic constructs a new Anthropic client.
func NewAnthropic(cfg AnthropicConfig) *Anthropic {
	if cfg.Model == "" {
		cfg.Model = "claude-sonnet-4-5"
	}
	return &Anthropic{cfg: cfg}
}

func (a *Anthropic) Complete(ctx context.Context, req Request) (*Response, error) {
	// TODO(phase-1): integrate github.com/anthropics/anthropic-sdk-go.
	// Enable prompt caching for the system message + tree view so
	// subsequent queries against the same document hit the cache.
	return nil, ErrNotImplemented
}

func (a *Anthropic) CountTokens(ctx context.Context, text string) (int, error) {
	// TODO(phase-1): use Anthropic count_tokens API or a local tokenizer.
	// Cheap rule of thumb until then: ~4 chars per token.
	return len(text) / 4, nil
}
