package models

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hallelx2/llmgate"
	"github.com/hallelx2/llmgate/judge/typesafe"
	"github.com/hallelx2/llmgate/provider/anthropic"
	"github.com/hallelx2/llmgate/provider/gemini"
	"github.com/hallelx2/llmgate/provider/openai"
	"github.com/hallelx2/llmgate/provider/vertex"
)

// keylessPlaceholder is sent as the bearer token to an openai-compatible
// server that needs none (Ollama, a local vLLM): the client library
// refuses to start without one.
const keylessPlaceholder = "not-needed"

// BuildClient constructs the generative client g describes. Its default
// model is g's ingest model; answers name the answer model per call.
func BuildClient(g Generative) (llmgate.Client, error) {
	if err := g.Validate(); err != nil {
		return nil, err
	}
	model := g.ResolvedIngestModel()
	switch g.Provider {
	case ProviderAnthropic:
		return anthropic.New(anthropic.Config{APIKey: g.APIKey, Model: model, BaseURL: g.BaseURL})
	case ProviderOpenAI:
		return openai.New(openai.Config{APIKey: g.APIKey, Model: model, BaseURL: g.BaseURL})
	case ProviderOpenAICompatible:
		key := g.APIKey
		if key == "" {
			key = keylessPlaceholder
		}
		return openai.New(openai.Config{APIKey: key, Model: model, BaseURL: g.BaseURL})
	case ProviderGemini:
		return gemini.New(gemini.Config{APIKey: g.APIKey, Model: model, AuthTimeout: 15 * time.Second})
	case ProviderVertex:
		// Bounded like gemini's AuthTimeout: finding credentials can ask
		// the metadata server, which hangs off GCP.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return vertex.New(ctx, vertex.Config{
			Project:  g.Project,
			Location: g.Location,
			Model:    model,
			Thinking: vertex.Thinking{Budget: g.ThinkingBudget, Level: g.ThinkingLevel},
		})
	}
	return nil, fmt.Errorf("unknown provider %q", g.Provider)
}

// BuildJudge constructs a bare TypeSafe Judge. The server wraps it with
// its limiter and retries through Options.BuildJudge.
func BuildJudge(j JudgeSettings) (llmgate.Judge, error) {
	if err := j.Validate(); err != nil {
		return nil, err
	}
	return typesafe.New(typesafe.Config{APIKey: j.APIKey, BaseURL: j.BaseURL, Model: j.Model})
}

// CheckResult is one provider's answer to a connection test.
type CheckResult struct {
	// Tested is false when there was nothing to test.
	Tested    bool   `json:"tested"`
	OK        bool   `json:"ok"`
	Model     string `json:"model,omitempty"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
	// Status is the provider's HTTP status on failure, when known.
	Status int    `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
	// Hint is what an administrator can do about the failure.
	Hint string `json:"hint,omitempty"`
}

// TestResult reports a connection test of both providers.
type TestResult struct {
	Generative CheckResult `json:"generative"`
	Answer     CheckResult `json:"answer"`
	Judge      CheckResult `json:"judge"`
}

// Test makes one tiny real call to each provider. With a nil update it
// tests what is in effect; with an update it tests what saving it would
// put in effect, without saving — so a key can be checked first.
func (r *Registry) Test(ctx context.Context, u *Update) (TestResult, error) {
	gen, judge := r.effective()
	if u != nil {
		stored := Settings{Generative: gen, Judge: judge}
		next, err := u.apply(stored)
		if err != nil {
			return TestResult{}, &ValidationError{Err: err}
		}
		gen, judge = next.Generative, next.Judge
		if u.ClearGenerative && u.Generative == nil {
			gen = r.opts.Env.Generative
		}
		if u.ClearJudge && u.Judge == nil {
			judge = r.opts.Env.Judge
		}
	}
	var res TestResult
	if gen != nil {
		client, err := r.opts.BuildClient(*gen)
		if err != nil {
			res.Generative = CheckResult{Tested: true, Error: err.Error(), Hint: hintFor(err)}
		} else {
			res.Generative = checkGenerative(ctx, client, gen.ResolvedIngestModel())
			if am := gen.ResolvedAnswerModel(); am != gen.ResolvedIngestModel() {
				res.Answer = checkGenerative(ctx, client, am)
			}
		}
	}
	if judge != nil {
		j, err := r.opts.BuildJudge(*judge)
		if err != nil {
			res.Judge = CheckResult{Tested: true, Error: err.Error(), Hint: hintFor(err)}
		} else {
			res.Judge = checkJudge(ctx, j, judge.Model)
		}
	}
	return res, nil
}

// effective returns copies of the settings in effect now.
func (r *Registry) effective() (*Generative, *JudgeSettings) {
	s := r.cur.Load()
	var g *Generative
	var j *JudgeSettings
	if s.gen != nil {
		c := *s.gen
		g = &c
	}
	if s.judge != nil {
		c := *s.judge
		j = &c
	}
	return g, j
}

const checkTimeout = 45 * time.Second

func checkGenerative(ctx context.Context, c llmgate.Client, model string) CheckResult {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	start := time.Now()
	resp, err := c.Complete(ctx, llmgate.Request{
		Model:     model,
		MaxTokens: 16,
		Messages:  []llmgate.Message{{Role: llmgate.RoleUser, Content: "Reply with the single word: ok"}},
	})
	res := CheckResult{Tested: true, Model: model, LatencyMS: time.Since(start).Milliseconds()}
	if err != nil {
		return failed(res, err)
	}
	res.OK = true
	if resp != nil && resp.Model != "" {
		res.Model = resp.Model
	}
	return res
}

func checkJudge(ctx context.Context, j llmgate.Judge, model string) CheckResult {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	start := time.Now()
	out, err := j.Judge(ctx, llmgate.JudgeRequest{
		Model: model,
		State: "Vectorless connection test. The sky is blue.",
		Questions: map[string]llmgate.Question{
			"sky": llmgate.Noul{Instructions: "Does the text say the sky is blue?"},
		},
	})
	res := CheckResult{Tested: true, Model: model, LatencyMS: time.Since(start).Milliseconds()}
	if err != nil {
		return failed(res, err)
	}
	if _, err := out.Noul("sky"); err != nil {
		return failed(res, err)
	}
	res.OK = true
	if out.Model != "" {
		res.Model = out.Model
	}
	return res
}

func failed(res CheckResult, err error) CheckResult {
	res.Error = err.Error()
	var le *llmgate.LLMError
	if errors.As(err, &le) {
		res.Status = le.StatusCode
	}
	res.Hint = hintFor(err)
	return res
}

// hintFor turns a provider failure into the thing to do about it.
func hintFor(err error) string {
	var le *llmgate.LLMError
	status := 0
	if errors.As(err, &le) {
		status = le.StatusCode
	}
	msg := strings.ToLower(err.Error())
	switch {
	case status == 401 || status == 403 || strings.Contains(msg, "api key") || strings.Contains(msg, "unauthorized") || strings.Contains(msg, "authentication"):
		return "The provider rejected the key. Check it was copied whole and belongs to this provider."
	case status == 402 || strings.Contains(msg, "insufficient") || strings.Contains(msg, "credit") || strings.Contains(msg, "balance"):
		return "The provider refused for billing: the account is out of credit. Top it up with the provider."
	case status == 404 || strings.Contains(msg, "model") && strings.Contains(msg, "not found"):
		return "The provider does not know this model, or the base URL path is wrong."
	case status == 429:
		return "The provider is rate limiting this key. Try again shortly."
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(msg, "timeout") || strings.Contains(msg, "no such host") || strings.Contains(msg, "connection refused"):
		return "The engine could not reach the provider. Check the base URL and that this server can reach it."
	}
	return ""
}
