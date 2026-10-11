package ingest

import "testing"

func TestModeFor(t *testing.T) {
	p := &Pipeline{Mode: ModeTOC}
	if got := p.modeFor(Payload{}); got != ModeTOC {
		t.Fatalf("default: %q, want toc", got)
	}
	if got := p.modeFor(Payload{Mode: ModeFull}); got != ModeFull {
		t.Fatalf("override: %q, want full", got)
	}
	for _, m := range []string{"", "toc", "full", "minimal"} {
		if !ValidMode(m) {
			t.Errorf("%q should be valid", m)
		}
	}
	for _, m := range []string{"hybrid", "extract", "generate", "FULL"} {
		if ValidMode(m) {
			t.Errorf("%q should be invalid", m)
		}
	}
}
