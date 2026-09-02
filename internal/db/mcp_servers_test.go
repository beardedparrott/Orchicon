package db_test

import (
	"context"
	"os"
	"testing"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
)

// TestMCPServerJoinTables exercises the MCP server join-table surface:
// project selection replace semantics (delete-not-in + insert), id-array
// validation against tenant scope, and tenant-default selection.
// Guarded by ORCHICON_TEST_DSN like the seed tests:
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/db/ -run TestMCPServerJoinTables -v
func TestMCPServerJoinTables(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed MCP join-table test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := migrate.Run(ctx, pool, assets.MigrationsFS, assets.MigrationsDir); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	// mcp_servers has an FK to tenants(id); create the tenant row first.
	const tenant = "tnt_mcpjoin"
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	if _, err := ttx.Exec(ctx, `INSERT INTO tenants (id, slug, name, status) VALUES ($1,$2,$3,'active')`,
		tenant, "mcp-join-"+db.NewID()[:8], "MCP Join Test"); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: tenant,
		Name: "MCP Join Test", Slug: "mcp-join-" + db.NewID()[:8],
		Status: "active", Goals: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		dttx, err := pool.BeginTenantTx(c, tenant)
		if err == nil {
			_, _ = dttx.Exec(c, `DELETE FROM mcp_servers WHERE tenant_id=$1`, tenant)
			_, _ = dttx.Exec(c, `DELETE FROM project_mcp_servers WHERE tenant_id=$1`, tenant)
			// The tenant default lives in tenant_settings.default_mcp_servers
			// (JSONB id array) — no tenant_default_mcp_servers table exists.
			_, _ = dttx.Exec(c, `UPDATE tenant_settings SET default_mcp_servers='[]'::jsonb WHERE tenant_id=$1`, tenant)
			_, _ = dttx.Exec(c, `DELETE FROM projects WHERE id=$1`, proj.ID)
			_ = dttx.Commit(c)
		}
	})

	// Two servers for the same tenant.
	s1, err := db.UpsertMCPServer(ctx, ttx.Tx, db.MCPServerRow{
		ID: db.NewID(), TenantID: tenant, Name: "filesystem", Transport: "stdio",
		Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-filesystem"},
		Enabled: true, InstallStatus: "not_installed",
	})
	if err != nil {
		t.Fatalf("upsert s1: %v", err)
	}
	s2, err := db.UpsertMCPServer(ctx, ttx.Tx, db.MCPServerRow{
		ID: db.NewID(), TenantID: tenant, Name: "fetch", Transport: "stdio",
		Command: "uvx", Args: []string{"mcp-server-fetch"}, Enabled: true,
		InstallStatus: "not_installed",
	})
	if err != nil {
		t.Fatalf("upsert s2: %v", err)
	}

	// Replace semantics: set {s1} then {s1,s2}; old rows not duplicated.
	if err := db.SetProjectMCPServers(ctx, ttx.Tx, tenant, proj.ID, []string{s1.ID}); err != nil {
		t.Fatalf("set project {s1}: %v", err)
	}
	got, err := db.ListProjectMCPServerIDs(ctx, ttx.Tx, tenant, proj.ID)
	if err != nil {
		t.Fatalf("list project: %v", err)
	}
	if len(got) != 1 || got[0] != s1.ID {
		t.Fatalf("project set after {s1} = %v, want [%s]", got, s1.ID)
	}
	if err := db.SetProjectMCPServers(ctx, ttx.Tx, tenant, proj.ID, []string{s1.ID, s2.ID}); err != nil {
		t.Fatalf("set project {s1,s2}: %v", err)
	}
	got, err = db.ListProjectMCPServerIDs(ctx, ttx.Tx, tenant, proj.ID)
	if err != nil {
		t.Fatalf("list project after replace: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("project set after replace = %v, want 2 entries (delete-not-in worked)", got)
	}
	// Replacing with an empty selection clears the set.
	if err := db.SetProjectMCPServers(ctx, ttx.Tx, tenant, proj.ID, nil); err != nil {
		t.Fatalf("set project {}: %v", err)
	}
	got, err = db.ListProjectMCPServerIDs(ctx, ttx.Tx, tenant, proj.ID)
	if err != nil {
		t.Fatalf("list project after clear: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("project set after clear = %v, want empty", got)
	}

	// id-array validation against tenant scope: ids from another tenant
	// are simply absent from the by-ids lookup.
	gotRows, err := db.ListMCPServersByIDs(ctx, ttx.Tx, tenant, []string{s1.ID, "does-not-exist"})
	if err != nil {
		t.Fatalf("list by ids: %v", err)
	}
	if len(gotRows) != 1 || gotRows[0].ID != s1.ID {
		t.Fatalf("by-ids lookup = %d rows, want only s1 (foreign/unknown ids dropped)", len(gotRows))
	}

	// Tenant-default selection.
	if err := db.SetTenantDefaultMCPServers(ctx, ttx.Tx, tenant, []string{s2.ID}); err != nil {
		t.Fatalf("set tenant default: %v", err)
	}
	gotDef, err := db.GetTenantDefaultMCPServers(ctx, ttx.Tx, tenant)
	if err != nil {
		t.Fatalf("get tenant default: %v", err)
	}
	if len(gotDef) != 1 || gotDef[0] != s2.ID {
		t.Fatalf("tenant default = %v, want [%s]", gotDef, s2.ID)
	}

	// Deletion contract: a server still referenced by a project or the
	// tenant default is refused by the DB FK backstop (the service layer
	// reports it as ReferencedError before ever reaching the DELETE). An
	// unreferenced server deletes cleanly.
	if _, err := db.GetMCPServer(ctx, ttx.Tx, tenant, s2.ID); err != nil {
		t.Fatalf("get s2 before delete: %v", err)
	}
	if err := db.SetTenantDefaultMCPServers(ctx, ttx.Tx, tenant, nil); err != nil {
		t.Fatalf("clear tenant default: %v", err)
	}
	if err := db.DeleteMCPServer(ctx, ttx.Tx, tenant, s2.ID); err != nil {
		t.Fatalf("delete s2: %v", err)
	}
	if _, err := db.GetMCPServer(ctx, ttx.Tx, tenant, s2.ID); err != db.ErrNotFound {
		t.Fatalf("get deleted s2 = %v, want ErrNotFound", err)
	}
}
