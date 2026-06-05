package config

import (
	"os"
	"testing"
)

// TestForwardTreeWalkMaxCitations covers the server-config
// pass-through for the TreeWalk final-citation cap: the operator can
// set it via VLS_ or VLE_ and have it reach the embedded engine
// config, with VLS_ winning when both are present and a garbled value
// preserving the engine default.
func TestForwardTreeWalkMaxCitations(t *testing.T) {
	prevVLS := os.Getenv("VLS_RETRIEVAL_TREEWALK_MAX_CITATIONS")
	prevVLE := os.Getenv("VLE_RETRIEVAL_TREEWALK_MAX_CITATIONS")
	defer func() {
		os.Setenv("VLS_RETRIEVAL_TREEWALK_MAX_CITATIONS", prevVLS)
		os.Setenv("VLE_RETRIEVAL_TREEWALK_MAX_CITATIONS", prevVLE)
	}()

	// Engine default is 3 with no env set.
	os.Unsetenv("VLS_RETRIEVAL_TREEWALK_MAX_CITATIONS")
	os.Unsetenv("VLE_RETRIEVAL_TREEWALK_MAX_CITATIONS")
	cfg := Default()
	applyEnvOverrides(&cfg)
	if cfg.Engine.Retrieval.TreeWalk.MaxCitations != 3 {
		t.Errorf("default max_citations = %d, want 3", cfg.Engine.Retrieval.TreeWalk.MaxCitations)
	}

	// VLE_ alone forwards through.
	os.Setenv("VLE_RETRIEVAL_TREEWALK_MAX_CITATIONS", "2")
	cfg2 := Default()
	applyEnvOverrides(&cfg2)
	if cfg2.Engine.Retrieval.TreeWalk.MaxCitations != 2 {
		t.Errorf("VLE_ forward: max_citations = %d, want 2", cfg2.Engine.Retrieval.TreeWalk.MaxCitations)
	}

	// VLS_ wins over VLE_.
	os.Setenv("VLS_RETRIEVAL_TREEWALK_MAX_CITATIONS", "1")
	cfg3 := Default()
	applyEnvOverrides(&cfg3)
	if cfg3.Engine.Retrieval.TreeWalk.MaxCitations != 1 {
		t.Errorf("VLS_ should win: max_citations = %d, want 1", cfg3.Engine.Retrieval.TreeWalk.MaxCitations)
	}

	// Garbled value preserves the engine default (3), does not zero it.
	os.Unsetenv("VLS_RETRIEVAL_TREEWALK_MAX_CITATIONS")
	os.Setenv("VLE_RETRIEVAL_TREEWALK_MAX_CITATIONS", "heaps")
	cfg4 := Default()
	applyEnvOverrides(&cfg4)
	if cfg4.Engine.Retrieval.TreeWalk.MaxCitations != 3 {
		t.Errorf("garbage value should preserve default 3, got %d", cfg4.Engine.Retrieval.TreeWalk.MaxCitations)
	}
}
