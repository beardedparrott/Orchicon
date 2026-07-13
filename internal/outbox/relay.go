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

// Relay polls the outbox table for unpublished rows and publishes them
// to NATS via the eventbus Publisher (docs/09_Database_Schema.md §6).
// Delivery is at-least-once; JetStream deduplication on event_id makes
// concurrent relays safe. The relay marks rows published only after a
// successful publish.
//
// Phase 3 adds lag metrics: orchicon_outbox_lag (gauge — relay health,
// docs/08 §5.2) and orchicon_outbox_published_total (counter).
type Relay struct {
	pool      *db.Pool
	publisher eventbus.Publisher
	log       *slog.Logger
	batchSize int
	interval  time.Duration

	// Metrics
	lagGauge      otelmetric.Int64ObservableGauge
	publishedCtr   atomic.Int64
	lagVal         atomic.Int64
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

// NewRelay constructs an outbox relay.
func NewRelay(pool *db.Pool, pub eventbus.Publisher, log *slog.Logger, opts ...RelayOption) *Relay {
	r := &Relay{
		pool:      pool,
		publisher: pub,
		log:       log,
		batchSize: 100,
		interval:  500 * time.Millisecond,
	}
	for _, o := range opts {
		o(r)
	}

	// Register lag gauge (orchicon_outbox_lag — docs/08 §5.2).
	gauge, err := telemetry.Meter().Int64ObservableGauge(
		"orchicon_outbox_lag",
		otelmetric.WithDescription("Number of unpublished outbox rows (relay health)"),
		otelmetric.WithUnit("rows"),
	)
	if err == nil {
		r.lagGauge = gauge
		_, _ = telemetry.Meter().RegisterCallback(
			func(ctx context.Context, o otelmetric.Observer) error {
				o.ObserveInt64(r.lagGauge, r.lagVal.Load())
				return nil
			},
			r.lagGauge,
		)
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

	// Lag reporter: periodically count unpublished rows.
	go r.lagReporter(ctx)

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

// lagReporter periodically counts unpublished outbox rows and updates the
// lag gauge. The count is an approximation (not transactionally
// consistent with the relay tick) but is sufficient for alerting
// (docs/08 §8: "orchicon_outbox_lag metric alerts before it harms
// correctness").
func (r *Relay) lagReporter(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			count, err := r.pool.CountUnpublished(ctx)
			if err != nil {
				r.log.Warn("failed to count unpublished outbox rows", "error", err)
				continue
			}
			r.lagVal.Store(count)
		}
	}
}
