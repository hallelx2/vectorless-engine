package handler

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/internal/config"
	"github.com/hallelx2/vectorless-engine/pkg/models"
)

type memModelStore struct {
	mu  sync.Mutex
	row *models.StoredValue
}

func (m *memModelStore) Get(context.Context) (models.StoredValue, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.row == nil {
		return models.StoredValue{}, false, nil
	}
	return *m.row, true, nil
}

func (m *memModelStore) Put(_ context.Context, v []byte, by string) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	at := time.Now()
	m.row = &models.StoredValue{Value: v, UpdatedAt: at, UpdatedBy: by}
	return at, nil
}

func (m *memModelStore) Delete(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.row = nil
	return nil
}

func (m *memModelStore) UpdatedAt(context.Context) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.row == nil {
		return time.Time{}, nil
	}
	return m.row.UpdatedAt, nil
}

const engineToken = "engine-token-for-tests"

// keylessServer is the engine as a self-host install boots it: auth on,
// an encryption key, and no model provider at all.
func keylessServer(t *testing.T) (*httptest.Server, *models.Registry) {
	t.Helper()
	c, err := models.NewCipher(strings.Repeat("k", 40))
	if err != nil {
		t.Fatal(err)
	}
	reg := models.New(models.Options{
		Store:  &memModelStore{},
		Cipher: c,
		BuildClient: func(g models.Generative) (llmgate.Client, error) {
			return &llmgate.Mock{Reply: "ok"}, nil
		},
		BuildJudge: func(models.JudgeSettings) (llmgate.Judge, error) { return &llmgate.MockJudge{}, nil },
	})
	cfg := config.Default()
	cfg.Auth.Mode = "api_key"
	cfg.Auth.APIKey = engineToken
	srv := httptest.NewServer(Router(Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config: cfg,
		Models: reg,
		LLM:    reg.Client(),
	}))
	t.Cleanup(srv.Close)
	return srv, reg
}

func do(t *testing.T, method, url, token, body string, hdr ...string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func TestKeylessEngineServesAndRefusesModelRoutesTyped(t *testing.T) {
	srv, _ := keylessServer(t)
	if code, _ := do(t, "GET", srv.URL+"/v1/health", "", ""); code != 200 {
		t.Fatalf("health = %d, want 200 on a keyless engine", code)
	}
	for _, path := range []string{"/v1/query/", "/v1/answer", "/v1/answer/treewalk", "/v1/answer/store", "/v1/documents/"} {
		code, body := do(t, "POST", srv.URL+path, engineToken, `{}`)
		if code != http.StatusServiceUnavailable || body["code"] != "models_not_configured" {
			t.Errorf("POST %s = %d %v, want 503 models_not_configured", path, code, body)
		}
		if msg, _ := body["error"].(string); !strings.Contains(msg, "model keys not configured") {
			t.Errorf("POST %s error = %q", path, msg)
		}
	}
	code, body := do(t, "GET", srv.URL+"/v1/models/status", engineToken, "")
	if code != 200 {
		t.Fatalf("models status = %d", code)
	}
	if g := body["generative"].(map[string]any); g["configured"] != false {
		t.Fatalf("status says configured: %v", body)
	}
}

func TestAdminModelsRequiresEngineToken(t *testing.T) {
	srv, _ := keylessServer(t)
	for _, c := range []struct{ method, path, token string }{
		{"GET", "/admin/v1/models/", ""},
		{"GET", "/admin/v1/models/", "wrong"},
		{"PUT", "/admin/v1/models/", ""},
		{"POST", "/admin/v1/models/test", "wrong"},
	} {
		if code, _ := do(t, c.method, srv.URL+c.path, c.token, `{}`); code != http.StatusUnauthorized {
			t.Errorf("%s %s with token %q = %d, want 401", c.method, c.path, c.token, code)
		}
	}
	if code, _ := do(t, "GET", srv.URL+"/admin/v1/models/", engineToken, ""); code != 200 {
		t.Fatalf("GET with the engine token = %d", code)
	}
}

func TestAdminModelsRefusedWithoutAuthMode(t *testing.T) {
	reg := models.New(models.Options{Store: &memModelStore{}})
	srv := httptest.NewServer(Router(Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Config: config.Default(), Models: reg}))
	defer srv.Close()
	if code, _ := do(t, "GET", srv.URL+"/admin/v1/models/", "", ""); code != http.StatusForbidden {
		t.Fatalf("admin on an auth.mode=none engine = %d, want 403", code)
	}
}

func TestAdminModelsSaveAppliesWithoutRestart(t *testing.T) {
	srv, reg := keylessServer(t)
	code, body := do(t, "PUT", srv.URL+"/admin/v1/models/", engineToken,
		`{"generative":{"provider":"anthropic","api_key":"sk-test-000000000000abcd","base_url":"https://api.z.ai/api/anthropic/v1","answer_model":"glm-4.5-air","ingest_model":"glm-4.5-air"}}`,
		"X-Vectorless-Actor", "owner@example.com")
	if code != 200 {
		t.Fatalf("PUT = %d %v", code, body)
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "sk-test") {
		t.Fatalf("response carries the key: %s", raw)
	}
	g := body["generative"].(map[string]any)
	if g["configured"] != true || g["source"] != "ui" || g["key_hint"] != "…abcd" || body["updated_by"] != "owner@example.com" {
		t.Fatalf("status after PUT = %v", body)
	}
	if !reg.GenerativeConfigured() || reg.AnswerModel() != "glm-4.5-air" {
		t.Fatal("registry did not apply the saved settings")
	}
	// The gate opens with no restart: the request now reaches the handler
	// (which rejects the empty body itself, not with 503).
	code, body = do(t, "POST", srv.URL+"/v1/answer", engineToken, `{}`)
	if code == http.StatusServiceUnavailable && body["code"] == "models_not_configured" {
		t.Fatal("answer still gated after keys were saved")
	}

	// Invalid settings are a 422 with a reason.
	code, body = do(t, "PUT", srv.URL+"/admin/v1/models/", engineToken, `{"generative":{"provider":"openai-compatible"}}`)
	if code != http.StatusUnprocessableEntity || body["code"] != "invalid_settings" {
		t.Fatalf("invalid PUT = %d %v", code, body)
	}
	// Test connection runs against the saved settings.
	code, body = do(t, "POST", srv.URL+"/admin/v1/models/test", engineToken, "")
	if code != 200 {
		t.Fatalf("test = %d %v", code, body)
	}
	if g := body["generative"].(map[string]any); g["ok"] != true {
		t.Fatalf("test result = %v", body)
	}
}
