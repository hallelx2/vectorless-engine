// Package models owns the engine's model providers at runtime: the
// generative model every ingest, navigation and answer call goes
// through, and the Judge (System One) that reads structure and chooses
// pincites.
//
// Providers come from two places. The environment (VLE_*/VLS_* and the
// YAML config) is read once at boot. An instance administrator can also
// enter them in the dashboard; those are sealed with AES-256-GCM under
// the instance encryption key and stored in instance_settings. Settings
// entered in the dashboard win over the environment for the provider
// they configure; clearing them falls back to the environment again.
//
// The rest of the engine holds a Registry's Client and Judge, which are
// stable proxies: a change is picked up on the next call, on every
// replica, without a restart. With no provider configured the engine
// still boots, and every call that needs a model fails with a typed
// NotConfiguredError rather than crashing the process.
package models

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Provider names accepted for the generative model.
const (
	ProviderAnthropic        = "anthropic"
	ProviderOpenAI           = "openai"
	ProviderGemini           = "gemini"
	ProviderOpenAICompatible = "openai-compatible"
	// ProviderVertex is Gemini through Google Cloud Vertex AI. It takes no
	// API key: Application Default Credentials authenticate it, and the
	// project it names is billed.
	ProviderVertex = "vertex"
)

// Providers lists the generative providers in the order the dashboard
// offers them.
var Providers = []string{ProviderAnthropic, ProviderOpenAI, ProviderGemini, ProviderVertex, ProviderOpenAICompatible}

// DefaultModels is the model used when a provider is chosen and no model
// is named. openai-compatible has none: its server decides what exists.
var DefaultModels = map[string]string{
	ProviderAnthropic: "claude-sonnet-4-5",
	ProviderOpenAI:    "gpt-4.1-mini",
	ProviderGemini:    "gemini-2.5-flash",
	ProviderVertex:    "gemini-2.5-flash-lite",
}

// Source says where an effective setting came from.
type Source string

const (
	SourceNone Source = "none"
	SourceEnv  Source = "env"
	SourceUI   Source = "ui"
)

// Generative configures the model that writes: ingest summaries and the
// contents fallback, navigation, and answers.
type Generative struct {
	Provider string `json:"provider"`
	APIKey   string `json:"api_key,omitempty"`
	// BaseURL points the provider at a compatible gateway: GLM's
	// Anthropic endpoint, OpenRouter, Ollama, vLLM, Azure. Required for
	// openai-compatible; optional for anthropic and openai.
	BaseURL string `json:"base_url,omitempty"`
	// AnswerModel writes the final answer. Empty uses IngestModel.
	AnswerModel string `json:"answer_model,omitempty"`
	// IngestModel is the default for every other call: ingest stages and
	// navigation. Empty uses the provider's default.
	IngestModel string `json:"ingest_model,omitempty"`

	// Project and Location address Vertex (provider "vertex" only).
	// Empty Project falls back to GOOGLE_CLOUD_PROJECT, then to the
	// credentials' project; empty Location is "global".
	Project  string `json:"project,omitempty"`
	Location string `json:"location,omitempty"`
	// ThinkingBudget and ThinkingLevel cap Gemini's thinking on Vertex.
	// At most one is set; neither leaves the model's default.
	ThinkingBudget *int   `json:"thinking_budget,omitempty"`
	ThinkingLevel  string `json:"thinking_level,omitempty"`
}

// JudgeSettings configures the Judge (TypeSafe Jev).
type JudgeSettings struct {
	APIKey  string `json:"api_key,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
	Model   string `json:"model,omitempty"`
}

// Settings is the stored document: what the administrator entered.
type Settings struct {
	Generative *Generative    `json:"generative,omitempty"`
	Judge      *JudgeSettings `json:"judge,omitempty"`
}

// ResolvedIngestModel is the default model for every call that does not
// name one.
func (g Generative) ResolvedIngestModel() string {
	if m := strings.TrimSpace(g.IngestModel); m != "" {
		return m
	}
	return DefaultModels[g.Provider]
}

// ResolvedAnswerModel is the model that writes answers.
func (g Generative) ResolvedAnswerModel() string {
	if m := strings.TrimSpace(g.AnswerModel); m != "" {
		return m
	}
	return g.ResolvedIngestModel()
}

// Validate reports whether g can be built.
func (g Generative) Validate() error {
	switch g.Provider {
	case ProviderAnthropic, ProviderOpenAI, ProviderGemini:
		if strings.TrimSpace(g.APIKey) == "" {
			return fmt.Errorf("%s needs an API key", g.Provider)
		}
	case ProviderVertex:
		if strings.TrimSpace(g.BaseURL) != "" {
			return errors.New("vertex does not take a base URL; set project and location instead")
		}
		if g.ThinkingBudget != nil && strings.TrimSpace(g.ThinkingLevel) != "" {
			return errors.New("vertex takes a thinking budget or a thinking level, not both")
		}
		if g.ThinkingBudget != nil && *g.ThinkingBudget < 0 {
			return errors.New("vertex thinking budget cannot be negative")
		}
		switch strings.TrimSpace(strings.ToLower(g.ThinkingLevel)) {
		case "", "minimal", "low", "medium", "high":
		default:
			return fmt.Errorf("vertex thinking level %q is not one of minimal, low, medium, high", g.ThinkingLevel)
		}
		return nil
	case ProviderOpenAICompatible:
		if strings.TrimSpace(g.BaseURL) == "" {
			return errors.New("openai-compatible needs a base URL (for example http://ollama:11434/v1)")
		}
		if g.ResolvedIngestModel() == "" {
			return errors.New("openai-compatible needs a model name: the server decides which models exist")
		}
	case "":
		return errors.New("provider is required")
	default:
		return fmt.Errorf("unknown provider %q (want one of %s)", g.Provider, strings.Join(Providers, ", "))
	}
	if g.Provider == ProviderGemini && strings.TrimSpace(g.BaseURL) != "" {
		return errors.New("gemini does not take a base URL; use openai-compatible for a Gemini-compatible gateway")
	}
	return validateURL("base URL", g.BaseURL)
}

// Validate reports whether j can be built.
func (j JudgeSettings) Validate() error {
	if strings.TrimSpace(j.APIKey) == "" {
		return errors.New("the TypeSafe Jev structure model needs an API key")
	}
	return validateURL("TypeSafe base URL", j.BaseURL)
}

func validateURL(what, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s must be an http(s) URL, got %q", what, raw)
	}
	return nil
}

// keyHint shows the last four characters of a long key so an
// administrator can tell which key is in use without seeing it.
func keyHint(key string) string {
	key = strings.TrimSpace(key)
	if len(key) < 12 {
		if key == "" {
			return ""
		}
		return "set"
	}
	return "…" + key[len(key)-4:]
}

// ErrNotConfigured is matched (errors.Is) by every NotConfiguredError.
var ErrNotConfigured = errors.New("model keys not configured")

// NotConfiguredError is returned by a call that needs a model the
// instance has no key for. Kind is "generative" or "judge".
type NotConfiguredError struct {
	Kind string
	// Cause is the build error when settings exist but are unusable.
	Cause error
}

func (e *NotConfiguredError) Error() string {
	what := "a model provider"
	if e.Kind == "judge" {
		what = "the structure model (TypeSafe Jev)"
	}
	msg := "model keys not configured: an administrator must add " + what + " in Settings → Model providers"
	if e.Cause != nil {
		msg += " (" + e.Cause.Error() + ")"
	}
	return msg
}

// Is makes errors.Is(err, ErrNotConfigured) true.
func (e *NotConfiguredError) Is(target error) bool { return target == ErrNotConfigured }

// Unwrap exposes the build error, when there is one.
func (e *NotConfiguredError) Unwrap() error { return e.Cause }

// ModelUnavailable marks the error as the provider being unavailable,
// not the request being wrong; retrieval's fallback honours it.
func (e *NotConfiguredError) ModelUnavailable() bool { return true }

// NotConfigured marks the error as expected configuration state, so a
// fallback taken because of it is not logged as an incident.
func (e *NotConfiguredError) NotConfigured() bool { return true }

// Status is the redacted view of the effective configuration: what the
// dashboard shows. It never carries a key.
type Status struct {
	Generative GenerativeStatus `json:"generative"`
	Judge      JudgeStatus      `json:"judge"`
	// EncryptionConfigured is false when the instance has no encryption
	// key: keys can then only come from the environment.
	EncryptionConfigured bool `json:"encryption_configured"`
	// UpdatedAt is when the dashboard's settings last changed; nil when
	// they never have.
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	UpdatedBy string     `json:"updated_by,omitempty"`
	// StoreError reports stored settings that could not be read (for
	// example a changed encryption key).
	StoreError string `json:"store_error,omitempty"`
}

// GenerativeStatus describes the effective generative provider.
type GenerativeStatus struct {
	Configured  bool   `json:"configured"`
	Source      Source `json:"source"`
	Provider    string `json:"provider,omitempty"`
	BaseURL     string `json:"base_url,omitempty"`
	AnswerModel string `json:"answer_model,omitempty"`
	IngestModel string `json:"ingest_model,omitempty"`
	KeyHint     string `json:"key_hint,omitempty"`
	// EnvAvailable says the environment also configures a provider, which
	// takes over if the dashboard's settings are cleared.
	EnvAvailable bool   `json:"env_available"`
	Error        string `json:"error,omitempty"`
}

// JudgeStatus describes the effective Judge.
type JudgeStatus struct {
	Configured   bool   `json:"configured"`
	Source       Source `json:"source"`
	BaseURL      string `json:"base_url,omitempty"`
	Model        string `json:"model,omitempty"`
	KeyHint      string `json:"key_hint,omitempty"`
	EnvAvailable bool   `json:"env_available"`
	Error        string `json:"error,omitempty"`
}

// Update is a change from the dashboard. A nil group leaves it as it is;
// Clear* removes the dashboard's value so the environment applies again.
// An empty APIKey keeps the stored key when the provider is unchanged,
// so the dashboard never has to hold a key to edit a model name.
type Update struct {
	Generative      *Generative    `json:"generative,omitempty"`
	Judge           *JudgeSettings `json:"judge,omitempty"`
	ClearGenerative bool           `json:"clear_generative,omitempty"`
	ClearJudge      bool           `json:"clear_judge,omitempty"`
}

// apply merges u onto stored and returns the result. It validates every
// group it touches.
func (u Update) apply(stored Settings) (Settings, error) {
	out := Settings{}
	if stored.Generative != nil {
		g := *stored.Generative
		out.Generative = &g
	}
	if stored.Judge != nil {
		j := *stored.Judge
		out.Judge = &j
	}
	if u.ClearGenerative {
		out.Generative = nil
	}
	if u.ClearJudge {
		out.Judge = nil
	}
	if u.Generative != nil {
		g := trimGenerative(*u.Generative)
		if g.APIKey == "" && out.Generative != nil && out.Generative.Provider == g.Provider {
			g.APIKey = out.Generative.APIKey
		}
		if err := g.Validate(); err != nil {
			return Settings{}, fmt.Errorf("model provider: %w", err)
		}
		out.Generative = &g
	}
	if u.Judge != nil {
		j := trimJudge(*u.Judge)
		if j.APIKey == "" && out.Judge != nil {
			j.APIKey = out.Judge.APIKey
		}
		if err := j.Validate(); err != nil {
			return Settings{}, fmt.Errorf("structure model: %w", err)
		}
		out.Judge = &j
	}
	return out, nil
}

func trimGenerative(g Generative) Generative {
	return Generative{
		Provider:    strings.TrimSpace(strings.ToLower(g.Provider)),
		APIKey:      strings.TrimSpace(g.APIKey),
		BaseURL:     strings.TrimRight(strings.TrimSpace(g.BaseURL), "/"),
		AnswerModel: strings.TrimSpace(g.AnswerModel),
		IngestModel: strings.TrimSpace(g.IngestModel),
		// Vertex: dropping these would silently bill a different project
		// or think by default.
		Project:        strings.TrimSpace(g.Project),
		Location:       strings.TrimSpace(strings.ToLower(g.Location)),
		ThinkingBudget: copyInt(g.ThinkingBudget),
		ThinkingLevel:  strings.TrimSpace(strings.ToLower(g.ThinkingLevel)),
	}
}

func copyInt(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func trimJudge(j JudgeSettings) JudgeSettings {
	return JudgeSettings{
		APIKey:  strings.TrimSpace(j.APIKey),
		BaseURL: strings.TrimRight(strings.TrimSpace(j.BaseURL), "/"),
		Model:   strings.TrimSpace(j.Model),
	}
}
