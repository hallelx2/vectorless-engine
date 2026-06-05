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
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hallelx2/llmgate"
	"github.com/hallelx2/llmgate/pricing"
	"github.com/hallelx2/llmgate/provider/anthropic"
	"github.com/hallelx2/llmgate/provider/gemini"
	"github.com/hallelx2/llmgate/provider/openai"

	"github.com/hallelx2/vectorless-engine/internal/api"
	"github.com/hallelx2/vectorless-engine/pkg/config"
	"github.com/hallelx2/vectorless-engine/pkg/db"
	"github.com/hallelx2/vectorless-engine/pkg/ingest"
	"github.com/hallelx2/vectorless-engine/pkg/parser"
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

	// Surface any model with no price-book entry: its cost reads $0, which
	// would otherwise masquerade as "free" in usage/benchmark accounting.
	pricing.WarnFunc = func(model string) {
		logger.Warn("llm model not in price book; cost reported as 0", "model", model)
	}

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
	strategy := buildStrategy(cfg.Retrieval, llmClient, store)

	// Wrap with caching if enabled.
	if cfg.Retrieval.Cache.Enabled {
		ttl := time.Duration(cfg.Retrieval.Cache.TTLSeconds) * time.Second
		if ttl == 0 {
			ttl = 10 * time.Minute
		}
		strategy = retrieval.NewCached(strategy, retrieval.CachedConfig{
			MaxEntries: cfg.Retrieval.Cache.MaxEntries,
			TTL:        ttl,
		})
		logger.Info("retrieval: cache enabled",
			"max_entries", cfg.Retrieval.Cache.MaxEntries,
			"ttl_seconds", cfg.Retrieval.Cache.TTLSeconds,
		)
	}

	// Multi-document query dispatcher.
	multiDoc := retrieval.NewMultiDoc(strategy, pool.LoadTree)

	// Planner: opt-in Phase 2.1. When disabled at boot we still
	// instantiate it lazily — the per-request `enable_planning` body
	// field overrides the config, so a server with planning.enabled=false
	// but a Planner configured can still serve opt-in callers.
	var planner *retrieval.Planner
	if llmClient != nil {
		plannerModel := cfg.Retrieval.Planning.Model
		if plannerModel == "" {
			plannerModel = modelFor(cfg.LLM)
		}
		planner = retrieval.NewPlannerWithCacheSize(llmClient, plannerModel, cfg.Retrieval.Planning.CacheSize)
		if cfg.Retrieval.Planning.Enabled {
			logger.Info("retrieval: planner enabled",
				"model", plannerModel,
				"cache_size", cfg.Retrieval.Planning.CacheSize,
				"decompose", cfg.Retrieval.Planning.Decompose,
			)
		}
	}

	// ReRanker: opt-in Phase 2.3. Instantiated whenever an LLM client
	// is wired — the per-request `enable_rerank` body field overrides
	// the config, mirroring the planner pattern.
	var reRanker *retrieval.ReRanker
	if llmClient != nil {
		reRankModel := cfg.Retrieval.ReRank.Model
		if reRankModel == "" {
			reRankModel = modelFor(cfg.LLM)
		}
		reRanker = retrieval.NewReRanker(llmClient, reRankModel)
		if cfg.Retrieval.ReRank.MaxContentChars > 0 {
			reRanker.MaxContentChars = cfg.Retrieval.ReRank.MaxContentChars
		}
		if cfg.Retrieval.ReRank.Enabled {
			logger.Info("retrieval: rerank enabled",
				"model", reRankModel,
				"max_content_chars", reRanker.MaxContentChars,
				"top_k", cfg.Retrieval.ReRank.TopK,
			)
		}
	}

	// Replay store: Phase 3.1. On by default; operators opt out via
	// retrieval.replay.enabled=false (or VLE_RETRIEVAL_REPLAY_ENABLED=false).
	// In-memory only — Phase 3.2 will swap this for a durable store
	// behind the same retrieval.ReplayStore interface.
	var replayStore retrieval.ReplayStore
	if cfg.Retrieval.Replay.Enabled {
		replayStore = retrieval.NewLRUReplayStore(retrieval.LRUReplayConfig{
			MaxEntries: cfg.Retrieval.Replay.MaxEntries,
			TTL:        time.Duration(cfg.Retrieval.Replay.TTLSeconds) * time.Second,
		})
		logger.Info("retrieval: replay store enabled",
			"max_entries", cfg.Retrieval.Replay.MaxEntries,
			"ttl_seconds", cfg.Retrieval.Replay.TTLSeconds,
		)
	}

	pipeline := ingest.NewPipeline(ingest.Pipeline{
		DB:                     pool,
		Storage:                store,
		LLM:                    llmClient,
		Parsers:                ingest.RegistryFromIngestParams(tableOptsFromConfig(cfg.Ingest.Tables), cfg.Ingest.MaxSections, time.Duration(cfg.Ingest.ParseTimeoutSeconds)*time.Second),
		Logger:                 logger,
		Mode:                   cfg.Ingest.Mode,
		HyDEEnabled:            cfg.Ingest.HyDE.Enabled,
		HyDEModel:              cfg.Ingest.HyDE.Model,
		HyDENumQuestions:       cfg.Ingest.HyDE.NumQuestions,
		HyDEConcurrency:        cfg.Ingest.HyDE.Concurrency,
		SummaryAxesEnabled:     cfg.Ingest.SummaryAxes.Enabled,
		SummaryAxesMaxTopics:   cfg.Ingest.SummaryAxes.MaxTopics,
		SummaryAxesMaxEntities: cfg.Ingest.SummaryAxes.MaxEntities,
		SummaryAxesMaxNumbers:  cfg.Ingest.SummaryAxes.MaxNumbers,
		GlobalLLMConcurrency:   cfg.Ingest.GlobalLLMConcurrency,
	})
	if cfg.Ingest.Mode == ingest.ModeMinimal {
		logger.Info("ingest: MINIMAL mode — parse→persist→ready; skipping summarize/HyDE/multi-axis/TOC + table extraction")
	} else if cfg.Ingest.Tables.Enabled {
		logger.Info("ingest: pdf table extraction enabled",
			"vertical_strategy", cfg.Ingest.Tables.VerticalStrategy,
			"horizontal_strategy", cfg.Ingest.Tables.HorizontalStrategy,
			"min_rows", cfg.Ingest.Tables.MinTableRows,
			"min_cols", cfg.Ingest.Tables.MinTableCols,
		)
	} else {
		logger.Info("ingest: pdf table extraction disabled")
	}
	q.Register(queue.KindIngestDocument, pipeline.Handler())

	// /v1/answer/treewalk gets its OWN TreeWalkStrategy instance,
	// independent of whatever selection strategy is configured in
	// retrieval.strategy. This way the endpoint is always available
	// (gated by retrieval.treewalk.enabled), even on a deployment
	// using chunked-tree as its default selection path.
	var treeWalkStrategy *retrieval.TreeWalkStrategy
	if cfg.Retrieval.TreeWalk.Enabled && llmClient != nil {
		treeWalkStrategy = buildTreeWalkStrategy(cfg.Retrieval, llmClient, store)
		logger.Info("retrieval: treewalk answer endpoint enabled",
			"max_hops", treeWalkStrategy.MaxHops,
			"page_content_limit", treeWalkStrategy.PageContentLimit,
			"model_override", cfg.Retrieval.TreeWalk.Model,
		)
	}

	deps := api.Deps{
		Logger:            logger,
		DB:                pool,
		Storage:           store,
		Queue:             q,
		Strategy:          strategy,
		Version:           version,
		MultiDoc:          multiDoc,
		LLM:               llmClient,
		LLMModel:          modelFor(cfg.LLM),
		AnswerSpan:        cfg.Retrieval.AnswerSpan,
		Answer:            cfg.Retrieval.Answer,
		Planner:           planner,
		Planning:          cfg.Retrieval.Planning,
		ReRanker:          reRanker,
		ReRank:            cfg.Retrieval.ReRank,
		Replay:            replayStore,
		Abstain:           cfg.Retrieval.Abstain,
		TreeWalkStrategy: treeWalkStrategy,
		TreeWalk:         cfg.Retrieval.TreeWalk,
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
			Token:             c.QStash.Token,
			WebhookBaseURL:    c.QStash.WebhookBaseURL,
			CurrentSigningKey: c.QStash.CurrentSigningKey,
			NextSigningKey:    c.QStash.NextSigningKey,
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

// modelFor returns the configured chat/general-purpose model name for
// the selected LLM driver. Used as a fallback when an API request
// omits an explicit model.
func modelFor(c config.LLMConfig) string {
	switch c.Driver {
	case "anthropic":
		return c.Anthropic.Model
	case "openai":
		return c.OpenAI.Model
	case "gemini":
		return c.Gemini.Model
	}
	return ""
}

func buildLLM(c config.LLMConfig) (llmgate.Client, error) {
	switch c.Driver {
	case "anthropic":
		return anthropic.New(anthropic.Config{
			APIKey:         c.Anthropic.APIKey,
			Model:          c.Anthropic.Model,
			ReasoningModel: c.Anthropic.ReasoningModel,
			BaseURL:        c.Anthropic.BaseURL,
		})
	case "openai":
		return openai.New(openai.Config{
			APIKey:         c.OpenAI.APIKey,
			Model:          c.OpenAI.Model,
			ReasoningModel: c.OpenAI.ReasoningModel,
		})
	case "gemini":
		return gemini.New(gemini.Config{
			APIKey:         c.Gemini.APIKey,
			Model:          c.Gemini.Model,
			ReasoningModel: c.Gemini.ReasoningModel,
		})
	default:
		// Config.Validate rejects unknown drivers; this is defensive.
		return nil, fmt.Errorf("unknown llm driver: %s", c.Driver)
	}
}

func buildStrategy(c config.RetrievalConfig, client llmgate.Client, store storage.Storage) retrieval.Strategy {
	switch c.Strategy {
	case "single-pass":
		return retrieval.NewSinglePass(client)
	case "chunked-tree":
		return retrieval.NewChunkedTree(client)
	case "agentic":
		a := retrieval.NewAgentic(client, storageFetcher{s: store})
		if c.Agentic.MaxHops > 0 {
			a.MaxHops = c.Agentic.MaxHops
		}
		a.ModelOverride = c.Agentic.Model
		return a
	case "treewalk":
		return buildTreeWalkStrategy(c, client, store)
	default:
		return retrieval.NewChunkedTree(client)
	}
}

// buildTreeWalkStrategy constructs the page-based agentic
// strategy with the storage-backed PageLoader and the configured
// caps. Used by buildStrategy when retrieval.strategy=treewalk AND
// by the /v1/answer/treewalk endpoint setup (which wires its own
// instance regardless of the selection strategy).
//
// The TOCProvider is left nil here. PR-A (toc-tree-builder) adds
// documents.toc_tree + a DB-backed provider; until it lands the
// strategy degrades to its synthesised view, which is the
// documented fallback path.
func buildTreeWalkStrategy(c config.RetrievalConfig, client llmgate.Client, store storage.Storage) *retrieval.TreeWalkStrategy {
	p := retrieval.NewTreeWalkStrategy(client)
	p.PageLoader = storagePageLoader{s: store}
	if c.TreeWalk.MaxHops > 0 {
		p.MaxHops = c.TreeWalk.MaxHops
	}
	if c.TreeWalk.PageContentLimit > 0 {
		p.PageContentLimit = c.TreeWalk.PageContentLimit
	}
	if c.TreeWalk.MaxCitations > 0 {
		p.MaxCitations = c.TreeWalk.MaxCitations
	}
	p.ModelOverride = c.TreeWalk.Model
	return p
}

// storageFetcher adapts a storage.Storage to retrieval.ContentFetcher.
// The agentic strategy reads section bodies one at a time, so we
// materialize the full reader contents into a []byte here rather than
// streaming — section bodies are typically a few KB.
type storageFetcher struct{ s storage.Storage }

func (sf storageFetcher) Get(ctx context.Context, ref string) ([]byte, error) {
	rc, _, err := sf.s.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// storagePageLoader adapts a storage.Storage to
// retrieval.PageContentLoader. Mirrors storageFetcher but lives
// behind a separate interface so the two callers (agentic /
// treewalk) can be wired independently. The TreeWalk strategy
// materialises section bodies once per get_pages observation, so
// reading the full reader into a []byte is the right shape.
type storagePageLoader struct{ s storage.Storage }

func (l storagePageLoader) Load(ctx context.Context, ref string) ([]byte, error) {
	rc, _, err := l.s.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
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

// tableOptsFromConfig translates the YAML/env Tables block into the
// parser-level TableOpts struct. Returns nil when tables are disabled so
// the PDF parser short-circuits without instantiating pdftable settings.
func tableOptsFromConfig(c config.TablesConfig) *parser.TableOpts {
	if !c.Enabled {
		return nil
	}
	return &parser.TableOpts{
		Enabled:            true,
		VerticalStrategy:   c.VerticalStrategy,
		HorizontalStrategy: c.HorizontalStrategy,
		MinTableRows:       c.MinTableRows,
		MinTableCols:       c.MinTableCols,
	}
}
