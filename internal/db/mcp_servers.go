package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// MCPServerRow is the data-access shape of a mcp_servers row (tenant-
// scoped MCP server entry; stdio or streamable HTTP). Env/headers are
// decoded JSONB maps whose values may be ${SECRET_NAME} references.
//
// EXACTLY ONE of ProjectID / ConversationID is non-empty: the row's OWNER
// (mcp_servers_owner_xor enforces it). A definition is no longer an
// ownerless tenant-wide row referenced by a join table — selection IS
// ownership.
type MCPServerRow struct {
	ID             string
	TenantID       string
	Name           string
	Transport      string // "stdio" | "streamable-http"
	Command        string
	Args           []string
	Env            map[string]string
	URL            string
	Headers        map[string]string
	Enabled        bool
	CatalogSlug    string
	InstallStatus  string // unknown|not_installed|installing|installed|failed
	InstallResult  []byte // jsonb: {runtime, command, ok, error, installed_at}
	ProjectID      string // owner (XOR with ConversationID)
	ConversationID string // owner (XOR with ProjectID)
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

const mcpServerCols = `id, tenant_id, name, transport, command, args, env, url, headers, enabled,
	catalog_slug, install_status, install_result, project_id, conversation_id, created_at, updated_at`

func scanMCPServer(row pgx.Row) (MCPServerRow, error) {
	var r MCPServerRow
	var argsRaw, envRaw, headersRaw []byte
	var projectID, conversationID *string
	err := row.Scan(&r.ID, &r.TenantID, &r.Name, &r.Transport, &r.Command, &argsRaw, &envRaw,
		&r.URL, &headersRaw, &r.Enabled, &r.CatalogSlug, &r.InstallStatus, &r.InstallResult,
		&projectID, &conversationID,
		&r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return r, err
	}
	if len(argsRaw) > 0 {
		if err := json.Unmarshal(argsRaw, &r.Args); err != nil {
			return r, fmt.Errorf("db: scan mcp server: args: %w", err)
		}
	}
	if len(envRaw) > 0 {
		if err := json.Unmarshal(envRaw, &r.Env); err != nil {
			return r, fmt.Errorf("db: scan mcp server: env: %w", err)
		}
	}
	if len(headersRaw) > 0 {
		if err := json.Unmarshal(headersRaw, &r.Headers); err != nil {
			return r, fmt.Errorf("db: scan mcp server: headers: %w", err)
		}
	}
	if projectID != nil {
		r.ProjectID = *projectID
	}
	if conversationID != nil {
		r.ConversationID = *conversationID
	}
	return r, nil
}

func marshalMCPJSON[T any](v T, fallback string) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	if string(b) == "null" || string(b) == "" {
		return fallback, nil
	}
	return string(b), nil
}

// GetMCPServer returns one row (ErrNotFound when absent).
func GetMCPServer(ctx context.Context, tx pgx.Tx, tenantID, id string) (MCPServerRow, error) {
	const q = `SELECT ` + mcpServerCols + ` FROM mcp_servers WHERE tenant_id=$1 AND id=$2`
	r, err := scanMCPServer(tx.QueryRow(ctx, q, tenantID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return MCPServerRow{}, ErrNotFound
	}
	if err != nil {
		return MCPServerRow{}, fmt.Errorf("db: get mcp server: %w", err)
	}
	return r, nil
}

// ListMCPServers returns every stored row for the tenant, ordered by name.
func ListMCPServers(ctx context.Context, tx pgx.Tx, tenantID string) ([]MCPServerRow, error) {
	const q = `SELECT ` + mcpServerCols + ` FROM mcp_servers WHERE tenant_id=$1 ORDER BY name ASC`
	rows, err := tx.Query(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("db: list mcp servers: %w", err)
	}
	defer rows.Close()
	var out []MCPServerRow
	for rows.Next() {
		r, err := scanMCPServer(rows)
		if err != nil {
			return nil, fmt.Errorf("db: list mcp servers: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertMCPServer inserts or updates one row. The conflict target is the
// PRIMARY KEY (id), not the name: name uniqueness is now OWNER-SCOPED
// (mcp_servers_project_name_key / mcp_servers_conversation_name_key), so
// two owners may each hold a `postgres`-named row while one owner may not
// hold two. Create mints a fresh id; Update passes the existing one, so
// the PK conflict is exact. Renaming stays delete+create.
func UpsertMCPServer(ctx context.Context, tx pgx.Tx, r MCPServerRow) (MCPServerRow, error) {
	argsJSON, err := marshalMCPJSON(orEmptyList(r.Args), "[]")
	if err != nil {
		return MCPServerRow{}, fmt.Errorf("db: upsert mcp server: args: %w", err)
	}
	envJSON, err := marshalMCPJSON(orEmptyMap(r.Env), "{}")
	if err != nil {
		return MCPServerRow{}, fmt.Errorf("db: upsert mcp server: env: %w", err)
	}
	headersJSON, err := marshalMCPJSON(orEmptyMap(r.Headers), "{}")
	if err != nil {
		return MCPServerRow{}, fmt.Errorf("db: upsert mcp server: headers: %w", err)
	}
	resultJSON := r.InstallResult
	if len(resultJSON) == 0 {
		resultJSON = []byte("{}")
	}
	const q = `INSERT INTO mcp_servers
		(id, tenant_id, name, transport, command, args, env, url, headers, enabled,
		 catalog_slug, install_status, install_result, project_id, conversation_id)
		VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7::jsonb,$8,$9::jsonb,$10,$11,$12,$13::jsonb,$14,$15)
		ON CONFLICT (id) DO UPDATE SET
			transport       = EXCLUDED.transport,
			command         = EXCLUDED.command,
			args            = EXCLUDED.args,
			env             = EXCLUDED.env,
			url             = EXCLUDED.url,
			headers         = EXCLUDED.headers,
			enabled         = EXCLUDED.enabled,
			catalog_slug    = EXCLUDED.catalog_slug,
			install_status  = EXCLUDED.install_status,
			install_result  = EXCLUDED.install_result,
			project_id      = EXCLUDED.project_id,
			conversation_id = EXCLUDED.conversation_id,
			updated_at      = now()
		RETURNING ` + mcpServerCols
	row, err := scanMCPServer(tx.QueryRow(ctx, q,
		r.ID, r.TenantID, r.Name, r.Transport, r.Command, argsJSON, envJSON, r.URL, headersJSON,
		r.Enabled, r.CatalogSlug, r.InstallStatus, string(resultJSON),
		nullIfEmpty(r.ProjectID), nullIfEmpty(r.ConversationID)))
	if err != nil {
		return MCPServerRow{}, fmt.Errorf("db: upsert mcp server: %w", err)
	}
	return row, nil
}

// UpdateMCPServerInstallResult records the auto-install outcome without
// touching the rest of the row.
func UpdateMCPServerInstallResult(ctx context.Context, tx pgx.Tx, tenantID, id, status string, result []byte) error {
	const q = `UPDATE mcp_servers SET install_status=$3, install_result=$4::jsonb, updated_at=now()
		WHERE tenant_id=$1 AND id=$2`
	ct, err := tx.Exec(ctx, q, tenantID, id, status, string(result))
	if err != nil {
		return fmt.Errorf("db: update mcp server install result: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateMCPServerConfig applies a partial config update (env/headers maps
// replace-or-merge via the JSON merge). Used by SetMCPServerSecret /
// ClearMCPServerSecret to write ${SECRET_NAME} references.
func UpdateMCPServerConfig(ctx context.Context, tx pgx.Tx, tenantID, id string, env map[string]string, headers map[string]string) error {
	const q = `UPDATE mcp_servers SET env=$3::jsonb, headers=$4::jsonb, updated_at=now()
		WHERE tenant_id=$1 AND id=$2`
	envJSON, err := marshalMCPJSON(orEmptyMap(env), "{}")
	if err != nil {
		return err
	}
	hdrsJSON, err := marshalMCPJSON(orEmptyMap(headers), "{}")
	if err != nil {
		return err
	}
	ct, err := tx.Exec(ctx, q, tenantID, id, envJSON, hdrsJSON)
	if err != nil {
		return fmt.Errorf("db: update mcp server config: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteMCPServer removes one row.
func DeleteMCPServer(ctx context.Context, tx pgx.Tx, tenantID, id string) error {
	const q = `DELETE FROM mcp_servers WHERE tenant_id=$1 AND id=$2`
	ct, err := tx.Exec(ctx, q, tenantID, id)
	if err != nil {
		return fmt.Errorf("db: delete mcp server: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListMCPServersByOwner returns the OWNER-SCOPED definitions visible to a
// scope, ordered by name (the union's project-first ordering). The
// predicate is an OR over both owner columns, so ONE call serves both the
// project scope (conversationID empty → project rows only) and the
// conversation scope (both ids → project ∪ conversation rows).
//
// It replaces the join-table pair (SetProjectMCPServers /
// ListProjectMCPServerIDs) of the reference model: there is no selection to
// write any more — the owner column IS the selection.
func ListMCPServersByOwner(ctx context.Context, tx pgx.Tx, tenantID, projectID, conversationID string) ([]MCPServerRow, error) {
	const q = `SELECT ` + mcpServerCols + ` FROM mcp_servers
		WHERE tenant_id=$1
		  AND ((project_id IS NOT NULL AND project_id=$2)
		    OR (conversation_id IS NOT NULL AND conversation_id=$3))
		ORDER BY name ASC`
	rows, err := tx.Query(ctx, q, tenantID, projectID, conversationID)
	if err != nil {
		return nil, fmt.Errorf("db: list mcp servers by owner: %w", err)
	}
	defer rows.Close()
	var out []MCPServerRow
	for rows.Next() {
		r, err := scanMCPServer(rows)
		if err != nil {
			return nil, fmt.Errorf("db: list mcp servers by owner: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListMCPServersByIDs returns the rows whose ids are in the list,
// tenant-scoped (used for validation + picker resolution). Unknown ids
// are simply absent from the result.
func ListMCPServersByIDs(ctx context.Context, tx pgx.Tx, tenantID string, ids []string) ([]MCPServerRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx,
		`SELECT `+mcpServerCols+` FROM mcp_servers WHERE tenant_id=$1 AND id = ANY($2)`,
		tenantID, ids)
	if err != nil {
		return nil, fmt.Errorf("db: list mcp servers by ids: %w", err)
	}
	defer rows.Close()
	var out []MCPServerRow
	for rows.Next() {
		r, err := scanMCPServer(rows)
		if err != nil {
			return nil, fmt.Errorf("db: list mcp servers by ids: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// nullIfEmpty maps "" to a SQL NULL (the owner columns are nullable and the
// XOR CHECK counts on that).
func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// InlineMCPServer is ONE inline MCP definition carried by a worker
// version's permissions jsonb ("mcp_servers"). Full specs and the legacy
// id-only / {id,command} shapes all decode into this.
//
// An inline definition has no mcp_servers ROW — the worker version owns it
// (see the epic), which is why EntryID is empty for one in
// mcpclient.ScopedServer.
type InlineMCPServer struct {
	ID      string            `json:"id"`
	Type    string            `json:"type,omitempty"` // "stdio" | "http"
	Command []string          `json:"command,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// Enabled nil means enabled (a legacy reference carries no flag).
	Enabled *bool  `json:"enabled,omitempty"`
	OnError string `json:"onError,omitempty"`
}

// MCPServersFromPermissions is THE parser of worker_versions.permissions'
// "mcp_servers" jsonb. THREE shapes decode here and NOTHING else parses
// that key, so the reader and the writers can never disagree:
//
//	1. full inline spec  {"id","type","command":["npx","-y","x"],"url","headers","env","enabled","onError"}
//	2. legacy reference  {"id":"01H…","command":"npx -y x"}   (command is a *string* → strings.Fields)
//	3. legacy bare id    "01H…"
//
// A malformed jsonb yields nil (read-time normalization degrades, never
// fails a session over worker config shape).
func MCPServersFromPermissions(permissions []byte) []InlineMCPServer {
	if len(permissions) == 0 {
		return nil
	}
	var p struct {
		MCPServers []json.RawMessage `json:"mcp_servers"`
	}
	if err := json.Unmarshal(permissions, &p); err != nil {
		return nil
	}
	var out []InlineMCPServer
	for _, raw := range p.MCPServers {
		s := strings.TrimSpace(string(raw))
		switch {
		case s == "" || s == "null":
			continue
		case strings.HasPrefix(s, "{"):
			var o struct {
				ID      string            `json:"id"`
				Type    string            `json:"type,omitempty"`
				Command json.RawMessage   `json:"command,omitempty"`
				URL     string            `json:"url,omitempty"`
				Headers map[string]string `json:"headers,omitempty"`
				Env     map[string]string `json:"env,omitempty"`
				Enabled *bool             `json:"enabled,omitempty"`
				OnError string            `json:"onError,omitempty"`
			}
			if err := json.Unmarshal(raw, &o); err != nil || o.ID == "" {
				continue
			}
			out = append(out, InlineMCPServer{
				ID:      o.ID,
				Type:    o.Type,
				Command: decodeInlineCommand(o.Command),
				URL:     o.URL,
				Headers: o.Headers,
				Env:     o.Env,
				Enabled: o.Enabled,
				OnError: o.OnError,
			})
		default:
			id := strings.Trim(s, `"`)
			if id != "" {
				out = append(out, InlineMCPServer{ID: id})
			}
		}
	}
	return out
}

// decodeInlineCommand decodes the "command" member, which is EITHER the
// full-spec argv array OR the legacy single string (split on fields, the
// same rule the old mcpclient string form used). Absent/null → nil.
func decodeInlineCommand(raw json.RawMessage) []string {
	t := strings.TrimSpace(string(raw))
	if t == "" || t == "null" {
		return nil
	}
	if strings.HasPrefix(t, "[") {
		var argv []string
		if err := json.Unmarshal(raw, &argv); err == nil {
			return argv
		}
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return strings.Fields(s)
}

// MCPServerRefsFromPermissions keeps the ID-ONLY view of the same jsonb for
// callers that only want ids. It DELEGATES to MCPServersFromPermissions:
// ONE parser, ONE place — a second id parser here is exactly how the reader
// and the writer drift apart.
func MCPServerRefsFromPermissions(permissions []byte) []string {
	defs := MCPServersFromPermissions(permissions)
	if len(defs) == 0 {
		return nil
	}
	ids := make([]string, 0, len(defs))
	for _, d := range defs {
		if d.ID != "" {
			ids = append(ids, d.ID)
		}
	}
	return ids
}

// InlineSkillFile is one inline skill file carried by a worker version's
// permissions jsonb ("skill_files"). The skill-FILE STORE is child 2's; this
// child only gives the run union its inline half, through the same
// single-parser rule.
type InlineSkillFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// SkillsFromPermissions is THE parser of worker_versions.permissions'
// "skill_files" array. Absent/malformed → nil. Entries without a path are
// dropped (an anonymous skill file cannot be deduped or placed).
func SkillsFromPermissions(permissions []byte) []InlineSkillFile {
	if len(permissions) == 0 {
		return nil
	}
	var p struct {
		SkillFiles []InlineSkillFile `json:"skill_files"`
	}
	if err := json.Unmarshal(permissions, &p); err != nil {
		return nil
	}
	var out []InlineSkillFile
	for _, s := range p.SkillFiles {
		if strings.TrimSpace(s.Path) == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

func orEmptyMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
