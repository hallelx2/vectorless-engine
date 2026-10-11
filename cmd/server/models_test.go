package main

import (
	"context"
	"errors"
	"testing"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/internal/config"
	"github.com/hallelx2/vectorless-engine/pkg/models"
)

// TestKeylessBootConfig: a server with no model key in its environment
// loads its config, and its registry builds with no provider instead of
// failing — the engine starts and answers "model keys not configured".
func TestKeylessBootConfig(t *testing.T) {
	for _, k := range []string{"VLS_ANTHROPIC_API_KEY", "VLE_ANTHROPIC_API_KEY", "VLS_LLM_ANTHROPIC_API_KEY", "VLE_LLM_ANTHROPIC_API_KEY", "TYPESAFE_API_KEY", "VLS_TYPESAFE_API_KEY", "VLE_TYPESAFE_API_KEY", "ANTHROPIC_API_KEY"} {
		t.Setenv(k, "")
	}
	t.Setenv("VLS_DATABASE_URL", "postgres://x@localhost/x")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("config without keys must load: %v", err)
	}
	env := envModelSettings(cfg)
	if env.Generative != nil || env.Judge != nil {
		t.Fatalf("keyless env produced settings: %+v", env)
	}
	reg := models.New(models.Options{Env: env})
	if reg.GenerativeConfigured() {
		t.Fatal("keyless registry reports a provider")
	}
	_, err = reg.Client().Complete(context.Background(), llmgate.Request{})
	if !errors.Is(err, models.ErrNotConfigured) {
		t.Fatalf("keyless call: %v", err)
	}
}

func TestEnvModelSettingsFromEnvironment(t *testing.T) {
	t.Setenv("VLS_DATABASE_URL", "postgres://x@localhost/x")
	t.Setenv("VLS_LLM_DRIVER", "anthropic")
	t.Setenv("VLE_LLM_ANTHROPIC_API_KEY", "anthropic-env-key-0001")
	t.Setenv("VLE_LLM_ANTHROPIC_BASE_URL", "https://api.z.ai/api/anthropic/v1")
	t.Setenv("VLE_LLM_ANTHROPIC_MODEL", "glm-4.5-air")
	t.Setenv("VLS_LLM_ANSWER_MODEL", "glm-4.6")
	t.Setenv("TYPESAFE_API_KEY", "typesafe-env-key-0001")
	t.Setenv("VLS_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	env := envModelSettings(cfg)
	if env.Generative == nil || env.Generative.Provider != "anthropic" || env.Generative.BaseURL != "https://api.z.ai/api/anthropic/v1" ||
		env.Generative.IngestModel != "glm-4.5-air" || env.Generative.AnswerModel != "glm-4.6" {
		t.Fatalf("generative = %+v", env.Generative)
	}
	if env.Judge == nil || env.Judge.APIKey != "typesafe-env-key-0001" {
		t.Fatalf("judge = %+v", env.Judge)
	}
	if cfg.Models.EncryptionKey == "" {
		t.Fatal("encryption key not read from VLS_ENCRYPTION_KEY")
	}
}

func TestEnvOpenAICompatible(t *testing.T) {
	t.Setenv("VLS_DATABASE_URL", "postgres://x@localhost/x")
	t.Setenv("VLS_LLM_DRIVER", "openai-compatible")
	t.Setenv("VLS_LLM_OPENAI_BASE_URL", "http://ollama:11434/v1")
	t.Setenv("VLS_LLM_OPENAI_MODEL", "llama3.1")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	env := envModelSettings(cfg)
	if env.Generative == nil || env.Generative.Provider != models.ProviderOpenAICompatible || env.Generative.IngestModel != "llama3.1" {
		t.Fatalf("generative = %+v", env.Generative)
	}
}
