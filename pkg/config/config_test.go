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
	if cfg.Retrieval.Strategy != "auto" {
		t.Errorf("retrieval.strategy = %q, want auto", cfg.Retrieval.Strategy)
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
	if cfg.Retrieval.Planning.Enabled {
		t.Error("retrieval.planning.enabled should default to false")
	}
	if cfg.Retrieval.Planning.CacheSize != 128 {
		t.Errorf("retrieval.planning.cache_size = %d, want 128", cfg.Retrieval.Planning.CacheSize)
	}
	if !cfg.Retrieval.Planning.Decompose {
		t.Error("retrieval.planning.decompose should default to true (when planning is enabled)")
	}
	if cfg.Retrieval.ReRank.Enabled {
		t.Error("retrieval.rerank.enabled should default to false (opt-in)")
	}
	if cfg.Retrieval.ReRank.MaxContentChars != 2000 {
		t.Errorf("retrieval.rerank.max_content_chars = %d, want 2000", cfg.Retrieval.ReRank.MaxContentChars)
	}
	if cfg.Retrieval.ReRank.TopK != 0 {
		t.Errorf("retrieval.rerank.top_k = %d, want 0 (keep all)", cfg.Retrieval.ReRank.TopK)
	}
	if !cfg.Retrieval.Replay.Enabled {
		t.Error("retrieval.replay.enabled should default to true (opt-out)")
	}
	if cfg.Retrieval.Replay.MaxEntries != 1024 {
		t.Errorf("retrieval.replay.max_entries = %d, want 1024", cfg.Retrieval.Replay.MaxEntries)
	}
	if cfg.Retrieval.Replay.TTLSeconds != 86400 {
		t.Errorf("retrieval.replay.ttl_seconds = %d, want 86400 (24h)", cfg.Retrieval.Replay.TTLSeconds)
	}
	if !cfg.Retrieval.Abstain.Enabled {
		t.Error("retrieval.abstain.enabled should default to true (opt-out)")
	}
	if cfg.Retrieval.Abstain.Below != 0.4 {
		t.Errorf("retrieval.abstain.below = %v, want 0.4", cfg.Retrieval.Abstain.Below)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("log.level = %q, want info", cfg.Log.Level)
	}
	if !cfg.Ingest.TOC.Enabled {
		t.Error("ingest.toc.enabled should default to true (opt-out)")
	}
	if cfg.Ingest.TOC.Concurrency != 4 {
		t.Errorf("ingest.toc.concurrency = %d, want 4", cfg.Ingest.TOC.Concurrency)
	}
	if cfg.Ingest.TOC.TOCCheckPages != 20 {
		t.Errorf("ingest.toc.toc_check_pages = %d, want 20", cfg.Ingest.TOC.TOCCheckPages)
	}
	if cfg.Ingest.ParseTimeoutSeconds != 120 {
		t.Errorf("ingest.parse_timeout_seconds = %d, want 120", cfg.Ingest.ParseTimeoutSeconds)
	}
	if cfg.Ingest.MaxSections != 400 {
		t.Errorf("ingest.max_sections = %d, want 400", cfg.Ingest.MaxSections)
	}
}

// TestIngestParseTimeoutEnvOverride covers VLE_INGEST_PARSE_TIMEOUT_SECONDS
// — the operator knob that lets a tuned whole-parse deadline reach the
// parser without a config-file edit.
func TestIngestParseTimeoutEnvOverride(t *testing.T) {
	prev := os.Getenv("VLE_INGEST_PARSE_TIMEOUT_SECONDS")
	defer os.Setenv("VLE_INGEST_PARSE_TIMEOUT_SECONDS", prev)

	os.Setenv("VLE_INGEST_PARSE_TIMEOUT_SECONDS", "300")
	cfg := Default()
	applyEnvOverrides(&cfg)
	if cfg.Ingest.ParseTimeoutSeconds != 300 {
		t.Errorf("ingest.parse_timeout_seconds = %d, want 300", cfg.Ingest.ParseTimeoutSeconds)
	}
}

// TestIngestParseTimeoutValidate rejects a negative parse timeout — a
// non-positive deadline would silently disable the bound, which must be
// an explicit choice, not a typo that slips through Load.
func TestIngestParseTimeoutValidate(t *testing.T) {
	cfg := Default()
	cfg.Ingest.ParseTimeoutSeconds = -1
	if err := cfg.Validate(); err == nil {
		t.Error("Validate should reject a negative ingest.parse_timeout_seconds")
	}
}

// TestIngestModeDefault locks the default ingest mode to "full" so the
// current full-enrichment behaviour is preserved unless explicitly
// switched.
func TestIngestModeDefault(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if cfg.Ingest.Mode != "full" {
		t.Errorf("ingest.mode = %q, want full (default)", cfg.Ingest.Mode)
	}
}

// TestIngestModeEnvOverride covers the VLE_INGEST_MODE override — the
// single env var that flips the engine into fast/minimal ingest.
func TestIngestModeEnvOverride(t *testing.T) {
	prev := os.Getenv("VLE_INGEST_MODE")
	defer os.Setenv("VLE_INGEST_MODE", prev)

	os.Setenv("VLE_INGEST_MODE", "minimal")
	cfg := Default()
	applyEnvOverrides(&cfg)
	if cfg.Ingest.Mode != "minimal" {
		t.Errorf("VLE_INGEST_MODE=minimal not applied, got %q", cfg.Ingest.Mode)
	}
}

// TestIngestModeValidate asserts Validate accepts the documented values
// (and empty, which Default normalises to full) and rejects garbage.
func TestIngestModeValidate(t *testing.T) {
	t.Parallel()
	for _, m := range []string{"", "full", "minimal"} {
		cfg := Default()
		cfg.Database.URL = "postgres://localhost/test"
		cfg.Ingest.Mode = m
		if err := cfg.Validate(); err != nil {
			t.Errorf("ingest.mode=%q should pass validation, got %v", m, err)
		}
	}

	cfg := Default()
	cfg.Database.URL = "postgres://localhost/test"
	cfg.Ingest.Mode = "turbo"
	if err := cfg.Validate(); err == nil {
		t.Error("ingest.mode=turbo should fail validation")
	}
}

func TestTOCEnvOverride(t *testing.T) {
	// Mutates env — restore on exit. Not parallel.
	keys := []string{
		"VLE_INGEST_TOC_ENABLED",
		"VLE_INGEST_TOC_MODEL",
		"VLE_INGEST_TOC_CONCURRENCY",
		"VLE_INGEST_TOC_TOC_CHECK_PAGES",
	}
	prev := make(map[string]string, len(keys))
	for _, k := range keys {
		prev[k] = os.Getenv(k)
	}
	defer func() {
		for k, v := range prev {
			os.Setenv(k, v)
		}
	}()

	os.Setenv("VLE_INGEST_TOC_ENABLED", "false")
	os.Setenv("VLE_INGEST_TOC_MODEL", "gemini-2.5-pro")
	os.Setenv("VLE_INGEST_TOC_CONCURRENCY", "12")
	os.Setenv("VLE_INGEST_TOC_TOC_CHECK_PAGES", "30")

	cfg := Default()
	applyEnvOverrides(&cfg)

	if cfg.Ingest.TOC.Enabled {
		t.Error("VLE_INGEST_TOC_ENABLED=false should disable the stage")
	}
	if cfg.Ingest.TOC.Model != "gemini-2.5-pro" {
		t.Errorf("VLE_INGEST_TOC_MODEL not applied, got %q", cfg.Ingest.TOC.Model)
	}
	if cfg.Ingest.TOC.Concurrency != 12 {
		t.Errorf("VLE_INGEST_TOC_CONCURRENCY=12 not applied, got %d", cfg.Ingest.TOC.Concurrency)
	}
	if cfg.Ingest.TOC.TOCCheckPages != 30 {
		t.Errorf("VLE_INGEST_TOC_TOC_CHECK_PAGES=30 not applied, got %d", cfg.Ingest.TOC.TOCCheckPages)
	}
}

func TestAbstainEnvOverride(t *testing.T) {
	// Mutates env — restore on exit. Not parallel.
	prevEnabled := os.Getenv("VLE_RETRIEVAL_ABSTAIN_ENABLED")
	prevBelow := os.Getenv("VLE_RETRIEVAL_ABSTAIN_BELOW")
	defer func() {
		os.Setenv("VLE_RETRIEVAL_ABSTAIN_ENABLED", prevEnabled)
		os.Setenv("VLE_RETRIEVAL_ABSTAIN_BELOW", prevBelow)
	}()

	os.Setenv("VLE_RETRIEVAL_ABSTAIN_ENABLED", "false")
	os.Setenv("VLE_RETRIEVAL_ABSTAIN_BELOW", "0.6")

	cfg := Default()
	applyEnvOverrides(&cfg)

	if cfg.Retrieval.Abstain.Enabled {
		t.Error("VLE_RETRIEVAL_ABSTAIN_ENABLED=false should disable abstention")
	}
	if cfg.Retrieval.Abstain.Below != 0.6 {
		t.Errorf("VLE_RETRIEVAL_ABSTAIN_BELOW=0.6 not applied, got %v", cfg.Retrieval.Abstain.Below)
	}
}

func TestAbstainEnvOverrideEnable(t *testing.T) {
	// Toggle on via env from an explicitly-disabled starting state.
	prev := os.Getenv("VLE_RETRIEVAL_ABSTAIN_ENABLED")
	defer os.Setenv("VLE_RETRIEVAL_ABSTAIN_ENABLED", prev)

	cfg := Default()
	cfg.Retrieval.Abstain.Enabled = false
	os.Setenv("VLE_RETRIEVAL_ABSTAIN_ENABLED", "true")
	applyEnvOverrides(&cfg)
	if !cfg.Retrieval.Abstain.Enabled {
		t.Error("VLE_RETRIEVAL_ABSTAIN_ENABLED=true should enable abstention even when previously disabled")
	}
}

// TestAbstainEnvOverrideRejectsBad asserts a garbage float and an
// out-of-range value both preserve the default rather than silently
// zeroing or accepting a value that would break the abstention check
// (Below must be in [0,1]).
func TestAbstainEnvOverrideRejectsBad(t *testing.T) {
	prev := os.Getenv("VLE_RETRIEVAL_ABSTAIN_BELOW")
	defer os.Setenv("VLE_RETRIEVAL_ABSTAIN_BELOW", prev)

	cases := []string{"not-a-float", "1.5", "-0.1", "abc"}
	for _, v := range cases {
		os.Setenv("VLE_RETRIEVAL_ABSTAIN_BELOW", v)
		cfg := Default()
		applyEnvOverrides(&cfg)
		if cfg.Retrieval.Abstain.Below != 0.4 {
			t.Errorf("bad ABSTAIN_BELOW=%q should preserve default 0.4, got %v",
				v, cfg.Retrieval.Abstain.Below)
		}
	}
}

// TestAbstainEnvOverrideParsesEdgeCases covers 0.0 and 1.0 (the
// inclusive bounds) and the canonical 0.4 default — these must all
// be accepted.
func TestAbstainEnvOverrideParsesEdgeCases(t *testing.T) {
	prev := os.Getenv("VLE_RETRIEVAL_ABSTAIN_BELOW")
	defer os.Setenv("VLE_RETRIEVAL_ABSTAIN_BELOW", prev)

	cases := map[string]float64{
		"0":   0.0,
		"0.0": 0.0,
		"1":   1.0,
		"1.0": 1.0,
		"0.5": 0.5,
	}
	for raw, want := range cases {
		os.Setenv("VLE_RETRIEVAL_ABSTAIN_BELOW", raw)
		cfg := Default()
		applyEnvOverrides(&cfg)
		if cfg.Retrieval.Abstain.Below != want {
			t.Errorf("ABSTAIN_BELOW=%q: got %v want %v", raw, cfg.Retrieval.Abstain.Below, want)
		}
	}
}

// TestValidateAbstainOutOfRange asserts Validate rejects out-of-range
// Below values. The env-override path silently drops them, but a YAML
// file or explicit struct edit can still land a bad value here.
func TestValidateAbstainOutOfRange(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Database.URL = "postgres://localhost/test"
	cfg.Retrieval.Abstain.Below = 1.5
	if err := cfg.Validate(); err == nil {
		t.Error("abstain.below=1.5 should fail validation")
	}

	cfg2 := Default()
	cfg2.Database.URL = "postgres://localhost/test"
	cfg2.Retrieval.Abstain.Below = -0.1
	if err := cfg2.Validate(); err == nil {
		t.Error("abstain.below=-0.1 should fail validation")
	}

	cfg3 := Default()
	cfg3.Database.URL = "postgres://localhost/test"
	cfg3.Retrieval.Abstain.Below = 0.0
	if err := cfg3.Validate(); err != nil {
		t.Errorf("abstain.below=0.0 should pass validation, got %v", err)
	}
}

func TestReplayEnvOverride(t *testing.T) {
	// Not parallel — mutates env. Restore on exit.
	prevEnabled := os.Getenv("VLE_RETRIEVAL_REPLAY_ENABLED")
	prevMax := os.Getenv("VLE_RETRIEVAL_REPLAY_MAX_ENTRIES")
	prevTTL := os.Getenv("VLE_RETRIEVAL_REPLAY_TTL_SECONDS")
	defer func() {
		os.Setenv("VLE_RETRIEVAL_REPLAY_ENABLED", prevEnabled)
		os.Setenv("VLE_RETRIEVAL_REPLAY_MAX_ENTRIES", prevMax)
		os.Setenv("VLE_RETRIEVAL_REPLAY_TTL_SECONDS", prevTTL)
	}()

	os.Setenv("VLE_RETRIEVAL_REPLAY_ENABLED", "false")
	os.Setenv("VLE_RETRIEVAL_REPLAY_MAX_ENTRIES", "256")
	os.Setenv("VLE_RETRIEVAL_REPLAY_TTL_SECONDS", "3600")

	cfg := Default()
	applyEnvOverrides(&cfg)

	if cfg.Retrieval.Replay.Enabled {
		t.Error("VLE_RETRIEVAL_REPLAY_ENABLED=false should disable replay")
	}
	if cfg.Retrieval.Replay.MaxEntries != 256 {
		t.Errorf("replay max_entries = %d, want 256", cfg.Retrieval.Replay.MaxEntries)
	}
	if cfg.Retrieval.Replay.TTLSeconds != 3600 {
		t.Errorf("replay ttl_seconds = %d, want 3600", cfg.Retrieval.Replay.TTLSeconds)
	}
}

func TestReplayEnvOverrideEnable(t *testing.T) {
	// Toggle on via env from an explicitly-disabled starting state.
	prev := os.Getenv("VLE_RETRIEVAL_REPLAY_ENABLED")
	defer os.Setenv("VLE_RETRIEVAL_REPLAY_ENABLED", prev)

	cfg := Default()
	cfg.Retrieval.Replay.Enabled = false
	os.Setenv("VLE_RETRIEVAL_REPLAY_ENABLED", "true")
	applyEnvOverrides(&cfg)
	if !cfg.Retrieval.Replay.Enabled {
		t.Error("VLE_RETRIEVAL_REPLAY_ENABLED=true should enable replay even when disabled in YAML")
	}
}

func TestReplayEnvOverrideRejectsBad(t *testing.T) {
	prevMax := os.Getenv("VLE_RETRIEVAL_REPLAY_MAX_ENTRIES")
	prevTTL := os.Getenv("VLE_RETRIEVAL_REPLAY_TTL_SECONDS")
	defer func() {
		os.Setenv("VLE_RETRIEVAL_REPLAY_MAX_ENTRIES", prevMax)
		os.Setenv("VLE_RETRIEVAL_REPLAY_TTL_SECONDS", prevTTL)
	}()

	os.Setenv("VLE_RETRIEVAL_REPLAY_MAX_ENTRIES", "not-a-number")
	os.Setenv("VLE_RETRIEVAL_REPLAY_TTL_SECONDS", "wat")

	cfg := Default()
	applyEnvOverrides(&cfg)
	if cfg.Retrieval.Replay.MaxEntries != 1024 {
		t.Errorf("bad max_entries env should preserve default, got %d", cfg.Retrieval.Replay.MaxEntries)
	}
	if cfg.Retrieval.Replay.TTLSeconds != 86400 {
		t.Errorf("bad ttl_seconds env should preserve default, got %d", cfg.Retrieval.Replay.TTLSeconds)
	}
}

func TestValidateReplayNegatives(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Database.URL = "postgres://localhost/test"
	cfg.Retrieval.Replay.MaxEntries = -1
	if err := cfg.Validate(); err == nil {
		t.Error("negative replay max_entries should fail validation")
	}

	cfg2 := Default()
	cfg2.Database.URL = "postgres://localhost/test"
	cfg2.Retrieval.Replay.TTLSeconds = -1
	if err := cfg2.Validate(); err == nil {
		t.Error("negative replay ttl_seconds should fail validation")
	}

	cfg3 := Default()
	cfg3.Database.URL = "postgres://localhost/test"
	cfg3.Retrieval.Replay.MaxEntries = 0
	cfg3.Retrieval.Replay.TTLSeconds = 0
	if err := cfg3.Validate(); err != nil {
		t.Errorf("zero replay values should pass validation (use defaults at runtime): %v", err)
	}
}

func TestReRankEnvOverride(t *testing.T) {
	// Not parallel — mutates env. Restore on exit.
	prevEnabled := os.Getenv("VLE_RETRIEVAL_RERANK_ENABLED")
	prevModel := os.Getenv("VLE_RETRIEVAL_RERANK_MODEL")
	prevMax := os.Getenv("VLE_RETRIEVAL_RERANK_MAX_CONTENT_CHARS")
	prevTopK := os.Getenv("VLE_RETRIEVAL_RERANK_TOP_K")
	defer func() {
		os.Setenv("VLE_RETRIEVAL_RERANK_ENABLED", prevEnabled)
		os.Setenv("VLE_RETRIEVAL_RERANK_MODEL", prevModel)
		os.Setenv("VLE_RETRIEVAL_RERANK_MAX_CONTENT_CHARS", prevMax)
		os.Setenv("VLE_RETRIEVAL_RERANK_TOP_K", prevTopK)
	}()

	os.Setenv("VLE_RETRIEVAL_RERANK_ENABLED", "true")
	os.Setenv("VLE_RETRIEVAL_RERANK_MODEL", "gemini-2.0-flash")
	os.Setenv("VLE_RETRIEVAL_RERANK_MAX_CONTENT_CHARS", "1500")
	os.Setenv("VLE_RETRIEVAL_RERANK_TOP_K", "3")

	cfg := Default()
	applyEnvOverrides(&cfg)

	if !cfg.Retrieval.ReRank.Enabled {
		t.Error("VLE_RETRIEVAL_RERANK_ENABLED=true should enable rerank")
	}
	if cfg.Retrieval.ReRank.Model != "gemini-2.0-flash" {
		t.Errorf("rerank model = %q, want gemini-2.0-flash", cfg.Retrieval.ReRank.Model)
	}
	if cfg.Retrieval.ReRank.MaxContentChars != 1500 {
		t.Errorf("rerank max_content_chars = %d, want 1500", cfg.Retrieval.ReRank.MaxContentChars)
	}
	if cfg.Retrieval.ReRank.TopK != 3 {
		t.Errorf("rerank top_k = %d, want 3", cfg.Retrieval.ReRank.TopK)
	}
}

func TestReRankEnvOverrideDisable(t *testing.T) {
	// Toggle off via env: start from a config that defaults to false,
	// then set =false explicitly; verify the path executes (not just
	// that the default value is preserved).
	prev := os.Getenv("VLE_RETRIEVAL_RERANK_ENABLED")
	defer os.Setenv("VLE_RETRIEVAL_RERANK_ENABLED", prev)

	cfg := Default()
	cfg.Retrieval.ReRank.Enabled = true // simulate a YAML-set true
	os.Setenv("VLE_RETRIEVAL_RERANK_ENABLED", "false")
	applyEnvOverrides(&cfg)
	if cfg.Retrieval.ReRank.Enabled {
		t.Error("VLE_RETRIEVAL_RERANK_ENABLED=false should disable rerank even when YAML set it true")
	}
}

func TestReRankEnvOverrideRejectsBad(t *testing.T) {
	// Garbage env values should be rejected, not silently zero the field.
	prevMax := os.Getenv("VLE_RETRIEVAL_RERANK_MAX_CONTENT_CHARS")
	prevTopK := os.Getenv("VLE_RETRIEVAL_RERANK_TOP_K")
	defer func() {
		os.Setenv("VLE_RETRIEVAL_RERANK_MAX_CONTENT_CHARS", prevMax)
		os.Setenv("VLE_RETRIEVAL_RERANK_TOP_K", prevTopK)
	}()

	os.Setenv("VLE_RETRIEVAL_RERANK_MAX_CONTENT_CHARS", "not-a-number")
	os.Setenv("VLE_RETRIEVAL_RERANK_TOP_K", "abc")

	cfg := Default()
	applyEnvOverrides(&cfg)
	if cfg.Retrieval.ReRank.MaxContentChars != 2000 {
		t.Errorf("bad max_content_chars env should preserve default, got %d", cfg.Retrieval.ReRank.MaxContentChars)
	}
	if cfg.Retrieval.ReRank.TopK != 0 {
		t.Errorf("bad top_k env should preserve default, got %d", cfg.Retrieval.ReRank.TopK)
	}
}

func TestValidateReRankNegatives(t *testing.T) {
	t.Parallel()

	// Negative max_content_chars rejected.
	cfg := Default()
	cfg.Database.URL = "postgres://localhost/test"
	cfg.Retrieval.ReRank.MaxContentChars = -1
	if err := cfg.Validate(); err == nil {
		t.Error("negative max_content_chars should fail validation")
	}

	// Negative top_k rejected.
	cfg2 := Default()
	cfg2.Database.URL = "postgres://localhost/test"
	cfg2.Retrieval.ReRank.TopK = -1
	if err := cfg2.Validate(); err == nil {
		t.Error("negative top_k should fail validation")
	}

	// Zero on both is valid (TopK=0 means "keep all", MaxContentChars=0
	// means "use default").
	cfg3 := Default()
	cfg3.Database.URL = "postgres://localhost/test"
	cfg3.Retrieval.ReRank.MaxContentChars = 0
	cfg3.Retrieval.ReRank.TopK = 0
	if err := cfg3.Validate(); err != nil {
		t.Errorf("zero rerank values should pass validation: %v", err)
	}
}

func TestPlanningEnvOverride(t *testing.T) {
	// Not parallel — mutates env. Restore on exit.
	prevEnabled := os.Getenv("VLE_RETRIEVAL_PLANNING_ENABLED")
	prevModel := os.Getenv("VLE_RETRIEVAL_PLANNING_MODEL")
	prevCache := os.Getenv("VLE_RETRIEVAL_PLANNING_CACHE_SIZE")
	prevDecompose := os.Getenv("VLE_RETRIEVAL_PLANNING_DECOMPOSE")
	defer func() {
		os.Setenv("VLE_RETRIEVAL_PLANNING_ENABLED", prevEnabled)
		os.Setenv("VLE_RETRIEVAL_PLANNING_MODEL", prevModel)
		os.Setenv("VLE_RETRIEVAL_PLANNING_CACHE_SIZE", prevCache)
		os.Setenv("VLE_RETRIEVAL_PLANNING_DECOMPOSE", prevDecompose)
	}()

	os.Setenv("VLE_RETRIEVAL_PLANNING_ENABLED", "true")
	os.Setenv("VLE_RETRIEVAL_PLANNING_MODEL", "gemini-2.0-flash")
	os.Setenv("VLE_RETRIEVAL_PLANNING_CACHE_SIZE", "256")
	os.Setenv("VLE_RETRIEVAL_PLANNING_DECOMPOSE", "false")

	cfg := Default()
	applyEnvOverrides(&cfg)

	if !cfg.Retrieval.Planning.Enabled {
		t.Error("VLE_RETRIEVAL_PLANNING_ENABLED=true should enable planning")
	}
	if cfg.Retrieval.Planning.Model != "gemini-2.0-flash" {
		t.Errorf("planning model = %q, want gemini-2.0-flash", cfg.Retrieval.Planning.Model)
	}
	if cfg.Retrieval.Planning.CacheSize != 256 {
		t.Errorf("planning cache_size = %d, want 256", cfg.Retrieval.Planning.CacheSize)
	}
	if cfg.Retrieval.Planning.Decompose {
		t.Error("VLE_RETRIEVAL_PLANNING_DECOMPOSE=false should disable decompose")
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
	for _, s := range []string{"auto", "single-pass", "chunked-tree", "agentic", "treewalk"} {
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

// TestTreeWalkDefaults locks in the TreeWalk block's defaults so
// a regression on shipping values is loud. Endpoint enabled by
// default, 8 hops, 16K char limit.
func TestTreeWalkDefaults(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if !cfg.Retrieval.TreeWalk.Enabled {
		t.Error("retrieval.treewalk.enabled should default to true (opt-out)")
	}
	if cfg.Retrieval.TreeWalk.MaxHops != 8 {
		t.Errorf("max_hops = %d, want 8", cfg.Retrieval.TreeWalk.MaxHops)
	}
	if cfg.Retrieval.TreeWalk.PageContentLimit != 16000 {
		t.Errorf("page_content_limit = %d, want 16000", cfg.Retrieval.TreeWalk.PageContentLimit)
	}
	if cfg.Retrieval.TreeWalk.MaxCitations != 3 {
		t.Errorf("max_citations = %d, want 3", cfg.Retrieval.TreeWalk.MaxCitations)
	}
	if cfg.Retrieval.TreeWalk.Model != "" {
		t.Errorf("model default should be empty (inherit), got %q", cfg.Retrieval.TreeWalk.Model)
	}
}

// TestTreeWalkEnvOverride exercises every env knob the TreeWalk
// block exposes.
func TestTreeWalkEnvOverride(t *testing.T) {
	prevEnabled := os.Getenv("VLE_RETRIEVAL_TREEWALK_ENABLED")
	prevHops := os.Getenv("VLE_RETRIEVAL_TREEWALK_MAX_HOPS")
	prevLimit := os.Getenv("VLE_RETRIEVAL_TREEWALK_PAGE_CONTENT_LIMIT")
	prevCits := os.Getenv("VLE_RETRIEVAL_TREEWALK_MAX_CITATIONS")
	prevModel := os.Getenv("VLE_RETRIEVAL_TREEWALK_MODEL")
	defer func() {
		os.Setenv("VLE_RETRIEVAL_TREEWALK_ENABLED", prevEnabled)
		os.Setenv("VLE_RETRIEVAL_TREEWALK_MAX_HOPS", prevHops)
		os.Setenv("VLE_RETRIEVAL_TREEWALK_PAGE_CONTENT_LIMIT", prevLimit)
		os.Setenv("VLE_RETRIEVAL_TREEWALK_MAX_CITATIONS", prevCits)
		os.Setenv("VLE_RETRIEVAL_TREEWALK_MODEL", prevModel)
	}()

	os.Setenv("VLE_RETRIEVAL_TREEWALK_ENABLED", "false")
	os.Setenv("VLE_RETRIEVAL_TREEWALK_MAX_HOPS", "12")
	os.Setenv("VLE_RETRIEVAL_TREEWALK_PAGE_CONTENT_LIMIT", "32000")
	os.Setenv("VLE_RETRIEVAL_TREEWALK_MAX_CITATIONS", "5")
	os.Setenv("VLE_RETRIEVAL_TREEWALK_MODEL", "gemini-2.0-flash")

	cfg := Default()
	applyEnvOverrides(&cfg)

	if cfg.Retrieval.TreeWalk.Enabled {
		t.Error("VLE_RETRIEVAL_TREEWALK_ENABLED=false should disable")
	}
	if cfg.Retrieval.TreeWalk.MaxHops != 12 {
		t.Errorf("max_hops = %d, want 12", cfg.Retrieval.TreeWalk.MaxHops)
	}
	if cfg.Retrieval.TreeWalk.PageContentLimit != 32000 {
		t.Errorf("page_content_limit = %d, want 32000", cfg.Retrieval.TreeWalk.PageContentLimit)
	}
	if cfg.Retrieval.TreeWalk.MaxCitations != 5 {
		t.Errorf("max_citations = %d, want 5", cfg.Retrieval.TreeWalk.MaxCitations)
	}
	if cfg.Retrieval.TreeWalk.Model != "gemini-2.0-flash" {
		t.Errorf("model = %q, want gemini-2.0-flash", cfg.Retrieval.TreeWalk.Model)
	}
}

// TestTreeWalkMaxCitationsVLSAlias: the VLS_ prefix reaches
// MaxCitations too (the deploy layer forwards VLS_*), and VLE_ wins
// when both are set.
func TestTreeWalkMaxCitationsVLSAlias(t *testing.T) {
	prevVLE := os.Getenv("VLE_RETRIEVAL_TREEWALK_MAX_CITATIONS")
	prevVLS := os.Getenv("VLS_RETRIEVAL_TREEWALK_MAX_CITATIONS")
	defer func() {
		os.Setenv("VLE_RETRIEVAL_TREEWALK_MAX_CITATIONS", prevVLE)
		os.Setenv("VLS_RETRIEVAL_TREEWALK_MAX_CITATIONS", prevVLS)
	}()

	// VLS_ alone reaches the field.
	os.Unsetenv("VLE_RETRIEVAL_TREEWALK_MAX_CITATIONS")
	os.Setenv("VLS_RETRIEVAL_TREEWALK_MAX_CITATIONS", "2")
	cfg := Default()
	applyEnvOverrides(&cfg)
	if cfg.Retrieval.TreeWalk.MaxCitations != 2 {
		t.Errorf("VLS_ alias: max_citations = %d, want 2", cfg.Retrieval.TreeWalk.MaxCitations)
	}

	// VLE_ wins when both are set.
	os.Setenv("VLE_RETRIEVAL_TREEWALK_MAX_CITATIONS", "4")
	cfg2 := Default()
	applyEnvOverrides(&cfg2)
	if cfg2.Retrieval.TreeWalk.MaxCitations != 4 {
		t.Errorf("VLE_ should win over VLS_: max_citations = %d, want 4", cfg2.Retrieval.TreeWalk.MaxCitations)
	}
}

// TestTreeWalkEnvOverrideEnable: toggle on from an explicitly
// disabled state.
func TestTreeWalkEnvOverrideEnable(t *testing.T) {
	prev := os.Getenv("VLE_RETRIEVAL_TREEWALK_ENABLED")
	defer os.Setenv("VLE_RETRIEVAL_TREEWALK_ENABLED", prev)

	cfg := Default()
	cfg.Retrieval.TreeWalk.Enabled = false
	os.Setenv("VLE_RETRIEVAL_TREEWALK_ENABLED", "true")
	applyEnvOverrides(&cfg)
	if !cfg.Retrieval.TreeWalk.Enabled {
		t.Error("VLE_RETRIEVAL_TREEWALK_ENABLED=true should enable from disabled")
	}
}

// TestTreeWalkEnvOverrideRejectsBad: garbled numerics preserve the
// default rather than silently zeroing the cap.
func TestTreeWalkEnvOverrideRejectsBad(t *testing.T) {
	prevHops := os.Getenv("VLE_RETRIEVAL_TREEWALK_MAX_HOPS")
	prevLimit := os.Getenv("VLE_RETRIEVAL_TREEWALK_PAGE_CONTENT_LIMIT")
	prevCits := os.Getenv("VLE_RETRIEVAL_TREEWALK_MAX_CITATIONS")
	defer func() {
		os.Setenv("VLE_RETRIEVAL_TREEWALK_MAX_HOPS", prevHops)
		os.Setenv("VLE_RETRIEVAL_TREEWALK_PAGE_CONTENT_LIMIT", prevLimit)
		os.Setenv("VLE_RETRIEVAL_TREEWALK_MAX_CITATIONS", prevCits)
	}()

	os.Setenv("VLE_RETRIEVAL_TREEWALK_MAX_HOPS", "abc")
	os.Setenv("VLE_RETRIEVAL_TREEWALK_PAGE_CONTENT_LIMIT", "not-a-number")
	os.Setenv("VLE_RETRIEVAL_TREEWALK_MAX_CITATIONS", "lots")

	cfg := Default()
	applyEnvOverrides(&cfg)
	if cfg.Retrieval.TreeWalk.MaxHops != 8 {
		t.Errorf("garbage max_hops env should preserve default 8, got %d", cfg.Retrieval.TreeWalk.MaxHops)
	}
	if cfg.Retrieval.TreeWalk.PageContentLimit != 16000 {
		t.Errorf("garbage page_content_limit env should preserve default, got %d", cfg.Retrieval.TreeWalk.PageContentLimit)
	}
	if cfg.Retrieval.TreeWalk.MaxCitations != 3 {
		t.Errorf("garbage max_citations env should preserve default 3, got %d", cfg.Retrieval.TreeWalk.MaxCitations)
	}
}

// TestValidateTreeWalkNegatives: negatives rejected by Validate.
func TestValidateTreeWalkNegatives(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.Database.URL = "postgres://localhost/test"
	cfg.Retrieval.TreeWalk.MaxHops = -1
	if err := cfg.Validate(); err == nil {
		t.Error("negative max_hops should fail validation")
	}

	cfg2 := Default()
	cfg2.Database.URL = "postgres://localhost/test"
	cfg2.Retrieval.TreeWalk.PageContentLimit = -1
	if err := cfg2.Validate(); err == nil {
		t.Error("negative page_content_limit should fail validation")
	}

	cfgCits := Default()
	cfgCits.Database.URL = "postgres://localhost/test"
	cfgCits.Retrieval.TreeWalk.MaxCitations = -1
	if err := cfgCits.Validate(); err == nil {
		t.Error("negative max_citations should fail validation")
	}

	cfg3 := Default()
	cfg3.Database.URL = "postgres://localhost/test"
	cfg3.Retrieval.TreeWalk.MaxHops = 0
	cfg3.Retrieval.TreeWalk.PageContentLimit = 0
	cfg3.Retrieval.TreeWalk.MaxCitations = 0
	if err := cfg3.Validate(); err != nil {
		t.Errorf("zero values should pass (defaults applied at runtime): %v", err)
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

func TestTablesDefaults(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if !cfg.Ingest.Tables.Enabled {
		t.Error("ingest.tables.enabled should default to true")
	}
	if cfg.Ingest.Tables.VerticalStrategy != "lines" {
		t.Errorf("vertical_strategy = %q, want lines", cfg.Ingest.Tables.VerticalStrategy)
	}
	if cfg.Ingest.Tables.HorizontalStrategy != "lines" {
		t.Errorf("horizontal_strategy = %q, want lines", cfg.Ingest.Tables.HorizontalStrategy)
	}
	if cfg.Ingest.Tables.MinTableRows != 2 {
		t.Errorf("min_table_rows = %d, want 2", cfg.Ingest.Tables.MinTableRows)
	}
	if cfg.Ingest.Tables.MinTableCols != 2 {
		t.Errorf("min_table_cols = %d, want 2", cfg.Ingest.Tables.MinTableCols)
	}
}

func TestTablesEnvOverride(t *testing.T) {
	// Mutates env — restore on exit. Not parallel.
	prevEnabled := os.Getenv("VLE_INGEST_TABLES_ENABLED")
	prevV := os.Getenv("VLE_INGEST_TABLES_VERTICAL_STRATEGY")
	prevH := os.Getenv("VLE_INGEST_TABLES_HORIZONTAL_STRATEGY")
	prevRows := os.Getenv("VLE_INGEST_TABLES_MIN_ROWS")
	prevCols := os.Getenv("VLE_INGEST_TABLES_MIN_COLS")
	defer func() {
		os.Setenv("VLE_INGEST_TABLES_ENABLED", prevEnabled)
		os.Setenv("VLE_INGEST_TABLES_VERTICAL_STRATEGY", prevV)
		os.Setenv("VLE_INGEST_TABLES_HORIZONTAL_STRATEGY", prevH)
		os.Setenv("VLE_INGEST_TABLES_MIN_ROWS", prevRows)
		os.Setenv("VLE_INGEST_TABLES_MIN_COLS", prevCols)
	}()

	os.Setenv("VLE_INGEST_TABLES_ENABLED", "false")
	os.Setenv("VLE_INGEST_TABLES_VERTICAL_STRATEGY", "text")
	os.Setenv("VLE_INGEST_TABLES_HORIZONTAL_STRATEGY", "lines_strict")
	os.Setenv("VLE_INGEST_TABLES_MIN_ROWS", "4")
	os.Setenv("VLE_INGEST_TABLES_MIN_COLS", "3")

	cfg := Default()
	applyEnvOverrides(&cfg)

	if cfg.Ingest.Tables.Enabled {
		t.Error("VLE_INGEST_TABLES_ENABLED=false should disable")
	}
	if cfg.Ingest.Tables.VerticalStrategy != "text" {
		t.Errorf("vertical_strategy = %q, want text", cfg.Ingest.Tables.VerticalStrategy)
	}
	if cfg.Ingest.Tables.HorizontalStrategy != "lines_strict" {
		t.Errorf("horizontal_strategy = %q, want lines_strict", cfg.Ingest.Tables.HorizontalStrategy)
	}
	if cfg.Ingest.Tables.MinTableRows != 4 {
		t.Errorf("min_table_rows = %d, want 4", cfg.Ingest.Tables.MinTableRows)
	}
	if cfg.Ingest.Tables.MinTableCols != 3 {
		t.Errorf("min_table_cols = %d, want 3", cfg.Ingest.Tables.MinTableCols)
	}
}

func TestTablesValidateRejectsBadStrategy(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.Ingest.Tables.VerticalStrategy = "magic"
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for unknown vertical_strategy")
	}
	cfg = Default()
	cfg.Ingest.Tables.HorizontalStrategy = "wacky"
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for unknown horizontal_strategy")
	}
	cfg = Default()
	cfg.Ingest.Tables.MinTableRows = -1
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for negative min_table_rows")
	}
}

// TestSummaryAxesDefaults locks the Phase 2.5 defaults: structured
// summaries opt-in by default, with the caps the spec calls for. The
// retrieval prompt downstream relies on these caps to keep the prompt
// budget bounded per section.
func TestSummaryAxesDefaults(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if !cfg.Ingest.SummaryAxes.Enabled {
		t.Error("ingest.summary_axes.enabled should default to true (opt-out)")
	}
	if cfg.Ingest.SummaryAxes.MaxTopics != 4 {
		t.Errorf("max_topics = %d, want 4", cfg.Ingest.SummaryAxes.MaxTopics)
	}
	if cfg.Ingest.SummaryAxes.MaxEntities != 8 {
		t.Errorf("max_entities = %d, want 8", cfg.Ingest.SummaryAxes.MaxEntities)
	}
	if cfg.Ingest.SummaryAxes.MaxNumbers != 6 {
		t.Errorf("max_numbers = %d, want 6", cfg.Ingest.SummaryAxes.MaxNumbers)
	}
}

// TestSummaryAxesEnvOverride covers the opt-out path: env disables the
// structured summarizer, and the numeric caps re-tune via env.
func TestSummaryAxesEnvOverride(t *testing.T) {
	// Mutates env — restore on exit. Not parallel.
	prevEnabled := os.Getenv("VLE_INGEST_SUMMARY_AXES_ENABLED")
	prevTopics := os.Getenv("VLE_INGEST_SUMMARY_AXES_MAX_TOPICS")
	prevEntities := os.Getenv("VLE_INGEST_SUMMARY_AXES_MAX_ENTITIES")
	prevNumbers := os.Getenv("VLE_INGEST_SUMMARY_AXES_MAX_NUMBERS")
	defer func() {
		os.Setenv("VLE_INGEST_SUMMARY_AXES_ENABLED", prevEnabled)
		os.Setenv("VLE_INGEST_SUMMARY_AXES_MAX_TOPICS", prevTopics)
		os.Setenv("VLE_INGEST_SUMMARY_AXES_MAX_ENTITIES", prevEntities)
		os.Setenv("VLE_INGEST_SUMMARY_AXES_MAX_NUMBERS", prevNumbers)
	}()

	os.Setenv("VLE_INGEST_SUMMARY_AXES_ENABLED", "false")
	os.Setenv("VLE_INGEST_SUMMARY_AXES_MAX_TOPICS", "10")
	os.Setenv("VLE_INGEST_SUMMARY_AXES_MAX_ENTITIES", "20")
	os.Setenv("VLE_INGEST_SUMMARY_AXES_MAX_NUMBERS", "15")

	cfg := Default()
	applyEnvOverrides(&cfg)
	if cfg.Ingest.SummaryAxes.Enabled {
		t.Error("VLE_INGEST_SUMMARY_AXES_ENABLED=false should disable")
	}
	if cfg.Ingest.SummaryAxes.MaxTopics != 10 {
		t.Errorf("max_topics env override: got %d, want 10", cfg.Ingest.SummaryAxes.MaxTopics)
	}
	if cfg.Ingest.SummaryAxes.MaxEntities != 20 {
		t.Errorf("max_entities env override: got %d, want 20", cfg.Ingest.SummaryAxes.MaxEntities)
	}
	if cfg.Ingest.SummaryAxes.MaxNumbers != 15 {
		t.Errorf("max_numbers env override: got %d, want 15", cfg.Ingest.SummaryAxes.MaxNumbers)
	}
}

// TestSummaryAxesEnvOverrideRejectsBad: garbage values preserve the
// default rather than zeroing the cap (which would silently fail to
// trim model output).
func TestSummaryAxesEnvOverrideRejectsBad(t *testing.T) {
	prevTopics := os.Getenv("VLE_INGEST_SUMMARY_AXES_MAX_TOPICS")
	defer os.Setenv("VLE_INGEST_SUMMARY_AXES_MAX_TOPICS", prevTopics)
	os.Setenv("VLE_INGEST_SUMMARY_AXES_MAX_TOPICS", "not-a-number")
	cfg := Default()
	applyEnvOverrides(&cfg)
	if cfg.Ingest.SummaryAxes.MaxTopics != 4 {
		t.Errorf("garbled env should preserve default 4, got %d", cfg.Ingest.SummaryAxes.MaxTopics)
	}
}

// TestSummaryAxesValidateNegatives: negative caps fail validation so a
// typo in the YAML doesn't silently disable trimming.
func TestSummaryAxesValidateNegatives(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		fn   func(*Config)
	}{
		{"topics", func(c *Config) { c.Ingest.SummaryAxes.MaxTopics = -1 }},
		{"entities", func(c *Config) { c.Ingest.SummaryAxes.MaxEntities = -1 }},
		{"numbers", func(c *Config) { c.Ingest.SummaryAxes.MaxNumbers = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Database.URL = "postgres://localhost/test"
			tc.fn(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Errorf("negative %s should fail validation", tc.name)
			}
		})
	}
}
