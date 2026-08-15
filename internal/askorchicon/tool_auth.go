package askorchicon

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// toolListAuditEvents returns a page of audit_events rows for the
// tenant — the actor-based "who did what" trail (distinct from the
// policy-decision view). Read-only; tenant-scoped via the transaction +
// RLS, mirroring AuthService.ListAuditEvents.
func toolListAuditEvents(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		Action     string `json:"action"`
		ActorID    string `json:"actor_id"`
		TargetType string `json:"target_type"`
		TargetID   string `json:"target_id"`
		PageSize   int    `json:"page_size"`
	}
	if len(args) > 0 && string(args) != "null" {
		if err := json.Unmarshal(args, &params); err != nil {
			return nil, fmt.Errorf("invalid args: %w", err)
		}
	}
	if params.PageSize <= 0 || params.PageSize > 1000 {
		params.PageSize = 100
	}
	tenantID := tenant.FromContext(ctx)
	if tenantID == "" {
		return nil, fmt.Errorf("no tenant in context")
	}
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	rows, err := pool.ListAuditEvents(ctx, tenantID,
		params.Action, params.ActorID, params.TargetType, params.TargetID, "", params.PageSize)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		return json.RawMessage("[]"), nil
	}
	// Marshal into a readable shape: before/after as JSON text strings
	// (the raw row carries []byte, which would marshal as base64).
	type eventView struct {
		ID              string `json:"id"`
		TenantID        string `json:"tenant_id"`
		ActorIdentityID string `json:"actor_identity_id"`
		ActorType       string `json:"actor_type"`
		AuthMethod      string `json:"auth_method"`
		Action          string `json:"action"`
		TargetType      string `json:"target_type"`
		TargetID        string `json:"target_id"`
		Before          string `json:"before"`
		After           string `json:"after"`
		TraceID         string `json:"trace_id"`
		OccurredAt      string `json:"occurred_at"`
	}
	out := make([]eventView, 0, len(rows))
	for _, r := range rows {
		out = append(out, eventView{
			ID:              r.ID,
			TenantID:        r.TenantID,
			ActorIdentityID: r.ActorIdentityID,
			ActorType:       r.ActorType,
			AuthMethod:      r.AuthMethod,
			Action:          r.Action,
			TargetType:      r.TargetType,
			TargetID:        r.TargetID,
			Before:          string(r.Before),
			After:           string(r.After),
			TraceID:         r.TraceID,
			OccurredAt:      r.OccurredAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	return json.Marshal(out)
}
