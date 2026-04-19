package llm

import "context"

// OpenAIConfig configures the OpenAI client.
type OpenAIConfig struct {
	APIKey         string
	Model          string
	ReasoningModel string
}

// OpenAI is a GPT-backed Client.
type OpenAI struct{ cfg OpenAIConfig }

// NewOpenAI constructs a new OpenAI client.
func NewOpenAI(cfg OpenAIConfig) *OpenAI {
	if cfg.Model == "" {
		cfg.Model = "gpt-4o-mini"
	}
	return &OpenAI{cfg: cfg}
}

func (o *OpenAI) Complete(ctx context.Context, req Request) (*Response, error) {
	// TODO(phase-1): integrate the official openai-go SDK. Use
	// response_format=json_schema when req.JSONSchema is set.
	return nil, ErrNotImplemented
}

func (o *OpenAI) CountTokens(ctx context.Context, text string) (int, error) {
	// TODO(phase-1): use tiktoken-go for accurate counts.
	return len(text) / 4, nil
}
