package middleware

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// tracerName is the instrumentation scope used for HTTP spans.
const tracerName = "github.com/hallelx2/vectorless-engine/middleware"

// Tracing creates a root span for each HTTP request and propagates the
// trace context from incoming headers (W3C Trace Context / B3). Child
// spans for engine operations (parse, summarise, LLM call) are created
// by the engine itself using the context passed through.
func Tracing(next http.Handler) http.Handler {
	tracer := otel.Tracer(tracerName)
	prop := otel.GetTextMapPropagator()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract parent context from incoming headers.
		ctx := prop.Extract(r.Context(), propagation.HeaderCarrier(r.Header))

		// Start a new span for this HTTP request.
		spanName := r.Method + " " + r.URL.Path
		ctx, span := tracer.Start(ctx, spanName,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.method", r.Method),
				attribute.String("http.url", r.URL.String()),
				attribute.String("http.route", r.URL.Path),
				attribute.String("http.user_agent", r.UserAgent()),
				attribute.String("net.peer.ip", r.RemoteAddr),
			),
		)
		defer span.End()

		// Wrap response writer to capture status code.
		rec := &tracingRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r.WithContext(ctx))

		// Record response attributes.
		span.SetAttributes(
			attribute.Int("http.status_code", rec.status),
		)
		if rec.status >= 400 {
			span.SetAttributes(attribute.Bool("error", true))
		}

		// Inject trace context into response headers (for downstream).
		prop.Inject(ctx, propagation.HeaderCarrier(w.Header()))
	})
}

type tracingRecorder struct {
	http.ResponseWriter
	status int
}

func (r *tracingRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// TraceIDFromContext extracts the trace ID string from the current span,
// useful for correlating logs with traces.
func TraceIDFromContext(ctx context.Context) string {
	span := trace.SpanFromContext(ctx)
	if span.SpanContext().HasTraceID() {
		return span.SpanContext().TraceID().String()
	}
	return ""
}
