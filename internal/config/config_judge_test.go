package config

import "testing"

// TestForwardJudgeAndStrategy: the server takes the Judge's key, endpoint
// and model, and the retrieval strategy, from the environment, so a
// deployment never has to put the key in a YAML file. VLS_ wins over
// VLE_, which wins over the provider's own TYPESAFE_ names.
func TestForwardJudgeAndStrategy(t *testing.T) {
	for _, k := range []string{"VLS_TYPESAFE_API_KEY", "VLE_TYPESAFE_API_KEY", "VLS_TYPESAFE_BASE_URL", "VLS_TYPESAFE_MODEL", "VLS_RETRIEVAL_STRATEGY", "VLE_RETRIEVAL_STRATEGY"} {
		t.Setenv(k, "")
	}
	t.Setenv("TYPESAFE_API_KEY", "provider-key")
	t.Setenv("TYPESAFE_BASE_URL", "https://judge.example")
	t.Setenv("TYPESAFE_MODEL", "jev")
	t.Setenv("VLE_RETRIEVAL_STRATEGY", "judgewalk")

	cfg := Default()
	applyEnvOverrides(&cfg)
	j := cfg.Engine.LLM.Judge.TypeSafe
	if j.APIKey != "provider-key" || j.BaseURL != "https://judge.example" || j.Model != "jev" {
		t.Fatalf("judge = %+v", j)
	}
	if cfg.Engine.Retrieval.Strategy != "judgewalk" {
		t.Fatalf("strategy = %q", cfg.Engine.Retrieval.Strategy)
	}

	t.Setenv("VLE_TYPESAFE_API_KEY", "engine-key")
	t.Setenv("VLS_TYPESAFE_API_KEY", "server-key")
	cfg = Default()
	applyEnvOverrides(&cfg)
	if got := cfg.Engine.LLM.Judge.TypeSafe.APIKey; got != "server-key" {
		t.Fatalf("VLS_ must win: got %q", got)
	}
}
