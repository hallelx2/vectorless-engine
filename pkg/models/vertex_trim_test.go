package models

import "testing"

func TestTrimKeepsVertexFields(t *testing.T) {
	b := 0
	g := trimGenerative(Generative{Provider: " Vertex ", Project: " p ", Location: "US-CENTRAL1", ThinkingBudget: &b, ThinkingLevel: ""})
	if g.Provider != ProviderVertex || g.Project != "p" || g.Location != "us-central1" {
		t.Errorf("trimmed = %+v", g)
	}
	if g.ThinkingBudget == nil || *g.ThinkingBudget != 0 {
		t.Fatalf("thinking budget lost: %v", g.ThinkingBudget)
	}
	b = 9
	if *g.ThinkingBudget != 0 {
		t.Error("trim must copy the budget, not alias it")
	}
}

func TestVertexThinkingLevelIsClosed(t *testing.T) {
	if err := (Generative{Provider: ProviderVertex, ThinkingLevel: "extreme"}).Validate(); err == nil {
		t.Error("an unknown thinking level validated")
	}
	if err := (Generative{Provider: ProviderVertex, ThinkingLevel: "High"}).Validate(); err != nil {
		t.Errorf("High should validate: %v", err)
	}
}
