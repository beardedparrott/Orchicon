package aigateway

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
)

// RecordPrefixCache records a session's prefix-cache hit/miss breakdown and
// the derived hit rate to the OTel metrics pipeline (D3, ADR-0009 D6). It is
// the session-terminal half of the native adapter's cache telemetry: the
// per-turn usage buckets already flow through Record (which emits the
// cache-aware orchicon_tokens_consumed / orchicon_cost_usd split); this emits
// the session-level hit/miss/hit-rate family so an operator can see
// prefix-cache effectiveness per execution. Best-effort — a metric error is
// dropped, never a control-flow event. Skipped when Turns==0.
func (u *UsageRecorder) RecordPrefixCache(ctx context.Context, tenantID, projectID, workerID, executionID string, turns, hits, misses, cacheRead, cacheWrite int64) error {
	u.metrics.emitPrefixCache(ctx, tenantID, projectID, workerID, executionID, turns, hits, misses, cacheRead, cacheWrite)
	return nil
}

// emitPrefixCache records the per-session prefix-cache hit/miss counters and
// the hit-rate gauge (D3, ADR-0009 D6). Skipped when Turns==0. Live usage
// only — never a synthesized estimate.
func (m *usageMetrics) emitPrefixCache(ctx context.Context, tenantID, projectID, workerID, executionID string, turns, hits, misses, cacheRead, cacheWrite int64) {
	if m == nil || turns <= 0 {
		return
	}
	m.ensure()
	attrs := []attribute.KeyValue{
		attribute.String("tenant", tenantID),
		attribute.String("project", projectID),
		attribute.String("execution", executionID),
		attribute.String("worker", workerID),
	}
	if m.prefixHit != nil && hits > 0 {
		m.prefixHit.Add(ctx, hits, otelmetric.WithAttributes(attrs...))
	}
	if m.prefixMiss != nil && misses > 0 {
		m.prefixMiss.Add(ctx, misses, otelmetric.WithAttributes(attrs...))
	}
	if m.prefixRate != nil {
		m.prefixRate.Record(ctx, float64(hits)/float64(turns), otelmetric.WithAttributes(attrs...))
	}
}
