package outbox

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/eventbus"
	"github.com/beardedparrott/orchicon/internal/telemetry"
	otelmetric "go.opentelemetry.io/otel/metric"
)

// Outbox lag alert thresholds. A relay that stops making progress used to
// accumulate silently for six weeks (8.4M rows / 8.4 GB, see
// docs/outbox-retention.md). Depth alone is ambiguous — a busy relay with a
// short backlog is healthy — so the age of the OLDEST unpublished row is the
// real stall signal. Either threshold breached on one poll tick raises the
// alert (metric counter + structured WARN), so a stalled relay is visible
// within one 5s reporting tick instead of weeks.
const (
	// outboxLagAlertDepth is the unpublished-row count above which the relay
	// reports a stall alert.
	outboxLagAlertDepth = 1000
	// outboxLagAlertAgeSec is the age (seconds) of the oldest unpublished row
	// above which the relay reports a stall alert.
	outboxLagAlertAgeSec = 300
)

// outboxPruneMaxBatchesPerTick bounds how many bounded delete batches one
// retention pass runs, so a pass cannot monopolise WAL or hold locks for an
// unbounded time even when millions of rows are eligible.
const outboxPruneMaxBatchesPerTick = 10

// Relay polls the outbox table for unpublished rows and publishes them
// to NATS via the eventbus Publisher (docs/09_Database_Schema.md §6).
// Delivery is at-least-once; JetStream deduplication on event_id makes
// concurrent relays safe. The relay marks rows published only after a
// successful publish.
//
// Phase 3 adds lag metrics: orchicon_outbox_lag (gauge — relay health,
// docs/08 §5.2) and orchicon_outbox_published_total (counter). Retention
// (docs/outbox-retention.md) adds a scheduled prune of PUBLISHED rows older
// than N days plus the orchicon_outbox_oldest_unpublished_seconds gauge and
// the orchicon_outbox_lag_alerts_total counter.
type Relay struct {
	pool      *db.Pool
	publisher eventbus.Publisher
	log       *slog.Logger
	batchSize int
	interval  time.Duration

	// Retention: published rows older than retention are pruned every
	// pruneInterval in batches of pruneBatch. retention <= 0 disables pruning.
	retention     time.Duration
	pruneBatch    int
	pruneInterval time.Duration

	// Metrics
	lagGauge     otelmetric.Int64ObservableGauge
	oldestGauge  otelmetric.Int64ObservableGauge
	alertCtr     otelmetric.Int64Counter
	publishedCtr atomic.Int64
	lagVal       atomic.Int64
	oldestSec    atomic.Int64
	lagAlerts    atomic.Int64
}

// RelayOption configures a Relay.
type RelayOption func(*Relay)

// WithBatchSize sets the number of outbox rows polled per tick.
func WithBatchSize(n int) RelayOption {
	return func(r *Relay) { r.batchSize = n }
}

// WithInterval sets the poll interval.
func WithInterval(d time.Duration) RelayOption {
	return func(r *Relay) { r.interval = d }
}

// WithRetention sets the retention window for published outbox rows. A
// non-positive duration disables pruning.
func WithRetention(d time.Duration) RelayOption {
	return func(r *Relay) { r.retention = d }
}

// WithPruneBatch bounds the rows deleted per prune statement (clamped to
// [1, 100000] at execution time).
func WithPruneBatch(n int) RelayOption {
	return func(r *Relay) { r.pruneBatch = n }
}

// WithPruneInterval sets how often a retention pass runs.
func WithPruneInterval(d time.Duration) RelayOption {
	return func(r *Relay) { r.pruneInterval = d }
}

// NewRelay constructs an outbox relay.
func NewRelay(pool *db.Pool, pub eventbus.Publisher, log *slog.Logger, opts ...RelayOption) *Relay {
	r := &Relay{
		pool:          pool,
		publisher:     pub,
		log:           log,
		batchSize:     100,
		interval:      500 * time.Millisecond,
		pruneBatch:    10000,
		pruneInterval: time.Hour,
		retention:     0, // disabled unless explicitly configured
	}
	for _, o := range opts {
		o(r)
	}

	// Register lag gauges (orchicon_outbox_lag — docs/08 §5.2 — and
	// orchicon_outbox_oldest_unpublished_seconds, the relay stall detector).
	// No unit on either: the Prometheus exporter appends "_<unit>" to metric
	// names, which would break the canonical names the dashboards query.
	gauge, err := telemetry.Meter().Int64ObservableGauge(
		"orchicon_outbox_lag",
		otelmetric.WithDescription("Number of unpublished outbox rows (relay health)"),
	)
	if err == nil {
		r.lagGauge = gauge
	}
	oldest, err := telemetry.Meter().Int64ObservableGauge(
		"orchicon_outbox_oldest_unpublished_seconds",
		otelmetric.WithDescription("Age in seconds of the oldest unpublished outbox row (relay stall detector)"),
	)
	if err == nil {
		r.oldestGauge = oldest
	}
	if r.lagGauge != nil && r.oldestGauge != nil {
		// One callback feeds both gauges so depth and age are observed from
		// the same reporting tick and cannot disagree.
		_, _ = telemetry.Meter().RegisterCallback(
			func(ctx context.Context, o otelmetric.Observer) error {
				o.ObserveInt64(r.lagGauge, r.lagVal.Load())
				o.ObserveInt64(r.oldestGauge, r.oldestSec.Load())
				return nil
			},
			r.lagGauge, r.oldestGauge,
		)
	}

	ctr, err := telemetry.Meter().Int64Counter(
		"orchicon_outbox_lag_alerts_total",
		otelmetric.WithDescription("Count of poll ticks where outbox relay lag breached the alert threshold (depth or oldest-row age)"),
	)
	if err == nil {
		r.alertCtr = ctr
	}

	return r
}

// Run polls the outbox until ctx is cancelled. It is safe to run
// multiple relays concurrently; JetStream dedup on event_id prevents
// duplicate delivery (docs/09 §6).
func (r *Relay) Run(ctx context.Context) error {
	r.log.Info("outbox relay started", "batch_size", r.batchSize, "interval", r.interval)
	t := time.NewTicker(r.interval)
	defer t.Stop()

	// Lag reporter: periodically count unpublished rows and record the age
	// of the oldest one.
	go r.lagReporter(ctx)
	// Retention: periodically prune published rows older than the window.
	go r.pruneLoop(ctx)

	for {
		select {
		case <-ctx.Done():
			r.log.Info("outbox relay stopped")
			return nil
		case <-t.C:
			if err := r.tick(ctx); err != nil {
				r.log.Error("outbox relay tick failed", "error", err)
			}
		}
	}
}

func (r *Relay) tick(ctx context.Context) error {
	rows, err := r.pool.PollOutbox(ctx, r.batchSize)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	publishedIDs := make([]string, 0, len(rows))
	for _, row := range rows {
		subject := eventbus.SubjectFor(row.AggregateType, row.EventType)
		if err := r.publisher.Publish(ctx, subject, row.EventID, row.Payload); err != nil {
			// Stop processing this batch on first failure; the row stays
			// unpublished and will be retried on the next tick. Rows
			// already published in this batch are marked below.
			r.log.Error("outbox publish failed",
				"event_id", row.EventID, "subject", subject, "error", err)
			break
		}
		publishedIDs = append(publishedIDs, row.ID)
	}
	if len(publishedIDs) == 0 {
		return nil
	}
	if err := r.pool.MarkPublished(ctx, publishedIDs); err != nil {
		return err
	}
	r.publishedCtr.Add(int64(len(publishedIDs)))
	r.log.Info("outbox published", "count", len(publishedIDs), "total", r.publishedCtr.Load())
	return nil
}

// lagReporter periodically counts unpublished outbox rows, records the age of
// the oldest one, and raises the stall alert when either threshold is
// breached. The count is an approximation (not transactionally consistent
// with the relay tick) but is sufficient for alerting (docs/08 §8:
// "orchicon_outbox_lag metric alerts before it harms correctness").
func (r *Relay) lagReporter(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.reportLagOnce(ctx)
		}
	}
}

// reportLagOnce samples outbox depth + oldest-unpublished age, stores them for
// the gauges, and raises the stall alert when either threshold is breached.
// Extracted from the reporter loop so tests can drive exactly one sample.
func (r *Relay) reportLagOnce(ctx context.Context) {
	count, oldest, err := r.pool.CountAndOldestUnpublished(ctx)
	if err != nil {
		r.log.Warn("failed to count unpublished outbox rows", "error", err)
		return
	}
	r.lagVal.Store(count)
	var ageSec int64
	if oldest != nil {
		ageSec = int64(time.Since(*oldest).Seconds())
		if ageSec < 0 {
			ageSec = 0
		}
	}
	r.oldestSec.Store(ageSec)
	if count > outboxLagAlertDepth || ageSec > outboxLagAlertAgeSec {
		r.lagAlerts.Add(1)
		if r.alertCtr != nil {
			r.alertCtr.Add(ctx, 1)
		}
		r.log.Warn("outbox relay lag: alert threshold exceeded",
			"unpublished", count,
			"oldest_age_seconds", ageSec,
			"depth_threshold", outboxLagAlertDepth,
			"age_threshold_seconds", outboxLagAlertAgeSec)
	}
}

// pruneLoop runs the retention pass every pruneInterval until ctx is
// cancelled. It is a no-op (and logs nothing) when retention is disabled.
func (r *Relay) pruneLoop(ctx context.Context) {
	if r.retention <= 0 {
		return
	}
	interval := r.pruneInterval
	if interval <= 0 {
		interval = time.Hour
	}
	r.log.Info("outbox retention prune enabled",
		"retention_days", int(r.retention.Hours()/24),
		"batch", r.pruneBatch, "interval", interval.String())
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := r.PruneOnce(ctx); err != nil {
				r.log.Warn("outbox prune failed", "error", err)
			}
		}
	}
}

// PruneOnce runs one bounded retention pass: up to
// outboxPruneMaxBatchesPerTick delete statements of at most r.pruneBatch rows
// each, stopping early when a batch comes back short (no more eligible rows).
// It returns the total rows deleted. It is exported so tests and operators can
// drive a single pass deterministically. A return of (0, nil) means either
// retention is disabled or nothing was eligible.
func (r *Relay) PruneOnce(ctx context.Context) (int64, error) {
	if r.retention <= 0 {
		return 0, nil
	}
	batch := r.pruneBatch
	if batch <= 0 {
		batch = 10000
	}
	cutoff := time.Now().UTC().Add(-r.retention)
	var total int64
	for i := 0; i < outboxPruneMaxBatchesPerTick; i++ {
		n, err := r.pool.PrunePublishedOutbox(ctx, cutoff, batch)
		if err != nil {
			return total, err
		}
		total += n
		if n < int64(batch) {
			break
		}
	}
	if total > 0 {
		r.log.Info("outbox prune",
			"rows", total,
			"retention_days", int(r.retention.Hours()/24))
	}
	return total, nil
}
