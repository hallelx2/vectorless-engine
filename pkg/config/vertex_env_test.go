package config

import "testing"

func TestVertexEnv(t *testing.T) {
	t.Setenv("VLS_LLM_VERTEX_PROJECT", "proj-a")
	t.Setenv("VLE_LLM_VERTEX_PROJECT", "proj-b")
	t.Setenv("VLE_LLM_VERTEX_LOCATION", "global")
	t.Setenv("VLS_LLM_VERTEX_MODEL", "gemini-2.5-flash-lite")
	t.Setenv("VLS_LLM_VERTEX_THINKING_BUDGET", "0")

	var v VertexBlock
	ApplyVertexEnv(&v, "VLS_", "VLE_")
	if v.Project != "proj-a" {
		t.Errorf("project = %q, want VLS_ to win", v.Project)
	}
	if v.Location != "global" || v.Model != "gemini-2.5-flash-lite" {
		t.Errorf("location/model = %q/%q", v.Location, v.Model)
	}
	if v.ThinkingBudget == nil || *v.ThinkingBudget != 0 {
		t.Errorf("thinking budget = %v, want 0 (an explicit zero turns thinking off)", v.ThinkingBudget)
	}

	t.Setenv("VLS_LLM_VERTEX_THINKING_BUDGET", "lots")
	var w VertexBlock
	ApplyVertexEnv(&w, "VLS_")
	if w.ThinkingBudget != nil {
		t.Errorf("a garbled budget must be ignored, got %v", *w.ThinkingBudget)
	}
}

func TestVertexDriverValidates(t *testing.T) {
	c := Default()
	c.LLM.Driver = "vertex"
	c.Database.URL = "postgres://localhost/engine"
	if err := c.Validate(); err != nil {
		t.Errorf("Validate with driver vertex = %v", err)
	}
}
