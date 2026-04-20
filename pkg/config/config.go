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
	Log       LogConfig       `yaml:"log"`
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
	Driver string         `yaml:"driver"`
	Local  LocalStorage   `yaml:"local"`
	S3     S3StorageBlock `yaml:"s3"`
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

// QueueConfig configures the background job queue.
type QueueConfig struct {
	Driver string      `yaml:"driver"`
	QStash QStashBlock `yaml:"qstash"`
	River  RiverBlock  `yaml:"river"`
	Asynq  AsynqBlock  `yaml:"asynq"`
}

// QStashBlock configures QStash.
type QStashBlock struct {
	Token          string `yaml:"token"`
	WebhookBaseURL string `yaml:"webhook_base_url"`
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
}

// ChunkedTreeBlock configures the chunked-tree strategy.
type ChunkedTreeBlock struct {
	MaxTokensPerCall         int  `yaml:"max_tokens_per_call"`
	MaxParallelCalls         int  `yaml:"max_parallel_calls"`
	IncludeSiblingBreadcrumb bool `yaml:"include_sibling_breadcrumbs"`
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
	if v := os.Getenv("VLE_QSTASH_TOKEN"); v != "" {
		c.Queue.QStash.Token = v
	}
	if v := os.Getenv("VLE_QSTASH_WEBHOOK_BASE_URL"); v != "" {
		c.Queue.QStash.WebhookBaseURL = v
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
	default:
		return fmt.Errorf("unknown storage.driver: %q", c.Storage.Driver)
	}

	switch c.Queue.Driver {
	case "qstash":
		if c.Queue.QStash.Token == "" {
			return errors.New("queue.qstash.token is required when driver=qstash")
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
	case "single-pass", "chunked-tree":
	default:
		return fmt.Errorf("unknown retrieval.strategy: %q", c.Retrieval.Strategy)
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

	return nil
}
