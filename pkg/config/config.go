// Package config loads engine configuration from a YAML file and/or
// environment variables (prefix VLE_).
//
// Precedence (highest wins):
//  1. Environment variables
//  2. YAML file supplied via --config flag
//  3. Built-in defaults
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration.
type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Database  DatabaseConfig  `yaml:"database"`
	Storage   StorageConfig   `yaml:"storage"`
	Queue     QueueConfig     `yaml:"queue"`
	LLM       LLMConfig       `yaml:"llm"`
	Retrieval RetrievalConfig `yaml:"retrieval"`
	Ingest    IngestConfig    `yaml:"ingest"`
	Log       LogConfig       `yaml:"log"`
}

// IngestConfig configures retrieval-quality boosters that run during
// the ingest pipeline (between summarize and StatusReady).
type IngestConfig struct {
	HyDE HyDEConfig `yaml:"hyde"`

	// GlobalLLMConcurrency caps the total number of LLM calls in flight
	// across the summarize and HyDE stages combined, which now run
	// concurrently. Each stage still respects its own per-stage cap
	// (summary_concurrency / hyde.concurrency), but neither can push the
	// shared counter above this ceiling.
	//
	// 0 (or omitted) defaults to 12 — enough headroom for the default
	// 4 + 4 per-stage caps while staying well below typical provider
	// per-tenant concurrency limits.
	GlobalLLMConcurrency int `yaml:"global_llm_concurrency"`
}

// HyDEConfig configures the HyDE candidate-question stage. For each
// leaf section the pipeline asks the LLM to enumerate questions the
// section's content can answer; those are later folded into the
// retrieval prompt to widen lexical/semantic overlap with user queries.
type HyDEConfig struct {
	// Enabled toggles the stage. Default: true. Disable to skip an LLM
	// call per leaf when ingest budget matters more than recall.
	Enabled bool `yaml:"enabled"`

	// Model, when non-empty, overrides the LLM model used for HyDE
	// generation. Defaults to the same model used for summarization.
	Model string `yaml:"model"`

	// NumQuestions caps the questions generated per leaf section.
	// Default: 5.
	NumQuestions int `yaml:"num_questions"`

	// Concurrency bounds parallel LLM calls during the HyDE stage.
	// Default: 4.
	Concurrency int `yaml:"concurrency"`
}

// ServerConfig configures the HTTP server.
//
// TLS is opt-in. If TLS.CertFile and TLS.KeyFile are both set the engine
// listens with TLS directly; otherwise it listens plaintext and expects
// a reverse proxy (Caddy, nginx, an ALB, ingress) to terminate TLS.
type ServerConfig struct {
	Addr         string        `yaml:"addr"`
	ReadTimeout  time.Duration `yaml:"read_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout"`
	TLS          TLSConfig     `yaml:"tls"`
}

// TLSConfig enables direct TLS termination inside the engine.
type TLSConfig struct {
	// CertFile is a PEM-encoded certificate chain.
	CertFile string `yaml:"cert_file"`
	// KeyFile is a PEM-encoded private key matching CertFile.
	KeyFile string `yaml:"key_file"`
	// MinVersion is the minimum TLS version (e.g. "1.2", "1.3"). Empty
	// defaults to Go's default (currently TLS 1.2).
	MinVersion string `yaml:"min_version"`
}

// Enabled reports whether direct-TLS serving is configured.
func (t TLSConfig) Enabled() bool {
	return t.CertFile != "" && t.KeyFile != ""
}

// DatabaseConfig configures Postgres.
type DatabaseConfig struct {
	URL      string `yaml:"url"`
	MaxConns int    `yaml:"max_conns"`
}

// StorageConfig configures the document storage backend.
type StorageConfig struct {
	Driver string          `yaml:"driver"`
	Local  LocalStorage    `yaml:"local"`
	S3     S3StorageBlock  `yaml:"s3"`
	GCS    GCSStorageBlock `yaml:"gcs"`
}

// LocalStorage configures filesystem-backed storage.
type LocalStorage struct {
	Root string `yaml:"root"`
}

// S3StorageBlock configures S3-compatible storage.
type S3StorageBlock struct {
	Endpoint     string `yaml:"endpoint"`
	Region       string `yaml:"region"`
	Bucket       string `yaml:"bucket"`
	AccessKey    string `yaml:"access_key"`
	SecretKey    string `yaml:"secret_key"`
	UsePathStyle bool   `yaml:"use_path_style"`
}

// GCSStorageBlock configures native Google Cloud Storage. Auths via
// Application Default Credentials, so no key fields needed.
type GCSStorageBlock struct {
	Bucket string `yaml:"bucket"`
}

// QueueConfig configures the background job queue.
type QueueConfig struct {
	Driver string      `yaml:"driver"`
	QStash QStashBlock `yaml:"qstash"`
	River  RiverBlock  `yaml:"river"`
	Asynq  AsynqBlock  `yaml:"asynq"`
}

// QStashBlock configures QStash.
//
// Token is the publish token (used to enqueue). CurrentSigningKey and
// NextSigningKey are used to verify inbound webhooks; both are surfaced
// on the Upstash console under "Signing Keys". NextSigningKey is only
// populated while rotating.
type QStashBlock struct {
	Token             string `yaml:"token"`
	WebhookBaseURL    string `yaml:"webhook_base_url"`
	CurrentSigningKey string `yaml:"current_signing_key"`
	NextSigningKey    string `yaml:"next_signing_key"`
}

// RiverBlock configures River.
type RiverBlock struct {
	NumWorkers int `yaml:"num_workers"`
}

// AsynqBlock configures Asynq.
type AsynqBlock struct {
	Addr        string `yaml:"addr"`
	Password    string `yaml:"password"`
	DB          int    `yaml:"db"`
	Concurrency int    `yaml:"concurrency"`
}

// LLMConfig configures the LLM provider.
type LLMConfig struct {
	Driver    string         `yaml:"driver"`
	Anthropic AnthropicBlock `yaml:"anthropic"`
	OpenAI    OpenAIBlock    `yaml:"openai"`
	Gemini    GeminiBlock    `yaml:"gemini"`
}

// AnthropicBlock configures the Anthropic provider.
type AnthropicBlock struct {
	APIKey         string `yaml:"api_key"`
	Model          string `yaml:"model"`
	ReasoningModel string `yaml:"reasoning_model"`
}

// OpenAIBlock configures the OpenAI provider.
type OpenAIBlock struct {
	APIKey         string `yaml:"api_key"`
	Model          string `yaml:"model"`
	ReasoningModel string `yaml:"reasoning_model"`
}

// GeminiBlock configures the Gemini provider.
type GeminiBlock struct {
	APIKey         string `yaml:"api_key"`
	Model          string `yaml:"model"`
	ReasoningModel string `yaml:"reasoning_model"`
}

// RetrievalConfig configures the retrieval strategy.
type RetrievalConfig struct {
	Strategy    string           `yaml:"strategy"`
	ChunkedTree ChunkedTreeBlock `yaml:"chunked_tree"`
	Agentic     AgenticBlock     `yaml:"agentic"`
	Cache       CacheBlock       `yaml:"cache"`
	AnswerSpan  AnswerSpanBlock  `yaml:"answer_span"`
	Answer      AnswerBlock      `yaml:"answer"`
}

// AnswerSpanBlock configures the answer-span extractor.
//
// When enabled, every section returned by /v1/query gets an extra
// `answer_span` field carrying the verbatim quote the model judged
// most relevant to the query, plus byte offsets back into the
// section's content. Costs one LLM call per returned section.
type AnswerSpanBlock struct {
	// Enabled toggles per-section span extraction on /v1/query. Default: false.
	Enabled bool `yaml:"enabled"`
	// Model overrides the budget's model for the span extraction call.
	// Empty means use the request's model. Keep this on a cheap/fast
	// model (the call is short and runs once per returned section).
	Model string `yaml:"model"`
	// MaxConcurrency caps parallel span-extraction calls per request.
	// Default: 4.
	MaxConcurrency int `yaml:"max_concurrency"`
	// MaxQuoteLen caps the per-section quote length (characters).
	// Default: 400.
	MaxQuoteLen int `yaml:"max_quote_len"`
}

// AnswerBlock configures the /v1/answer endpoint, which runs retrieval
// + span extraction + a synthesis LLM call to return a quote-grounded
// answer in a single round-trip.
type AnswerBlock struct {
	// Model overrides the budget's model for the synthesis call.
	// Empty means use the request's model.
	Model string `yaml:"model"`
	// MaxSections caps how many sections are fed into synthesis.
	// Default: 5.
	MaxSections int `yaml:"max_sections"`
	// MaxAnswerTokens bounds the synthesised answer length.
	// Default: 1024.
	MaxAnswerTokens int `yaml:"max_answer_tokens"`
}

// CacheBlock configures the retrieval-result cache.
type CacheBlock struct {
	// Enabled turns the retrieval cache on. Default: true.
	Enabled bool `yaml:"enabled"`

	// MaxEntries is the maximum number of cached retrieval results.
	// Default: 1024.
	MaxEntries int `yaml:"max_entries"`

	// TTLSeconds is how long (in seconds) a cached result remains valid.
	// Default: 600 (10 minutes).
	TTLSeconds int `yaml:"ttl_seconds"`
}

// ChunkedTreeBlock configures the chunked-tree strategy.
type ChunkedTreeBlock struct {
	MaxTokensPerCall         int  `yaml:"max_tokens_per_call"`
	MaxParallelCalls         int  `yaml:"max_parallel_calls"`
	IncludeSiblingBreadcrumb bool `yaml:"include_sibling_breadcrumbs"`
}

// AgenticBlock configures the agentic-navigation strategy.
//
// The agentic loop trades sequential latency for the ability to handle
// arbitrarily large trees: the model issues outline/expand/read actions
// until it picks a final set of section IDs or hits MaxHops.
type AgenticBlock struct {
	// MaxHops caps the number of LLM turns one query consumes, counting
	// the terminal "done" turn. Default: 6.
	MaxHops int `yaml:"max_hops"`

	// Model optionally overrides the budget's model for navigation
	// turns. Empty means use the budget's model. Useful when the
	// retrieval engine wants the navigation loop on a fast/cheap
	// model while answering is on a stronger one.
	Model string `yaml:"model"`
}

// LogConfig configures logging.
type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Default returns a Config with built-in defaults pre-applied.
func Default() Config {
	return Config{
		Server: ServerConfig{
			Addr:         ":8080",
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 120 * time.Second,
		},
		Database: DatabaseConfig{MaxConns: 10},
		Storage: StorageConfig{
			Driver: "local",
			Local:  LocalStorage{Root: "./data/documents"},
		},
		Queue: QueueConfig{
			Driver: "river",
			River:  RiverBlock{NumWorkers: 10},
			Asynq:  AsynqBlock{Concurrency: 20},
		},
		LLM: LLMConfig{Driver: "anthropic"},
		Retrieval: RetrievalConfig{
			Strategy: "chunked-tree",
			ChunkedTree: ChunkedTreeBlock{
				MaxTokensPerCall:         60000,
				MaxParallelCalls:         8,
				IncludeSiblingBreadcrumb: true,
			},
			Agentic: AgenticBlock{
				MaxHops: 6,
			},
			Cache: CacheBlock{
				Enabled:    true,
				MaxEntries: 1024,
				TTLSeconds: 600,
			},
			AnswerSpan: AnswerSpanBlock{
				Enabled:        false,
				MaxConcurrency: 4,
				MaxQuoteLen:    400,
			},
			Answer: AnswerBlock{
				MaxSections:     5,
				MaxAnswerTokens: 1024,
			},
		},
		Ingest: IngestConfig{
			GlobalLLMConcurrency: 12,
			HyDE: HyDEConfig{
				Enabled:      true,
				NumQuestions: 5,
				Concurrency:  4,
			},
		},
		Log: LogConfig{Level: "info", Format: "json"},
	}
}

// Load reads configuration from a YAML file (optional) and applies
// environment overrides on top. Pass an empty path to skip the file.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config: %w", err)
		}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnvOverrides(&cfg)
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// firstEnv returns the first non-empty environment variable value from
// the supplied names, checked in order.
func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

func applyEnvOverrides(c *Config) {
	// Minimal, deliberately shallow env handling — production-heavy values
	// that are typically rotated live here. Extend as needed.
	if v := os.Getenv("VLE_DATABASE_URL"); v != "" {
		c.Database.URL = v
	}
	if v := os.Getenv("VLE_SERVER_ADDR"); v != "" {
		c.Server.Addr = v
	}
	if v := os.Getenv("VLE_LOG_LEVEL"); v != "" {
		c.Log.Level = v
	}
	if v := os.Getenv("VLE_ANTHROPIC_API_KEY"); v != "" {
		c.LLM.Anthropic.APIKey = v
	}
	if v := os.Getenv("VLE_OPENAI_API_KEY"); v != "" {
		c.LLM.OpenAI.APIKey = v
	}
	if v := os.Getenv("VLE_GEMINI_API_KEY"); v != "" {
		c.LLM.Gemini.APIKey = v
	}
	// Accept both VLE_-prefixed and bare QSTASH_* env vars. The bare
	// names match what the Upstash console documents and what the
	// dashboard already uses, so ops can set them once for both
	// services. VLE_-prefixed wins if both are set.
	if v := firstEnv("VLE_QSTASH_TOKEN", "QSTASH_TOKEN"); v != "" {
		c.Queue.QStash.Token = v
	}
	if v := firstEnv("VLE_QSTASH_WEBHOOK_BASE_URL", "QSTASH_WEBHOOK_BASE_URL"); v != "" {
		c.Queue.QStash.WebhookBaseURL = v
	}
	if v := firstEnv("VLE_QSTASH_CURRENT_SIGNING_KEY", "QSTASH_CURRENT_SIGNING_KEY"); v != "" {
		c.Queue.QStash.CurrentSigningKey = v
	}
	if v := firstEnv("VLE_QSTASH_NEXT_SIGNING_KEY", "QSTASH_NEXT_SIGNING_KEY"); v != "" {
		c.Queue.QStash.NextSigningKey = v
	}
	// Asynq / Redis env overrides. Accept both VLE_-prefixed and bare
	// REDIS_* names so ops can set them once for multiple services.
	if v := firstEnv("VLE_ASYNQ_ADDR", "REDIS_ADDR"); v != "" {
		c.Queue.Asynq.Addr = v
	}
	if v := firstEnv("VLE_ASYNQ_PASSWORD", "REDIS_PASSWORD"); v != "" {
		c.Queue.Asynq.Password = v
	}
	if v := os.Getenv("VLE_STORAGE_DRIVER"); v != "" {
		c.Storage.Driver = v
	}
	if v := os.Getenv("VLE_QUEUE_DRIVER"); v != "" {
		c.Queue.Driver = v
	}
	if v := os.Getenv("VLE_LLM_DRIVER"); v != "" {
		c.LLM.Driver = v
	}
	if v := os.Getenv("VLE_TLS_CERT_FILE"); v != "" {
		c.Server.TLS.CertFile = v
	}
	if v := os.Getenv("VLE_TLS_KEY_FILE"); v != "" {
		c.Server.TLS.KeyFile = v
	}
	if v := os.Getenv("VLE_RETRIEVAL_AGENTIC_MAX_HOPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.Retrieval.Agentic.MaxHops = n
		}
	}
	if v := os.Getenv("VLE_RETRIEVAL_AGENTIC_MODEL"); v != "" {
		c.Retrieval.Agentic.Model = v
	}
	// Ingest / HyDE knobs. Booleans accept the usual truthy strings —
	// kept narrow so a typo doesn't silently flip the flag.
	if v := os.Getenv("VLE_INGEST_HYDE_ENABLED"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			c.Ingest.HyDE.Enabled = true
		case "0", "false", "no", "off":
			c.Ingest.HyDE.Enabled = false
		}
	}
	if v := os.Getenv("VLE_INGEST_HYDE_MODEL"); v != "" {
		c.Ingest.HyDE.Model = v
	}
	if v := os.Getenv("VLE_INGEST_HYDE_NUM_QUESTIONS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Ingest.HyDE.NumQuestions = n
		}
	}
	if v := os.Getenv("VLE_INGEST_HYDE_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Ingest.HyDE.Concurrency = n
		}
	}
	if v := os.Getenv("VLE_INGEST_GLOBAL_LLM_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.Ingest.GlobalLLMConcurrency = n
		}
	}
	if v := os.Getenv("VLE_RETRIEVAL_ANSWER_SPAN_ENABLED"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			c.Retrieval.AnswerSpan.Enabled = true
		case "0", "false", "no", "off":
			c.Retrieval.AnswerSpan.Enabled = false
		}
	}
	if v := os.Getenv("VLE_RETRIEVAL_ANSWER_SPAN_MODEL"); v != "" {
		c.Retrieval.AnswerSpan.Model = v
	}
	if v := os.Getenv("VLE_RETRIEVAL_ANSWER_SPAN_MAX_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Retrieval.AnswerSpan.MaxConcurrency = n
		}
	}
	if v := os.Getenv("VLE_RETRIEVAL_ANSWER_MODEL"); v != "" {
		c.Retrieval.Answer.Model = v
	}
	if v := os.Getenv("VLE_RETRIEVAL_ANSWER_MAX_SECTIONS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Retrieval.Answer.MaxSections = n
		}
	}
}

// Validate checks that required fields for the selected drivers are set.
func (c Config) Validate() error {
	switch c.Storage.Driver {
	case "local":
		if c.Storage.Local.Root == "" {
			return errors.New("storage.local.root is required when driver=local")
		}
	case "s3":
		if c.Storage.S3.Bucket == "" || c.Storage.S3.Endpoint == "" {
			return errors.New("storage.s3 requires bucket and endpoint")
		}
	case "gcs":
		if c.Storage.GCS.Bucket == "" {
			return errors.New("storage.gcs.bucket is required when driver=gcs")
		}
	default:
		return fmt.Errorf("unknown storage.driver: %q", c.Storage.Driver)
	}

	switch c.Queue.Driver {
	case "qstash":
		if c.Queue.QStash.Token == "" {
			return errors.New("queue.qstash.token is required when driver=qstash")
		}
		if c.Queue.QStash.WebhookBaseURL == "" {
			return errors.New("queue.qstash.webhook_base_url is required when driver=qstash")
		}
		if c.Queue.QStash.CurrentSigningKey == "" {
			return errors.New("queue.qstash.current_signing_key is required when driver=qstash")
		}
	case "river":
		if c.Database.URL == "" {
			return errors.New("database.url is required when queue.driver=river")
		}
	case "asynq":
		if c.Queue.Asynq.Addr == "" {
			return errors.New("queue.asynq.addr is required when driver=asynq")
		}
	default:
		return fmt.Errorf("unknown queue.driver: %q", c.Queue.Driver)
	}

	switch c.LLM.Driver {
	case "anthropic", "openai", "gemini":
		// API keys are checked lazily at first call so the engine can boot
		// in dev without all providers configured.
	default:
		return fmt.Errorf("unknown llm.driver: %q", c.LLM.Driver)
	}

	switch c.Retrieval.Strategy {
	case "single-pass", "chunked-tree", "agentic":
	default:
		return fmt.Errorf("unknown retrieval.strategy: %q", c.Retrieval.Strategy)
	}

	if c.Retrieval.Agentic.MaxHops < 0 {
		return fmt.Errorf("retrieval.agentic.max_hops must be >= 0, got %d", c.Retrieval.Agentic.MaxHops)
	}

	if c.Server.TLS.CertFile != "" && c.Server.TLS.KeyFile == "" {
		return errors.New("server.tls.key_file is required when cert_file is set")
	}
	if c.Server.TLS.KeyFile != "" && c.Server.TLS.CertFile == "" {
		return errors.New("server.tls.cert_file is required when key_file is set")
	}
	if v := c.Server.TLS.MinVersion; v != "" && v != "1.2" && v != "1.3" {
		return fmt.Errorf("server.tls.min_version must be 1.2 or 1.3, got %q", v)
	}

	if c.Ingest.HyDE.NumQuestions < 0 {
		return fmt.Errorf("ingest.hyde.num_questions must be >= 0, got %d", c.Ingest.HyDE.NumQuestions)
	}
	if c.Ingest.HyDE.Concurrency < 0 {
		return fmt.Errorf("ingest.hyde.concurrency must be >= 0, got %d", c.Ingest.HyDE.Concurrency)
	}
	if c.Ingest.GlobalLLMConcurrency < 0 {
		return fmt.Errorf("ingest.global_llm_concurrency must be >= 0, got %d", c.Ingest.GlobalLLMConcurrency)
	}

	return nil
}
