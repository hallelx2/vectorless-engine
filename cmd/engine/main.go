// Command engine is the vectorless retrieval engine.
//
// Run `engine --config config.yaml` to start the HTTP server and any
// configured background workers. See README.md for architecture.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hallelx2/vectorless-engine/internal/api"
	"github.com/hallelx2/vectorless-engine/pkg/config"
	"github.com/hallelx2/vectorless-engine/pkg/db"
	"github.com/hallelx2/vectorless-engine/pkg/ingest"
	"github.com/hallelx2/llmgate"
	"github.com/hallelx2/vectorless-engine/pkg/queue"
	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/storage"
)

// version is set at build time via -ldflags "-X main.version=..."
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "", "path to config.yaml (optional; env vars take precedence)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := newLogger(cfg.Log)
	logger.Info("starting vectorless-engine",
		"version", version,
		"storage_driver", cfg.Storage.Driver,
		"queue_driver", cfg.Queue.Driver,
		"llm_driver", cfg.LLM.Driver,
		"retrieval_strategy", cfg.Retrieval.Strategy,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.Database.URL, int32(cfg.Database.MaxConns))
	if err != nil {
		return fmt.Errorf("init db: %w", err)
	}
	defer pool.Close()
	if err := pool.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate db: %w", err)
	}
	logger.Info("db: migrations applied")

	store, err := buildStorage(cfg.Storage)
	if err != nil {
		return fmt.Errorf("init storage: %w", err)
	}

	q, err := buildQueue(cfg.Queue, cfg.Database.URL)
	if err != nil {
		return fmt.Errorf("init queue: %w", err)
	}
	defer q.Close()

	llmClient, err := buildLLM(cfg.LLM)
	if err != nil {
		return fmt.Errorf("init llm: %w", err)
	}
	strategy := buildStrategy(cfg.Retrieval, llmClient)

	pipeline := ingest.NewPipeline(ingest.Pipeline{
		DB:      pool,
		Storage: store,
		LLM:     llmClient,
		Parsers: ingest.DefaultRegistry(),
		Logger:  logger,
	})
	q.Register(queue.KindIngestDocument, pipeline.Handler())

	deps := api.Deps{
		Logger:   logger,
		DB:       pool,
		Storage:  store,
		Queue:    q,
		Strategy: strategy,
		Version:  version,
	}

	srv := &http.Server{
		Addr:         cfg.Server.Addr,
		Handler:      api.Router(deps),
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		TLSConfig:    buildTLSConfig(cfg.Server.TLS),
	}

	// Start queue workers alongside the HTTP server.
	errs := make(chan error, 2)
	go func() {
		logger.Info("queue: starting workers", "driver", cfg.Queue.Driver)
		if err := q.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errs <- fmt.Errorf("queue: %w", err)
		}
	}()
	go func() {
		if cfg.Server.TLS.Enabled() {
			logger.Info("https: listening (direct TLS)",
				"addr", cfg.Server.Addr,
				"cert_file", cfg.Server.TLS.CertFile)
			if err := srv.ListenAndServeTLS(cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- fmt.Errorf("https: %w", err)
			}
			return
		}
		logger.Info("http: listening (plaintext — terminate TLS at your proxy)",
			"addr", cfg.Server.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("http: %w", err)
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-errs:
		logger.Error("subsystem failed", "err", err)
		stop()
	}

	// Graceful shutdown.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http shutdown error", "err", err)
	}
	logger.Info("bye")
	return nil
}

func buildStorage(c config.StorageConfig) (storage.Storage, error) {
	switch c.Driver {
	case "local":
		return storage.NewLocal(c.Local.Root)
	case "s3":
		return storage.NewS3(storage.S3Config{
			Endpoint:     c.S3.Endpoint,
			Region:       c.S3.Region,
			Bucket:       c.S3.Bucket,
			AccessKey:    c.S3.AccessKey,
			SecretKey:    c.S3.SecretKey,
			UsePathStyle: c.S3.UsePathStyle,
		})
	default:
		return nil, fmt.Errorf("unknown storage driver: %s", c.Driver)
	}
}

func buildQueue(c config.QueueConfig, dbURL string) (queue.Queue, error) {
	switch c.Driver {
	case "qstash":
		return queue.NewQStash(queue.QStashConfig{
			Token:          c.QStash.Token,
			WebhookBaseURL: c.QStash.WebhookBaseURL,
		})
	case "river":
		return queue.NewRiver(queue.RiverConfig{
			DatabaseURL: dbURL,
			NumWorkers:  c.River.NumWorkers,
		})
	case "asynq":
		return queue.NewAsynq(queue.AsynqConfig{
			Addr:        c.Asynq.Addr,
			Password:    c.Asynq.Password,
			DB:          c.Asynq.DB,
			Concurrency: c.Asynq.Concurrency,
		})
	default:
		return nil, fmt.Errorf("unknown queue driver: %s", c.Driver)
	}
}

func buildLLM(c config.LLMConfig) (llmgate.Client, error) {
	switch c.Driver {
	case "anthropic":
		return llmgate.NewAnthropic(llmgate.AnthropicConfig{
			APIKey:         c.Anthropic.APIKey,
			Model:          c.Anthropic.Model,
			ReasoningModel: c.Anthropic.ReasoningModel,
		})
	case "openai":
		return llmgate.NewOpenAI(llmgate.OpenAIConfig{
			APIKey:         c.OpenAI.APIKey,
			Model:          c.OpenAI.Model,
			ReasoningModel: c.OpenAI.ReasoningModel,
		})
	case "gemini":
		return llmgate.NewGemini(llmgate.GeminiConfig{
			APIKey:         c.Gemini.APIKey,
			Model:          c.Gemini.Model,
			ReasoningModel: c.Gemini.ReasoningModel,
		})
	default:
		// Config.Validate rejects unknown drivers; this is defensive.
		return nil, fmt.Errorf("unknown llm driver: %s", c.Driver)
	}
}

func buildStrategy(c config.RetrievalConfig, client llmgate.Client) retrieval.Strategy {
	switch c.Strategy {
	case "single-pass":
		return retrieval.NewSinglePass(client)
	case "chunked-tree":
		return retrieval.NewChunkedTree(client)
	default:
		return retrieval.NewChunkedTree(client)
	}
}

// buildTLSConfig returns a *tls.Config when direct TLS is enabled, or nil
// when the engine should serve plaintext (behind a proxy). Returning nil
// leaves http.Server's TLSConfig unset, which is exactly what ListenAndServe
// expects.
func buildTLSConfig(c config.TLSConfig) *tls.Config {
	if !c.Enabled() {
		return nil
	}
	min := uint16(tls.VersionTLS12)
	if c.MinVersion == "1.3" {
		min = tls.VersionTLS13
	}
	return &tls.Config{MinVersion: min}
}

func newLogger(c config.LogConfig) *slog.Logger {
	level := slog.LevelInfo
	switch c.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}

	var h slog.Handler
	switch c.Format {
	case "console":
		h = slog.NewTextHandler(os.Stdout, opts)
	default:
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(h)
}
