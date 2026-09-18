package config

import "testing"

func TestJudgeKeyFromEnv(t *testing.T) {
	t.Setenv("VLE_TYPESAFE_API_KEY", "")
	t.Setenv("TYPESAFE_API_KEY", "bare")
	c := Default()
	applyEnvOverrides(&c)
	if c.LLM.Judge.TypeSafe.APIKey != "bare" {
		t.Errorf("bare TYPESAFE_API_KEY not picked up: %q", c.LLM.Judge.TypeSafe.APIKey)
	}
	t.Setenv("VLE_TYPESAFE_API_KEY", "prefixed")
	c = Default()
	applyEnvOverrides(&c)
	if c.LLM.Judge.TypeSafe.APIKey != "prefixed" {
		t.Errorf("VLE_ prefix should win: %q", c.LLM.Judge.TypeSafe.APIKey)
	}
}

func TestJudgeIsOffByDefault(t *testing.T) {
	t.Setenv("VLE_TYPESAFE_API_KEY", "")
	t.Setenv("TYPESAFE_API_KEY", "")
	c := Default()
	applyEnvOverrides(&c)
	if c.LLM.Judge.TypeSafe.APIKey != "" {
		t.Errorf("no key configured, got %q", c.LLM.Judge.TypeSafe.APIKey)
	}
}
