package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// EventSubscriptionRow is the data-access shape of an event_subscriptions
// table row. SecretHash is the only secret material stored; the plaintext
// secret is shown once on create and never persisted (AGENTS.md security).
type EventSubscriptionRow struct {
	ID         string
	TenantID   string
	Name       string
	TargetURL  string
	EventFilter string
	Scope      string
	ScopeRef   string
	SecretHint string
	SecretHash string
	MaxRetries int
	Status     string
	Version    int
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// CreateSubscription inserts a new webhook subscription.
func CreateSubscription(ctx context.Context, tx pgx.Tx, r EventSubscriptionRow) (EventSubscriptionRow, error) {
	if r.ID == "" {
		r.ID = NewID()
	}
	if r.EventFilter == "" {
		r.EventFilter = "*"
	}
	if r.Scope == "" {
		r.Scope = "tenant"
	}
	if r.MaxRetries == 0 {
		r.MaxRetries = 5
	}
	if r.Status == "" {
		r.Status = "active"
	}
	const q = `INSERT INTO event_subscriptions (id, tenant_id, name, target_url, event_filter,
		scope, scope_ref, secret_hint, secret_hash, max_retries, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING id, tenant_id, name, target_url, event_filter, scope, scope_ref,
			secret_hint, secret_hash, max_retries, status, version, created_at, updated_at`
	err := tx.QueryRow(ctx, q, r.ID, r.TenantID, r.Name, r.TargetURL, r.EventFilter,
		r.Scope, r.ScopeRef, r.SecretHint, r.SecretHash, r.MaxRetries, r.Status).Scan(
		&r.ID, &r.TenantID, &r.Name, &r.TargetURL, &r.EventFilter, &r.Scope, &r.ScopeRef,
		&r.SecretHint, &r.SecretHash, &r.MaxRetries, &r.Status, &r.Version, &r.CreatedAt, &r.UpdatedAt,
	)
	if err != nil {
		return EventSubscriptionRow{}, fmt.Errorf("db: create subscription: %w", err)
	}
	return r, nil
}

// GetSubscription fetches a single subscription by id within the tenant scope.
func GetSubscription(ctx context.Context, tx pgx.Tx, tenantID, id string) (EventSubscriptionRow, error) {
	const q = `SELECT id, tenant_id, name, target_url, event_filter, scope, scope_ref,
		secret_hint, secret_hash, max_retries, status, version, created_at, updated_at
		FROM event_subscriptions WHERE id = $1 AND tenant_id = $2`
	var r EventSubscriptionRow
	err := tx.QueryRow(ctx, q, id, tenantID).Scan(
		&r.ID, &r.TenantID, &r.Name, &r.TargetURL, &r.EventFilter, &r.Scope, &r.ScopeRef,
		&r.SecretHint, &r.SecretHash, &r.MaxRetries, &r.Status, &r.Version, &r.CreatedAt, &r.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return EventSubscriptionRow{}, ErrNotFound
	}
	if err != nil {
		return EventSubscriptionRow{}, fmt.Errorf("db: get subscription: %w", err)
	}
	return r, nil
}

// ListSubscriptions returns a page of subscriptions for the tenant.
func ListSubscriptions(ctx context.Context, tx pgx.Tx, tenantID string, pageSize int, afterID string) ([]EventSubscriptionRow, error) {
	if pageSize <= 0 || pageSize > 1000 {
		pageSize = 100
	}
	const q = `SELECT id, tenant_id, name, target_url, event_filter, scope, scope_ref,
		secret_hint, secret_hash, max_retries, status, version, created_at, updated_at
		FROM event_subscriptions
		WHERE tenant_id = $1 AND ($2 = '' OR id > $2) AND status <> 'deleted'
		ORDER BY id ASC LIMIT $3`
	rows, err := tx.Query(ctx, q, tenantID, afterID, pageSize)
	if err != nil {
		return nil, fmt.Errorf("db: list subscriptions: %w", err)
	}
	defer rows.Close()
	var out []EventSubscriptionRow
	for rows.Next() {
		var r EventSubscriptionRow
		if err := rows.Scan(&r.ID, &r.TenantID, &r.Name, &r.TargetURL, &r.EventFilter, &r.Scope,
			&r.ScopeRef, &r.SecretHint, &r.SecretHash, &r.MaxRetries, &r.Status, &r.Version,
			&r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("db: scan subscription: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateSubscriptionFields is a partial update applied with optimistic
// concurrency.
type UpdateSubscriptionFields struct {
	TargetURL   *string
	EventFilter *string
	Status      *string
	MaxRetries  *int
}

// UpdateSubscription applies a partial update with optimistic concurrency.
func UpdateSubscription(ctx context.Context, tx pgx.Tx, tenantID, id string, expectedVersion int, f UpdateSubscriptionFields) (EventSubscriptionRow, error) {
	q := `UPDATE event_subscriptions SET updated_at = now(), version = version + 1`
	args := []any{tenantID, id, expectedVersion}
	setIdx := len(args) + 1
	if f.TargetURL != nil {
		q += fmt.Sprintf(`, target_url = $%d`, setIdx)
		args = append(args, *f.TargetURL)
		setIdx++
	}
	if f.EventFilter != nil {
		q += fmt.Sprintf(`, event_filter = $%d`, setIdx)
		args = append(args, *f.EventFilter)
		setIdx++
	}
	if f.Status != nil {
		q += fmt.Sprintf(`, status = $%d`, setIdx)
		args = append(args, *f.Status)
		setIdx++
	}
	if f.MaxRetries != nil {
		q += fmt.Sprintf(`, max_retries = $%d`, setIdx)
		args = append(args, *f.MaxRetries)
		setIdx++
	}
	q += ` WHERE tenant_id = $1 AND id = $2 AND version = $3`
	q += ` RETURNING id, tenant_id, name, target_url, event_filter, scope, scope_ref,
		secret_hint, secret_hash, max_retries, status, version, created_at, updated_at`
	var r EventSubscriptionRow
	err := tx.QueryRow(ctx, q, args...).Scan(
		&r.ID, &r.TenantID, &r.Name, &r.TargetURL, &r.EventFilter, &r.Scope, &r.ScopeRef,
		&r.SecretHint, &r.SecretHash, &r.MaxRetries, &r.Status, &r.Version, &r.CreatedAt, &r.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return EventSubscriptionRow{}, ErrNotFound
	}
	if err != nil {
		return EventSubscriptionRow{}, fmt.Errorf("db: update subscription: %w", err)
	}
	return r, nil
}

// SoftDeleteSubscription marks a subscription deleted (soft delete so
// historical deliveries remain queryable).
func SoftDeleteSubscription(ctx context.Context, tx pgx.Tx, tenantID, id string, expectedVersion int) error {
	const q = `UPDATE event_subscriptions SET status = 'deleted', updated_at = now(), version = version + 1
		WHERE tenant_id = $1 AND id = $2 AND version = $3`
	ct, err := tx.Exec(ctx, q, tenantID, id, expectedVersion)
	if err != nil {
		return fmt.Errorf("db: delete subscription: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListActiveSubscriptions returns all active subscriptions matching the
// given event type, across all tenants. The dispatcher reads across
// tenants (it runs outside RLS — like the outbox relay). The event
// filter is a glob matched server-side for simplicity in v0.1.
func ListActiveSubscriptions(ctx context.Context, p *Pool, eventType string) ([]EventSubscriptionRow, error) {
	const q = `SELECT id, tenant_id, name, target_url, event_filter, scope, scope_ref,
		secret_hint, secret_hash, max_retries, status, version, created_at, updated_at
		FROM event_subscriptions
		WHERE status = 'active'`
	rows, err := p.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: list active subscriptions: %w", err)
	}
	defer rows.Close()
	var out []EventSubscriptionRow
	for rows.Next() {
		var r EventSubscriptionRow
		if err := rows.Scan(&r.ID, &r.TenantID, &r.Name, &r.TargetURL, &r.EventFilter, &r.Scope,
			&r.ScopeRef, &r.SecretHint, &r.SecretHash, &r.MaxRetries, &r.Status, &r.Version,
			&r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("db: scan active subscription: %w", err)
		}
		if matchGlob(r.EventFilter, eventType) {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

// matchGlob is a minimal glob matcher for event filters. "*" matches
// everything; "project.*" matches "project.created" but not
// "worker.published"; "project.created" matches exactly.
func matchGlob(filter, eventType string) bool {
	if filter == "" || filter == "*" {
		return true
	}
	if filter == eventType {
		return true
	}
	if len(filter) > 1 && filter[len(filter)-2:] == ".*" {
		prefix := filter[:len(filter)-2]
		return eventType == prefix || (len(eventType) > len(prefix) && eventType[:len(prefix)] == prefix && eventType[len(prefix)] == '.')
	}
	return false
}

// --- webhook_deliveries ----------------------------------------------------

// WebhookDeliveryRow is the data-access shape of a webhook_deliveries
// table row.
type WebhookDeliveryRow struct {
	ID             string
	TenantID       string
	SubscriptionID string
	EventID        string
	EventType      string
	Payload        []byte
	Attempt        int
	StatusCode     int
	Status         string
	Error          string
	NextAttemptAt  *time.Time
	OccurredAt     time.Time
}

// CreateDelivery inserts a new delivery attempt row.
func CreateDelivery(ctx context.Context, p *Pool, r WebhookDeliveryRow) (WebhookDeliveryRow, error) {
	if r.ID == "" {
		r.ID = NewID()
	}
	if r.Status == "" {
		r.Status = "retrying"
	}
	const q = `INSERT INTO webhook_deliveries (id, tenant_id, subscription_id, event_id,
		event_type, payload, attempt, status_code, status, error, next_attempt_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING id, tenant_id, subscription_id, event_id, event_type, payload, attempt,
			status_code, status, error, next_attempt_at, occurred_at`
	var nextAny any
	if r.NextAttemptAt != nil {
		nextAny = *r.NextAttemptAt
	}
	err := p.QueryRow(ctx, q, r.ID, r.TenantID, r.SubscriptionID, r.EventID, r.EventType,
		r.Payload, r.Attempt, r.StatusCode, r.Status, r.Error, nextAny).Scan(
		&r.ID, &r.TenantID, &r.SubscriptionID, &r.EventID, &r.EventType, &r.Payload, &r.Attempt,
		&r.StatusCode, &r.Status, &r.Error, &r.NextAttemptAt, &r.OccurredAt,
	)
	if err != nil {
		return WebhookDeliveryRow{}, fmt.Errorf("db: create delivery: %w", err)
	}
	return r, nil
}

// UpdateDeliveryResult records the outcome of a delivery attempt and
// schedules the next attempt if retrying.
func UpdateDeliveryResult(ctx context.Context, p *Pool, id string, statusCode int, status, errMsg string, nextAttempt *time.Time) error {
	var nextAny any
	if nextAttempt != nil {
		nextAny = *nextAttempt
	}
	const q = `UPDATE webhook_deliveries SET status_code = $1, status = $2, error = $3,
		next_attempt_at = $4 WHERE id = $5`
	_, err := p.Exec(ctx, q, statusCode, status, errMsg, nextAny, id)
	if err != nil {
		return fmt.Errorf("db: update delivery: %w", err)
	}
	return nil
}

// ListDeliveries returns a page of deliveries, optionally filtered by
// subscription and status.
func ListDeliveries(ctx context.Context, tx pgx.Tx, tenantID, subscriptionID, status string, pageSize int, afterID string) ([]WebhookDeliveryRow, error) {
	if pageSize <= 0 || pageSize > 1000 {
		pageSize = 100
	}
	q := `SELECT id, tenant_id, subscription_id, event_id, event_type, payload, attempt,
		status_code, status, error, next_attempt_at, occurred_at
		FROM webhook_deliveries
		WHERE tenant_id = $1 AND ($2 = '' OR subscription_id = $2) AND ($3 = '' OR status = $3)
			AND ($4 = '' OR id > $4)
		ORDER BY id ASC LIMIT $5`
	rows, err := tx.Query(ctx, q, tenantID, subscriptionID, status, afterID, pageSize)
	if err != nil {
		return nil, fmt.Errorf("db: list deliveries: %w", err)
	}
	defer rows.Close()
	var out []WebhookDeliveryRow
	for rows.Next() {
		var r WebhookDeliveryRow
		if err := rows.Scan(&r.ID, &r.TenantID, &r.SubscriptionID, &r.EventID, &r.EventType,
			&r.Payload, &r.Attempt, &r.StatusCode, &r.Status, &r.Error, &r.NextAttemptAt,
			&r.OccurredAt); err != nil {
			return nil, fmt.Errorf("db: scan delivery: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetDelivery fetches a single delivery by id within the tenant scope.
func GetDelivery(ctx context.Context, tx pgx.Tx, tenantID, id string) (WebhookDeliveryRow, error) {
	const q = `SELECT id, tenant_id, subscription_id, event_id, event_type, payload, attempt,
		status_code, status, error, next_attempt_at, occurred_at
		FROM webhook_deliveries WHERE id = $1 AND tenant_id = $2`
	var r WebhookDeliveryRow
	err := tx.QueryRow(ctx, q, id, tenantID).Scan(
		&r.ID, &r.TenantID, &r.SubscriptionID, &r.EventID, &r.EventType, &r.Payload, &r.Attempt,
		&r.StatusCode, &r.Status, &r.Error, &r.NextAttemptAt, &r.OccurredAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WebhookDeliveryRow{}, ErrNotFound
	}
	if err != nil {
		return WebhookDeliveryRow{}, fmt.Errorf("db: get delivery: %w", err)
	}
	return r, nil
}

// ReenqueueDelivery creates a fresh delivery attempt for replay. The
// original delivery's status is left intact (audit trail preserved).
func ReenqueueDelivery(ctx context.Context, p *Pool, r WebhookDeliveryRow) (WebhookDeliveryRow, error) {
	r.ID = NewID()
	r.Attempt = 0
	r.StatusCode = 0
	r.Status = "retrying"
	r.Error = ""
	r.OccurredAt = time.Now().UTC()
	return CreateDelivery(ctx, p, r)
}

// PayloadEnvelope is the JSON envelope POSTed to webhook targets. It
// carries enough context for the receiver to identify and verify the
// delivery.
type PayloadEnvelope struct {
	EventID   string          `json:"event_id"`
	EventType string          `json:"event_type"`
	TenantID  string          `json:"tenant_id"`
	Subject   string          `json:"subject"`
	OccurredAt time.Time      `json:"occurred_at"`
	Data      json.RawMessage `json:"data"`
}
