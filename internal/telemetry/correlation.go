package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"

	"github.com/beardedparrott/orchicon/internal/tenant"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/trace"
)

// CorrelationIDKey is the baggage + span attribute key carrying the
// correlation_id — the unit of "this came from one user action" that
// propagates across API → reconciler → adapter → AI Gateway
// (docs/08 §3, §5.1).
const CorrelationIDKey = "orchicon.correlation_id"

// CorrelationIDFromContext returns the correlation_id from baggage if
// present, otherwise an empty string. The middleware ensures a
// correlation_id is set on every inbound request, so downstream spans
// (reconciler, adapter, gateway) inherit it via the propagated context.
func CorrelationIDFromContext(ctx context.Context) string {
	m := baggage.FromContext(ctx)
	if c := m.Member(CorrelationIDKey); c.Value() != "" {
		return c.Value()
	}
	return ""
}

// EnsureCorrelationID returns the correlation_id from the context, or
// generates a new one if none is present. The caller should store the
// returned id in baggage + span attributes so it propagates.
func EnsureCorrelationID(ctx context.Context) string {
	if id := CorrelationIDFromContext(ctx); id != "" {
		return id
	}
	return newCorrelationID()
}

// WithCorrelationID returns a context carrying the correlation_id in
// baggage so it propagates across process boundaries (OTel baggage
// is extracted/injected by the W3C propagator — docs/08 §6).
func WithCorrelationID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	m, err := baggage.New(member(CorrelationIDKey, id))
	if err != nil {
		return ctx
	}
	return baggage.ContextWithBaggage(ctx, m)
}

// RecordCorrelation records the correlation_id + tenant_id as span
// attributes on the current span (docs/08 §6: correlation_id is an OTel
// span attribute). Call at the start of every span-producing scope
// (reconciler, adapter, gateway) so the trace joins by attribute.
func RecordCorrelation(ctx context.Context, correlationID string) {
	span := trace.SpanFromContext(ctx)
	if span == nil {
		return
	}
	attrs := []attribute.KeyValue{attribute.String(CorrelationIDKey, correlationID)}
	if tid := tenant.FromContext(ctx); tid != "" {
		attrs = append(attrs, attribute.String("orchicon.tenant_id", tid))
	}
	span.SetAttributes(attrs...)
}

// newCorrelationID generates a 16-byte hex id. Not a ULID — it is a
// per-action correlation handle, not a sortable entity id.
func newCorrelationID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func member(k, v string) baggage.Member {
	m, _ := baggage.NewMember(k, v)
	return m
}

// StartSpan is a convenience wrapper that starts a span, records the
// correlation_id + tenant_id attributes, and returns the span context.
// Use for the canonical spans (docs/08 §5.1):
//   - reconcile.<kind>.<id>
//   - dispatch.<task_id>
//   - adapter.execute.<execution_id>
//   - gateway.<provider>.<model>.request
func StartSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	ctx, span := otel.Tracer("orchicon").Start(ctx, name, opts...)
	RecordCorrelation(ctx, CorrelationIDFromContext(ctx))
	return ctx, span
}

// SpanOption is an alias for trace.SpanStartOption for the convenience wrapper.
type SpanOption = trace.SpanStartOption
