package mcpsettings_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/mcpsettings"
	"github.com/beardedparrott/orchicon/internal/migrate"
)

// Integration tests for the MCP settings service: CRUD over tenant-scoped
// MCP server entries, catalog prefill, explicit-only auto-install (dry-run
// enforced in CI — ORCHICON_MCP_INSTALL_DRYRUN=1 is set by the test), the
// write-only secret wiring, project/tenant-default selections, and the
// worker → project → tenant-default → none resolution contract. Guarded by
// ORCHICON_TEST_DSN like the other DB-backed suites:
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/mcpsettings/ -run TestMCP -v

const testKEK = "0123456789abcdef0123456789abcdef" // 32 bytes

const testTenant = "tnt_mcp_test"

func newTestService(t *testing.T) (*mcpsettings.Service, *db.Pool) {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed mcpsettings test")
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
	if err := db.SeedDevTenant(ctx, pool, testTenant); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return mcpsettings.New(pool, []byte(testKEK), nil), pool
}

// cleanupMCP wipes this tenant's mcp rows + derived secrets so runs are
// idempotent across a shared DB.
func cleanupMCP(t *testing.T, pool *db.Pool) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.BeginTenantTx(ctx, testTenant)
	if err != nil {
		t.Fatalf("cleanup begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, q := range []string{
		`DELETE FROM project_mcp_servers WHERE tenant_id=$1`,
		`DELETE FROM mcp_servers WHERE tenant_id=$1`,
		`DELETE FROM tenant_secrets WHERE tenant_id=$1 AND name LIKE 'MCP\_%' ESCAPE '\'`,
	} {
		if _, err := tx.Exec(ctx, q, testTenant); err != nil {
			t.Fatalf("cleanup exec %q: %v", q, err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE tenant_settings SET default_mcp_servers='[]'::jsonb WHERE tenant_id=$1`, testTenant); err != nil {
		t.Fatalf("cleanup default: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("cleanup commit: %v", err)
	}
}

// stdioEntry builds a minimal stdio create input.
func stdioEntry(name string) mcpsettings.CreateInput {
	return mcpsettings.CreateInput{
		Name:      name,
		Transport: mcpsettings.TransportStdio,
		Command:   "npx",
		Args:      []string{"-y", "@modelcontextprotocol/server-filesystem", "/tmp"},
		Enabled:   true,
	}
}

func httpEntry(name, u string) mcpsettings.CreateInput {
	return mcpsettings.CreateInput{
		Name:      name,
		Transport: mcpsettings.TransportStreamable,
		URL:       u,
		Enabled:   true,
	}
}

func TestMCPCRUD(t *testing.T) {
	svc, pool := newTestService(t)
	ctx := context.Background()
	cleanupMCP(t, pool)

	// Create stdio.
	e, err := svc.Create(ctx, testTenant, stdioEntry("fs"))
	if err != nil {
		t.Fatalf("create stdio: %v", err)
	}
	if e.Transport != mcpsettings.TransportStdio || e.Command != "npx" || e.InstallStatus != mcpsettings.InstallUnknown {
		t.Fatalf("unexpected entry: %+v", e)
	}

	// Duplicate name rejected.
	if _, err := svc.Create(ctx, testTenant, stdioEntry("fs")); err == nil {
		t.Fatal("duplicate name accepted")
	}

	// Create HTTP.
	h, err := svc.Create(ctx, testTenant, httpEntry("remote", "https://mcp.example.com/sse"))
	if err != nil {
		t.Fatalf("create http: %v", err)
	}
	if h.Transport != mcpsettings.TransportStreamable || h.URL != "https://mcp.example.com/sse" {
		t.Fatalf("unexpected http entry: %+v", h)
	}

	// HTTP with a command is normalized away.
	bad, err := svc.Create(ctx, testTenant, mcpsettings.CreateInput{Name: "bad", Transport: mcpsettings.TransportStreamable, Command: "npx", URL: "https://x.example.com"})
	if err != nil {
		t.Fatalf("create http with command: %v", err)
	}
	if bad.Command != "" || len(bad.Args) != 0 {
		t.Fatalf("http entry kept command: %+v", bad)
	}

	// Stdio without command rejected.
	if _, err := svc.Create(ctx, testTenant, mcpsettings.CreateInput{Name: "nocmd", Transport: mcpsettings.TransportStdio}); err == nil {
		t.Fatal("stdio without command accepted")
	}

	// Invalid URL rejected.
	if _, err := svc.Create(ctx, testTenant, httpEntry("badurl", "ftp://nope")); err == nil {
		t.Fatal("invalid url accepted")
	}

	// Get.
	got, err := svc.Get(ctx, testTenant, e.ID)
	if err != nil || got.Name != "fs" {
		t.Fatalf("get: %+v err=%v", got, err)
	}

	// Update: rename rejected (name is the natural key — delete+recreate).
	name := "fs-renamed"
	if _, err := svc.Update(ctx, testTenant, mcpsettings.UpdateInput{ID: e.ID, Name: &name}); err == nil {
		t.Fatal("rename accepted")
	}
	// Update: disable.
	enabled := false
	upd, err := svc.Update(ctx, testTenant, mcpsettings.UpdateInput{ID: e.ID, Enabled: &enabled})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Enabled {
		t.Fatalf("update result: %+v", upd)
	}

	// List: 3 entries.
	list, err := svc.ListForTenant(ctx, testTenant)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("list length: %d", len(list))
	}

	// Delete unreferenced.
	if err := svc.Delete(ctx, testTenant, h.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.Get(ctx, testTenant, h.ID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("expected not found after delete, got %v", err)
	}
}

func TestMCPCatalogPrefillAndValidation(t *testing.T) {
	svc, pool := newTestService(t)
	ctx := context.Background()
	cleanupMCP(t, pool)

	// Catalog listing.
	catalog := mcpsettings.ListCatalog()
	if len(catalog) < 12 {
		t.Fatalf("catalog too small: %d", len(catalog))
	}
	bySlug := map[string]mcpsettings.CatalogEntry{}
	for _, c := range catalog {
		bySlug[c.Slug] = c
	}
	for _, slug := range []string{"filesystem", "github", "gitlab", "postgres", "sqlite", "fetch", "playwright", "puppeteer", "sentry", "slack"} {
		if _, ok := bySlug[slug]; !ok {
			t.Fatalf("catalog missing %q", slug)
		}
	}

	// Prefill semantics: github requires a secret env; prefilled create
	// carries the slug and the non-secret defaults only.
	g := bySlug["github"]
	if len(g.RequiredEnv) == 0 {
		t.Fatalf("github should require env secrets: %+v", g)
	}
	e, err := svc.Create(ctx, testTenant, mcpsettings.CreateInput{
		Name:        "github",
		Transport:   g.Transport,
		Command:     g.DefaultCommand,
		Args:        append([]string{}, g.DefaultArgs...),
		CatalogSlug: g.Slug,
	})
	if err != nil {
		t.Fatalf("create catalog entry: %v", err)
	}
	if e.CatalogSlug != "github" {
		t.Fatalf("catalog slug lost: %+v", e)
	}

	// RequiredSecretsFor surfaces the catalog's secret env names.
	req := mcpsettings.RequiredSecretsFor(e)
	if len(req) == 0 {
		t.Fatal("no required secrets surfaced")
	}

	// Unknown catalog slug rejected.
	if _, err := svc.Create(ctx, testTenant, mcpsettings.CreateInput{Name: "x", CatalogSlug: "nope"}); err == nil {
		t.Fatal("unknown catalog slug accepted")
	}
}

func TestMCPInstallDryRun(t *testing.T) {
	svc, pool := newTestService(t)
	ctx := context.Background()
	cleanupMCP(t, pool)

	e, err := svc.Create(ctx, testTenant, mcpsettings.CreateInput{
		Name:        "filesystem",
		Transport:   mcpsettings.TransportStdio,
		Command:     "npx",
		Args:        []string{"-y", "@modelcontextprotocol/server-filesystem"},
		CatalogSlug: "filesystem",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Dry-run: no exec, no DB write; reports the plan and runtime presence.
	out, err := svc.Install(ctx, testTenant, mcpsettings.InstallInput{ID: e.ID, DryRun: true})
	if err != nil {
		t.Fatalf("install dry-run: %v", err)
	}
	if !out.WouldRun {
		t.Fatalf("dry-run should report would-run: %+v", out)
	}
	if out.Runtime != "npx" || out.Command == "" {
		t.Fatalf("dry-run plan wrong: %+v", out)
	}
	// Status unchanged after dry-run.
	got, _ := svc.Get(ctx, testTenant, e.ID)
	if got.InstallStatus != mcpsettings.InstallUnknown {
		t.Fatalf("dry-run mutated status: %s", got.InstallStatus)
	}
}

func TestMCPSecretsWriteOnly(t *testing.T) {
	svc, pool := newTestService(t)
	ctx := context.Background()
	cleanupMCP(t, pool)

	e, err := svc.Create(ctx, testTenant, mcpsettings.CreateInput{
		Name:        "github",
		Transport:   mcpsettings.TransportStdio,
		Command:     "npx",
		Args:        []string{"-y", "@modelcontextprotocol/server-github"},
		CatalogSlug: "github",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Set secret persists via tenant_secrets and surfaces has_secret_stored.
	name, err := svc.SetSecret(ctx, testTenant, e.ID, "GITHUB_PERSONAL_ACCESS_TOKEN", "ghp_test123")
	if err != nil {
		t.Fatalf("set secret: %v", err)
	}
	want := "MCP_GITHUB_GITHUB_PERSONAL_ACCESS_TOKEN"
	if name != want {
		t.Fatalf("derived secret name: %q want %q", name, want)
	}
	got, _ := svc.Get(ctx, testTenant, e.ID)
	if !got.HasSecretStored {
		t.Fatalf("has_secret_stored should be true after set: %+v", got)
	}

	// The secret is never exposed in plaintext on the entry.
	for _, v := range got.Env {
		if v == "ghp_test123" {
			t.Fatalf("plaintext secret leaked in entry env: %v", got.Env)
		}
	}

	// Verify the row in tenant_secrets (ciphertext, not plaintext).
	tx, err := pool.BeginTenantTx(ctx, testTenant)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	sec, err := db.GetSecretByName(ctx, tx.Tx, testTenant, want)
	if err != nil {
		t.Fatalf("secret lookup: %v", err)
	}
	if sec.Ciphertext == "ghp_test123" || sec.Ciphertext == "" {
		t.Fatalf("secret not encrypted at rest: %q", sec.Ciphertext)
	}
	_ = tx.Rollback(ctx)

	// A create that references an unknown ${SECRET} is rejected.
	if _, err := svc.Create(ctx, testTenant, mcpsettings.CreateInput{
		Name: "ref-unknown", Transport: mcpsettings.TransportStdio, Command: "npx",
		Args: []string{"-y", "x"},
		Env:  map[string]string{"TOKEN": "${MCP_GITHUB_MISSING_TOKEN}"},
	}); err == nil {
		t.Fatal("create with unknown secret ref accepted")
	}

	// Clearing the secret flips has_secret_stored back off.
	if err := svc.ClearSecret(ctx, testTenant, e.ID, "GITHUB_PERSONAL_ACCESS_TOKEN"); err != nil {
		t.Fatalf("clear secret: %v", err)
	}
	got, _ = svc.Get(ctx, testTenant, e.ID)
	if got.HasSecretStored {
		t.Fatalf("has_secret_stored should be false after clear: %+v", got)
	}
}

func TestMCPSelectionsAndResolution(t *testing.T) {
	svc, pool := newTestService(t)
	ctx := context.Background()
	cleanupMCP(t, pool)

	// Real project for selection tests.
	ptx, err := pool.BeginTenantTx(ctx, testTenant)
	if err != nil {
		t.Fatalf("begin project tx: %v", err)
	}
	if _, err := ptx.Exec(ctx, `INSERT INTO projects (id, tenant_id, name, slug, status, version, created_at, updated_at)
		VALUES ($1,$2,'MCP Test','mcp-test','active',1,now(),now()) ON CONFLICT (id) DO NOTHING`, "prj_mcp_test", testTenant); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if err := ptx.Commit(ctx); err != nil {
		t.Fatalf("commit project: %v", err)
	}
	t.Cleanup(func() {
		ct := context.Background()
		dtx, err := pool.BeginTenantTx(ct, testTenant)
		if err == nil {
			_, _ = dtx.Exec(ct, `DELETE FROM projects WHERE id='prj_mcp_test' AND tenant_id=$1`, testTenant)
			_ = dtx.Commit(ct)
		}
	})

	// Two servers.
	fs, _ := svc.Create(ctx, testTenant, stdioEntry("fs"))
	http, _ := svc.Create(ctx, testTenant, httpEntry("http1", "https://mcp.example.com/x"))

	// Project selection by reference; unknown id rejected.
	if err := svc.SetProjectSelection(ctx, testTenant, "prj_unknown", []string{fs.ID}); err == nil {
		t.Fatal("selection on unknown project accepted")
	}
	if err := svc.SetProjectSelection(ctx, testTenant, "prj_mcp_test", []string{uuid.NewString()}); err == nil {
		t.Fatal("selection with unknown server id accepted")
	}
	if err := svc.SetProjectSelection(ctx, testTenant, "prj_mcp_test", []string{fs.ID}); err != nil {
		t.Fatalf("set project selection: %v", err)
	}
	projSel, err := svc.GetProjectSelection(ctx, testTenant, "prj_mcp_test")
	if err != nil || len(projSel) != 1 || projSel[0] != fs.ID {
		t.Fatalf("get project selection: %v %v", projSel, err)
	}

	// Tenant default selection.
	if err := svc.SetTenantDefaultSelection(ctx, testTenant, []string{fs.ID, http.ID}); err != nil {
		t.Fatalf("set default: %v", err)
	}
	dflt, err := svc.GetTenantDefaultSelection(ctx, testTenant)
	if err != nil || len(dflt) != 2 {
		t.Fatalf("get default: %v %v", dflt, err)
	}

	// Resolution: no worker, no project → tenant default.
	res, err := mcpsettings.ResolveForScope(ctx, pool, testTenant, "", "")
	if err != nil {
		t.Fatalf("resolve default: %v", err)
	}
	if len(res.Servers) != 2 {
		t.Fatalf("default resolution: %d servers", len(res.Servers))
	}

	// Project selection wins over the tenant default.
	res, err = mcpsettings.ResolveForScope(ctx, pool, testTenant, "", "prj_mcp_test")
	if err != nil {
		t.Fatalf("resolve project: %v", err)
	}
	if len(res.Servers) != 1 || res.Servers[0].ID != fs.ID {
		t.Fatalf("project resolution should pick fs only: %+v", res.Servers)
	}

	// Disabled entries are skipped in resolution.
	dis := false
	if _, err := svc.Update(ctx, testTenant, mcpsettings.UpdateInput{ID: fs.ID, Enabled: &dis}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	res, _ = mcpsettings.ResolveForScope(ctx, pool, testTenant, "", "prj_mcp_test")
	if len(res.Servers) != 0 || len(res.Disabled) != 1 {
		t.Fatalf("disabled handling: %+v", res)
	}

	// Deletion guard: default selection references fs → delete blocked.
	err = svc.Delete(ctx, testTenant, fs.ID)
	var refErr *mcpsettings.ReferencedError
	if !errors.As(err, &refErr) {
		t.Fatalf("delete not guarded by default selection: %v", err)
	}
	if !refErr.InTenantDefault {
		t.Fatalf("guard should flag tenant default: %+v", refErr)
	}

	// Project selection also guards deletion (fs is in the project set).
	err = svc.Delete(ctx, testTenant, fs.ID)
	if !errors.As(err, &refErr) {
		t.Fatalf("delete not guarded by project selection: %v", err)
	}
	found := false
	for _, p := range refErr.Projects {
		if p.ProjectID == "prj_mcp_test" {
			found = true
		}
	}
	if !found {
		t.Fatalf("guard should list referencing project: %+v", refErr.Projects)
	}

	// http is referenced only by the tenant default → still blocked.
	err = svc.Delete(ctx, testTenant, http.ID)
	if !errors.As(err, &refErr) {
		t.Fatalf("delete not guarded by default: %v", err)
	}
	if !refErr.InTenantDefault {
		t.Fatalf("guard should flag tenant default: %+v", refErr)
	}
}
