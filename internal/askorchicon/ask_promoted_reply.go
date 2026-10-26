package askorchicon

// ask_promoted_reply.go — answering a card that came from an MCP-served ask.
//
// The ask tool runs in the `orchicon mcp` SIDECAR, which returns immediately: the ask never enters this
// process's in-memory pending registry, so ReplyPermissionAsk finds nothing to apply. The card is
// derived from the transcript instead (tool_ledger.promoteRecordedAsk), and the answer is delivered as
// the conversation's next message — which is what the tool's own contract promises: "the question is
// recorded … the user answers in their next message".

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/beardedparrott/orchicon/internal/db"
)

// findPromotedAsk locates an ask by ID in the conversation's transcript and reports whether it is still
// awaiting an answer, plus the row it lives on.
//
// IT QUERIES BY ID, NOT BY SCANNING A WINDOW — and that is the fix for the card that would not go away.
// The first version read the newest 200 rows; once the ask fell outside that window the lookup returned
// nothing, ReplyPermissionAsk answered "expired", the server never replaced the awaiting marker, and the
// card stayed open FOREVER. A settle path that can silently stop finding its own ask is a card that
// sticks, so the lookup is a jsonb containment match on the id — exact, and independent of how long the
// conversation is.
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
			// A result exists. It is awaiting an answer only if it is the marker; anything else means
			// it has been answered already and must not be answered twice.
			//
			// AN EMPTY RESULT IS NOT AN ANSWER, AND IT IS THE OLD-ROW CASE. claude's transcripts recorded
			// before this fix carry a DUPLICATE ask whose first call holds "" — the phantom the client now
			// correctly declines to treat as answered, and which it will therefore draw as an open card.
			// Refusing the reply here would make that card permanently unanswerable: drawn as a question,
			// dead when clicked. Nothing was said, so nobody answered it.
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
