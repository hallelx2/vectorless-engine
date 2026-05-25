package handler

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/hallelx2/vectorless-engine/pkg/db"
	"github.com/hallelx2/vectorless-engine/pkg/queue"
	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/storage"

	"github.com/hallelx2/vectorless-engine/gen/vectorless/v1/vectorlessv1connect"
	"github.com/hallelx2/vectorless-engine/internal/config"
	"github.com/hallelx2/vectorless-engine/internal/connecthandler"
	"github.com/hallelx2/vectorless-engine/internal/middleware"
)

// Deps bundles the server's runtime dependencies for injection.
type Deps struct {
	Logger   *slog.Logger
	DB       *db.Pool
	Storage  storage.Storage
	Queue    queue.Queue
	Strategy retrieval.Strategy
	MultiDoc *retrieval.MultiDoc
	Version  string
	Config   config.Config
}

// Router builds the chi router with all v1 routes and the full
// middleware stack described in SERVER.md:
//
//  1. RequestID — generate or propagate X-Request-ID
//  2. RealIP — honour X-Forwarded-For behind a trusted proxy
//  3. Recovery — convert panics into 500s with a logged stack trace
//  4. AccessLog — structured access log (method, path, status, duration)
//  5. Metrics — Prometheus histograms + counters
//  6. Tracing — OpenTelemetry root span per request (optional)
//  7. Auth — skipped for /v1/health, /v1/version, /metrics
//  8. RateLimit — optional, token bucket per principal
//  9. The handler itself
func Router(d Deps) http.Handler {
	r := chi.NewRouter()

	// ── Middleware stack (order matters) ───────────────────────────

	// CORS must be first so preflight OPTIONS responses are sent
	// before any auth or rate-limit middleware can reject them.
	if d.Config.CORS.Enabled {
		r.Use(middleware.CORS(middleware.CORSConfig{
			AllowedOrigins: d.Config.CORS.AllowedOrigins,
			MaxAge:         d.Config.CORS.MaxAge,
		}))
	}

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recovery(d.Logger))
	r.Use(middleware.AccessLog(d.Logger))

	if d.Config.Metrics.Enabled {
		r.Use(middleware.Metrics)
	}

	// OpenTelemetry tracing (adds root span per request).
	if d.Config.Tracing.Enabled {
		r.Use(middleware.Tracing)
	}

	// Auth: build the authenticator from config.
	var auth middleware.Authenticator
	switch d.Config.Auth.Mode {
	case "api_key":
		auth = middleware.NewStaticAPIKey(d.Config.Auth.APIKey)
	default:
		auth = middleware.NoAuth{}
	}
	r.Use(middleware.Auth(auth))

	// Rate limit (optional).
	if d.Config.RateLimit.Enabled {
		r.Use(middleware.RateLimit(d.Config.RateLimit.RequestsPerMinute))
	}

	// Per-principal rate limit (optional, Phase 3).
	if d.Config.RateLimit.PerPrincipalRPM > 0 {
		r.Use(middleware.PrincipalRateLimit(d.Config.RateLimit.PerPrincipalRPM))
	}

	// Governance: max body size and per-endpoint timeout.
	r.Use(middleware.MaxBodySize(d.Config.Governance.MaxBodySizeBytes))
	r.Use(middleware.EndpointTimeout(
		d.Config.Governance.DefaultTimeout,
		d.Config.Governance.QueryTimeout,
	))

	// Idempotency: cache POST /v1/documents responses by
	// Idempotency-Key header to prevent duplicate ingestion.
	r.Use(middleware.Idempotency(middleware.IdempotencyConfig{}))

	// ── REST Handlers (hand-written, chi) ─────────────────────────
	health := NewHealthHandler(d.Version)
	docs := NewDocumentsHandler(d.Logger, d.DB, d.Storage, d.Queue)
	query := NewQueryHandler(d.Logger, d.DB, d.Storage, d.Strategy)
	queryStream := NewQueryStreamHandler(d.Logger, d.DB, d.Storage, d.Strategy)
	queryMulti := NewQueryMultiHandler(d.Logger, d.Storage, d.Strategy, d.MultiDoc)
	queryStreamMulti := NewQueryStreamMultiHandler(d.Logger, d.Storage, d.MultiDoc)
	webhook := NewWebhookHandler(d.Logger, d.Queue)

	// ── Connect-RPC Handlers (generated stubs, three-transport) ───
	// These serve the same API over Connect (HTTP/JSON), gRPC, and
	// gRPC-Web — all from the same handler.
	connectHealth := connecthandler.NewHealthService(d.Version)
	connectDocs := connecthandler.NewDocumentsService(d.Logger, d.DB, d.Storage, d.Queue)
	connectQuery := connecthandler.NewQueryService(d.Logger, d.DB, d.Storage, d.Strategy, d.MultiDoc)

	// Mount Connect-RPC service handlers. Each returns (path, handler).
	healthPath, healthHandler := vectorlessv1connect.NewHealthServiceHandler(connectHealth)
	docsPath, docsHandler := vectorlessv1connect.NewDocumentsServiceHandler(connectDocs)
	queryPath, queryHandler := vectorlessv1connect.NewQueryServiceHandler(connectQuery)

	r.Mount(healthPath, healthHandler)
	r.Mount(docsPath, docsHandler)
	r.Mount(queryPath, queryHandler)

	// ── REST Routes (kept for backward compat + curl-friendliness) ─

	// Prometheus metrics endpoint (outside /v1 versioning).
	if d.Config.Metrics.Enabled {
		r.Handle("/metrics", promhttp.Handler())
	}

	r.Route("/v1", func(r chi.Router) {
		// Health / meta
		r.Get("/health", health.HandleHealth)
		r.Get("/version", health.HandleVersion)

		// Documents
		r.Route("/documents", func(r chi.Router) {
			r.Get("/", docs.HandleListDocuments)
			r.Post("/", docs.HandleIngestDocument)
			r.Get("/{id}", docs.HandleGetDocument)
			r.Delete("/{id}", docs.HandleDeleteDocument)
			r.Get("/{id}/tree", docs.HandleGetTree)
			r.Get("/{id}/source", docs.HandleGetDocumentSource)
		})

		// Sections
		r.Get("/sections/{id}", docs.HandleGetSection)

		// Query
		r.Route("/query", func(r chi.Router) {
			r.Post("/", query.HandleQuery)
			r.Post("/stream", queryStream.HandleQueryStream)
			r.Post("/multi", queryMulti.HandleQueryMulti)
			r.Post("/multi/stream", queryStreamMulti.HandleQueryStreamMulti)
		})
	})

	// Internal: queue webhook (QStash).
	r.Post("/internal/jobs/{kind}", webhook.HandleQueueWebhook)

	return r
}
