package models

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hallelx2/llmgate"
)

// memStore is an in-memory Store shared by several registries, the way
// replicas share one database.
type memStore struct {
	mu  sync.Mutex
	row *StoredValue
	now time.Time
}

func (m *memStore) Get(context.Context) (StoredValue, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.row == nil {
		return StoredValue{}, false, nil
	}
	return *m.row, true, nil
}

func (m *memStore) Put(_ context.Context, v []byte, by string) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = m.now.Add(time.Second)
	if m.now.IsZero() || m.now.Before(time.Unix(1, 0)) {
		m.now = time.Unix(1_700_000_000, 0)
	}
	m.row = &StoredValue{Value: append([]byte(nil), v...), UpdatedAt: m.now, UpdatedBy: by}
	return m.now, nil
}

func (m *memStore) Delete(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.row = nil
	return nil
}

func (m *memStore) UpdatedAt(context.Context) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.row == nil {
		return time.Time{}, nil
	}
	return m.row.UpdatedAt, nil
}

// fakeClient answers with the provider and key it was built from, so a
// test can see which settings a call went to.
func fakeBuilder(built *[]Generative) ClientBuilder {
	return func(g Generative) (llmgate.Client, error) {
		if built != nil {
			*built = append(*built, g)
		}
		return &llmgate.Mock{Respond: func(_ context.Context, req llmgate.Request) (*llmgate.Response, error) {
			model := req.Model
			if model == "" {
				model = g.ResolvedIngestModel()
			}
			return &llmgate.Response{Content: g.Provider + ":" + g.APIKey, Model: model}, nil
		}}, nil
	}
}

func fakeJudgeBuilder(j JudgeSettings) (llmgate.Judge, error) {
	return &llmgate.MockJudge{}, nil
}

const testKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func newTestRegistry(t *testing.T, env Settings, store Store, withCipher bool) *Registry {
	t.Helper()
	var c *Cipher
	if withCipher {
		var err error
		c, err = NewCipher(testKey)
		if err != nil {
			t.Fatal(err)
		}
	}
	return New(Options{Env: env, Store: store, Cipher: c, BuildClient: fakeBuilder(nil), BuildJudge: fakeJudgeBuilder})
}

func TestKeylessRegistryFailsTyped(t *testing.T) {
	r := newTestRegistry(t, Settings{}, nil, false)
	if r.GenerativeConfigured() || r.JudgeConfigured() {
		t.Fatal("a registry with no settings reports a provider configured")
	}
	_, err := r.Client().Complete(context.Background(), llmgate.Request{})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Complete without a provider: want ErrNotConfigured, got %v", err)
	}
	var nc *NotConfiguredError
	if !errors.As(err, &nc) || nc.Kind != "generative" {
		t.Fatalf("want a generative NotConfiguredError, got %#v", err)
	}
	if !strings.Contains(err.Error(), "model keys not configured") {
		t.Fatalf("error does not say what is wrong: %q", err)
	}
	if _, err := r.Judge().Judge(context.Background(), llmgate.JudgeRequest{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Judge without a provider: want ErrNotConfigured, got %v", err)
	}
	if n, err := r.Client().CountTokens(context.Background(), "abcdefgh"); err != nil || n != 2 {
		t.Fatalf("CountTokens must work keyless: n=%d err=%v", n, err)
	}
	if ActiveJudge(r.Judge()) != nil {
		t.Fatal("ActiveJudge must be nil with no Judge configured")
	}
	if r.AnswerModel() != "" {
		t.Fatalf("AnswerModel with no provider = %q, want empty", r.AnswerModel())
	}
	st := r.Status()
	if st.Generative.Source != SourceNone || st.Judge.Source != SourceNone {
		t.Fatalf("status sources = %s/%s, want none", st.Generative.Source, st.Judge.Source)
	}
}

func TestEnvProvidersApply(t *testing.T) {
	env := Settings{
		Generative: &Generative{Provider: "anthropic", APIKey: "env-key-000000000001", IngestModel: "glm-4.5-air"},
		Judge:      &JudgeSettings{APIKey: "env-judge-key-0000001"},
	}
	r := newTestRegistry(t, env, &memStore{}, true)
	if err := r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	resp, err := r.Client().Complete(context.Background(), llmgate.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "anthropic:env-key-000000000001" {
		t.Fatalf("call went to %q", resp.Content)
	}
	if r.AnswerModel() != "glm-4.5-air" {
		t.Fatalf("answer model = %q, want the ingest model when none is set", r.AnswerModel())
	}
	if ActiveJudge(r.Judge()) == nil {
		t.Fatal("ActiveJudge nil with an env Judge")
	}
	st := r.Status()
	if st.Generative.Source != SourceEnv || st.Generative.KeyHint != "…0001" {
		t.Fatalf("status = %+v", st.Generative)
	}
}

func TestSaveHotReloadsAndPersistsSealed(t *testing.T) {
	store := &memStore{}
	env := Settings{Generative: &Generative{Provider: "anthropic", APIKey: "env-key-000000000001"}}
	r := newTestRegistry(t, env, store, true)
	ctx := context.Background()

	// The client a caller captured before the change must follow it.
	held := r.Client()

	st, err := r.Save(ctx, Update{
		Generative: &Generative{Provider: "openai-compatible", APIKey: "ui-secret-key-000000002", BaseURL: "http://ollama:11434/v1/", IngestModel: "llama3.1", AnswerModel: "llama3.1:70b"},
		Judge:      &JudgeSettings{APIKey: "ui-judge-key-000000003"},
	}, "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if st.Generative.Source != SourceUI || st.Generative.BaseURL != "http://ollama:11434/v1" {
		t.Fatalf("status after save = %+v", st.Generative)
	}
	if st.UpdatedBy != "admin@example.com" {
		t.Fatalf("updated_by = %q", st.UpdatedBy)
	}
	resp, err := held.Complete(ctx, llmgate.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "openai-compatible:ui-secret-key-000000002" {
		t.Fatalf("held client still goes to %q after save", resp.Content)
	}
	if r.AnswerModel() != "llama3.1:70b" {
		t.Fatalf("answer model = %q", r.AnswerModel())
	}

	// The row is sealed: no key, base URL or model name in the clear.
	row, ok, _ := store.Get(ctx)
	if !ok {
		t.Fatal("nothing stored")
	}
	for _, plain := range []string{"ui-secret-key", "ui-judge-key", "ollama", "llama"} {
		if bytes.Contains(row.Value, []byte(plain)) {
			t.Fatalf("stored row carries %q in the clear", plain)
		}
	}

	// The status never carries a key.
	if strings.Contains(st.Generative.KeyHint, "secret") || st.Generative.KeyHint != "…0002" {
		t.Fatalf("key hint = %q", st.Generative.KeyHint)
	}
}

func TestOtherReplicaPicksUpChangeByPolling(t *testing.T) {
	store := &memStore{}
	a := newTestRegistry(t, Settings{}, store, true)
	b := newTestRegistry(t, Settings{}, store, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Watch(ctx, 10*time.Millisecond)

	if b.GenerativeConfigured() {
		t.Fatal("replica b configured before any save")
	}
	if _, err := a.Save(ctx, Update{Generative: &Generative{Provider: "gemini", APIKey: "gemini-key-00000000009"}}, "x"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !b.GenerativeConfigured() {
		if time.Now().After(deadline) {
			t.Fatal("replica b never reloaded the saved settings")
		}
		time.Sleep(5 * time.Millisecond)
	}
	resp, err := b.Client().Complete(ctx, llmgate.Request{})
	if err != nil || resp.Content != "gemini:gemini-key-00000000009" {
		t.Fatalf("replica b call: %v %v", resp, err)
	}

	// Clearing on a propagates too.
	if _, err := a.Save(ctx, Update{ClearGenerative: true}, "x"); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for b.GenerativeConfigured() {
		if time.Now().After(deadline) {
			t.Fatal("replica b never saw the clear")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestUpdateKeepsStoredKeyAndClearFallsBackToEnv(t *testing.T) {
	store := &memStore{}
	env := Settings{Generative: &Generative{Provider: "anthropic", APIKey: "env-key-000000000001"}}
	r := newTestRegistry(t, env, store, true)
	ctx := context.Background()
	if _, err := r.Save(ctx, Update{Generative: &Generative{Provider: "openai", APIKey: "ui-key-0000000000005", IngestModel: "gpt-4.1-mini"}}, "x"); err != nil {
		t.Fatal(err)
	}
	// Change only the model: no key in the update keeps the stored one.
	if _, err := r.Save(ctx, Update{Generative: &Generative{Provider: "openai", IngestModel: "gpt-4.1"}}, "x"); err != nil {
		t.Fatal(err)
	}
	resp, _ := r.Client().Complete(ctx, llmgate.Request{})
	if resp.Content != "openai:ui-key-0000000000005" || resp.Model != "gpt-4.1" {
		t.Fatalf("after model-only update: %q model %q", resp.Content, resp.Model)
	}
	// Switching provider without a key is refused.
	if _, err := r.Save(ctx, Update{Generative: &Generative{Provider: "gemini"}}, "x"); err == nil {
		t.Fatal("switching provider without a key was accepted")
	} else {
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("want ValidationError, got %T %v", err, err)
		}
	}
	// Clearing returns to the environment.
	st, err := r.Save(ctx, Update{ClearGenerative: true}, "x")
	if err != nil {
		t.Fatal(err)
	}
	if st.Generative.Source != SourceEnv {
		t.Fatalf("after clear source = %s, want env", st.Generative.Source)
	}
	resp, _ = r.Client().Complete(ctx, llmgate.Request{})
	if resp.Content != "anthropic:env-key-000000000001" {
		t.Fatalf("after clear the call went to %q", resp.Content)
	}
	if _, ok, _ := store.Get(ctx); ok {
		t.Fatal("clearing everything left a row behind")
	}
}

func TestSaveRefusesWithoutEncryptionKey(t *testing.T) {
	r := newTestRegistry(t, Settings{}, &memStore{}, false)
	_, err := r.Save(context.Background(), Update{Generative: &Generative{Provider: "anthropic", APIKey: "k-000000000000000001"}}, "x")
	if !errors.Is(err, ErrNoEncryptionKey) {
		t.Fatalf("want ErrNoEncryptionKey, got %v", err)
	}
	if r.Status().EncryptionConfigured {
		t.Fatal("status claims an encryption key")
	}
}

func TestValidation(t *testing.T) {
	cases := []struct {
		name string
		g    Generative
		ok   bool
	}{
		{"anthropic needs key", Generative{Provider: "anthropic"}, false},
		{"anthropic with gateway", Generative{Provider: "anthropic", APIKey: "k", BaseURL: "https://api.z.ai/api/anthropic/v1"}, true},
		{"compatible needs base url", Generative{Provider: "openai-compatible", IngestModel: "m"}, false},
		{"compatible needs model", Generative{Provider: "openai-compatible", BaseURL: "http://x:1/v1"}, false},
		{"compatible keyless ok", Generative{Provider: "openai-compatible", BaseURL: "http://x:1/v1", IngestModel: "m"}, true},
		{"bad url", Generative{Provider: "openai", APIKey: "k", BaseURL: "ftp://x"}, false},
		{"unknown provider", Generative{Provider: "mistral", APIKey: "k"}, false},
		{"gemini no base url", Generative{Provider: "gemini", APIKey: "k", BaseURL: "https://x"}, false},
	}
	for _, c := range cases {
		err := c.g.Validate()
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%v", c.name, err, c.ok)
		}
	}
}

func TestTestReportsEachProvider(t *testing.T) {
	r := New(Options{
		Store: &memStore{},
		BuildClient: func(g Generative) (llmgate.Client, error) {
			return &llmgate.Mock{Respond: func(_ context.Context, req llmgate.Request) (*llmgate.Response, error) {
				if req.Model == "bad-model" {
					return nil, llmgate.NewLLMError("anthropic", 404, "model not found", nil)
				}
				return &llmgate.Response{Content: "ok", Model: req.Model}, nil
			}}, nil
		},
		BuildJudge: func(JudgeSettings) (llmgate.Judge, error) {
			return &llmgate.MockJudge{Err: llmgate.NewLLMError("typesafe", 402, "insufficient credit", nil)}, nil
		},
	})
	res, err := r.Test(context.Background(), &Update{
		Generative: &Generative{Provider: "anthropic", APIKey: "k-0000000000000000001", IngestModel: "glm-4.5-air", AnswerModel: "bad-model"},
		Judge:      &JudgeSettings{APIKey: "j-0000000000000000001"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Generative.OK || res.Generative.Model != "glm-4.5-air" {
		t.Fatalf("generative = %+v", res.Generative)
	}
	if res.Answer.OK || res.Answer.Status != 404 || res.Answer.Hint == "" {
		t.Fatalf("answer = %+v", res.Answer)
	}
	if res.Judge.OK || res.Judge.Status != 402 || !strings.Contains(res.Judge.Hint, "credit") {
		t.Fatalf("judge = %+v", res.Judge)
	}
	// A test does not save.
	if r.GenerativeConfigured() {
		t.Fatal("Test changed the registry")
	}
}

func TestCipher(t *testing.T) {
	c, err := NewCipher(testKey)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := c.Seal([]byte("secret"), "a")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("secret")) {
		t.Fatal("sealed value carries the plaintext")
	}
	if pt, err := c.Open(sealed, "a"); err != nil || string(pt) != "secret" {
		t.Fatalf("open: %q %v", pt, err)
	}
	if _, err := c.Open(sealed, "b"); !errors.Is(err, ErrSealedCorrupt) {
		t.Fatalf("open with another aad: %v", err)
	}
	other, _ := NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if _, err := other.Open(sealed, "a"); !errors.Is(err, ErrSealedCorrupt) {
		t.Fatalf("open with another key: %v", err)
	}
	// Two seals of one value differ (fresh nonce).
	again, _ := c.Seal([]byte("secret"), "a")
	if bytes.Equal(sealed, again) {
		t.Fatal("nonce reused")
	}
	if _, err := NewCipher("short"); err == nil {
		t.Fatal("a short passphrase was accepted")
	}
	if _, err := NewCipher(""); !errors.Is(err, ErrNoEncryptionKey) {
		t.Fatalf("empty key: %v", err)
	}
	if _, err := NewCipher(strings.Repeat("p", 40)); err != nil {
		t.Fatalf("long passphrase: %v", err)
	}
}

func TestChangedEncryptionKeyKeepsEnvAndReports(t *testing.T) {
	store := &memStore{}
	a := newTestRegistry(t, Settings{}, store, true)
	if _, err := a.Save(context.Background(), Update{Generative: &Generative{Provider: "openai", APIKey: "k-0000000000000000001"}}, "x"); err != nil {
		t.Fatal(err)
	}
	other, _ := NewCipher(strings.Repeat("z", 40))
	env := Settings{Generative: &Generative{Provider: "anthropic", APIKey: "env-key-000000000001"}}
	b := New(Options{Env: env, Store: store, Cipher: other, BuildClient: fakeBuilder(nil), BuildJudge: fakeJudgeBuilder})
	if err := b.Reload(context.Background()); !errors.Is(err, ErrSealedCorrupt) {
		t.Fatalf("reload with another key: %v", err)
	}
	st := b.Status()
	if st.StoreError == "" || st.Generative.Source != SourceEnv {
		t.Fatalf("status = %+v", st)
	}
}
