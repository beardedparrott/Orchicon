// Package telemetry sets up the OpenTelemetry pipeline: tracer, meter, log exporter, and OTLP gRPC
// exporter (→ the telemetry backends via the OTel collector in deploy/compose).
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
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otel_log "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdk_log "go.opentelemetry.io/otel/sdk/log"
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
	logShutdown    func(context.Context) error
	conn           *grpc.ClientConn
	log            *slog.Logger
}

// Shutdown flushes and shuts down all exporters.
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
	if s.logShutdown != nil {
		if err := s.logShutdown(ctx); err != nil {
			s.log.Warn("log exporter shutdown failed", "error", err)
		}
	}
	if s.conn != nil {
		s.conn.Close()
	}
}

// Setup initializes the global OTel tracer and meter, exporting via OTLP gRPC to the collector at
// cfg.OTelEndpoint.
func Setup(ctx context.Context, cfg config.Config, log *slog.Logger) (*Shutdowner, error) {
	// v0.1: single dev tenant.
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName("orchicon-control-plane"),
		attribute.String("orchicon.tenant_id", "tnt_dev"),
	))
	if err != nil {
		return nil, fmt.Errorf("telemetry: resource: %w", err)
	}

	endpoint := cfg.OTelEndpoint
	shutdown := &Shutdowner{log: log}

	// Shared non-blocking gRPC connection.
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

	// Log exporter (same non-blocking connection).
	logExporter, err := otlploggrpc.New(ctx,
		otlploggrpc.WithGRPCConn(conn),
		otlploggrpc.WithTimeout(10*time.Second),
	)
	if err != nil {
		log.Warn("log exporter unavailable (logs disabled)", "error", err)
	} else {
		lp := sdk_log.NewLoggerProvider(
			sdk_log.WithProcessor(sdk_log.NewBatchProcessor(logExporter)),
			sdk_log.WithResource(res),
		)
		global.SetLoggerProvider(lp)
		shutdown.logShutdown = lp.Shutdown
		log.Info("log exporter initialized")
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

// Logger returns the global OTel log provider logger for emitting structured log records to the
// telemetry pipeline.
func Logger() otel_log.Logger {
	return global.GetLoggerProvider().Logger("orchicon")
}

// EmitLog creates and dispatches a log record through the OTel pipeline.
func EmitLog(ctx context.Context, severity string, body string, attrs ...attribute.KeyValue) {
	logger := Logger()
	if logger == nil {
		return
	}
	r := otel_log.Record{}
	r.SetTimestamp(time.Now())
	r.SetSeverity(otel_log.Severity(severityLevel(severity)))
	r.SetBody(otel_log.StringValue(body))

	// Convert otel attribute.KeyValue pairs to log attributes.
	var logAttrs []otel_log.KeyValue
	for _, a := range attrs {
		logAttrs = append(logAttrs, otel_log.KeyValueFromAttribute(a))
	}
	r.AddAttributes(logAttrs...)

	logger.Emit(ctx, r)
}

// severityLevel maps standard severity strings to OTel severity levels.
func severityLevel(s string) int {
	switch s {
	case "TRACE":
		return int(otel_log.SeverityTrace)
	case "DEBUG":
		return int(otel_log.SeverityDebug)
	case "INFO":
		return int(otel_log.SeverityInfo)
	case "WARN":
		return int(otel_log.SeverityWarn)
	case "ERROR":
		return int(otel_log.SeverityError)
	case "FATAL":
		return int(otel_log.SeverityFatal)
	default:
		return int(otel_log.SeverityInfo)
	}
}

// Middleware wraps an http.Handler with OTel trace extraction and span creation.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), headerCarrier(r.Header))
		// Phase 8: ensure a correlation_id propagates across the whole user action (API → reconciler →
		// adapter → AI Gateway — docs/08 §3, §5.1).
		correlationID := CorrelationIDFromContext(ctx)
		if correlationID == "" {
			correlationID = newCorrelationID()
			ctx = WithCorrelationID(ctx, correlationID)
		}
		route := r.URL.Path
		if route == "" {
			route = "/"
		}
		// The middleware is the ONLY span producer today, so this is the only place those attributes are
		// written — Tempo tag filters read them.
		ctx = WithAttribution(ctx, r.Header.Get(hintProjectID), r.Header.Get(hintWorkItemID), r.Header.Get(hintExecutionID))
		spanAttrs := []attribute.KeyValue{
			attribute.String("http.method", r.Method),
			attribute.String("http.route", route),
			attribute.String(CorrelationIDKey, correlationID),
		}
		spanAttrs = append(spanAttrs, attributionAttrsFromContext(ctx)...)
		ctx, span := Tracer().Start(ctx, "api."+route,
			trace.WithAttributes(spanAttrs...),
		)
		defer span.End()

		// Propagate the correlation_id back to the caller via the response header so clients can join
		// logs/telemetry to the originating request (docs/08 §6).
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

// Flush delegates to the underlying writer if it implements http.Flusher.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
