package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// SessionPart is one entry of the durable per-execution session transcript
// (execution_session_parts). kind identifies the side of the conversation:
// user_message (goal / nudge / human), text, tool_use, reasoning,
// step_start, step_finish, error. payload is the raw opencode part/message
// JSON (high-fidelity enough to reconstruct the session later).
type SessionPart struct {
	ExecutionID string
	TenantID    string
	Seq         int64
	Kind        string
	Payload     []byte
	CreatedAt   time.Time
}

// AppendExecutionSessionParts inserts transcript entries idempotently
// (ON CONFLICT DO NOTHING, so a replayed batch cannot double-record).
// seq must be globally increasing per execution; the caller owns the
// counter.
func AppendExecutionSessionParts(ctx context.Context, tx pgx.Tx, tenantID string, parts []SessionPart) error {
	if len(parts) == 0 {
		return nil
	}
	const q = `INSERT INTO execution_session_parts (execution_id, tenant_id, seq, kind, payload, created_at)
		VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (tenant_id, execution_id, seq) DO NOTHING`
	for _, p := range parts {
		payload := p.Payload
		if payload == nil {
			payload = []byte("{}")
		}
		if _, err := tx.Exec(ctx, q, p.ExecutionID, tenantID, p.Seq, p.Kind, payload); err != nil {
			return fmt.Errorf("db: append session part: %w", err)
		}
	}
	return nil
}

// ListExecutionSessionParts returns the transcript for an execution in
// chronological order. beforeSeq paginates backwards (exclusive); 0 = from
// the start. limit bounds the page.
func ListExecutionSessionParts(ctx context.Context, tx pgx.Tx, tenantID, executionID string, limit int, beforeSeq int64) ([]SessionPart, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	var q string
	var args []any
	if beforeSeq > 0 {
		q = `SELECT execution_id, tenant_id, seq, kind, payload, created_at
			FROM execution_session_parts
			WHERE tenant_id = $1 AND execution_id = $2 AND seq < $3
			ORDER BY seq DESC LIMIT $4`
		args = []any{tenantID, executionID, beforeSeq, limit}
	} else {
		q = `SELECT execution_id, tenant_id, seq, kind, payload, created_at
			FROM execution_session_parts
			WHERE tenant_id = $1 AND execution_id = $2
			ORDER BY seq ASC LIMIT $3`
		args = []any{tenantID, executionID, limit}
	}
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list session parts: %w", err)
	}
	defer rows.Close()
	var out []SessionPart
	for rows.Next() {
		var p SessionPart
		var payload []byte
		if err := rows.Scan(&p.ExecutionID, &p.TenantID, &p.Seq, &p.Kind, &payload, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan session part: %w", err)
		}
		p.Payload = payload
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListExecutionSessionPartsTail returns the LAST `limit` parts of an
// execution's transcript (ORDER BY seq DESC LIMIT n), which is what the
// recovery-resumed dispatch seeds into the .orchicon/worker.recovery file.
// The caller reverses the result to restore chronological order. limit is
// clamped to 1..1000.
func ListExecutionSessionPartsTail(ctx context.Context, tx pgx.Tx, tenantID, executionID string, limit int) ([]SessionPart, error) {
	if limit <= 0 {
		limit = 1
	}
	if limit > 1000 {
		limit = 1000
	}
	const q = `SELECT execution_id, tenant_id, seq, kind, payload, created_at
		FROM execution_session_parts
		WHERE tenant_id = $1 AND execution_id = $2
		ORDER BY seq DESC LIMIT $3`
	rows, err := tx.Query(ctx, q, tenantID, executionID, limit)
	if err != nil {
		return nil, fmt.Errorf("db: list session parts tail: %w", err)
	}
	defer rows.Close()
	var out []SessionPart
	for rows.Next() {
		var p SessionPart
		var payload []byte
		if err := rows.Scan(&p.ExecutionID, &p.TenantID, &p.Seq, &p.Kind, &payload, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan session part: %w", err)
		}
		p.Payload = payload
		out = append(out, p)
	}
	return out, rows.Err()
}

// LatestToolUseParts returns the MOST RECENT `limit` tool_use parts of an
// execution's transcript (kind='tool_use' ORDER BY seq DESC LIMIT n), which
// is what the todos parser walks to find the latest todowrite payload.
// limit is clamped to 1..1000; the caller may reverse the result if it needs
// chronological order. The query is index-backed by the
// UNIQUE(tenant_id, execution_id, seq) constraint.
func LatestToolUseParts(ctx context.Context, tx pgx.Tx, tenantID, executionID string, limit int) ([]SessionPart, error) {
	if limit <= 0 {
		limit = 1000
	}
	if limit > 1000 {
		limit = 1000
	}
	const q = `SELECT execution_id, tenant_id, seq, kind, payload, created_at
		FROM execution_session_parts
		WHERE tenant_id = $1 AND execution_id = $2 AND kind = $3
		ORDER BY seq DESC LIMIT $4`
	rows, err := tx.Query(ctx, q, tenantID, executionID, SessionPartToolUse, limit)
	if err != nil {
		return nil, fmt.Errorf("db: list latest tool use parts: %w", err)
	}
	defer rows.Close()
	var out []SessionPart
	for rows.Next() {
		var p SessionPart
		var payload []byte
		if err := rows.Scan(&p.ExecutionID, &p.TenantID, &p.Seq, &p.Kind, &payload, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan session part: %w", err)
		}
		p.Payload = payload
		out = append(out, p)
	}
	return out, rows.Err()
}

// SessionPartKind constants.
const (
	SessionPartUserMessage  = "user_message"
	SessionPartText         = "text"
	SessionPartToolUse      = "tool_use"
	SessionPartReasoning    = "reasoning"
	SessionPartStepStart    = "step_start"
	SessionPartStepFinish   = "step_finish"
	SessionPartError        = "error"
	SessionPartSessionInfo  = "session_info"
	SessionPartSystemPrompt = "system_prompt"
)

// MarshalPartPayload encodes a free-form payload map as JSON bytes.
func MarshalPartPayload(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}
