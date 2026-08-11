package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ConversationRow is the in-memory representation of an ask_orchicon_conversations row.
type ConversationRow struct {
	ID        string
	TenantID  string
	Title     string
	ModelRef  string
	SessionID string
	// Mode is the per-conversation persona: 'brainstorm' (default, open
	// systems-thinking partner) or 'orchicon' (governed platform expert).
	// Read at turn-dispatch time and applied per message via the opencode
	// per-turn system prompt.
	Mode      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// MessageRow is the in-memory representation of an ask_orchicon_messages row.
type MessageRow struct {
	ID              string
	TenantID        string
	ConversationID  string
	Role            string
	Content         string
	ToolCalls       []byte
	ToolResults     []byte
	Attachments     []byte
	Metadata        []byte
	Reasoning       []string
	CreatedAt       time.Time
}

// AgentConfigRow is the in-memory representation of an ask_orchicon_agent_config row.
type AgentConfigRow struct {
	ID              string
	TenantID        string
	SystemPrompt    string
	Role            string
	Skills          string
	Behavior        string
	AgentsMD        string
	ToolDefinitions []byte
	ContextSources  []byte
	Permissions     []byte
	BudgetOverrides []byte
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// --- Conversations ---

func CreateConversation(ctx context.Context, tx pgx.Tx, c ConversationRow) (ConversationRow, error) {
	// An empty mode falls back to the migration's 'brainstorm' default
	// (COALESCE guards any caller that omits it), so absent/unspecified
	// always lands on the default persona.
	const q = `INSERT INTO ask_orchicon_conversations (id, tenant_id, title, model_ref, mode)
		VALUES ($1, $2, $3, $4, COALESCE(NULLIF($5, ''), 'brainstorm'))
		RETURNING id, tenant_id, title, model_ref, session_id, mode, created_at, updated_at`
	row := c
	err := tx.QueryRow(ctx, q, c.ID, c.TenantID, c.Title, c.ModelRef, c.Mode).Scan(
		&row.ID, &row.TenantID, &row.Title, &row.ModelRef, &row.SessionID, &row.Mode, &row.CreatedAt, &row.UpdatedAt,
	)
	if err != nil {
		return ConversationRow{}, fmt.Errorf("db: create conversation: %w", err)
	}
	return row, nil
}

func GetConversation(ctx context.Context, tx pgx.Tx, tenantID, id string) (ConversationRow, error) {
	const q = `SELECT id, tenant_id, title, model_ref, session_id, mode, created_at, updated_at
		FROM ask_orchicon_conversations WHERE tenant_id = $1 AND id = $2`
	row, err := tx.Query(ctx, q, tenantID, id)
	if err != nil {
		return ConversationRow{}, fmt.Errorf("db: get conversation: %w", err)
	}
	defer row.Close()
	if row.Next() {
		return scanConversation(row)
	}
	return ConversationRow{}, ErrNotFound
}

func ListConversations(ctx context.Context, tx pgx.Tx, tenantID string, limit int, afterID string) ([]ConversationRow, error) {
	var rows []ConversationRow
	var q string
	var args []any
	if afterID != "" {
		q = `SELECT id, tenant_id, title, model_ref, session_id, mode, created_at, updated_at
			FROM ask_orchicon_conversations
			WHERE tenant_id = $1 AND updated_at < (SELECT updated_at FROM ask_orchicon_conversations WHERE tenant_id = $1 AND id = $2)
			ORDER BY updated_at DESC LIMIT $3`
		args = []any{tenantID, afterID, limit}
	} else {
		q = `SELECT id, tenant_id, title, model_ref, session_id, mode, created_at, updated_at
			FROM ask_orchicon_conversations
			WHERE tenant_id = $1
			ORDER BY updated_at DESC LIMIT $2`
		args = []any{tenantID, limit}
	}
	iter, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list conversations: %w", err)
	}
	defer iter.Close()
	for iter.Next() {
		r, err := scanConversation(iter)
		if err != nil {
			return nil, err
		}
		rows = append(rows, r)
	}
	if rows == nil {
		rows = []ConversationRow{}
	}
	return rows, nil
}

func UpdateConversationTitle(ctx context.Context, tx pgx.Tx, tenantID, id, title string) (ConversationRow, error) {
	const q = `UPDATE ask_orchicon_conversations SET title = $3, updated_at = now()
		WHERE tenant_id = $1 AND id = $2
		RETURNING id, tenant_id, title, model_ref, session_id, mode, created_at, updated_at`
	row, err := tx.Query(ctx, q, tenantID, id, title)
	if err != nil {
		return ConversationRow{}, fmt.Errorf("db: update conversation title: %w", err)
	}
	defer row.Close()
	if row.Next() {
		return scanConversation(row)
	}
	return ConversationRow{}, ErrNotFound
}

// UpdateConversationMode switches a conversation's persona (brainstorm /
// orchicon). The new mode takes effect on the NEXT message: it is read at
// turn-dispatch time and applied via the opencode per-turn system prompt, so
// no session change or serve restart is needed. Returns the updated row.
func UpdateConversationMode(ctx context.Context, tx pgx.Tx, tenantID, id, mode string) (ConversationRow, error) {
	const q = `UPDATE ask_orchicon_conversations SET mode = $3, updated_at = now()
		WHERE tenant_id = $1 AND id = $2
		RETURNING id, tenant_id, title, model_ref, session_id, mode, created_at, updated_at`
	row, err := tx.Query(ctx, q, tenantID, id, mode)
	if err != nil {
		return ConversationRow{}, fmt.Errorf("db: update conversation mode: %w", err)
	}
	defer row.Close()
	if row.Next() {
		return scanConversation(row)
	}
	return ConversationRow{}, ErrNotFound
}

// UpdateConversationSessionID persists the opencode serve session id on the
// conversation (Task 1 session transport). It is called as soon as a fresh
// session is created (best-effort, its own tiny tenant tx) so a crash
// mid-turn cannot orphan a session the next message would have to rediscover,
// and again when a lost session is recreated.
func UpdateConversationSessionID(ctx context.Context, tx pgx.Tx, tenantID, id, sessionID string) error {
	const q = `UPDATE ask_orchicon_conversations SET session_id = $3, updated_at = now()
		WHERE tenant_id = $1 AND id = $2`
	_, err := tx.Exec(ctx, q, tenantID, id, sessionID)
	if err != nil {
		return fmt.Errorf("db: update conversation session id: %w", err)
	}
	return nil
}

func UpdateConversationTimestamp(ctx context.Context, tx pgx.Tx, tenantID, id string) error {
	const q = `UPDATE ask_orchicon_conversations SET updated_at = now()
		WHERE tenant_id = $1 AND id = $2`
	_, err := tx.Exec(ctx, q, tenantID, id)
	if err != nil {
		return fmt.Errorf("db: update conversation timestamp: %w", err)
	}
	return nil
}

func DeleteConversation(ctx context.Context, tx pgx.Tx, tenantID, id string) error {
	const q = `DELETE FROM ask_orchicon_conversations WHERE tenant_id = $1 AND id = $2`
	_, err := tx.Exec(ctx, q, tenantID, id)
	if err != nil {
		return fmt.Errorf("db: delete conversation: %w", err)
	}
	return nil
}

// --- Messages ---

func CreateMessage(ctx context.Context, tx pgx.Tx, m MessageRow) (MessageRow, error) {
	// The reasoning jsonb column is NOT NULL DEFAULT '[]': a nil slice is
	// marshaled as an empty array so inserts never violate the constraint.
	reasoningJSON := []byte("[]")
	if m.Reasoning != nil {
		reasoningJSON, _ = json.Marshal(m.Reasoning)
	}
	const q = `INSERT INTO ask_orchicon_messages
		(id, tenant_id, conversation_id, role, content, tool_calls, tool_results, attachments, metadata, reasoning)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id, tenant_id, conversation_id, role, content, tool_calls, tool_results, attachments, metadata, reasoning, created_at`
	row := m
	err := tx.QueryRow(ctx, q, m.ID, m.TenantID, m.ConversationID, m.Role, m.Content,
		m.ToolCalls, m.ToolResults, m.Attachments, m.Metadata, reasoningJSON).Scan(
		&row.ID, &row.TenantID, &row.ConversationID, &row.Role, &row.Content,
		&row.ToolCalls, &row.ToolResults, &row.Attachments, &row.Metadata, &row.Reasoning, &row.CreatedAt,
	)
	if err != nil {
		return MessageRow{}, fmt.Errorf("db: create message: %w", err)
	}
	return row, nil
}

func ListMessages(ctx context.Context, tx pgx.Tx, tenantID, conversationID string, limit int, afterID string) ([]MessageRow, error) {
	var rows []MessageRow
	var q string
	var args []any
	if afterID != "" {
		q = `SELECT id, tenant_id, conversation_id, role, content, tool_calls, tool_results, attachments, metadata, reasoning, created_at
			FROM ask_orchicon_messages
			WHERE tenant_id = $1 AND conversation_id = $2 AND created_at < (
				SELECT created_at FROM ask_orchicon_messages WHERE tenant_id = $1 AND id = $3
			)
			ORDER BY created_at DESC LIMIT $4`
		args = []any{tenantID, conversationID, afterID, limit}
	} else {
		q = `SELECT id, tenant_id, conversation_id, role, content, tool_calls, tool_results, attachments, metadata, reasoning, created_at
			FROM ask_orchicon_messages
			WHERE tenant_id = $1 AND conversation_id = $2
			ORDER BY created_at DESC LIMIT $3`
		args = []any{tenantID, conversationID, limit}
	}
	iter, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list messages: %w", err)
	}
	defer iter.Close()
	for iter.Next() {
		r, err := scanMessage(iter)
		if err != nil {
			return nil, err
		}
		rows = append(rows, r)
	}
	if rows == nil {
		rows = []MessageRow{}
	}
	return rows, nil
}

// DeleteConversationMessages deletes all messages for a conversation.
func DeleteConversationMessages(ctx context.Context, tx pgx.Tx, tenantID, conversationID string) error {
	const q = `DELETE FROM ask_orchicon_messages WHERE tenant_id = $1 AND conversation_id = $2`
	_, err := tx.Exec(ctx, q, tenantID, conversationID)
	if err != nil {
		return fmt.Errorf("db: delete conversation messages: %w", err)
	}
	return nil
}

// CountConversationMessages returns the number of messages in a conversation.
func CountConversationMessages(ctx context.Context, tx pgx.Tx, tenantID, conversationID string) (int, error) {
	const q = `SELECT COUNT(*) FROM ask_orchicon_messages WHERE tenant_id = $1 AND conversation_id = $2`
	var count int
	err := tx.QueryRow(ctx, q, tenantID, conversationID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("db: count conversation messages: %w", err)
	}
	return count, nil
}

// LastMessagePreview returns the content of the most recent message in a conversation.
func LastMessagePreview(ctx context.Context, tx pgx.Tx, tenantID, conversationID string) (string, error) {
	const q = `SELECT content FROM ask_orchicon_messages
		WHERE tenant_id = $1 AND conversation_id = $2 AND role = 'user'
		ORDER BY created_at DESC LIMIT 1`
	row, err := tx.Query(ctx, q, tenantID, conversationID)
	if err != nil {
		return "", fmt.Errorf("db: last message preview: %w", err)
	}
	defer row.Close()
	if row.Next() {
		var content string
		if err := row.Scan(&content); err != nil {
			return "", err
		}
		if len(content) > 120 {
			content = content[:120]
		}
		return content, nil
	}
	return "", nil
}

// --- Agent Config ---

func GetAgentConfig(ctx context.Context, tx pgx.Tx, tenantID string) (AgentConfigRow, error) {
	const q = `SELECT id, tenant_id, system_prompt, role, skills, behavior, agents_md,
		tool_definitions, context_sources, permissions, budget_overrides, created_at, updated_at
		FROM ask_orchicon_agent_config WHERE tenant_id = $1 AND id = 'default'`
	row, err := tx.Query(ctx, q, tenantID)
	if err != nil {
		return AgentConfigRow{}, fmt.Errorf("db: get agent config: %w", err)
	}
	defer row.Close()
	if row.Next() {
		return scanAgentConfig(row)
	}
	return AgentConfigRow{}, ErrNotFound
}

func UpsertAgentConfig(ctx context.Context, tx pgx.Tx, tenantID string, c AgentConfigRow) (AgentConfigRow, error) {
	const q = `INSERT INTO ask_orchicon_agent_config
		(id, tenant_id, system_prompt, role, skills, behavior, agents_md,
		 tool_definitions, context_sources, permissions, budget_overrides, updated_at)
		VALUES ('default', $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now())
		ON CONFLICT (tenant_id, id) DO UPDATE SET
			system_prompt = CASE WHEN $2 <> '' THEN $2 ELSE ask_orchicon_agent_config.system_prompt END,
			role = CASE WHEN $3 <> '' THEN $3 ELSE ask_orchicon_agent_config.role END,
			skills = CASE WHEN $4 <> '' THEN $4 ELSE ask_orchicon_agent_config.skills END,
			behavior = CASE WHEN $5 <> '' THEN $5 ELSE ask_orchicon_agent_config.behavior END,
			agents_md = CASE WHEN $6 <> '' THEN $6 ELSE ask_orchicon_agent_config.agents_md END,
			tool_definitions = CASE WHEN $7 <> '[]'::jsonb THEN $7 ELSE ask_orchicon_agent_config.tool_definitions END,
			context_sources = CASE WHEN $8 <> '[]'::jsonb THEN $8 ELSE ask_orchicon_agent_config.context_sources END,
			permissions = CASE WHEN $9 <> '[]'::jsonb THEN $9 ELSE ask_orchicon_agent_config.permissions END,
			budget_overrides = CASE WHEN $10 <> '{}'::jsonb THEN $10 ELSE ask_orchicon_agent_config.budget_overrides END,
			updated_at = now()
		RETURNING id, tenant_id, system_prompt, role, skills, behavior, agents_md,
			tool_definitions, context_sources, permissions, budget_overrides, created_at, updated_at`
	row, err := tx.Query(ctx, q, tenantID,
		c.SystemPrompt, c.Role, c.Skills, c.Behavior, c.AgentsMD,
		c.ToolDefinitions, c.ContextSources, c.Permissions, c.BudgetOverrides,
	)
	if err != nil {
		return AgentConfigRow{}, fmt.Errorf("db: upsert agent config: %w", err)
	}
	defer row.Close()
	if row.Next() {
		return scanAgentConfig(row)
	}
	return AgentConfigRow{}, fmt.Errorf("db: upsert agent config: no row returned")
}

// --- Scanners ---

func scanConversation(row pgx.Rows) (ConversationRow, error) {
	var r ConversationRow
	if err := row.Scan(&r.ID, &r.TenantID, &r.Title, &r.ModelRef, &r.SessionID, &r.Mode, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return ConversationRow{}, fmt.Errorf("db: scan conversation: %w", err)
	}
	return r, nil
}

func scanMessage(row pgx.Rows) (MessageRow, error) {
	var r MessageRow
	if err := row.Scan(&r.ID, &r.TenantID, &r.ConversationID, &r.Role, &r.Content,
		&r.ToolCalls, &r.ToolResults, &r.Attachments, &r.Metadata, &r.Reasoning, &r.CreatedAt,
	); err != nil {
		return MessageRow{}, fmt.Errorf("db: scan message: %w", err)
	}
	if r.Reasoning == nil {
		r.Reasoning = []string{}
	}
	return r, nil
}

func scanAgentConfig(row pgx.Rows) (AgentConfigRow, error) {
	var r AgentConfigRow
	if err := row.Scan(&r.ID, &r.TenantID, &r.SystemPrompt, &r.Role, &r.Skills, &r.Behavior, &r.AgentsMD,
		&r.ToolDefinitions, &r.ContextSources, &r.Permissions, &r.BudgetOverrides,
		&r.CreatedAt, &r.UpdatedAt,
	); err != nil {
		return AgentConfigRow{}, fmt.Errorf("db: scan agent config: %w", err)
	}
	return r, nil
}
