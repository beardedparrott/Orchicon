// Package telemetry sets up the OpenTelemetry pipeline: tracer,
// meter, and OTLP exporter (→ SigNoz/ClickHouse via the OTel collector
// in deploy/compose). Per docs/08 §5, telemetry flows from the producer
// to the OTel collector to SigNoz — it does not flow through NATS.
//
// The pipeline is best-effort: if the collector is unreachable,
// telemetry is dropped with bounded in-process buffering and control
// flow is not blocked (docs/08 §8 invariant #5).
//
// The gRPC connection to the collector is non-blocking (grpc.NewClient
// dials in the background), so the control plane starts immediately
// without waiting for the telemetry stack to be healthy.
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/beardedparrott/orchicon/internal/config"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Shutdowner holds the cleanup functions for the OTel pipeline.
type Shutdowner struct {
	tracerShutdown func(context.Context) error
	meterShutdown  func(context.Context) error
	conn           *grpc.ClientConn
	log            *slog.Logger
}

// Shutdown flushes and shuts down the tracer and meter exporters.
func (s *Shutdowner) Shutdown(ctx context.Context) {
	if s == nil {
		return
	}
	if s.tracerShutdown != nil {
		if err := s.tracerShutdown(ctx); err != nil {
			s.log.Warn("tracer shutdown failed", "error", err)
		}
	}
	if s.meterShutdown != nil {
		if err := s.meterShutdown(ctx); err != nil {
			s.log.Warn("meter shutdown failed", "error", err)
		}
	}
	if s.conn != nil {
		s.conn.Close()
	}
}

// Setup initializes the global OTel tracer and meter, exporting via OTLP
// gRPC to the collector at cfg.OTelEndpoint. It returns a Shutdowner for
// graceful cleanup. If the exporter cannot be created, telemetry is
// disabled but the process continues (docs/08 §8).
//
// The gRPC connection is non-blocking — grpc.NewClient dials in the
// background, so the control plane is not blocked waiting for the OTel
// collector to be reachable at startup (prevents the 20s startup delay
// when the telemetry stack is still initializing).
func Setup(ctx context.Context, cfg config.Config, log *slog.Logger) (*Shutdowner, error) {
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName("orchicon-control-plane"),
	))
	if err != nil {
		return nil, fmt.Errorf("telemetry: resource: %w", err)
	}

	endpoint := cfg.OTelEndpoint
	shutdown := &Shutdowner{log: log}

	// Shared non-blocking gRPC connection. grpc.NewClient returns
	// immediately and connects in the background — the exporter will
	// queue spans until the connection is established, then flush them.
	// This eliminates the 10s+10s blocking dial at startup when the
	// OTel collector is still starting up.
	conn, err := grpc.NewClient(endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry: grpc dial: %w", err)
	}
	shutdown.conn = conn

	// Trace exporter (non-blocking — conn dials in background).
	traceExporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithGRPCConn(conn),
		otlptracegrpc.WithTimeout(10*time.Second),
	)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("telemetry: trace exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)
	shutdown.tracerShutdown = tp.Shutdown

	// Metric exporter (same non-blocking connection).
	metricExporter, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithGRPCConn(conn),
		otlpmetricgrpc.WithTimeout(10*time.Second),
	)
	if err != nil {
		log.Warn("metric exporter unavailable (metrics disabled)", "error", err)
	} else {
		mp := metric.NewMeterProvider(
			metric.WithReader(metric.NewPeriodicReader(metricExporter, metric.WithInterval(15*time.Second))),
			metric.WithResource(res),
		)
		otel.SetMeterProvider(mp)
		shutdown.meterShutdown = mp.Shutdown
	}

	// W3C TraceContext + Baggage propagation (docs/08 §6).
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	log.Info("otel pipeline initialized", "endpoint", endpoint)
	return shutdown, nil
}

// Tracer returns the global tracer for creating spans.
func Tracer() trace.Tracer {
	return otel.Tracer("orchicon")
}

// Meter returns the global meter for creating instruments.
func Meter() otelmetric.Meter {
	return otel.Meter("orchicon")
}

// Middleware wraps an http.Handler with OTel trace extraction and span
// creation. Every API call gets a root span (docs/08 §5.1: api.<path>).
// The W3C traceparent header is extracted so frontend and backend spans
// share the same trace (docs/10 §8).
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), headerCarrier(r.Header))
		// Phase 8: ensure a correlation_id propagates across the whole
		// user action (API → reconciler → adapter → AI Gateway — docs/08
		// §3, §5.1). Extract from baggage if present (injected by an
		// upstream caller); otherwise generate one and inject into baggage
		// so all downstream spans carry it.
		correlationID := CorrelationIDFromContext(ctx)
		if correlationID == "" {
			correlationID = newCorrelationID()
			ctx = WithCorrelationID(ctx, correlationID)
		}
		route := r.URL.Path
		if route == "" {
			route = "/"
		}
		ctx, span := Tracer().Start(ctx, "api."+route,
			trace.WithAttributes(
				attribute.String("http.method", r.Method),
				attribute.String("http.route", route),
				attribute.String(CorrelationIDKey, correlationID),
			),
		)
		defer span.End()

		// Propagate the correlation_id back to the caller via the
		// response header so clients can join logs/telemetry to the
		// originating request (docs/08 §6).
		w.Header().Set("x-orchicon-correlation-id", correlationID)

		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r.WithContext(ctx))

		span.SetAttributes(attribute.Int("http.status_code", sw.status))
		if sw.status >= 500 {
			span.SetStatus(codes.Error, http.StatusText(sw.status))
		}
	})
}

func headerCarrier(h http.Header) propagation.TextMapCarrier {
	return propagation.HeaderCarrier(h)
}

// statusWriter wraps http.ResponseWriter to capture the status code.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush delegates to the underlying writer if it implements
// http.Flusher. Required for Connect server streams
// (StreamProjectEvents etc.) — Connect checks for http.Flusher and
// returns CodeInternal if the wrapped writer doesn't expose it.
// The underlying net/http response writer does implement Flusher in
// practice; the type-asserting no-op below handles test doubles and
// any future writer that doesn't.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
