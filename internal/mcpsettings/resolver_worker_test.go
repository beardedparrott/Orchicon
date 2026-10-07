package mcpsettings_test

// resolver_worker_test.go — AC 2 and AC 4 (DB-backed).
//
// AC 2: a worker version's inline `permissions.mcp_servers` specs actually reach
// the resolver and are applied, unioned with the project's own, whether the
// caller CARRIES them (a pinned dispatch: ScopeRef.OwnPermissions, from
// ExecutionManifest.Permissions) or not (the storage fallback).
//
// AC 4: a PUBLISHED version's inline specs cannot be edited — asserted against
// db.UpdateDraftVersion's `WHERE status = 'draft'` clause, the property that is
// the whole reason worker definitions live inline rather than as owner rows.

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/mcpsettings"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// workerUnionFixture is the shared seed: one project, one project-owned MCP
// definition, one worker with a published version carrying a version-only inline
// spec. It returns the ids and the two server ids the assertions key on.
type workerUnionFixture struct {
	tenantID     string
	projectID    string
	workerID     string
	projectSrvID string
	versionSrvID string
	perms        []byte
}

const (
	fixtureProjectSrv = "proj-srv"
	fixtureVersionSrv = "ver-srv"
)

func seedWorkerUnion(t *testing.T, ctx context.Context, pool *db.Pool) workerUnionFixture {
	t.Helper()
	tenantID := "tnt_workerunion_" + db.NewID()[10:22]
	if err := db.SeedDevTenant(ctx, pool, tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer ttx.Rollback(ctx)

	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: tenantID, Name: "Worker Union",
		Slug: "worker-union-" + db.NewID()[10:22], Status: "active", Goals: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	workerID := "w-" + db.NewID()[10:22]
	if _, err := db.CreateWorker(ctx, ttx.Tx, db.WorkerRow{
		ID: workerID, TenantID: tenantID, Name: "Worker Union " + workerID[:8],
		Slug: workerID, Status: domain.WorkerPublished,
	}); err != nil {
		t.Fatalf("create worker: %v", err)
	}
	perms, _ := json.Marshal(map[string]any{
		"mcp_servers": []map[string]any{{
			"id": fixtureVersionSrv, "type": "stdio", "command": []string{"npx", "-y", fixtureVersionSrv},
		}},
	})
	if _, err := db.CreateWorkerVersion(ctx, ttx.Tx, db.WorkerVersionRow{
		ID: db.NewID(), TenantID: tenantID, WorkerID: workerID, Version: 1,
		Status: domain.WorkerVersionPublished, ModelRef: "orchicon/deepseek/deepseek-v4-flash",
		Permissions: perms,
	}); err != nil {
		t.Fatalf("create worker version: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit setup: %v", err)
	}

	svc := mcpsettings.New(pool, nil, nil)
	projSrv, err := svc.Create(ctx, tenantID, mcpsettings.CreateInput{
		Name: fixtureProjectSrv, ProjectID: proj.ID,
		Transport: mcpsettings.TransportStdio, Command: "npx",
		Args: []string{"-y", fixtureProjectSrv}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create project-owned definition: %v", err)
	}
	return workerUnionFixture{
		tenantID: tenantID, projectID: proj.ID, workerID: workerID,
		projectSrvID: projSrv.ID, versionSrvID: fixtureVersionSrv, perms: perms,
	}
}

// findServer returns the served entry with the given id, and whether it exists.
func findServer(res mcpclient.Resolution, id string) (mcpclient.ScopedServer, bool) {
	for _, s := range res.Servers {
		if s.Spec.ID == id || s.EntryID == id {
			return s, true
		}
	}
	return mcpclient.ScopedServer{}, false
}

// AC 2 (pinned): the version's inline specs arrive as ScopeRef.OwnPermissions —
// the ExecutionManifest.Permissions path — and the union contains BOTH halves.
func TestResolveWorkerUnionsCarriedVersionSpecs(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed worker-union test")
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
	f := seedWorkerUnion(t, ctx, pool)

	res, err := mcpsettings.NewResolver(pool).ResolveScope(
		tenant.WithID(ctx, f.tenantID),
		mcpclient.ScopeRef{
			Kind: mcpclient.ScopeWorker, ProjectID: f.projectID,
			WorkerID: f.workerID, Version: 1, OwnPermissions: f.perms,
		})
	if err != nil {
		t.Fatalf("ResolveScope(worker): %v", err)
	}

	// The version-only spec is present, with worker provenance naming the
	// PINNED version.
	verSrv, ok := findServer(res, fixtureVersionSrv)
	if !ok {
		t.Fatalf("the version's inline spec did not reach the resolver: %+v", idsOf(res))
	}
	if verSrv.From != mcpclient.ScopeWorker {
		t.Errorf("version spec provenance = %q, want %q", verSrv.From, mcpclient.ScopeWorker)
	}
	if verSrv.FromID != "inline:"+f.workerID+"@1" {
		t.Errorf("version spec FromID = %q, want inline:%s@1", verSrv.FromID, f.workerID)
	}
	// The project half is present too — this is the UNION, not the version alone.
	// (A project definition's Spec.ID is its mcp_servers ROW id.)
	projSrv, ok := findServer(res, f.projectSrvID)
	if !ok {
		t.Fatalf("the project-owned spec is missing from the union: %+v", idsOf(res))
	}
	if projSrv.From != mcpclient.ScopeProject {
		t.Errorf("project spec provenance = %q, want %q", projSrv.From, mcpclient.ScopeProject)
	}
	if projSrv.FromID != "project:"+f.projectID {
		t.Errorf("project spec FromID = %q, want project:%s", projSrv.FromID, f.projectID)
	}
	// ORDER: project rows first, then the own set (the documented contract).
	if len(res.Servers) < 2 || res.Servers[0].Spec.ID != f.projectSrvID {
		t.Errorf("union order = %+v, want the project-owned row first", idsOf(res))
	}
}

// AC 2 (fallback): with NO carried bytes the storage read holds — a direct /
// TUI / ContinueSession caller must keep resolving the latest published version.
func TestResolveWorkerFallsBackToStoredVersionSpecs(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed worker-union test")
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
	f := seedWorkerUnion(t, ctx, pool)

	res, err := mcpsettings.NewResolver(pool).ResolveScope(
		tenant.WithID(ctx, f.tenantID),
		mcpclient.ScopeRef{
			Kind: mcpclient.ScopeWorker, ProjectID: f.projectID,
			WorkerID: f.workerID, // no Version, no OwnPermissions
		})
	if err != nil {
		t.Fatalf("ResolveScope(worker, storage path): %v", err)
	}
	if _, ok := findServer(res, fixtureVersionSrv); !ok {
		t.Fatalf("the stored version's inline spec did not resolve: %+v", idsOf(res))
	}
	if _, ok := findServer(res, f.projectSrvID); !ok {
		t.Fatalf("the project half is missing: %+v", idsOf(res))
	}
	// Unpinned provenance keeps child 1's shape (no "@<n>").
	verSrv, _ := findServer(res, fixtureVersionSrv)
	if verSrv.FromID != "inline:"+f.workerID {
		t.Errorf("unpinned FromID = %q, want inline:%s", verSrv.FromID, f.workerID)
	}
}

// AC 4: a PUBLISHED version's inline specs are immutable. db.UpdateDraftVersion
// is the only writer of a version row's permissions and it gates on
// `AND status = 'draft'`, so the update must match NO row (ErrNotFound) and the
// stored permissions must be byte-identical afterwards.
func TestPublishedVersionInlineSpecsAreImmutable(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed immutability test")
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
	f := seedWorkerUnion(t, ctx, pool)

	tx, err := pool.BeginTenantTx(ctx, f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)

	published, err := db.GetLatestWorkerVersion(ctx, tx.Tx, f.tenantID, f.workerID, true)
	if err != nil {
		t.Fatalf("get published version: %v", err)
	}
	if published.Status != domain.WorkerVersionPublished {
		t.Fatalf("fixture version status = %q, want published", published.Status)
	}

	// Attempt to overwrite the PUBLISHED row's inline specs through the one
	// writer. The WHERE clause matches no row.
	evil, _ := json.Marshal(map[string]any{
		"mcp_servers": []map[string]any{{"id": "tampered", "type": "stdio", "command": []string{"/bin/tampered"}}},
	})
	published.Permissions = evil
	if _, err := db.UpdateDraftVersion(ctx, tx.Tx, published); err != db.ErrNotFound {
		t.Fatalf("editing a published version's permissions returned %v, want db.ErrNotFound", err)
	}

	// Re-read: the stored specs are UNCHANGED. Compare SEMANTICALLY (jsonb
	// normalises key order, so a byte comparison would be a false failure): the
	// stored permissions must still carry the published version's definition and
	// NOT the tampered one.
	after, err := db.GetLatestWorkerVersion(ctx, tx.Tx, f.tenantID, f.workerID, true)
	if err != nil {
		t.Fatalf("re-read published version: %v", err)
	}
	if string(after.Permissions) == string(evil) {
		t.Fatalf("the published version's permissions were overwritten: %s", after.Permissions)
	}
	got := db.MCPServerRefsFromPermissions(after.Permissions)
	if len(got) != 1 || got[0] != fixtureVersionSrv {
		t.Fatalf("published inline specs = %v, want exactly [%s]", got, fixtureVersionSrv)
	}
}
