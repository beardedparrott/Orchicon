package askorchicon

// ask_promoted_reply.go — answering a card that came from an MCP-served ask.
//
// The tool runs in the `orchicon mcp` SIDECAR, which returns immediately: the ask never enters this
// process's in-memory pending registry, so ReplyPermissionAsk finds nothing to apply. The card is
// derived from the transcript instead (tool_ledger.promoteRecordedAsk), and the answer is delivered as
// the conversation's next message — which is what the tool's own contract promises.

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// promotedAskOpen reports whether askID names an UNANSWERED ask_user call in this conversation's
// transcript — i.e. a card that came from a promoted (MCP-served) ask rather than from the in-process
// pending registry.
//
// IT IS THE AUTHORISATION CHECK AS WELL AS THE LOOKUP. Answering is only allowed for an id that is
// genuinely an open ask in THIS conversation and tenant; anything else stays "expired". Without that,
// a caller could pass any id and inject a message into a conversation by claiming it was an answer.
func (s *Service) promotedAskOpen(ctx context.Context, tenantID, convID, askID string) bool {
	if s.pool == nil || askID == "" {
		return false
	}
	rows, err := s.messagesWithTools(ctx, tenantID, convID)
	if err != nil {
		return false
	}
	for _, m := range rows {
		answered := make(map[string]bool, len(m.toolResults))
		for _, r := range m.toolResults {
			answered[r.toolCallID] = true
		}
		for _, c := range m.toolCalls {
			if c.id != askID {
				continue
			}
			// The id must be an ask call (any spelling, including MCP-qualified)…
			if !isAskToolName(c.functionName) {
				return false
			}
			// …and it must still be OPEN: a call with a result has been answered.
			return !answered[c.id]
		}
	}
	return false
}

// messagesWithTools reads the most recent transcript rows carrying tool calls, with their results, so
// an ask id can be resolved against what the transcript actually holds.
//
// BOUNDED, because this runs on a reply: the newest page is more than enough — an open ask is by
// definition recent, and the alternative (scan every message a long conversation ever had) would make
// answering a card cost more than the rest of the turn.
func (s *Service) messagesWithTools(ctx context.Context, tenantID, convID string) ([]askLedgerRow, error) {
	tx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return listToolCallsAndResults(ctx, tx.Tx, tenantID, convID, 200)
}

// askLedgerRow is the pair of tool columns for one transcript row.
type askLedgerRow struct {
	toolCalls   []askLedgerCall
	toolResults []askLedgerResult
}

type askLedgerCall struct {
	id           string
	functionName string
}

type askLedgerResult struct {
	toolCallID string
}

// listToolCallsAndResults reads the pair of tool columns for the newest limit rows.
func listToolCallsAndResults(ctx context.Context, tx pgx.Tx, tenantID, convID string, limit int) ([]askLedgerRow, error) {
	rows, err := tx.Query(ctx, `
		select coalesce(tool_calls, '[]'::jsonb), coalesce(tool_results, '[]'::jsonb)
		  from ask_orchicon_messages
		 where tenant_id = $1 and conversation_id = $2
		 order by created_at desc
		 limit $3`, tenantID, convID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []askLedgerRow
	for rows.Next() {
		var calls, results []byte
		if err := rows.Scan(&calls, &results); err != nil {
			return nil, err
		}
		out = append(out, askLedgerRow{
			toolCalls:   decodeAskLedgerCalls(calls),
			toolResults: decodeAskLedgerResults(results),
		})
	}
	return out, rows.Err()
}

// decodeLedgerCalls pulls just the identity fields out of a stored tool_calls column. Unknown shapes
// yield nothing — this is a lookup, never a read-path failure.
func decodeAskLedgerCalls(raw []byte) []askLedgerCall {
	var rows []struct {
		ID           string `json:"id"`
		FunctionName string `json:"function_name"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil
	}
	out := make([]askLedgerCall, 0, len(rows))
	for _, r := range rows {
		out = append(out, askLedgerCall{id: r.ID, functionName: r.FunctionName})
	}
	return out
}

// decodeLedgerResults pulls the call ids a row's results answer.
func decodeAskLedgerResults(raw []byte) []askLedgerResult {
	var rows []struct {
		ToolCallID string `json:"tool_call_id"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil
	}
	out := make([]askLedgerResult, 0, len(rows))
	for _, r := range rows {
		out = append(out, askLedgerResult{toolCallID: r.ToolCallID})
	}
	return out
}
