package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
)

// writeJSON encodes v as JSON and writes it with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr writes a JSON error response: {"error": "<msg>"}.
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// marshalJSONForReplay marshals v to JSON exactly as it would be sent
// on the wire so the bytes can be both stored in the replay log AND
// returned to the caller in lock-step. A trailing newline is appended
// to match encoding/json.Encoder.Encode's behaviour so the replay
// path returns byte-identical responses.
func marshalJSONForReplay(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	raw = append(raw, '\n')
	return raw, nil
}

// writeJSONWithReplay writes pre-marshalled JSON bytes verbatim and
// stores them in the replay store under the given token. Both writes
// MUST see the same bytes; this is the single point where that
// invariant is enforced. When store is nil or token is empty the
// replay write is skipped silently.
func writeJSONWithReplay(w http.ResponseWriter, store retrieval.ReplayStore, status int, raw []byte, token string, entry retrieval.ReplayEntry) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
	if store != nil && token != "" {
		entry.ResponseJSON = raw
		entry.CreatedAt = time.Now()
		store.Put(token, entry)
	}
}

// guessContentType infers a MIME type from the file extension.
func guessContentType(filename string) string {
	name := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(name, ".md"), strings.HasSuffix(name, ".markdown"):
		return "text/markdown"
	case strings.HasSuffix(name, ".txt"):
		return "text/plain"
	case strings.HasSuffix(name, ".html"), strings.HasSuffix(name, ".htm"):
		return "text/html"
	case strings.HasSuffix(name, ".pdf"):
		return "application/pdf"
	case strings.HasSuffix(name, ".docx"):
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	}
	return "application/octet-stream"
}

// Token-usage headers the control plane meters (X-Vectorless-Tokens-In/Out).
const (
	hdrTokensIn  = "X-Vectorless-Tokens-In"
	hdrTokensOut = "X-Vectorless-Tokens-Out"
)

// declareTokenTrailers must run before a streamed response writes its
// status: a stream only knows its token counts at the end, so they
// travel as HTTP trailers.
func declareTokenTrailers(h http.Header) {
	h.Set("Trailer", hdrTokensIn+", "+hdrTokensOut)
}

// setTokenHeaders reports a response's token usage, read from its
// "usage" block. On a streamed response, called after the last write,
// the values become the trailers declared earlier.
func setTokenHeaders(h http.Header, resp map[string]any) {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return
	}
	in, out := toInt64(u["input_tokens"]), toInt64(u["output_tokens"])
	h.Set(hdrTokensIn, strconv.FormatInt(in, 10))
	h.Set(hdrTokensOut, strconv.FormatInt(out, 10))
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}
