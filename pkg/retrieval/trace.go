package retrieval

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// SystemPromptVersion is the build-time version stamp folded into every
// trace token. Bump this whenever selectionSystemPrompt (or any other
// retrieval system prompt whose change should invalidate replay) is
// edited so that previously-cached replay entries are no longer
// considered byte-equivalent.
//
// The version is a free-form string; "v1", "v2", … is the established
// convention. Replay clients should treat it as opaque.
const SystemPromptVersion = "v1"

// ComputeTraceToken returns the canonical replay trace token for a
// retrieval result.
//
// The token is sha256(doc_id || \0 || doc_version || \0 ||
// retrieval_model || \0 || system_prompt_version || \0 ||
// sorted(selected_ids).joined("\0")), hex-encoded lowercase. The output
// is 64 hex characters.
//
// Sorting the IDs lexicographically makes the token order-invariant:
// two strategies that select the same set of sections — even via
// different reasoning paths — produce the same token. The NUL separator
// prevents pathological IDs containing the chosen delimiter from
// colliding (e.g. "a,b" + "c" vs "a" + "b,c").
//
// The doc_version parameter is a caller-controlled string so the engine
// can layer document versioning on top in a future phase without
// changing the function signature; today callers pass "1".
func ComputeTraceToken(docID tree.DocumentID, docVersion, model string, ids []tree.SectionID) string {
	// Defensive copy: callers reasonably expect ComputeTraceToken not to
	// mutate the slice they pass in. Sorting in place would be a subtle
	// foot-gun the next time someone reads selected_ids after computing
	// the token.
	sorted := make([]string, len(ids))
	for i, id := range ids {
		sorted[i] = string(id)
	}
	sort.Strings(sorted)

	h := sha256.New()
	h.Write([]byte(string(docID)))
	h.Write([]byte{0})
	h.Write([]byte(docVersion))
	h.Write([]byte{0})
	h.Write([]byte(model))
	h.Write([]byte{0})
	h.Write([]byte(SystemPromptVersion))
	h.Write([]byte{0})
	for i, id := range sorted {
		if i > 0 {
			h.Write([]byte{0})
		}
		h.Write([]byte(id))
	}
	return hex.EncodeToString(h.Sum(nil))
}
