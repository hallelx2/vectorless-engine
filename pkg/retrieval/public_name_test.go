package retrieval

import (
	"strings"
	"testing"
)

func TestPublicNameNeverSaysJudge(t *testing.T) {
	for _, internal := range []string{strategyNameJudgeWalk, strategyNameTreeWalk, "agentic", "chunked-tree", "single-pass"} {
		public := PublicName(internal)
		if strings.Contains(strings.ToLower(public), "judge") {
			t.Errorf("%s is shown to customers as %q", internal, public)
		}
		if InternalName(public) != internal {
			t.Errorf("%s does not round-trip: public %q resolves to %q", internal, public, InternalName(public))
		}
	}
}
