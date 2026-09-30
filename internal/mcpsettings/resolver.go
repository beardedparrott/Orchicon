package mcpsettings

import (
	"context"
	"errors"
	"fmt"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/secretcrypto"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/beardedparrott/orchicon/internal/workflow"
	"github.com/jackc/pgx/v5"
)

// Resolver is the ONE implementation of mcpclient.ScopeResolver: the
// storage-backed, scope-addressed resolution of MCP definitions.
//
// THERE IS NO PRECEDENCE CHAIN ANY MORE. A scope resolves to the UNION of
// the project-owned definitions and the scope's OWN definitions, deduped
// and order-stable. The old rule (worker → project → tenant default →
// none) was implemented in three places and the tenant tier is gone, so
// "worker wins / project wins" no longer exists — an owner-scoped
// definition is simply part of the union.
//
// The three input shapes it serves:
//
//	ScopeConversation — project-owned ∪ conversation-owned rows
//	ScopeWorker       — project-owned ∪ the worker version's inline specs (per-session)
//	ScopeRun          — project-owned ∪ EVERY step worker version's inline specs (child 4's bake)
//	ScopeProject      — the project's own rows only (the mechanical repoint's scope)
type Resolver struct {
	pool *db.Pool
}

// NewResolver constructs the storage-backed resolver.
func NewResolver(pool *db.Pool) *Resolver {
	return &Resolver{pool: pool}
}

var _ mcpclient.ScopeResolver = (*Resolver)(nil)

// ResolveScope implements mcpclient.ScopeResolver. The tenant is read from
// the context (tenant.FromContext) — the bridge scopes the session context
// with the execution's tenant before resolving.
func (r *Resolver) ResolveScope(ctx context.Context, ref mcpclient.ScopeRef) (mcpclient.Resolution, error) {
	tenantID := tenant.FromContext(ctx)
	if tenantID == "" {
		return mcpclient.Resolution{}, fmt.Errorf("mcpsettings resolver: no tenant in context")
	}
	switch ref.Kind {
	case mcpclient.ScopeProject:
		return r.resolveProject(ctx, tenantID, ref.ProjectID)
	case mcpclient.ScopeConversation:
		return r.resolveConversation(ctx, tenantID, ref.ProjectID, ref.ConversationID)
	case mcpclient.ScopeWorker:
		return r.resolveWorker(ctx, tenantID, ref.ProjectID, ref.WorkerID)
	case mcpclient.ScopeRun:
		return r.resolveRun(ctx, tenantID, ref.RunID)
	}
	return mcpclient.Resolution{}, nil
}

// ResolveRunUnion is the RUN-SCOPE entry point for callers that hold a run
// id (child 4's container bake). It takes the RUN and, internally, its
// project — NEVER an executing worker: its result is applied ONCE per
// container, so a per-execution input would make it order-dependent.
func (r *Resolver) ResolveRunUnion(ctx context.Context, tenantID, runID string) (mcpclient.Resolution, error) {
	if tenantID == "" {
		return mcpclient.Resolution{}, fmt.Errorf("mcpsettings resolver: no tenant")
	}
	return r.resolveRun(ctx, tenantID, runID)
}

// resolveProject is the project scope: the project's OWN definitions.
func (r *Resolver) resolveProject(ctx context.Context, tenantID, projectID string) (mcpclient.Resolution, error) {
	tx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return mcpclient.Resolution{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := db.ListMCPServersByOwner(ctx, tx.Tx, tenantID, projectID, "")
	if err != nil {
		return mcpclient.Resolution{}, err
	}
	return resolutionFromRows(rows), nil
}

// resolveConversation is the conversation scope: project-owned ∪
// conversation-owned rows, in ONE owner-scoped read.
func (r *Resolver) resolveConversation(ctx context.Context, tenantID, projectID, conversationID string) (mcpclient.Resolution, error) {
	tx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return mcpclient.Resolution{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := db.ListMCPServersByOwner(ctx, tx.Tx, tenantID, projectID, conversationID)
	if err != nil {
		return mcpclient.Resolution{}, err
	}
	// Provenance is read off each ROW's own owner by resolutionFromRows: a
	// project-owned row is From=project, a conversation-owned row is
	// From=conversation. The union of the two is the conversation scope.
	return resolutionFromRows(rows), nil
}

// resolveWorker is the per-session scope: project-owned ∪ the worker's
// latest published version's INLINE specs (its permissions.mcp_servers).
func (r *Resolver) resolveWorker(ctx context.Context, tenantID, projectID, workerID string) (mcpclient.Resolution, error) {
	tx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return mcpclient.Resolution{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := db.ListMCPServersByOwner(ctx, tx.Tx, tenantID, projectID, "")
	if err != nil {
		return mcpclient.Resolution{}, err
	}
	res := resolutionFromRows(rows)
	if workerID == "" {
		return res, nil
	}
	perms, err := workerPermissions(ctx, tx.Tx, tenantID, workerID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return res, err
	}
	inline := db.MCPServersFromPermissions(perms)
	res = unionInline(res, inline, "inline:"+workerID)
	res.Skills = unionSkills(res.Skills, db.SkillsFromPermissions(perms))
	return res, nil
}

// resolveRun is the RUN scope — the one to get right by construction.
//
// It takes the RUN, never an executing worker: the run's own published
// version is the DAG, and every step's worker version is resolved by the
// ONE shared walk (adapter.ResolveRunSteps) that the adapter demand set
// also reads (AC 2/AC 3). The result is the project-owned definitions plus
// EVERY step version's inline specs plus the skill-file union.
func (r *Resolver) resolveRun(ctx context.Context, tenantID, runID string) (mcpclient.Resolution, error) {
	if runID == "" {
		return mcpclient.Resolution{}, nil
	}
	tx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return mcpclient.Resolution{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	run, err := db.GetWorkflowRun(ctx, tx.Tx, tenantID, runID)
	if err != nil {
		return mcpclient.Resolution{}, fmt.Errorf("mcpsettings resolver: get run %s: %w", runID, err)
	}
	rows, err := db.ListMCPServersByOwner(ctx, tx.Tx, tenantID, run.ProjectID, "")
	if err != nil {
		return mcpclient.Resolution{}, err
	}
	res := resolutionFromRows(rows)

	version, err := db.GetWorkflowVersion(ctx, tx.Tx, tenantID, run.WorkflowID, run.WorkflowVersion)
	if err != nil {
		// A run whose version is gone has no steps to union: the project
		// rows are the whole answer, never an error (the run itself will
		// fail at start for the same reason — see the reconciler).
		return res, nil
	}
	steps, err := workflow.ParseSteps(version.Steps)
	if err != nil {
		return res, nil
	}
	// THE ONE WALK: the same call the adapter demand set reads, so the two
	// consumers cannot drift (AC 2).
	for _, step := range adapter.ResolveRunSteps(ctx, tx.Tx, tenantID, steps) {
		if len(step.Version.Permissions) == 0 {
			continue
		}
		res = unionInline(res, db.MCPServersFromPermissions(step.Version.Permissions),
			fmt.Sprintf("inline:%s@%d", step.WorkerID, step.Version.Version))
		res.Skills = unionSkills(res.Skills, db.SkillsFromPermissions(step.Version.Permissions))
	}
	return res, nil
}

// resolutionFromRows converts owner-scoped DB rows into the resolution
// shape. Disabled rows are collected (reported) but never resolve.
//
// ORDER IS PART OF THE CONTRACT: PROJECT-owned rows first (name ASC, the
// query's order), then the scope's OWN rows (name ASC). One owner-scoped
// query returns both kinds for a conversation scope, so the split is done
// here rather than depending on how the names happen to sort.
func resolutionFromRows(rows []db.MCPServerRow) mcpclient.Resolution {
	var res mcpclient.Resolution
	appendRow := func(row db.MCPServerRow) {
		if !row.Enabled {
			res.Disabled = append(res.Disabled, row.ID)
			return
		}
		from := mcpclient.ScopeProject
		fromID := "project:" + row.ProjectID
		if row.ConversationID != "" {
			from = mcpclient.ScopeConversation
			fromID = "conversation:" + row.ConversationID
		}
		res.Servers = append(res.Servers, mcpclient.ScopedServer{
			Spec:    specFromRow(row),
			From:    from,
			FromID:  fromID,
			EntryID: row.ID,
		})
		res.SelectedIDs = append(res.SelectedIDs, row.ID)
	}
	for _, row := range rows {
		if row.ConversationID == "" {
			appendRow(row)
		}
	}
	for _, row := range rows {
		if row.ConversationID != "" {
			appendRow(row)
		}
	}
	return res
}

// unionInline appends inline (worker-owned) definitions to a resolution,
// deduped by inline id, first occurrence wins, in declaration order.
func unionInline(res mcpclient.Resolution, inline []db.InlineMCPServer, fromID string) mcpclient.Resolution {
	seen := map[string]bool{}
	for _, s := range res.Servers {
		if s.EntryID != "" {
			seen[s.EntryID] = true
		}
	}
	for _, in := range inline {
		if in.ID == "" || seen[in.ID] {
			continue
		}
		seen[in.ID] = true
		res.SelectedIDs = append(res.SelectedIDs, in.ID)
		if in.Enabled != nil && !*in.Enabled {
			res.Disabled = append(res.Disabled, in.ID)
			continue
		}
		res.Servers = append(res.Servers, mcpclient.ScopedServer{
			Spec:   specFromInline(in),
			From:   mcpclient.ScopeWorker,
			FromID: fromID,
			// EntryID stays empty: an inline definition has no row.
		})
	}
	return res
}

// unionSkills appends skill files deduped by path, first occurrence wins,
// order-stable.
func unionSkills(res []mcpclient.InlineSkillFile, add []db.InlineSkillFile) []mcpclient.InlineSkillFile {
	seen := map[string]bool{}
	for _, s := range res {
		seen[s.Path] = true
	}
	for _, s := range add {
		if s.Path == "" || seen[s.Path] {
			continue
		}
		seen[s.Path] = true
		res = append(res, mcpclient.InlineSkillFile{Path: s.Path, Content: s.Content})
	}
	return res
}

// specFromRow maps a stored row to the neutral transport shape.
func specFromRow(r db.MCPServerRow) mcpclient.ServerSpec {
	spec := mcpclient.ServerSpec{
		ID:      r.ID,
		Command: append([]string{r.Command}, r.Args...),
		URL:     r.URL,
		Headers: r.Headers,
		Env:     r.Env,
	}
	switch r.Transport {
	case TransportStreamable, "http":
		spec.Type = mcpclient.TypeHTTP
	default:
		spec.Type = mcpclient.TypeStdio
	}
	return spec
}

// specFromInline maps a worker version's inline definition to the neutral
// transport shape.
func specFromInline(in db.InlineMCPServer) mcpclient.ServerSpec {
	spec := mcpclient.ServerSpec{
		ID:      in.ID,
		Command: in.Command,
		URL:     in.URL,
		Headers: in.Headers,
		Env:     in.Env,
		OnError: in.OnError,
	}
	switch in.Type {
	case "http", "streamable-http":
		spec.Type = mcpclient.TypeHTTP
	case "stdio":
		spec.Type = mcpclient.TypeStdio
	default:
		// Empty/unrecognized type: infer from the fields, the same rule
		// ServerSpec.TransportType applies.
		if in.URL != "" {
			spec.Type = mcpclient.TypeHTTP
		} else {
			spec.Type = mcpclient.TypeStdio
		}
	}
	return spec
}

// workerPermissions returns the latest version's permissions jsonb for a
// worker (the latest version row is the one an execution would use).
func workerPermissions(ctx context.Context, tx pgx.Tx, tenantID, workerID string) ([]byte, error) {
	var perms []byte
	err := tx.QueryRow(ctx,
		`SELECT permissions FROM worker_versions wv
		 WHERE wv.tenant_id=$1 AND wv.worker_id=$2
		 ORDER BY wv.version DESC LIMIT 1`, tenantID, workerID).Scan(&perms)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, db.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("db: worker permissions: %w", err)
	}
	return perms, nil
}

// ResolveSecretRefs replaces ${SECRET_NAME} references in a server's
// env/header values with the stored tenant-secret plaintext (never baked,
// never stored inline). A reference to an unset secret is a clear error
// naming the secret — callers surface it as a per-server session error.
// Values without a reference pass through unchanged.
func ResolveSecretRefs(ctx context.Context, pool *db.Pool, kek []byte, tenantID string, env, headers map[string]string) (map[string]string, map[string]string, error) {
	resolve := func(m map[string]string) (map[string]string, error) {
		if len(m) == 0 {
			return m, nil
		}
		// Collect unique referenced secret names.
		names := map[string]bool{}
		for _, v := range m {
			if name, ok := secretRefName(v); ok {
				names[name] = true
			}
		}
		if len(names) == 0 {
			return m, nil
		}
		values := map[string]string{}
		tx, err := pool.BeginTenantTx(ctx, tenantID)
		if err != nil {
			return nil, err
		}
		for name := range names {
			row, err := db.GetSecretByName(ctx, tx.Tx, tenantID, name)
			if errors.Is(err, db.ErrNotFound) {
				_ = tx.Rollback(ctx)
				return nil, fmt.Errorf("secret %q referenced by the server is not stored", name)
			}
			if err != nil {
				_ = tx.Rollback(ctx)
				return nil, err
			}
			pt, err := secretcrypto.Decrypt(row.Ciphertext, kek)
			if err != nil {
				_ = tx.Rollback(ctx)
				return nil, fmt.Errorf("decrypt secret %q: %w", name, err)
			}
			values[name] = string(pt)
		}
		_ = tx.Rollback(ctx) // read-only
		out := make(map[string]string, len(m))
		for k, v := range m {
			if name, ok := secretRefName(v); ok {
				out[k] = values[name]
			} else {
				out[k] = v
			}
		}
		return out, nil
	}
	envOut, err := resolve(env)
	if err != nil {
		return nil, nil, err
	}
	headOut, err := resolve(headers)
	if err != nil {
		return nil, nil, err
	}
	return envOut, headOut, nil
}
