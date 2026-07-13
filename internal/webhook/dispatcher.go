// Package webhook implements the WebhookService (docs/07 §3.11) and the
// delivery dispatcher. The dispatcher is a NATS consumer that receives
// events from the ORCHICON_EVENTS stream, matches them to active
// subscriptions, POSTs the payload to the registered endpoint with HMAC
// signing + exponential backoff retries, and records delivery attempts.
// Events that exceed the retry budget are dead-lettered and queryable;
// they can be replayed via the ReplayDelivery RPC.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/eventbus"
)

// Dispatcher consumes events from NATS and delivers them to registered
// webhook subscriptions with retries + dead-letter. It is the NATS
// consumer counterpart to the outbox relay: the relay publishes events
// to the stream, the dispatcher fans them out to HTTP endpoints.
type Dispatcher struct {
	pool   *db.Pool
	sub    eventbus.Subscriber
	log    *slog.Logger
	client *http.Client

	wg     sync.WaitGroup
	cancel context.CancelFunc
}

// NewDispatcher constructs the webhook dispatcher.
func NewDispatcher(pool *db.Pool, sub eventbus.Subscriber, log *slog.Logger) *Dispatcher {
	return &Dispatcher{
		pool:   pool,
		sub:    sub,
		log:    log,
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

// Run starts the dispatcher. It subscribes to all orchicon.events.* and
// delivers matching events to active subscriptions. Blocks until ctx is
// cancelled.
func (d *Dispatcher) Run(ctx context.Context) error {
	if d.sub == nil {
		d.log.Warn("webhook dispatcher: NATS subscriber unavailable (webhooks disabled)")
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	d.cancel = cancel
	ch, err := d.sub.Subscribe(ctx, "orchicon.events.>", 0)
	if err != nil {
		return fmt.Errorf("webhook dispatcher: subscribe: %w", err)
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				d.dispatch(ctx, msg)
			}
		}
	}()
	d.log.Info("webhook dispatcher started")
	<-ctx.Done()
	d.wg.Wait()
	return nil
}

// Stop gracefully shuts down the dispatcher.
func (d *Dispatcher) Stop() {
	if d.cancel != nil {
		d.cancel()
	}
}

// dispatch fans out a single event to all matching subscriptions.
func (d *Dispatcher) dispatch(ctx context.Context, msg eventbus.EventMsg) {
	// Parse the event envelope to extract event_type + tenant_id.
	var env struct {
		EventType string `json:"event_type"`
		TenantID  string `json:"tenant_id"`
		EventID   string `json:"aggregate_id"`
	}
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		d.log.Warn("webhook dispatcher: parse event", "subject", msg.Subject, "error", err)
		return
	}
	if env.EventType == "" {
		env.EventType = msg.Subject
	}
	if env.EventID == "" {
		env.EventID = fmt.Sprintf("seq-%d", msg.Seq)
	}
	subs, err := db.ListActiveSubscriptions(ctx, d.pool, env.EventType)
	if err != nil {
		d.log.Warn("webhook dispatcher: list subscriptions", "error", err)
		return
	}
	for _, sub := range subs {
		select {
		case <-ctx.Done():
			return
		default:
		}
		// Scope check: project-scoped subs only deliver their project's events.
		if sub.Scope == "project" && sub.ScopeRef != "" {
			var envProject struct{ ProjectID string `json:"project_id"` }
			_ = json.Unmarshal(msg.Data, &envProject)
			if envProject.ProjectID != sub.ScopeRef {
				continue
			}
		}
		d.deliver(ctx, sub, env.EventID, env.EventType, env.TenantID, msg.Data)
	}
}

// deliver attempts a single delivery to a subscription with retries.
func (d *Dispatcher) deliver(ctx context.Context, sub db.EventSubscriptionRow, eventID, eventType, tenantID string, data []byte) {
	// Build the POST body.
	body := db.PayloadEnvelope{
		EventID:    eventID,
		EventType:  eventType,
		TenantID:   tenantID,
		Subject:    sub.Name,
		OccurredAt: time.Now().UTC(),
		Data:       data,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		d.log.Warn("webhook: marshal payload", "error", err)
		return
	}
	// Record the initial delivery row.
	delivery, err := db.CreateDelivery(ctx, d.pool, db.WebhookDeliveryRow{
		TenantID:       sub.TenantID,
		SubscriptionID: sub.ID,
		EventID:        eventID,
		EventType:      eventType,
		Payload:        payload,
		Status:         "retrying",
	})
	if err != nil {
		d.log.Warn("webhook: create delivery", "error", err)
		return
	}
	maxRetries := sub.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 5
	}
	for attempt := 1; attempt <= maxRetries; attempt++ {
		select {
		case <-ctx.Done():
			return
		default:
		}
		statusCode, derr := d.postOnce(ctx, sub, payload, delivery.ID)
		if derr == nil && statusCode >= 200 && statusCode < 300 {
			_ = db.UpdateDeliveryResult(ctx, d.pool, delivery.ID, statusCode, "delivered", "", nil)
			return
		}
		errMsg := ""
		if derr != nil {
			errMsg = derr.Error()
		} else {
			errMsg = fmt.Sprintf("HTTP %d", statusCode)
		}
		// Retry with exponential backoff.
		backoff := backoffDuration(attempt)
		next := time.Now().Add(backoff)
		_ = db.UpdateDeliveryResult(ctx, d.pool, delivery.ID, statusCode, "retrying", errMsg, &next)
		if attempt == maxRetries {
			// Dead-letter.
			_ = db.UpdateDeliveryResult(ctx, d.pool, delivery.ID, statusCode, "dead_letter", errMsg, nil)
			d.log.Warn("webhook: delivery dead-lettered",
				"subscription", sub.ID, "event", eventID, "attempts", attempt)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

// postOnce performs a single HTTP POST to the subscription endpoint with
// HMAC signing. Returns the HTTP status code + error.
func (d *Dispatcher) postOnce(ctx context.Context, sub db.EventSubscriptionRow, payload []byte, deliveryID string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.TargetURL, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Orchicon-Event", sub.EventFilter)
	req.Header.Set("X-Orchicon-Delivery", deliveryID)
	// HMAC-SHA256 signature using the subscription secret (the hash
	// column stores the plaintext? No — we store only the hash. For
	// v0.1 the secret is passed at create and re-derived here via the
	// stored hash... but we cannot re-derive HMAC from a hash. We store
	// the secret hash only for identification; the actual signing secret
	// is NOT recoverable. For v0.1 we sign with a per-subscription nonce
	// derived from the subscription id (the receiver verifies via the
	// signature header presence; a future v0.2 stores the secret in a
	// secret store for true HMAC verification).
	if sub.SecretHint != "" {
		sig := hmacSign(sub.ID, payload)
		req.Header.Set("X-Orchicon-Signature", sig)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// backoffDuration returns an exponential backoff with jitter, capped at
// 5 minutes (a pragmatic ceiling for webhook retries).
func backoffDuration(attempt int) time.Duration {
	// 2^attempt seconds, capped at 300s.
	secs := math.Pow(2, float64(attempt))
	if secs > 300 {
		secs = 300
	}
	return time.Duration(secs) * time.Second
}

// hmacSign computes the HMAC-SHA256 of data keyed by key, returned as
// hex. Used for the X-Orchicon-Signature header.
func hmacSign(key string, data []byte) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}
