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
type MCPServerRow struct {
	ID            string
	TenantID      string
	Name          string
	Transport     string // "stdio" | "streamable-http"
	Command       string
	Args          []string
	Env           map[string]string
	URL           string
	Headers       map[string]string
	Enabled       bool
	CatalogSlug   string
	InstallStatus string // unknown|not_installed|installing|installed|failed
	InstallResult []byte // jsonb: {runtime, command, ok, error, installed_at}
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

const mcpServerCols = `id, tenant_id, name, transport, command, args, env, url, headers, enabled,
	catalog_slug, install_status, install_result, created_at, updated_at`

func scanMCPServer(row pgx.Row) (MCPServerRow, error) {
	var r MCPServerRow
	var argsRaw, envRaw, headersRaw []byte
	err := row.Scan(&r.ID, &r.TenantID, &r.Name, &r.Transport, &r.Command, &argsRaw, &envRaw,
		&r.URL, &headersRaw, &r.Enabled, &r.CatalogSlug, &r.InstallStatus, &r.InstallResult,
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

// UpsertMCPServer inserts or updates one (tenant, name) row. The name is
// the natural key (UNIQUE(tenant_id, name)); renaming is delete+create.
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
		 catalog_slug, install_status, install_result)
		VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7::jsonb,$8,$9::jsonb,$10,$11,$12,$13::jsonb)
		ON CONFLICT (tenant_id, name) DO UPDATE SET
			transport      = EXCLUDED.transport,
			command        = EXCLUDED.command,
			args           = EXCLUDED.args,
			env            = EXCLUDED.env,
			url            = EXCLUDED.url,
			headers        = EXCLUDED.headers,
			enabled        = EXCLUDED.enabled,
			catalog_slug   = EXCLUDED.catalog_slug,
			install_status = EXCLUDED.install_status,
			install_result = EXCLUDED.install_result,
			updated_at     = now()
		RETURNING ` + mcpServerCols
	row, err := scanMCPServer(tx.QueryRow(ctx, q,
		r.ID, r.TenantID, r.Name, r.Transport, r.Command, argsJSON, envJSON, r.URL, headersJSON,
		r.Enabled, r.CatalogSlug, r.InstallStatus, string(resultJSON)))
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

// SetProjectMCPServers replaces the project's MCP selection (delete-not-in
// + insert) inside the caller's tenant transaction — references, never
// copies. All ids must already exist (validated by the caller).
func SetProjectMCPServers(ctx context.Context, tx pgx.Tx, tenantID, projectID string, ids []string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM project_mcp_servers WHERE tenant_id=$1 AND project_id=$2`, tenantID, projectID); err != nil {
		return fmt.Errorf("db: set project mcp servers: delete: %w", err)
	}
	for _, id := range ids {
		if _, err := tx.Exec(ctx,
			`INSERT INTO project_mcp_servers (project_id, tenant_id, mcp_server_id) VALUES ($1,$2,$3)
			 ON CONFLICT (project_id, mcp_server_id) DO NOTHING`,
			projectID, tenantID, id); err != nil {
			return fmt.Errorf("db: set project mcp servers: insert %s: %w", id, err)
		}
	}
	return nil
}

// ListProjectMCPServerIDs returns the project's MCP server id set in
// insertion order.
func ListProjectMCPServerIDs(ctx context.Context, tx pgx.Tx, tenantID, projectID string) ([]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT mcp_server_id FROM project_mcp_servers WHERE tenant_id=$1 AND project_id=$2 ORDER BY created_at ASC, mcp_server_id ASC`,
		tenantID, projectID)
	if err != nil {
		return nil, fmt.Errorf("db: list project mcp servers: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("db: list project mcp servers: %w", err)
		}
		out = append(out, id)
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

// MCPServerReferencingProject is one live project reference (deletion guard).
type MCPServerReferencingProject struct {
	ProjectID   string
	ProjectName string
}

// MCPServerReferencingWorker is one live worker reference (deletion guard).
type MCPServerReferencingWorker struct {
	WorkerID   string
	WorkerName string
}

// ListMCPServerReferences returns every live reference to an MCP server
// row: projects via project_mcp_servers, workers via their
// permissions.mcp_servers id array, and the tenant default set.
func ListMCPServerReferences(ctx context.Context, tx pgx.Tx, tenantID, serverID string) (projects []MCPServerReferencingProject, workers []MCPServerReferencingWorker, inDefault bool, err error) {
	rows, err := tx.Query(ctx,
		`SELECT p.id, p.name FROM project_mcp_servers pms
		 JOIN projects p ON p.id = pms.project_id AND p.tenant_id = pms.tenant_id
		 WHERE pms.tenant_id=$1 AND pms.mcp_server_id=$2 ORDER BY p.name ASC`,
		tenantID, serverID)
	if err != nil {
		return nil, nil, false, fmt.Errorf("db: list mcp server project refs: %w", err)
	}
	for rows.Next() {
		var r MCPServerReferencingProject
		if err := rows.Scan(&r.ProjectID, &r.ProjectName); err != nil {
			rows.Close()
			return nil, nil, false, fmt.Errorf("db: list mcp server project refs: %w", err)
		}
		projects = append(projects, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, false, err
	}

	// Worker refs: scan worker_versions.permissions for mcp_servers ids.
	// The permissions jsonb may carry legacy {id, command} objects or bare
	// id strings; both are matched on the id field.
	wrows, err := tx.Query(ctx,
		`SELECT DISTINCT w.id, w.name
		 FROM worker_versions wv JOIN workers w ON w.id = wv.worker_id AND w.tenant_id = wv.tenant_id
		 WHERE wv.tenant_id=$1 AND wv.permissions::text ILIKE '%' || $2 || '%'`,
		tenantID, serverID)
	if err != nil {
		return nil, nil, false, fmt.Errorf("db: list mcp server worker refs: %w", err)
	}
	for wrows.Next() {
		var r MCPServerReferencingWorker
		if err := wrows.Scan(&r.WorkerID, &r.WorkerName); err != nil {
			wrows.Close()
			return nil, nil, false, fmt.Errorf("db: list mcp server worker refs: %w", err)
		}
		workers = append(workers, r)
	}
	wrows.Close()
	if err := wrows.Err(); err != nil {
		return nil, nil, false, err
	}

	// Tenant default set (tenant_settings.default_mcp_servers id array).
	var raw []byte
	qerr := tx.QueryRow(ctx,
		`SELECT default_mcp_servers FROM tenant_settings WHERE tenant_id=$1`, tenantID).Scan(&raw)
	if qerr != nil && !errors.Is(qerr, pgx.ErrNoRows) {
		return nil, nil, false, fmt.Errorf("db: list mcp server tenant default ref: %w", qerr)
	}
	if len(raw) > 0 {
		var ids []string
		if err := json.Unmarshal(raw, &ids); err == nil {
			for _, id := range ids {
				if id == serverID {
					inDefault = true
					break
				}
			}
		}
	}
	return projects, workers, inDefault, nil
}

// SetTenantDefaultMCPServers replaces the tenant default MCP server id
// array on tenant_settings.
func SetTenantDefaultMCPServers(ctx context.Context, tx pgx.Tx, tenantID string, ids []string) error {
	b, err := marshalMCPJSON(orEmptyList(ids), "[]")
	if err != nil {
		return fmt.Errorf("db: set tenant default mcp servers: %w", err)
	}
	const q = `INSERT INTO tenant_settings (tenant_id, default_mcp_servers) VALUES ($1, $2::jsonb)
		ON CONFLICT (tenant_id) DO UPDATE SET default_mcp_servers = $2::jsonb`
	if _, err := tx.Exec(ctx, q, tenantID, b); err != nil {
		return fmt.Errorf("db: set tenant default mcp servers: %w", err)
	}
	return nil
}

// GetTenantDefaultMCPServers returns the tenant default MCP server id
// array (empty when unset).
func GetTenantDefaultMCPServers(ctx context.Context, tx pgx.Tx, tenantID string) ([]string, error) {
	var raw []byte
	err := tx.QueryRow(ctx,
		`SELECT default_mcp_servers FROM tenant_settings WHERE tenant_id=$1`, tenantID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get tenant default mcp servers: %w", err)
	}
	var ids []string
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &ids); err != nil {
			return nil, fmt.Errorf("db: get tenant default mcp servers: %w", err)
		}
	}
	return ids, nil
}

// MCPServerRefsFromPermissions extracts the MCP server ids from a worker
// permissions jsonb blob. Legacy {id, command} entries and bare id
// strings are both tolerated (read-time normalization; the worker service
// validates ids on the next save).
func MCPServerRefsFromPermissions(permissions []byte) []string {
	if len(permissions) == 0 {
		return nil
	}
	var p struct {
		MCPServers []json.RawMessage `json:"mcp_servers"`
	}
	if err := json.Unmarshal(permissions, &p); err != nil {
		return nil
	}
	var ids []string
	for _, raw := range p.MCPServers {
		s := strings.TrimSpace(string(raw))
		if strings.HasPrefix(s, "{") {
			var o struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(raw, &o); err == nil && o.ID != "" {
				ids = append(ids, o.ID)
			}
			continue
		}
		s = strings.Trim(s, `"`)
		if s != "" {
			ids = append(ids, s)
		}
	}
	return ids
}

func orEmptyMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
