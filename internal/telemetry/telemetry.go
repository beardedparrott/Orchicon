// Package telemetry sets up the OpenTelemetry pipeline: tracer,
// meter, and OTLP exporter (→ SigNoz/ClickHouse via the OTel collector
// in deploy/compose). Per docs/08 §5, telemetry flows from the producer
// to the OTel collector to SigNoz — it does not flow through NATS.
//
// The pipeline is best-effort: if the collector is unreachable,
// telemetry is dropped with bounded in-process buffering and control
// flow is not blocked (docs/08 §8 invariant #5).
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
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Shutdowner holds the cleanup functions for the OTel pipeline.
type Shutdowner struct {
	tracerShutdown func(context.Context) error
	meterShutdown  func(context.Context) error
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
}

// Setup initializes the global OTel tracer and meter, exporting via OTLP
// gRPC to the collector at cfg.OTelEndpoint. It returns a Shutdowner for
// graceful cleanup. If the exporter cannot be created, telemetry is
// disabled but the process continues (docs/08 §8).
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

	// Trace exporter.
	traceExporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithTimeout(10*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry: trace exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)
	shutdown.tracerShutdown = tp.Shutdown

	// Metric exporter.
	metricExporter, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpoint(endpoint),
		otlpmetricgrpc.WithInsecure(),
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
		route := r.URL.Path
		if route == "" {
			route = "/"
		}
		ctx, span := Tracer().Start(ctx, "api."+route,
			trace.WithAttributes(
				attribute.String("http.method", r.Method),
				attribute.String("http.route", route),
			),
		)
		defer span.End()

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
