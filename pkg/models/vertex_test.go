package models

import (
	"strings"
	"testing"
)

func intp(n int) *int { return &n }

func TestVertexValidate(t *testing.T) {
	ok := []Generative{
		{Provider: ProviderVertex},
		{Provider: ProviderVertex, Project: "p", Location: "us-central1", IngestModel: "gemini-3.5-flash-lite", ThinkingBudget: intp(0)},
		{Provider: ProviderVertex, ThinkingLevel: "low"},
	}
	for _, g := range ok {
		if err := g.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v, want nil (vertex needs no API key)", g, err)
		}
	}
	bad := map[string]Generative{
		"base URL":     {Provider: ProviderVertex, BaseURL: "https://example.com"},
		"both":         {Provider: ProviderVertex, ThinkingBudget: intp(0), ThinkingLevel: "low"},
		"negative cap": {Provider: ProviderVertex, ThinkingBudget: intp(-1)},
	}
	for name, g := range bad {
		if err := g.Validate(); err == nil {
			t.Errorf("%s: Validate = nil, want an error", name)
		}
	}
}

func TestVertexDefaultsAndListing(t *testing.T) {
	g := Generative{Provider: ProviderVertex}
	if got := g.ResolvedIngestModel(); got != "gemini-2.5-flash-lite" {
		t.Errorf("default model = %q", got)
	}
	if !strings.Contains(strings.Join(Providers, ","), ProviderVertex) {
		t.Error("vertex missing from Providers")
	}
}
