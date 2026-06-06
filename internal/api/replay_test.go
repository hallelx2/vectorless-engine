package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// newReplayRouter wires only the routes /v1/replay actually touches.
// This avoids spinning up DB / Storage / Queue / Strategy just to
// exercise the replay endpoint contract.
func newReplayRouter(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Post("/replay", d.handleReplay)
	})
	return r
}

// TestReplayByteExact: the central invariant of Phase 3.1.
// Put a response into the store, replay it, assert the bytes
// returned by the handler match what was stored — character for
// character.
func TestReplayByteExact(t *testing.T) {
	store := retrieval.NewLRUReplayStore(retrieval.LRUReplayConfig{MaxEntries: 10})
	want := []byte(`{"answer":"hello","strategy":"chunked-tree","trace_token":"abc123"}` + "\n")
	store.Put("token-1", retrieval.ReplayEntry{
		DocumentID:   "doc_x",
		Query:        "what is x?",
		ResponseJSON: want,
	})

	d := Deps{Replay: store}
	srv := httptest.NewServer(newReplayRouter(d))
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{
		"trace_token": "token-1",
		"query":       "what is x?",
		"document_id": "doc_x",
	})
	resp, err := http.Post(srv.URL+"/v1/replay", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }() // best-effort close
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, want) {
		t.Errorf("replay bytes differ:\n  got  %q\n  want %q", got, want)
	}
}

// TestReplayUnknownToken: 404 with a clear error message.
func TestReplayUnknownToken(t *testing.T) {
	store := retrieval.NewLRUReplayStore(retrieval.LRUReplayConfig{MaxEntries: 10})
	d := Deps{Replay: store}
	srv := httptest.NewServer(newReplayRouter(d))
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{
		"trace_token": "never-stored",
		"query":       "q",
		"document_id": "doc_x",
	})
	resp, err := http.Post(srv.URL+"/v1/replay", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }() // best-effort close
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestReplayDocumentIDMismatch: 409 with details=document_id differs.
func TestReplayDocumentIDMismatch(t *testing.T) {
	store := retrieval.NewLRUReplayStore(retrieval.LRUReplayConfig{MaxEntries: 10})
	store.Put("t", retrieval.ReplayEntry{
		DocumentID:   "doc_real",
		Query:        "q",
		ResponseJSON: []byte(`{"x":1}` + "\n"),
	})
	d := Deps{Replay: store}
	srv := httptest.NewServer(newReplayRouter(d))
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{
		"trace_token": "t",
		"query":       "q",
		"document_id": "doc_fake",
	})
	resp, err := http.Post(srv.URL+"/v1/replay", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }() // best-effort close
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var errBody map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&errBody)
	if errBody["error"] != "input mismatch" {
		t.Errorf("error = %q, want input mismatch", errBody["error"])
	}
	if !strings.Contains(errBody["details"], "document_id") {
		t.Errorf("details should mention document_id, got %q", errBody["details"])
	}
}

// TestReplayQueryMismatch: 409 with details=query differs.
func TestReplayQueryMismatch(t *testing.T) {
	store := retrieval.NewLRUReplayStore(retrieval.LRUReplayConfig{MaxEntries: 10})
	store.Put("t", retrieval.ReplayEntry{
		DocumentID:   "doc_x",
		Query:        "real query",
		ResponseJSON: []byte(`{"x":1}` + "\n"),
	})
	d := Deps{Replay: store}
	srv := httptest.NewServer(newReplayRouter(d))
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{
		"trace_token": "t",
		"query":       "tampered query",
		"document_id": "doc_x",
	})
	resp, err := http.Post(srv.URL+"/v1/replay", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }() // best-effort close
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var errBody map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&errBody)
	if !strings.Contains(errBody["details"], "query") {
		t.Errorf("details should mention query, got %q", errBody["details"])
	}
}

// TestReplayDisabled: when Deps.Replay is nil the endpoint returns
// 501 Not Implemented. This is the opt-out path documented in the
// config block.
func TestReplayDisabled(t *testing.T) {
	d := Deps{Replay: nil}
	srv := httptest.NewServer(newReplayRouter(d))
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{
		"trace_token": "anything",
		"query":       "q",
		"document_id": "doc_x",
	})
	resp, err := http.Post(srv.URL+"/v1/replay", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }() // best-effort close
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", resp.StatusCode)
	}
}

// TestReplayRequiresFields: every field in the body is required.
// A missing field is a client error, not a 404 (which would
// otherwise be confusing — "the token isn't found" when really
// "you didn't send a token").
func TestReplayRequiresFields(t *testing.T) {
	store := retrieval.NewLRUReplayStore(retrieval.LRUReplayConfig{MaxEntries: 10})
	d := Deps{Replay: store}
	srv := httptest.NewServer(newReplayRouter(d))
	defer srv.Close()

	cases := []map[string]any{
		{"query": "q", "document_id": "doc_x"},                  // missing trace_token
		{"trace_token": "t", "document_id": "doc_x"},            // missing query
		{"trace_token": "t", "query": "q"},                      // missing document_id
		{"trace_token": "", "query": "q", "document_id": "doc"}, // empty trace_token
	}
	for i, body := range cases {
		raw, _ := json.Marshal(body)
		resp, err := http.Post(srv.URL+"/v1/replay", "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		_ = resp.Body.Close() // best-effort close
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("case %d: status = %d, want 400", i, resp.StatusCode)
		}
	}
}

// TestReplayBadJSON: malformed JSON request body → 400.
func TestReplayBadJSON(t *testing.T) {
	store := retrieval.NewLRUReplayStore(retrieval.LRUReplayConfig{MaxEntries: 10})
	d := Deps{Replay: store}
	srv := httptest.NewServer(newReplayRouter(d))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/replay", "application/json", strings.NewReader("{not json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }() // best-effort close
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestReplayEndToEndByteExact simulates the production flow: the
// server marshals a /v1/query response via marshalJSONForReplay,
// stores it under the same trace token it surfaced to the client,
// and the replay endpoint hands the bytes back verbatim. This is
// the end-to-end byte-exactness invariant the Phase 3.1 spec
// demands.
//
// The test uses Go's encoding/json directly (the same package the
// handler uses) so any drift between "serialised on write" and
// "served on replay" surfaces here.
func TestReplayEndToEndByteExact(t *testing.T) {
	store := retrieval.NewLRUReplayStore(retrieval.LRUReplayConfig{MaxEntries: 10})
	d := Deps{Replay: store}

	// Build a representative response that exercises Go map JSON
	// emission (lexicographic key sort) and a varied payload shape.
	traceToken := retrieval.ComputeTraceToken("doc_x", "1", "claude-sonnet-4-5",
		[]tree.SectionID{"sec_a", "sec_b"})
	resp := map[string]any{
		"document_id": "doc_x",
		"query":       "what does the report say?",
		"strategy":    "chunked-tree",
		"model":       "claude-sonnet-4-5",
		"sections": []map[string]any{
			{"id": "sec_a", "title": "Setup"},
			{"id": "sec_b", "title": "Usage"},
		},
		"elapsed_ms":  42,
		"trace_token": traceToken,
	}

	raw, err := marshalJSONForReplay(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Simulate the handler writing the response to wire AND storing.
	d.Replay.Put(traceToken, retrieval.ReplayEntry{
		DocumentID:   "doc_x",
		Query:        "what does the report say?",
		Model:        "claude-sonnet-4-5",
		ResponseJSON: raw,
	})

	// Re-marshal the same Go value: encoding/json sorts map keys
	// lexicographically, so the bytes must be identical. This is
	// the property that makes byte-exact replay viable even when
	// the response is built from a map[string]any rather than a
	// struct with a fixed field order.
	raw2, err := marshalJSONForReplay(resp)
	if err != nil {
		t.Fatalf("remarshal: %v", err)
	}
	if !bytes.Equal(raw, raw2) {
		t.Errorf("encoding/json is non-deterministic on map[string]any:\n  first  %q\n  second %q", raw, raw2)
	}

	// Replay over HTTP. Bytes must equal what we stored.
	srv := httptest.NewServer(newReplayRouter(d))
	defer srv.Close()
	body, _ := json.Marshal(map[string]any{
		"trace_token": traceToken,
		"query":       "what does the report say?",
		"document_id": "doc_x",
	})
	got, err := http.Post(srv.URL+"/v1/replay", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = got.Body.Close() }() // best-effort close
	if got.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", got.StatusCode)
	}
	gotBytes, _ := io.ReadAll(got.Body)
	if !bytes.Equal(gotBytes, raw) {
		t.Errorf("end-to-end byte drift:\n  stored %q\n  got    %q", raw, gotBytes)
	}
}

// TestReplayPreservesUnicodeAndWhitespace replays a payload chosen
// to expose any normalisation in the storage path: unicode, mixed
// whitespace, embedded newlines. The byte sequence must come back
// identical.
func TestReplayPreservesUnicodeAndWhitespace(t *testing.T) {
	store := retrieval.NewLRUReplayStore(retrieval.LRUReplayConfig{MaxEntries: 10})
	// Hand-crafted bytes — deliberately not pretty-printed JSON, and
	// includes content that round-tripping through encoding/json
	// would re-escape.
	want := []byte("{\"text\":\"héllo\\nworld 🌍\",\"k\":  42}\n")
	store.Put("u", retrieval.ReplayEntry{
		DocumentID:   tree.DocumentID("doc_u"),
		Query:        "q",
		ResponseJSON: want,
	})

	d := Deps{Replay: store}
	srv := httptest.NewServer(newReplayRouter(d))
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{
		"trace_token": "u",
		"query":       "q",
		"document_id": "doc_u",
	})
	resp, err := http.Post(srv.URL+"/v1/replay", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }() // best-effort close
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, want) {
		t.Errorf("byte drift:\n  got  %q\n  want %q", got, want)
	}
}
