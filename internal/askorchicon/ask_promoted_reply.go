package askorchicon

// ask_promoted_reply.go — answering a card that came from an MCP-served ask.

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/beardedparrott/orchicon/internal/db"
)

// findPromotedAsk locates an ask by ID in the conversation's transcript and reports whether it is
// still awaiting an answer, plus the row it lives on.
func (s *Service) findPromotedAsk(ctx context.Context, tenantID, convID, askID string) (msgID string, awaiting bool, ok bool) {
	if s.pool == nil || askID == "" {
		return "", false, false
	}
	tx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return "", false, false
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		id      string
		raw     []byte
		results []byte
	)
	// tool_calls @> '[{"id":"<askID>"}]' is an exact match on the array element's id — no LIKE, no
	// substring, and no dependence on ordering or recency.
	err = tx.Tx.QueryRow(ctx, `
		select id, coalesce(tool_calls, '[]'::jsonb), coalesce(tool_results, '[]'::jsonb)
		  from ask_orchicon_messages
		 where tenant_id = $1
		   and conversation_id = $2
		   and tool_calls @> jsonb_build_array(jsonb_build_object('id', $3::text))
		 order by created_at desc
		 limit 1`, tenantID, convID, askID).Scan(&id, &raw, &results)
	if err != nil {
		return "", false, false
	}

	var calls []struct {
		ID           string `json:"id"`
		FunctionName string `json:"function_name"`
	}
	if err := json.Unmarshal(raw, &calls); err != nil {
		return "", false, false
	}
	isAsk := false
	for _, c := range calls {
		if c.ID == askID && isAskToolName(c.FunctionName) {
			isAsk = true
			break
		}
	}
	if !isAsk {
		// The id belongs to some OTHER tool: it is not an ask this endpoint may answer.
		return "", false, false
	}

	var rs []struct {
		ToolCallID string `json:"tool_call_id"`
		Output     string `json:"output"`
	}
	_ = json.Unmarshal(results, &rs)
	for _, r := range rs {
		if r.ToolCallID == askID {
			// A result exists.
			return id, IsAwaitingAnswer(r.Output) || strings.TrimSpace(r.Output) == "", true
		}
	}
	// No result at all: a promoted ask always gets the marker, so this is an ask recorded before the
	// marker existed. Treat it as awaiting — the operator is looking at a card, so let them answer it.
	return id, true, true
}

// recordPromotedAnswer replaces the awaiting marker with the operator's words, so the transcript records
// the answer and the card settles for every client — including a reload.
func (s *Service) recordPromotedAnswer(ctx context.Context, tenantID, msgID, askID, answer string) error {
	tx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var raw []byte
	if err := tx.Tx.QueryRow(ctx,
		`select coalesce(tool_results, '[]'::jsonb) from ask_orchicon_messages where tenant_id = $1 and id = $2`,
		tenantID, msgID).Scan(&raw); err != nil {
		return err
	}
	var results []struct {
		ToolCallID string `json:"tool_call_id"`
		Output     string `json:"output"`
		IsError    bool   `json:"is_error"`
	}
	if err := json.Unmarshal(raw, &results); err != nil {
		return err
	}
	changed := false
	for i := range results {
		if results[i].ToolCallID == askID && IsAwaitingAnswer(results[i].Output) {
			results[i].Output = answer
			changed = true
		}
	}
	if !changed {
		return nil
	}
	out, err := json.Marshal(results)
	if err != nil {
		return err
	}
	if _, err := tx.Tx.Exec(ctx,
		`update ask_orchicon_messages set tool_results = $3::jsonb where tenant_id = $1 and id = $2`,
		tenantID, msgID, string(out)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var _ = db.NewID // keep the db import meaningful if the helpers above are the only users
