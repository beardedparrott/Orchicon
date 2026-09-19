package db

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
	// systems-thinking partner) — brainstorm is the sole mode since 2026-08-26.
	// Read at turn-dispatch time and applied per message via the opencode
	// per-turn system prompt.
	Mode string
	// ProjectID is the project this conversation belongs to, or "" when unassigned.
	//
	// The SECOND level of organization over conversations (categories are the
	// first, and an orthogonal axis — a label someone applied, rather than a fact
	// about the chat). It is also the CONTEXT the agent is told about: a project's
	// project_dir is the folder the chat's work happens in.
	ProjectID string
	CreatedAt time.Time
	UpdatedAt time.Time
	// MessageCount is populated by the LIST query only (ListConversations); the
	// single-row queries leave it 0 because their callers compute the count
	// separately via CountConversationMessages. See scanConversationWithCount.
	MessageCount int
}

// MessageRow is the in-memory representation of an ask_orchicon_messages row.
type MessageRow struct {
	ID             string
	TenantID       string
	ConversationID string
	Role           string
	Content        string
	ToolCalls      []byte
	ToolResults    []byte
	Attachments    []byte
	Metadata       []byte
	Reasoning      []string
	CreatedAt      time.Time
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

// conversationCols is the column list every conversation query returns, in scanConversation's order.
//
// IT IS ONE SOURCE RATHER THAN EIGHT COPIES, because the RETURNING lists and the scans have to agree EXACTLY:
// a column added to one query and missed in another is a runtime scan error on whichever path was missed, and
// this file had NINE such lists (the insert, the get, the list, three updates' RETURNING, and two scans).
// Adding project_id would otherwise have been a nine-place edit with eight chances to miss one.
var conversationCols = []string{
	"id", "tenant_id", "title", "model_ref", "session_id", "mode", "project_id", "created_at", "updated_at",
}

// conversationSelect renders conversationCols for a query, optionally qualified (the LIST query aliases the
// table as `c` because it computes a correlated message count).
func conversationSelect(qualifier string) string {
	parts := make([]string, 0, len(conversationCols))
	for _, c := range conversationCols {
		if qualifier != "" {
			parts = append(parts, qualifier+"."+c)
			continue
		}
		parts = append(parts, c)
	}
	return strings.Join(parts, ", ")
}

// --- Conversations ---

func CreateConversation(ctx context.Context, tx pgx.Tx, c ConversationRow) (ConversationRow, error) {
	// An empty mode falls back to the migration's 'brainstorm' default
	// (COALESCE guards any caller that omits it), so absent/unspecified
	// always lands on the default persona.
	//
	// project_id takes the row's value verbatim; an empty one is the column's own default, i.e. unassigned. The
	// service validates a NON-empty id against the projects table before it gets here (see
	// Service.SetConversationProject), so this layer stays a plain write.
	q := `INSERT INTO ask_orchicon_conversations (id, tenant_id, title, model_ref, mode, project_id)
		VALUES ($1, $2, $3, $4, COALESCE(NULLIF($5, ''), 'brainstorm'), $6)
		RETURNING ` + conversationSelect("")
	row := c
	err := tx.QueryRow(ctx, q, c.ID, c.TenantID, c.Title, c.ModelRef, c.Mode, c.ProjectID).Scan(
		&row.ID, &row.TenantID, &row.Title, &row.ModelRef, &row.SessionID, &row.Mode, &row.ProjectID,
		&row.CreatedAt, &row.UpdatedAt,
	)
	if err != nil {
		return ConversationRow{}, fmt.Errorf("db: create conversation: %w", err)
	}
	return row, nil
}

func GetConversation(ctx context.Context, tx pgx.Tx, tenantID, id string) (ConversationRow, error) {
	q := `SELECT ` + conversationSelect("") + ` FROM ask_orchicon_conversations WHERE tenant_id = $1 AND id = $2`
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
	// message_count is part of the LIST shape (scanConversationWithCount). It is
	// a correlated COUNT so one query serves the whole page: both the rail and
	// the conversation detail display a per-conversation message count, and
	// without it ListConversations reported 0 for EVERY row while
	// GetConversation reported the real number — the same conversation rendered
	// as "0 msgs" in one place and "messages 2" in another. The predicate
	// mirrors CountConversationMessages exactly so the two cannot disagree.
	listCols := `SELECT ` + conversationSelect("c") + `,
			(SELECT COUNT(*) FROM ask_orchicon_messages m
			  WHERE m.tenant_id = c.tenant_id AND m.conversation_id = c.id)
		FROM ask_orchicon_conversations c`
	if afterID != "" {
		q = listCols + `
			WHERE c.tenant_id = $1 AND c.updated_at < (SELECT p.updated_at FROM ask_orchicon_conversations p WHERE p.tenant_id = $1 AND p.id = $2)
			ORDER BY c.updated_at DESC LIMIT $3`
		args = []any{tenantID, afterID, limit}
	} else {
		q = listCols + `
			WHERE c.tenant_id = $1
			ORDER BY c.updated_at DESC LIMIT $2`
		args = []any{tenantID, limit}
	}
	iter, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list conversations: %w", err)
	}
	defer iter.Close()
	for iter.Next() {
		r, err := scanConversationWithCount(iter)
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
	q := `UPDATE ask_orchicon_conversations SET title = $3, updated_at = now()
		WHERE tenant_id = $1 AND id = $2
		RETURNING ` + conversationSelect("")
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
	q := `UPDATE ask_orchicon_conversations SET mode = $3, updated_at = now()
		WHERE tenant_id = $1 AND id = $2
		RETURNING ` + conversationSelect("")
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

// UpdateConversationModel persists a conversation's model_ref override. An
// empty modelRef clears the override (the conversation then resolves the
// tenant default at dispatch). Mirrors UpdateConversationMode's RETURNING
// contract so the caller can echo the updated row.
func UpdateConversationModel(ctx context.Context, tx pgx.Tx, tenantID, id, modelRef string) (ConversationRow, error) {
	q := `UPDATE ask_orchicon_conversations SET model_ref = $3, updated_at = now()
		WHERE tenant_id = $1 AND id = $2
		RETURNING ` + conversationSelect("")
	row, err := tx.Query(ctx, q, tenantID, id, modelRef)
	if err != nil {
		return ConversationRow{}, fmt.Errorf("db: update conversation model: %w", err)
	}
	defer row.Close()
	if row.Next() {
		return scanConversation(row)
	}
	return ConversationRow{}, ErrNotFound
}

// SetConversationProject moves a conversation into a project, or clears the association when projectID is
// empty. It is the write behind the TUI rail's create/move, the GUI's project-folder drop target and the
// /project command — ONE write, so the clients cannot disagree about what "belongs to a project" means.
//
// The caller validates a non-empty id against the projects table first (see Service.SetConversationProject);
// this layer is a plain write, mirroring UpdateConversationMode's contract.
func SetConversationProject(ctx context.Context, tx pgx.Tx, tenantID, id, projectID string) (ConversationRow, error) {
	q := `UPDATE ask_orchicon_conversations SET project_id = $3, updated_at = now()
		WHERE tenant_id = $1 AND id = $2
		RETURNING ` + conversationSelect("")
	row, err := tx.Query(ctx, q, tenantID, id, projectID)
	if err != nil {
		return ConversationRow{}, fmt.Errorf("db: set conversation project: %w", err)
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

// UpsertMessage creates a message row under its id or replaces the existing
// one's content/reasoning/metadata AND tool ledger (tool_calls/tool_results
// are part of the conflict overwrite so live tool activity mirrored mid-turn
// is never resurrected over the terminal snapshot). The running turn's PARTIAL
// reply is written under the acked assistant message id as it is collected
// (so a client that lost the live stream can watch it grow via ListMessages);
// the finalize then upserts the complete reply over the partial. The row is only
// ever visible while the turn is in flight (its terminal state is written by
// the finalize).
func UpsertMessage(ctx context.Context, tx pgx.Tx, m MessageRow) (MessageRow, error) {
	reasoningJSON := []byte("[]")
	if m.Reasoning != nil {
		reasoningJSON, _ = json.Marshal(m.Reasoning)
	}
	const q = `INSERT INTO ask_orchicon_messages
		(id, tenant_id, conversation_id, role, content, tool_calls, tool_results, attachments, metadata, reasoning)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (tenant_id, id) DO UPDATE SET
			content = EXCLUDED.content,
			reasoning = EXCLUDED.reasoning,
			metadata = EXCLUDED.metadata,
			tool_calls = EXCLUDED.tool_calls,
			tool_results = EXCLUDED.tool_results
		RETURNING id, tenant_id, conversation_id, role, content, tool_calls, tool_results, attachments, metadata, reasoning, created_at`
	row := m
	err := tx.QueryRow(ctx, q, m.ID, m.TenantID, m.ConversationID, m.Role, m.Content,
		m.ToolCalls, m.ToolResults, m.Attachments, m.Metadata, reasoningJSON).Scan(
		&row.ID, &row.TenantID, &row.ConversationID, &row.Role, &row.Content,
		&row.ToolCalls, &row.ToolResults, &row.Attachments, &row.Metadata, &row.Reasoning, &row.CreatedAt,
	)
	if err != nil {
		return MessageRow{}, fmt.Errorf("db: upsert message: %w", err)
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
	if err := row.Scan(&r.ID, &r.TenantID, &r.Title, &r.ModelRef, &r.SessionID, &r.Mode, &r.ProjectID,
		&r.CreatedAt, &r.UpdatedAt); err != nil {
		return ConversationRow{}, fmt.Errorf("db: scan conversation: %w", err)
	}
	return r, nil
}

// scanConversationWithCount scans the LIST shape: scanConversation's columns plus
// the trailing per-conversation message count. It is separate from
// scanConversation because only ListConversations selects that column — the
// single-row Get/Update queries all share conversationSelect's list, which the
// scan above consumes.
func scanConversationWithCount(row pgx.Rows) (ConversationRow, error) {
	var r ConversationRow
	if err := row.Scan(&r.ID, &r.TenantID, &r.Title, &r.ModelRef, &r.SessionID, &r.Mode, &r.ProjectID,
		&r.CreatedAt, &r.UpdatedAt, &r.MessageCount); err != nil {
		return ConversationRow{}, fmt.Errorf("db: scan conversation with count: %w", err)
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
