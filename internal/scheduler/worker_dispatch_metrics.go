package scheduler

import (
	"context"
	"sync"

	"github.com/beardedparrott/orchicon/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
)

// workerDispatchMetricsT mirrors recoverySeedMetrics' lazy-init pattern
// (internal/scheduler/recovery_seed.go) for the per-worker concurrency
// gate: a single counter, labelled by worker, so a deferral is
// measurable and attributable, not only visible in a step run's result.
type workerDispatchMetricsT struct {
	initOnce sync.Once
	deferred otelmetric.Int64Counter
}

func (m *workerDispatchMetricsT) ensure() {
	m.initOnce.Do(func() {
		if c, err := telemetry.Meter().Int64Counter("orchicon_worker_dispatch_deferred",
			otelmetric.WithDescription("Dispatches deferred by the per-worker concurrency gate")); err == nil {
			m.deferred = c
		}
	})
}

func (m *workerDispatchMetricsT) recordDeferred(workerID string) {
	m.ensure()
	if m.deferred != nil {
		m.deferred.Add(context.Background(), 1, otelmetric.WithAttributes(attribute.String("worker_id", workerID)))
	}
}

var workerDispatchMetrics = &workerDispatchMetricsT{}
