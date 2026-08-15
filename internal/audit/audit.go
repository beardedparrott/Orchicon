// Package audit records actor-based audit entries for user actions.
//
// Every mutating RPC and auth action writes an audit_events row in the
// same transaction as the mutation (the transactional-outbox pattern this
// repo applies to cross-service events, AGENTS.md invariant #3): the
// caller owns the pgx.Tx via db.BeginTenantTx, calls audit.Record
// immediately before commit, and the audit row commits atomically with
// the state change — an audit row exists iff the mutation committed.
//
// Actor resolution lives in internal/auth (auth.ActorFromContext) so this
// package stays a leaf over internal/db; secrets never enter before/after
// snapshots (D7 in architecture-notes/audit-entries-mutating-rpcs.md).
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/trace"

	"github.com/beardedparrott/orchicon/internal/db"
)

// Entry is the actor/target description of one audit row. Before/After
// are JSON snapshots of the mutation; a nil slice is persisted as "{}".
type Entry struct {
	// TenantID is the tenant the row is scoped to (RLS backstop).
	TenantID string
	// ActorIdentityID is the acting identity's ULID ("" for system).
	ActorIdentityID string
	// ActorType is "user" for any real actor, "system" when none.
	ActorType string
	// AuthMethod is how the actor authenticated ("" for legacy rows).
	AuthMethod string
	// Action is the <entity>.<verb> action, e.g. "work_item.created".
	Action string
	// TargetType is the entity kind, e.g. "work_item".
	TargetType string
	// TargetID is the entity ULID (tenant id for tenant-level ops).
	TargetID string
	// Before is the pre-mutation snapshot (nil for creates).
	Before json.RawMessage
	// After is the post-mutation snapshot (nil for deletes).
	After json.RawMessage
}

// Record writes an audit row into the caller's transaction. It must be
// called in the same transaction as the mutation so the audit row commits
// atomically with it. An Entry with no tenant is an error (the RLS
// backstop requires the session variable); a Record failure must fail the
// mutation (atomicity is the point).
func Record(ctx context.Context, tx pgx.Tx, e Entry) error {
	if e.TenantID == "" {
		return errors.New("audit: tenant required")
	}
	if e.Action == "" {
		return errors.New("audit: action required")
	}
	return db.CreateAuditEvent(ctx, tx, db.AuditEventRow{
		TenantID:        e.TenantID,
		ActorIdentityID: e.ActorIdentityID,
		ActorType:       e.ActorType,
		AuthMethod:      e.AuthMethod,
		Action:          e.Action,
		TargetType:      e.TargetType,
		TargetID:        e.TargetID,
		Before:          e.Before,
		After:           e.After,
		TraceID:         TraceIDFromContext(ctx),
	})
}

// TraceIDFromContext returns the OTel trace id from the request's span
// context, or "" when there is no active span (safe under the no-op
// tracer when telemetry is off).
func TraceIDFromContext(ctx context.Context) string {
	sc := trace.SpanFromContext(ctx).SpanContext()
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

// Snapshot marshals v into a JSON snapshot for before/after. A marshal
// failure is non-fatal: it logs and returns "{}" so a bad JSON field can
// never abort a commit (atomicity of the mutation outranks completeness
// of a snapshot).
func Snapshot(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		slog.Error("audit: snapshot marshal failed", "error", err)
		return json.RawMessage("{}")
	}
	return b
}

// SnapshotStatus is a tiny helper for status-only transitions
// (publish/deprecate/assign/revoke) — before/after of just the changed
// status field, keeping the trail compact.
func SnapshotStatus(status string) json.RawMessage {
	return Snapshot(struct {
		Status string `json:"status"`
	}{Status: status})
}
