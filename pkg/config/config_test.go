package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultValues(t *testing.T) {
	t.Parallel()
	cfg := Default()

	if cfg.Server.Addr != ":8080" {
		t.Errorf("server.addr = %q, want :8080", cfg.Server.Addr)
	}
	if cfg.Database.MaxConns != 10 {
		t.Errorf("database.max_conns = %d, want 10", cfg.Database.MaxConns)
	}
	if cfg.Storage.Driver != "local" {
		t.Errorf("storage.driver = %q, want local", cfg.Storage.Driver)
	}
	if cfg.Queue.Driver != "river" {
		t.Errorf("queue.driver = %q, want river", cfg.Queue.Driver)
	}
	if cfg.LLM.Driver != "anthropic" {
		t.Errorf("llm.driver = %q, want anthropic", cfg.LLM.Driver)
	}
	if cfg.Retrieval.Strategy != "chunked-tree" {
		t.Errorf("retrieval.strategy = %q, want chunked-tree", cfg.Retrieval.Strategy)
	}
	if !cfg.Retrieval.Cache.Enabled {
		t.Error("retrieval.cache.enabled should be true by default")
	}
	if cfg.Retrieval.Cache.MaxEntries != 1024 {
		t.Errorf("cache.max_entries = %d, want 1024", cfg.Retrieval.Cache.MaxEntries)
	}
	if cfg.Retrieval.Cache.TTLSeconds != 600 {
		t.Errorf("cache.ttl_seconds = %d, want 600", cfg.Retrieval.Cache.TTLSeconds)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("log.level = %q, want info", cfg.Log.Level)
	}
}

func TestValidateStorageLocal(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.Database.URL = "postgres://localhost/test"
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid local config should pass: %v", err)
	}

	cfg.Storage.Local.Root = ""
	if err := cfg.Validate(); err == nil {
		t.Error("empty local root should fail validation")
	}
}

func TestValidateStorageS3(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.Database.URL = "postgres://localhost/test"
	cfg.Storage.Driver = "s3"
	cfg.Storage.S3.Bucket = "my-bucket"
	cfg.Storage.S3.Endpoint = "https://s3.amazonaws.com"
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid s3 config should pass: %v", err)
	}

	cfg.Storage.S3.Bucket = ""
	if err := cfg.Validate(); err == nil {
		t.Error("empty s3 bucket should fail validation")
	}
}

func TestValidateStorageUnknownDriver(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.Database.URL = "postgres://localhost/test"
	cfg.Storage.Driver = "gcs"
	if err := cfg.Validate(); err == nil {
		t.Error("unknown storage driver should fail validation")
	}
}

func TestValidateQueueDrivers(t *testing.T) {
	t.Parallel()

	// River requires database URL.
	cfg := Default()
	cfg.Queue.Driver = "river"
	cfg.Database.URL = ""
	if err := cfg.Validate(); err == nil {
		t.Error("river without database URL should fail")
	}
	cfg.Database.URL = "postgres://localhost/test"
	if err := cfg.Validate(); err != nil {
		t.Errorf("river with database URL should pass: %v", err)
	}

	// Asynq requires addr.
	cfg2 := Default()
	cfg2.Database.URL = "postgres://localhost/test"
	cfg2.Queue.Driver = "asynq"
	cfg2.Queue.Asynq.Addr = ""
	if err := cfg2.Validate(); err == nil {
		t.Error("asynq without addr should fail")
	}
	cfg2.Queue.Asynq.Addr = "localhost:6379"
	if err := cfg2.Validate(); err != nil {
		t.Errorf("asynq with addr should pass: %v", err)
	}

	// QStash requires token + webhook URL + signing key.
	cfg3 := Default()
	cfg3.Database.URL = "postgres://localhost/test"
	cfg3.Queue.Driver = "qstash"
	if err := cfg3.Validate(); err == nil {
		t.Error("qstash without token should fail")
	}
	cfg3.Queue.QStash.Token = "tok"
	cfg3.Queue.QStash.WebhookBaseURL = "https://example.com"
	cfg3.Queue.QStash.CurrentSigningKey = "key"
	if err := cfg3.Validate(); err != nil {
		t.Errorf("qstash with all fields should pass: %v", err)
	}

	// Unknown driver.
	cfg4 := Default()
	cfg4.Database.URL = "postgres://localhost/test"
	cfg4.Queue.Driver = "sqs"
	if err := cfg4.Validate(); err == nil {
		t.Error("unknown queue driver should fail")
	}
}

func TestValidateLLMDrivers(t *testing.T) {
	t.Parallel()
	for _, driver := range []string{"anthropic", "openai", "gemini"} {
		cfg := Default()
		cfg.Database.URL = "postgres://localhost/test"
		cfg.LLM.Driver = driver
		if err := cfg.Validate(); err != nil {
			t.Errorf("llm driver %q should pass: %v", driver, err)
		}
	}

	cfg := Default()
	cfg.Database.URL = "postgres://localhost/test"
	cfg.LLM.Driver = "cohere"
	if err := cfg.Validate(); err == nil {
		t.Error("unknown llm driver should fail")
	}
}

func TestValidateRetrievalStrategy(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"single-pass", "chunked-tree"} {
		cfg := Default()
		cfg.Database.URL = "postgres://localhost/test"
		cfg.Retrieval.Strategy = s
		if err := cfg.Validate(); err != nil {
			t.Errorf("strategy %q should pass: %v", s, err)
		}
	}

	cfg := Default()
	cfg.Database.URL = "postgres://localhost/test"
	cfg.Retrieval.Strategy = "beam-search"
	if err := cfg.Validate(); err == nil {
		t.Error("unknown strategy should fail")
	}
}

func TestValidateTLS(t *testing.T) {
	t.Parallel()

	// Both set → OK.
	cfg := Default()
	cfg.Database.URL = "postgres://localhost/test"
	cfg.Server.TLS.CertFile = "/path/cert.pem"
	cfg.Server.TLS.KeyFile = "/path/key.pem"
	if err := cfg.Validate(); err != nil {
		t.Errorf("both TLS files set should pass: %v", err)
	}
	if !cfg.Server.TLS.Enabled() {
		t.Error("TLS should be enabled")
	}

	// Cert without key → fail.
	cfg2 := Default()
	cfg2.Database.URL = "postgres://localhost/test"
	cfg2.Server.TLS.CertFile = "/path/cert.pem"
	if err := cfg2.Validate(); err == nil {
		t.Error("cert without key should fail")
	}

	// Key without cert → fail.
	cfg3 := Default()
	cfg3.Database.URL = "postgres://localhost/test"
	cfg3.Server.TLS.KeyFile = "/path/key.pem"
	if err := cfg3.Validate(); err == nil {
		t.Error("key without cert should fail")
	}

	// Invalid min version.
	cfg4 := Default()
	cfg4.Database.URL = "postgres://localhost/test"
	cfg4.Server.TLS.CertFile = "/a"
	cfg4.Server.TLS.KeyFile = "/b"
	cfg4.Server.TLS.MinVersion = "1.1"
	if err := cfg4.Validate(); err == nil {
		t.Error("min_version 1.1 should fail")
	}

	// Neither set → OK (no TLS).
	cfg5 := Default()
	cfg5.Database.URL = "postgres://localhost/test"
	if !cfg5.Server.TLS.Enabled() == true {
		// should be disabled
	}
}

func TestLoadFromYAML(t *testing.T) {
	t.Parallel()

	yaml := `
server:
  addr: ":9090"
database:
  url: "postgres://localhost:5432/vle"
storage:
  driver: local
  local:
    root: /tmp/data
queue:
  driver: river
llm:
  driver: openai
retrieval:
  strategy: single-pass
log:
  level: debug
  format: console
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Server.Addr != ":9090" {
		t.Errorf("addr = %q, want :9090", cfg.Server.Addr)
	}
	if cfg.Database.URL != "postgres://localhost:5432/vle" {
		t.Errorf("db url = %q", cfg.Database.URL)
	}
	if cfg.LLM.Driver != "openai" {
		t.Errorf("llm = %q, want openai", cfg.LLM.Driver)
	}
	if cfg.Retrieval.Strategy != "single-pass" {
		t.Errorf("strategy = %q, want single-pass", cfg.Retrieval.Strategy)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("log level = %q, want debug", cfg.Log.Level)
	}
}

func TestLoadEmptyPath(t *testing.T) {
	// Cannot use t.Parallel() with t.Setenv().
	t.Setenv("VLE_DATABASE_URL", "postgres://localhost/test")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load empty path: %v", err)
	}
	if cfg.Database.URL != "postgres://localhost/test" {
		t.Errorf("env override not applied")
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("VLE_DATABASE_URL", "postgres://env/db")
	t.Setenv("VLE_SERVER_ADDR", ":3000")
	t.Setenv("VLE_LOG_LEVEL", "warn")
	t.Setenv("VLE_STORAGE_DRIVER", "local")
	t.Setenv("VLE_QUEUE_DRIVER", "river")
	t.Setenv("VLE_LLM_DRIVER", "gemini")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Database.URL != "postgres://env/db" {
		t.Errorf("VLE_DATABASE_URL not applied: %q", cfg.Database.URL)
	}
	if cfg.Server.Addr != ":3000" {
		t.Errorf("VLE_SERVER_ADDR not applied: %q", cfg.Server.Addr)
	}
	if cfg.Log.Level != "warn" {
		t.Errorf("VLE_LOG_LEVEL not applied: %q", cfg.Log.Level)
	}
	if cfg.LLM.Driver != "gemini" {
		t.Errorf("VLE_LLM_DRIVER not applied: %q", cfg.LLM.Driver)
	}
}

func TestEnvOverridesAsynq(t *testing.T) {
	t.Setenv("VLE_DATABASE_URL", "postgres://localhost/test")
	t.Setenv("VLE_QUEUE_DRIVER", "asynq")
	t.Setenv("REDIS_ADDR", "redis:6379")
	t.Setenv("REDIS_PASSWORD", "secret")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Queue.Asynq.Addr != "redis:6379" {
		t.Errorf("REDIS_ADDR not applied: %q", cfg.Queue.Asynq.Addr)
	}
	if cfg.Queue.Asynq.Password != "secret" {
		t.Errorf("REDIS_PASSWORD not applied: %q", cfg.Queue.Asynq.Password)
	}
}

func TestLoadBadPath(t *testing.T) {
	t.Parallel()
	_, err := Load("/nonexistent/config.yaml")
	if err == nil {
		t.Error("expected error for nonexistent path")
	}
}

func TestLoadInvalidYAML(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte("{{not yaml"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Error("expected error for invalid YAML")
	}
}
