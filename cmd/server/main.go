// Command server is the vectorless transport server.
//
// It wraps the vectorless engine as a thin HTTP + gRPC service, adding
// authentication, observability, and rate limiting. The engine runs
// in-process — there is no network hop between server and engine.
//
// Usage:
//
//	vectorless-server --config config.yaml          # HTTP + embedded workers
//	vectorless-server --config config.yaml --role worker  # queue workers only
//
// See docs/SERVER.md in the engine repo for the full design document.
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

	"github.com/hallelx2/vectorless-engine/pkg/db"
	"github.com/hallelx2/vectorless-engine/pkg/ingest"
	"github.com/hallelx2/vectorless-engine/pkg/parser"
	"github.com/hallelx2/vectorless-engine/pkg/queue"
	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/storage"
	"github.com/hallelx2/vectorless-engine/pkg/tree"

	"github.com/hallelx2/vectorless-engine/internal/config"
	"github.com/hallelx2/vectorless-engine/internal/handler"
	"github.com/hallelx2/vectorless-engine/internal/telemetry"

	enginecfg "github.com/hallelx2/vectorless-engine/pkg/config"
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
	role := flag.String("role", "server", `role to run: "server" (HTTP + workers) or "worker" (queue workers only)`)
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := newLogger(cfg.Engine.Log)
	logger.Info("starting vectorless-server",
		"version", version,
		"role", *role,
		"addr", cfg.Server.Addr,
		"auth_mode", cfg.Auth.Mode,
		"metrics_enabled", cfg.Metrics.Enabled,
		"tracing_enabled", cfg.Tracing.Enabled,
		"storage_driver", cfg.Engine.Storage.Driver,
		"queue_driver", cfg.Engine.Queue.Driver,
		"llm_driver", cfg.Engine.LLM.Driver,
		"retrieval_strategy", cfg.Engine.Retrieval.Strategy,
	)

	// Surface any model with no price-book entry: its cost reads $0, which
	// would otherwise masquerade as "free" in usage/benchmark accounting.
	pricing.WarnFunc = func(model string) {
		logger.Warn("llm model not in price book; cost reported as 0", "model", model)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ── OpenTelemetry tracing (optional) ──────────────────────────
	if cfg.Tracing.Enabled {
		shutdown, err := telemetry.InitTracer(ctx, telemetry.TracingConfig{
			Endpoint:    cfg.Tracing.Endpoint,
			Insecure:    cfg.Tracing.Insecure,
			ServiceName: cfg.Tracing.ServiceName,
			Version:     version,
			SampleRate:  cfg.Tracing.SampleRate,
		})
		if err != nil {
			return fmt.Errorf("init tracing: %w", err)
		}
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := shutdown(shutdownCtx); err != nil {
				logger.Warn("trace export shutdown error", "err", err)
			}
		}()
		logger.Info("tracing: OTLP export enabled", "endpoint", cfg.Tracing.Endpoint)
	}

	// ── Database ──────────────────────────────────────────────────
	pool, err := db.Open(ctx, cfg.Engine.Database.URL, int32(cfg.Engine.Database.MaxConns))
	if err != nil {
		return fmt.Errorf("init db: %w", err)
	}
	defer pool.Close()
	if err := pool.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate db: %w", err)
	}
	logger.Info("db: migrations applied")

	// ── Storage ───────────────────────────────────────────────────
	store, err := buildStorage(cfg.Engine.Storage)
	if err != nil {
		return fmt.Errorf("init storage: %w", err)
	}

	// ── Queue ─────────────────────────────────────────────────────
	q, err := buildQueue(cfg.Engine.Queue, cfg.Engine.Database.URL)
	if err != nil {
		return fmt.Errorf("init queue: %w", err)
	}
	defer func() { _ = q.Close() }() // best-effort close

	// ── LLM + retrieval strategy ──────────────────────────────────
	llmClient, err := buildLLM(cfg.Engine.LLM)
	if err != nil {
		return fmt.Errorf("init llm: %w", err)
	}
	strategy := buildStrategy(cfg.Engine.Retrieval, llmClient, store, pool)

	// Wrap with caching if enabled in engine config.
	if cfg.Engine.Retrieval.Cache.Enabled {
		ttl := time.Duration(cfg.Engine.Retrieval.Cache.TTLSeconds) * time.Second
		if ttl == 0 {
			ttl = 10 * time.Minute
		}
		strategy = retrieval.NewCached(strategy, retrieval.CachedConfig{
			MaxEntries: cfg.Engine.Retrieval.Cache.MaxEntries,
			TTL:        ttl,
		})
		logger.Info("retrieval: cache enabled",
			"max_entries", cfg.Engine.Retrieval.Cache.MaxEntries,
			"ttl_seconds", cfg.Engine.Retrieval.Cache.TTLSeconds,
		)
	}

	// Multi-document query dispatcher.
	multiDoc := retrieval.NewMultiDoc(strategy, pool.LoadTree)

	// Pre-built set of selectable strategies, keyed by config name.
	// Backs the per-request "strategy" override on /v1/query so the
	// benchmark can A/B chunked-tree vs treewalk against this same
	// running engine without a redeploy. Built from the raw client so
	// each override behaves identically to booting with that strategy
	// as the default (no shared cache wrapper across overrides).
	strategies := buildStrategySet(cfg.Engine.Retrieval, llmClient, store, pool)

	// Replay store: every /v1/answer and /v1/answer/treewalk response
	// is stamped with a deterministic trace_token and its body bytes
	// persisted here so /v1/replay can return them verbatim. On by
	// default; operators opt out via retrieval.replay.enabled=false.
	var replayStore retrieval.ReplayStore
	if cfg.Engine.Retrieval.Replay.Enabled {
		replayStore = retrieval.NewLRUReplayStore(retrieval.LRUReplayConfig{
			MaxEntries: cfg.Engine.Retrieval.Replay.MaxEntries,
			TTL:        time.Duration(cfg.Engine.Retrieval.Replay.TTLSeconds) * time.Second,
		})
		logger.Info("retrieval: replay store enabled",
			"max_entries", cfg.Engine.Retrieval.Replay.MaxEntries,
			"ttl_seconds", cfg.Engine.Retrieval.Replay.TTLSeconds,
		)
	}

	// /v1/answer/treewalk gets its OWN TreeWalkStrategy instance,
	// independent of whatever selection strategy retrieval.strategy
	// chose, so the endpoint is always available (gated by
	// retrieval.treewalk.enabled) even on a chunked-tree deployment.
	var treeWalkStrategy *retrieval.TreeWalkStrategy
	if cfg.Engine.Retrieval.TreeWalk.Enabled && llmClient != nil {
		treeWalkStrategy = buildTreeWalkStrategy(cfg.Engine.Retrieval, llmClient, store, pool)
		logger.Info("retrieval: treewalk answer endpoint enabled",
			"max_hops", treeWalkStrategy.MaxHops,
			"page_content_limit", treeWalkStrategy.PageContentLimit,
			"model_override", cfg.Engine.Retrieval.TreeWalk.Model,
		)
	}

	// ── Ingest pipeline ───────────────────────────────────────────
	pipeline := ingest.NewPipeline(ingest.Pipeline{
		DB:                     pool,
		Storage:                store,
		LLM:                    llmClient,
		Parsers:                ingest.RegistryFromIngestParams(tableOptsFromConfig(cfg.Engine.Ingest.Tables), cfg.Engine.Ingest.MaxSections, time.Duration(cfg.Engine.Ingest.ParseTimeoutSeconds)*time.Second),
		Logger:                 logger,
		Mode:                   cfg.Engine.Ingest.Mode,
		HyDEEnabled:            cfg.Engine.Ingest.HyDE.Enabled,
		HyDEModel:              cfg.Engine.Ingest.HyDE.Model,
		HyDENumQuestions:       cfg.Engine.Ingest.HyDE.NumQuestions,
		HyDEConcurrency:        cfg.Engine.Ingest.HyDE.Concurrency,
		SummaryAxesEnabled:     cfg.Engine.Ingest.SummaryAxes.Enabled,
		SummaryAxesMaxTopics:   cfg.Engine.Ingest.SummaryAxes.MaxTopics,
		SummaryAxesMaxEntities: cfg.Engine.Ingest.SummaryAxes.MaxEntities,
		SummaryAxesMaxNumbers:  cfg.Engine.Ingest.SummaryAxes.MaxNumbers,
		TOCEnabled:             cfg.Engine.Ingest.TOC.Enabled,
		TOCModel:               cfg.Engine.Ingest.TOC.Model,
		TOCConcurrency:         cfg.Engine.Ingest.TOC.Concurrency,
		TOCCheckPages:          cfg.Engine.Ingest.TOC.TOCCheckPages,
		GlobalLLMConcurrency:   cfg.Engine.Ingest.GlobalLLMConcurrency,
	})
	if cfg.Engine.Ingest.Mode == ingest.ModeMinimal {
		logger.Info("ingest: MINIMAL mode — parse→persist→ready; skipping summarize/HyDE/multi-axis/TOC + table extraction")
	} else if cfg.Engine.Ingest.Tables.Enabled {
		logger.Info("ingest: pdf table extraction enabled",
			"vertical_strategy", cfg.Engine.Ingest.Tables.VerticalStrategy,
			"horizontal_strategy", cfg.Engine.Ingest.Tables.HorizontalStrategy,
			"min_rows", cfg.Engine.Ingest.Tables.MinTableRows,
			"min_cols", cfg.Engine.Ingest.Tables.MinTableCols,
		)
	} else {
		logger.Info("ingest: pdf table extraction disabled")
	}
	q.Register(queue.KindIngestDocument, pipeline.Handler())

	// ── Start subsystems ──────────────────────────────────────────
	errs := make(chan error, 2)

	// Always start queue workers (both "server" and "worker" roles).
	go func() {
		logger.Info("queue: starting workers", "driver", cfg.Engine.Queue.Driver)
		if err := q.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errs <- fmt.Errorf("queue: %w", err)
		}
	}()

	// Only start the HTTP server in "server" role.
	if *role == "server" {
		deps := handler.Deps{
			Logger:           logger,
			DB:               pool,
			Storage:          store,
			Queue:            q,
			Strategy:         strategy,
			MultiDoc:         multiDoc,
			Version:          version,
			Config:           cfg,
			Strategies:       strategies,
			LLM:              llmClient,
			LLMModel:         modelFor(cfg.Engine.LLM),
			AnswerSpan:       cfg.Engine.Retrieval.AnswerSpan,
			Answer:           cfg.Engine.Retrieval.Answer,
			Replay:           replayStore,
			TreeWalkStrategy: treeWalkStrategy,
			TreeWalk:         cfg.Engine.Retrieval.TreeWalk,
		}

		srv := &http.Server{
			Addr:         cfg.Server.Addr,
			Handler:      handler.Router(deps),
			ReadTimeout:  cfg.Server.ReadTimeout,
			WriteTimeout: cfg.Server.WriteTimeout,
			TLSConfig:    buildTLSConfig(cfg.Server.TLS),
		}

		go func() {
			if cfg.Server.TLS.Enabled() {
				logger.Info("https: listening (direct TLS)",
					"addr", cfg.Server.Addr,
					"cert_file", cfg.Server.TLS.CertFile,
				)
				if err := srv.ListenAndServeTLS(cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
					errs <- fmt.Errorf("https: %w", err)
				}
				return
			}
			logger.Info("http: listening (plaintext — terminate TLS at your proxy)",
				"addr", cfg.Server.Addr,
			)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- fmt.Errorf("http: %w", err)
			}
		}()

		// Wait for shutdown signal or error.
		select {
		case <-ctx.Done():
			logger.Info("shutdown signal received")
		case err := <-errs:
			logger.Error("subsystem failed", "err", err)
			stop()
		}

		// Graceful shutdown: drain in-flight requests.
		drainTimeout := cfg.Server.DrainTimeout
		if drainTimeout == 0 {
			drainTimeout = 15 * time.Second
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("http shutdown error", "err", err)
		}
	} else {
		// Worker-only role: just wait for ctx cancellation.
		select {
		case <-ctx.Done():
			logger.Info("shutdown signal received")
		case err := <-errs:
			logger.Error("subsystem failed", "err", err)
			stop()
		}
	}

	logger.Info("bye")
	return nil
}

// ── Builder helpers (reused from engine, adapted for server config) ──

func buildStorage(c enginecfg.StorageConfig) (storage.Storage, error) {
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
	case "gcs":
		// Auths via Application Default Credentials — on Cloud Run
		// that's the runtime SA via the metadata server.
		return storage.NewGCS(context.Background(), storage.GCSConfig{
			Bucket: c.GCS.Bucket,
		})
	default:
		return nil, fmt.Errorf("unknown storage driver: %s", c.Driver)
	}
}

func buildQueue(c enginecfg.QueueConfig, dbURL string) (queue.Queue, error) {
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
// the selected LLM driver. Used as the engine-default fallback when an
// API request omits an explicit model (answer + answer/treewalk).
func modelFor(c enginecfg.LLMConfig) string {
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

func buildLLM(c enginecfg.LLMConfig) (llmgate.Client, error) {
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
		return nil, fmt.Errorf("unknown llm driver: %s", c.Driver)
	}
}

// buildStrategy constructs the retrieval strategy named by
// retrieval.strategy. The DB pool is threaded through so the
// treewalk strategy can wire a TOC provider that reads
// documents.toc_tree (the other strategies ignore it).
func buildStrategy(c enginecfg.RetrievalConfig, client llmgate.Client, store storage.Storage, pool *db.Pool) retrieval.Strategy {
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
		return buildTreeWalkStrategy(c, client, store, pool)
	case "auto":
		return retrieval.NewAuto(retrieval.NewSinglePass(client), buildTreeWalkStrategy(c, client, store, pool))
	default:
		return retrieval.NewChunkedTree(client)
	}
}

// buildStrategySet pre-builds one instance of every selectable
// strategy, keyed by its config name. The deployed /v1/query handler
// uses this map to honour a per-request "strategy" override without
// rebuilding a strategy on the hot path: selection is a map lookup.
//
// This is what lets the benchmark A/B chunked-tree vs treewalk
// against the SAME running engine — no redeploy, no config flip. The
// caps (agentic max-hops, treewalk page limits, model overrides) come
// from the same config blocks the default builder reads, so an
// override behaves identically to booting with that strategy as the
// default.
func buildStrategySet(c enginecfg.RetrievalConfig, client llmgate.Client, store storage.Storage, pool *db.Pool) map[string]retrieval.Strategy {
	agentic := retrieval.NewAgentic(client, storageFetcher{s: store})
	if c.Agentic.MaxHops > 0 {
		agentic.MaxHops = c.Agentic.MaxHops
	}
	agentic.ModelOverride = c.Agentic.Model

	return map[string]retrieval.Strategy{
		"single-pass":  retrieval.NewSinglePass(client),
		"chunked-tree": retrieval.NewChunkedTree(client),
		"agentic":      agentic,
		"treewalk":     buildTreeWalkStrategy(c, client, store, pool),
		"auto":         retrieval.NewAuto(retrieval.NewSinglePass(client), buildTreeWalkStrategy(c, client, store, pool)),
	}
}

// buildTreeWalkStrategy constructs the page-based agentic strategy
// with the storage-backed PageLoader, a DB-backed TOC provider, and
// the configured caps. Ported from cmd/engine so the DEPLOYED
// cmd/server binary can serve retrieval.strategy=treewalk AND the
// /v1/answer/treewalk endpoint.
//
// The TOC provider reads documents.toc_tree via the worker-scoped
// document lookup. The strategy degrades to its synthesised view
// (built from the loaded section tree) whenever the column is NULL or
// the read errors, so a document ingested before the TOC builder ran
// still navigates cleanly.
func buildTreeWalkStrategy(c enginecfg.RetrievalConfig, client llmgate.Client, store storage.Storage, pool *db.Pool) *retrieval.TreeWalkStrategy {
	p := retrieval.NewTreeWalkStrategy(client)
	p.PageLoader = storagePageLoader{s: store}
	if pool != nil {
		p.TOC = dbTOCProvider{db: pool}
	}
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

// storagePageLoader adapts a storage.Storage to
// retrieval.PageContentLoader. Mirrors storageFetcher but lives behind
// a separate interface so the two callers (agentic / treewalk) can be
// wired independently. The TreeWalk strategy materialises section
// bodies once per get_pages observation, so reading the full reader
// into a []byte is the right shape.
type storagePageLoader struct{ s storage.Storage }

func (l storagePageLoader) Load(ctx context.Context, ref string) ([]byte, error) {
	rc, _, err := l.s.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }() // best-effort close
	return io.ReadAll(rc)
}

// dbTOCProvider adapts the DB pool to retrieval.TOCProvider. It reads
// the persisted documents.toc_tree JSONB and returns it verbatim for
// the get_document_structure tool. A NULL column (the "not yet
// generated" state) surfaces as retrieval.ErrNoTOC, which the strategy
// treats as a graceful-degrade signal: it synthesises the TOC view
// from the section tree instead of failing the request.
//
// GetTOC carries only a document ID (the TOCProvider contract), so the
// lookup uses the worker-scoped accessor. That is safe here: the
// caller has already resolved + authorised the tree for this document
// via the org-scoped LoadTree before the strategy ever calls GetTOC,
// and the TOC tree is the same structural metadata (titles + page
// ranges, no bodies) already present on that authorised tree.
type dbTOCProvider struct{ db *db.Pool }

func (p dbTOCProvider) GetTOC(ctx context.Context, docID tree.DocumentID) ([]byte, error) {
	doc, err := p.db.GetDocumentForWorker(ctx, docID)
	if err != nil {
		return nil, err
	}
	if len(doc.TOCTree) == 0 {
		return nil, retrieval.ErrNoTOC
	}
	return doc.TOCTree, nil
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
	defer func() { _ = rc.Close() }() // best-effort close
	return io.ReadAll(rc)
}

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

func newLogger(c enginecfg.LogConfig) *slog.Logger {
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

// tableOptsFromConfig translates the engine's TablesConfig (from the
// embedded engine config block) into the parser-level TableOpts. Returns
// nil when tables are disabled so the PDF parser short-circuits without
// instantiating pdftable settings.
func tableOptsFromConfig(c enginecfg.TablesConfig) *parser.TableOpts {
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
