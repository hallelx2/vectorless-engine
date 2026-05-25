// Package config loads vectorless-server configuration from a YAML file
// and/or environment variables (prefix VLS_).
//
// The server config extends the engine config with server-specific
// concerns: authentication, observability, and rate limiting. Engine
// settings (database, storage, queue, LLM, retrieval) are delegated to
// the engine's own config package.
//
// Precedence (highest wins):
//
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

	enginecfg "github.com/hallelx2/vectorless-engine/pkg/config"
)

// Config is the root server configuration. It embeds the engine config
// so the server binary can boot the full stack from one config file.
type Config struct {
	// Engine embeds the full engine configuration (database, storage,
	// queue, LLM, retrieval, logging).
	Engine enginecfg.Config `yaml:"engine"`

	// Server configures the HTTP/gRPC listener.
	Server ServerConfig `yaml:"server"`

	// Auth configures request authentication.
	Auth AuthConfig `yaml:"auth"`

	// Metrics configures Prometheus metrics.
	Metrics MetricsConfig `yaml:"metrics"`

	// Tracing configures OpenTelemetry distributed tracing.
	Tracing TracingConfig `yaml:"tracing"`

	// RateLimit configures optional global rate limiting.
	RateLimit RateLimitConfig `yaml:"rate_limit"`

	// CORS configures Cross-Origin Resource Sharing headers.
	CORS CORSConfig `yaml:"cors"`

	// Governance configures request size limits and timeouts.
	Governance GovernanceConfig `yaml:"governance"`
}

// ServerConfig configures the HTTP server.
type ServerConfig struct {
	// Addr is the listen address (e.g. ":8080", "0.0.0.0:443").
	Addr string `yaml:"addr"`

	// ReadTimeout bounds how long the server waits for request headers +
	// body. Default 30s.
	ReadTimeout time.Duration `yaml:"read_timeout"`

	// WriteTimeout bounds how long the server waits for the full
	// response to be written. Default 120s.
	WriteTimeout time.Duration `yaml:"write_timeout"`

	// DrainTimeout is the grace period for in-flight requests during
	// shutdown. Default 15s (matches Kubernetes terminationGracePeriodSeconds).
	DrainTimeout time.Duration `yaml:"drain_timeout"`

	// TLS enables direct TLS termination. When both CertFile and
	// KeyFile are set, the server listens with TLS. Otherwise it
	// serves plaintext and expects a reverse proxy to terminate TLS.
	TLS TLSConfig `yaml:"tls"`
}

// TLSConfig enables direct TLS termination.
type TLSConfig struct {
	CertFile   string `yaml:"cert_file"`
	KeyFile    string `yaml:"key_file"`
	MinVersion string `yaml:"min_version"`
}

// Enabled reports whether direct TLS is configured.
func (t TLSConfig) Enabled() bool {
	return t.CertFile != "" && t.KeyFile != ""
}

// AuthConfig configures request authentication.
type AuthConfig struct {
	// Mode selects the authentication strategy.
	//   "none"    — all requests are anonymous (default).
	//   "api_key" — require a static Bearer token.
	Mode string `yaml:"mode"`

	// APIKey is the expected Bearer token when Mode is "api_key".
	// Compared with crypto/subtle.ConstantTimeCompare.
	APIKey string `yaml:"api_key"`
}

// MetricsConfig configures Prometheus metrics.
type MetricsConfig struct {
	// Enabled toggles the /metrics endpoint. Default true.
	Enabled bool `yaml:"enabled"`
}

// TracingConfig configures OpenTelemetry tracing.
type TracingConfig struct {
	// Enabled toggles OTLP trace export. Default false.
	Enabled bool `yaml:"enabled"`

	// Endpoint is the OTLP gRPC collector endpoint
	// (e.g. "localhost:4317", "otel-collector:4317").
	Endpoint string `yaml:"endpoint"`

	// Insecure disables TLS for the OTLP exporter. Use for local dev.
	Insecure bool `yaml:"insecure"`

	// ServiceName overrides the service name in traces. Default
	// "vectorless-server".
	ServiceName string `yaml:"service_name"`

	// SampleRate is the fraction of traces to sample [0.0, 1.0].
	// Default 1.0 (sample everything).
	SampleRate float64 `yaml:"sample_rate"`
}

// RateLimitConfig configures optional global rate limiting.
type RateLimitConfig struct {
	// Enabled toggles rate limiting. Default false.
	Enabled bool `yaml:"enabled"`

	// RequestsPerMinute is the maximum number of requests per minute
	// across all clients (global bucket). Default 600.
	RequestsPerMinute int `yaml:"requests_per_minute"`

	// PerPrincipalRPM is the per-principal rate limit. Each
	// authenticated principal gets their own bucket. 0 = disabled.
	PerPrincipalRPM int `yaml:"per_principal_rpm"`
}

// CORSConfig configures Cross-Origin Resource Sharing headers.
type CORSConfig struct {
	// Enabled toggles CORS header injection. Default true.
	Enabled bool `yaml:"enabled"`

	// AllowedOrigins is the list of origins allowed to make cross-origin
	// requests. Use ["*"] to allow any origin (suitable for development).
	AllowedOrigins []string `yaml:"allowed_origins"`

	// MaxAge is the Access-Control-Max-Age value in seconds. Browsers
	// cache preflight responses for this duration. Default 86400 (24h).
	MaxAge int `yaml:"max_age"`
}

// GovernanceConfig configures request body size limits and per-request
// timeouts.
type GovernanceConfig struct {
	// MaxBodySizeBytes is the maximum allowed request body size.
	// Default 33554432 (32 MiB), suitable for multipart uploads.
	MaxBodySizeBytes int64 `yaml:"max_body_size_bytes"`

	// DefaultTimeout is the per-request context deadline for most
	// endpoints. Default 30s.
	DefaultTimeout time.Duration `yaml:"default_timeout"`

	// QueryTimeout is the per-request context deadline for query
	// endpoints, which typically involve embedding + retrieval.
	// Default 120s.
	QueryTimeout time.Duration `yaml:"query_timeout"`
}

// Default returns a Config with sensible defaults.
func Default() Config {
	return Config{
		Engine: enginecfg.Default(),
		Server: ServerConfig{
			Addr:         ":8080",
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 120 * time.Second,
			DrainTimeout: 15 * time.Second,
		},
		Auth: AuthConfig{
			Mode: "none",
		},
		Metrics: MetricsConfig{
			Enabled: true,
		},
		Tracing: TracingConfig{
			Enabled:     false,
			ServiceName: "vectorless-server",
			SampleRate:  1.0,
		},
		RateLimit: RateLimitConfig{
			Enabled:           false,
			RequestsPerMinute: 600,
		},
		CORS: CORSConfig{
			Enabled:        true,
			AllowedOrigins: []string{"*"},
			MaxAge:         86400,
		},
		Governance: GovernanceConfig{
			MaxBodySizeBytes: 33554432,       // 32 MiB
			DefaultTimeout:   30 * time.Second,
			QueryTimeout:     120 * time.Second,
		},
	}
}

// Load reads configuration from a YAML file (optional) and applies
// environment overrides. Pass an empty path to skip the file.
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
	// ── Server ────────────────────────────────────────────────────
	if v := os.Getenv("VLS_ADDR"); v != "" {
		c.Server.Addr = v
	}

	// ── Auth ──────────────────────────────────────────────────────
	if v := os.Getenv("VLS_AUTH_MODE"); v != "" {
		c.Auth.Mode = v
	}
	if v := os.Getenv("VLS_AUTH_API_KEY"); v != "" {
		c.Auth.APIKey = v
	}

	// ── Metrics ───────────────────────────────────────────────────
	if v := os.Getenv("VLS_METRICS_ENABLED"); v != "" {
		c.Metrics.Enabled = v == "true" || v == "1"
	}

	// ── Tracing ───────────────────────────────────────────────────
	if v := os.Getenv("VLS_TRACING_ENABLED"); v != "" {
		c.Tracing.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("VLS_TRACING_ENDPOINT"); v != "" {
		c.Tracing.Endpoint = v
	}
	if v := os.Getenv("VLS_TRACING_INSECURE"); v != "" {
		c.Tracing.Insecure = v == "true" || v == "1"
	}
	if v := os.Getenv("VLS_TRACING_SERVICE_NAME"); v != "" {
		c.Tracing.ServiceName = v
	}

	// ── Rate limit ────────────────────────────────────────────────
	if v := os.Getenv("VLS_RATE_LIMIT_ENABLED"); v != "" {
		c.RateLimit.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("VLS_RATE_LIMIT_RPM"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.RateLimit.RequestsPerMinute = n
		}
	}

	// ── CORS ──────────────────────────────────────────────────────
	if v := os.Getenv("VLS_CORS_ENABLED"); v != "" {
		c.CORS.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("VLS_CORS_ORIGINS"); v != "" {
		c.CORS.AllowedOrigins = strings.Split(v, ",")
	}

	// ── TLS ───────────────────────────────────────────────────────
	if v := os.Getenv("VLS_TLS_CERT_FILE"); v != "" {
		c.Server.TLS.CertFile = v
	}
	if v := os.Getenv("VLS_TLS_KEY_FILE"); v != "" {
		c.Server.TLS.KeyFile = v
	}

	// ── Engine pass-through ───────────────────────────────────────
	// The engine's own config.Load handles VLE_* env vars, but when
	// running inside the server binary the operator may prefer VLS_*
	// or the engine vars. We forward the most critical ones so a
	// single env-var namespace works.
	if v := firstEnv("VLS_DATABASE_URL", "VLE_DATABASE_URL"); v != "" {
		c.Engine.Database.URL = v
	}
	if v := firstEnv("VLS_LOG_LEVEL", "VLE_LOG_LEVEL"); v != "" {
		c.Engine.Log.Level = v
	}
	if v := firstEnv("VLS_ANTHROPIC_API_KEY", "VLE_ANTHROPIC_API_KEY"); v != "" {
		c.Engine.LLM.Anthropic.APIKey = v
	}
	if v := firstEnv("VLS_OPENAI_API_KEY", "VLE_OPENAI_API_KEY"); v != "" {
		c.Engine.LLM.OpenAI.APIKey = v
	}
	if v := firstEnv("VLS_GEMINI_API_KEY", "VLE_GEMINI_API_KEY"); v != "" {
		c.Engine.LLM.Gemini.APIKey = v
	}
	if v := firstEnv("VLS_STORAGE_DRIVER", "VLE_STORAGE_DRIVER"); v != "" {
		c.Engine.Storage.Driver = v
	}
	if v := firstEnv("VLS_QUEUE_DRIVER", "VLE_QUEUE_DRIVER"); v != "" {
		c.Engine.Queue.Driver = v
	}
	if v := firstEnv("VLS_LLM_DRIVER", "VLE_LLM_DRIVER"); v != "" {
		c.Engine.LLM.Driver = v
	}
}

// firstEnv returns the first non-empty value from the named env vars.
func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// Validate checks required fields for the selected modes.
func (c Config) Validate() error {
	// Auth validation.
	switch c.Auth.Mode {
	case "none":
		// nothing required
	case "api_key":
		if c.Auth.APIKey == "" {
			return errors.New("auth.api_key is required when auth.mode=api_key")
		}
	default:
		return fmt.Errorf("unknown auth.mode: %q (want \"none\" or \"api_key\")", c.Auth.Mode)
	}

	// TLS validation.
	if c.Server.TLS.CertFile != "" && c.Server.TLS.KeyFile == "" {
		return errors.New("server.tls.key_file is required when cert_file is set")
	}
	if c.Server.TLS.KeyFile != "" && c.Server.TLS.CertFile == "" {
		return errors.New("server.tls.cert_file is required when key_file is set")
	}
	if v := c.Server.TLS.MinVersion; v != "" && v != "1.2" && v != "1.3" {
		return fmt.Errorf("server.tls.min_version must be 1.2 or 1.3, got %q", v)
	}

	// Tracing validation.
	if c.Tracing.Enabled && c.Tracing.Endpoint == "" {
		return errors.New("tracing.endpoint is required when tracing.enabled=true")
	}

	// Delegate engine-level validation.
	if err := c.Engine.Validate(); err != nil {
		return fmt.Errorf("engine config: %w", err)
	}

	return nil
}
