package mcpsettings_test

import (
	"context"
	"errors"
	"os"
	"testing"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/mcpsettings"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// Integration tests for the MCP settings service: CRUD over OWNER-SCOPED
// MCP definitions, catalog prefill, explicit-only auto-install (dry-run
// enforced in CI — ORCHICON_MCP_INSTALL_DRYRUN=1 is set by the test), the
// write-only secret wiring, and the ONE union resolution across scopes.
// Guarded by ORCHICON_TEST_DSN like the other DB-backed suites:
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/mcpsettings/ -run TestMCP -v

const testKEK = "0123456789abcdef0123456789abcdef" // 32 bytes

// testTenant is UNIQUE PER RUN. A fixed id makes two concurrent test
// processes contend on the same tenants row (mcp_servers' FK takes a
// key-share lock on it) and DEADLOCK instead of failing.
var testTenant = "tnt_mcp_test_" + db.NewID()[10:22]

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
	// mcp_servers is owner-scoped (no join table, no tenant-default column):
	// the rows themselves are the whole selection.
	for _, q := range []string{
		`DELETE FROM mcp_servers WHERE tenant_id=$1`,
		`DELETE FROM tenant_secrets WHERE tenant_id=$1 AND name LIKE 'MCP\_%' ESCAPE '\'`,
	} {
		if _, err := tx.Exec(ctx, q, testTenant); err != nil {
			t.Fatalf("cleanup exec %q: %v", q, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("cleanup commit: %v", err)
	}
}

// seedOwner creates a project and a conversation in the test tenant and
// returns their ids plus a cleanup. Every definition needs an OWNER.
func seedOwner(t *testing.T, pool *db.Pool) (projectID, conversationID string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.BeginTenantTx(ctx, testTenant)
	if err != nil {
		t.Fatalf("begin owner tx: %v", err)
	}
	suffix := db.NewID()[10:22]
	proj, err := db.CreateProject(ctx, tx.Tx, db.ProjectRow{
		ID: "prj_mcp_" + suffix, TenantID: testTenant,
		Name: "MCP Test", Slug: "mcp-test-" + suffix,
		Status: "active", Goals: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	conv, err := db.CreateConversation(ctx, tx.Tx, db.ConversationRow{
		ID: "conv_mcp_" + suffix, TenantID: testTenant, Title: "MCP Test",
	})
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit owner tx: %v", err)
	}
	return proj.ID, conv.ID
}

// stdioEntry builds a minimal stdio create input owned by projectID.
func stdioEntry(name, projectID string) mcpsettings.CreateInput {
	return mcpsettings.CreateInput{
		Name:      name,
		ProjectID: projectID,
		Transport: mcpsettings.TransportStdio,
		Command:   "npx",
		Args:      []string{"-y", "@modelcontextprotocol/server-filesystem", "/tmp"},
		Enabled:   true,
	}
}

func httpEntry(name, u, conversationID string) mcpsettings.CreateInput {
	return mcpsettings.CreateInput{
		Name:           name,
		ConversationID: conversationID,
		Transport:      mcpsettings.TransportStreamable,
		URL:            u,
		Enabled:        true,
	}
}

// resolverFor builds the storage-backed resolver for the test tenant.
func resolverFor(pool *db.Pool) *mcpsettings.Resolver {
	return mcpsettings.NewResolver(pool)
}

func TestMCPCRUD(t *testing.T) {
	svc, pool := newTestService(t)
	ctx := context.Background()
	cleanupMCP(t, pool)
	projID, _ := seedOwner(t, pool)

	// Create stdio.
	e, err := svc.Create(ctx, testTenant, stdioEntry("fs", projID))
	if err != nil {
		t.Fatalf("create stdio: %v", err)
	}
	if e.Transport != mcpsettings.TransportStdio || e.Command != "npx" || e.InstallStatus != mcpsettings.InstallUnknown {
		t.Fatalf("unexpected entry: %+v", e)
	}

	// Duplicate name rejected.
	// The definition carries its owner.
	if e.ProjectID != projID || e.ConversationID != "" {
		t.Fatalf("owner not carried on the entry: %+v", e)
	}
	// A duplicate name in the SAME scope is rejected.
	if _, err := svc.Create(ctx, testTenant, stdioEntry("fs", projID)); err == nil {
		t.Fatal("duplicate name accepted")
	}

	// Create HTTP (conversation-owned).
	convID := ""
	_, convID = seedOwner(t, pool)
	_ = convID
	h, err := svc.Create(ctx, testTenant, httpEntry("remote", "https://mcp.example.com/sse", convID))
	if err != nil {
		t.Fatalf("create http: %v", err)
	}
	if h.Transport != mcpsettings.TransportStreamable || h.URL != "https://mcp.example.com/sse" {
		t.Fatalf("unexpected http entry: %+v", h)
	}

	// HTTP with a command is normalized away.
	bad, err := svc.Create(ctx, testTenant, mcpsettings.CreateInput{Name: "bad", ProjectID: projID, Transport: mcpsettings.TransportStreamable, Command: "npx", URL: "https://x.example.com"})
	if err != nil {
		t.Fatalf("create http with command: %v", err)
	}
	if bad.Command != "" || len(bad.Args) != 0 {
		t.Fatalf("http entry kept command: %+v", bad)
	}

	// Stdio without command rejected.
	if _, err := svc.Create(ctx, testTenant, mcpsettings.CreateInput{Name: "nocmd", ProjectID: projID, Transport: mcpsettings.TransportStdio}); err == nil {
		t.Fatal("stdio without command accepted")
	}

	// Invalid URL rejected.
	if _, err := svc.Create(ctx, testTenant, httpEntry("badurl", "ftp://nope", convID)); err == nil {
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
	projID, _ := seedOwner(t, pool)

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
		ProjectID:   projID,
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
	if _, err := svc.Create(ctx, testTenant, mcpsettings.CreateInput{Name: "x", ProjectID: projID, CatalogSlug: "nope"}); err == nil {
		t.Fatal("unknown catalog slug accepted")
	}
}

func TestMCPInstallDryRun(t *testing.T) {
	svc, pool := newTestService(t)
	ctx := context.Background()
	cleanupMCP(t, pool)
	projID, _ := seedOwner(t, pool)

	e, err := svc.Create(ctx, testTenant, mcpsettings.CreateInput{
		Name:        "filesystem",
		ProjectID:   projID,
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
	projID, _ := seedOwner(t, pool)

	e, err := svc.Create(ctx, testTenant, mcpsettings.CreateInput{
		Name:        "github",
		ProjectID:   projID,
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
		Name: "ref-unknown", ProjectID: projID, Transport: mcpsettings.TransportStdio, Command: "npx",
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

// TestMCPUnionResolutionFourCases is the FOUR-CASE UNION test for BOTH
// scopes: project-only, own-only, both, neither. The resolution is a UNION
// now, not a precedence chain — project-owned and scope-owned definitions
// are BOTH present when both exist, and neither is overridden.
func TestMCPUnionResolutionFourCases(t *testing.T) {
	svc, pool := newTestService(t)
	ctx := context.Background()
	cleanupMCP(t, pool)
	projID, convID := seedOwner(t, pool)
	res := resolverFor(pool)
	sctx := tenant.WithID(ctx, testTenant)

	// NEITHER: no definitions anywhere.
	got, err := res.ResolveScope(sctx, mcpclient.ScopeRef{Kind: mcpclient.ScopeProject, ProjectID: projID})
	if err != nil {
		t.Fatalf("resolve (neither, project): %v", err)
	}
	if len(got.Servers) != 0 {
		t.Fatalf("neither case resolved %+v, want nothing", got.Servers)
	}
	got, err = res.ResolveScope(sctx, mcpclient.ScopeRef{Kind: mcpclient.ScopeConversation, ProjectID: projID, ConversationID: convID})
	if err != nil {
		t.Fatalf("resolve (neither, conversation): %v", err)
	}
	if len(got.Servers) != 0 {
		t.Fatalf("neither case (conversation) resolved %+v, want nothing", got.Servers)
	}

	// PROJECT-ONLY.
	projSrv, err := svc.Create(ctx, testTenant, stdioEntry("project-srv", projID))
	if err != nil {
		t.Fatalf("create project-owned: %v", err)
	}
	got, _ = res.ResolveScope(sctx, mcpclient.ScopeRef{Kind: mcpclient.ScopeProject, ProjectID: projID})
	if !hasServer(got, projSrv.ID) || len(got.Servers) != 1 {
		t.Fatalf("project-only case = %+v, want just the project's server", got.Servers)
	}
	if got.Servers[0].From != mcpclient.ScopeProject || got.Servers[0].FromID != "project:"+projID {
		t.Fatalf("project provenance lost: %+v", got.Servers[0])
	}
	got, _ = res.ResolveScope(sctx, mcpclient.ScopeRef{Kind: mcpclient.ScopeConversation, ProjectID: projID, ConversationID: convID})
	if !hasServer(got, projSrv.ID) {
		t.Fatalf("conversation scope did not inherit the project's server: %+v", got.Servers)
	}

	// OWN-ONLY (a conversation-owned definition with no project one would be
	// own-only; here it is the second half of BOTH, so use a fresh project
	// scope to isolate it).
	otherProj, _ := seedOwner(t, pool)
	ownSrv, err := svc.Create(ctx, testTenant, httpEntry("conv-srv", "https://mcp.example.com/own", convID))
	if err != nil {
		t.Fatalf("create conversation-owned: %v", err)
	}
	got, _ = res.ResolveScope(sctx, mcpclient.ScopeRef{Kind: mcpclient.ScopeConversation, ProjectID: otherProj, ConversationID: convID})
	if !hasServer(got, ownSrv.ID) || len(got.Servers) != 1 {
		t.Fatalf("own-only case = %+v, want just the conversation's server", got.Servers)
	}
	if got.Servers[0].From != mcpclient.ScopeConversation || got.Servers[0].FromID != "conversation:"+convID {
		t.Fatalf("conversation provenance lost: %+v", got.Servers[0])
	}

	// BOTH: the conversation scope sees the project's AND its own.
	got, err = res.ResolveScope(sctx, mcpclient.ScopeRef{Kind: mcpclient.ScopeConversation, ProjectID: projID, ConversationID: convID})
	if err != nil {
		t.Fatalf("resolve (both): %v", err)
	}
	if !hasServer(got, projSrv.ID) || !hasServer(got, ownSrv.ID) {
		t.Fatalf("both case = %+v, want the project's AND the conversation's", got.Servers)
	}
	if len(got.Servers) != 2 {
		t.Fatalf("both case = %d servers, want exactly 2: %+v", len(got.Servers), got.Servers)
	}
	// Order-stable: project rows (name ASC) first, then the conversation's.
	if got.Servers[0].EntryID != projSrv.ID {
		t.Errorf("union order = %+v, want the project row first", got.Servers)
	}

	// A DISABLED definition is reported, never resolved.
	dis := false
	if _, err := svc.Update(ctx, testTenant, mcpsettings.UpdateInput{ID: projSrv.ID, Enabled: &dis}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	got, _ = res.ResolveScope(sctx, mcpclient.ScopeRef{Kind: mcpclient.ScopeConversation, ProjectID: projID, ConversationID: convID})
	if hasServer(got, projSrv.ID) {
		t.Fatalf("a disabled definition resolved: %+v", got.Servers)
	}
	if len(got.Disabled) != 1 || got.Disabled[0] != projSrv.ID {
		t.Fatalf("disabled reporting = %v, want [%s]", got.Disabled, projSrv.ID)
	}
}

func hasServer(r mcpclient.Resolution, id string) bool {
	for _, s := range r.Servers {
		if s.EntryID == id || s.Spec.ID == id {
			return true
		}
	}
	return false
}

// TestMCPCreateOwnerValidation pins the owner XOR and the existence checks:
// both owners set, neither set, an unknown project, and an unknown
// conversation are all rejected; an existing project and an existing
// conversation are both accepted.
func TestMCPCreateOwnerValidation(t *testing.T) {
	svc, pool := newTestService(t)
	ctx := context.Background()
	cleanupMCP(t, pool)
	projID, convID := seedOwner(t, pool)

	base := mcpsettings.CreateInput{Name: "srv", Transport: mcpsettings.TransportStdio, Command: "npx"}

	// Neither owner.
	if _, err := svc.Create(ctx, testTenant, base); err == nil {
		t.Error("a create with NO owner was accepted")
	}
	// Both owners.
	both := base
	both.ProjectID = projID
	both.ConversationID = convID
	if _, err := svc.Create(ctx, testTenant, both); err == nil {
		t.Error("a create with BOTH owners was accepted")
	}
	// Unknown project.
	badProj := base
	badProj.ProjectID = "prj_does_not_exist"
	if _, err := svc.Create(ctx, testTenant, badProj); err == nil {
		t.Error("a create against an UNKNOWN project was accepted")
	}
	// Unknown conversation.
	badConv := base
	badConv.ConversationID = "conv_does_not_exist"
	if _, err := svc.Create(ctx, testTenant, badConv); err == nil {
		t.Error("a create against an UNKNOWN conversation was accepted")
	}
	// A valid conversation owner is accepted.
	if _, err := svc.Create(ctx, testTenant, mcpsettings.CreateInput{
		Name: "conv-owned", ConversationID: convID,
		Transport: mcpsettings.TransportStdio, Command: "npx",
	}); err != nil {
		t.Errorf("a create with a VALID conversation owner failed: %v", err)
	}
}

// TestMCPOwnerIsImmutable pins that the scope cannot be changed by an update.
func TestMCPOwnerIsImmutable(t *testing.T) {
	svc, pool := newTestService(t)
	ctx := context.Background()
	cleanupMCP(t, pool)
	projID, convID := seedOwner(t, pool)

	e, err := svc.Create(ctx, testTenant, stdioEntry("srv", projID))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Echoing the SAME owner is fine.
	if _, err := svc.Update(ctx, testTenant, mcpsettings.UpdateInput{ID: e.ID, ProjectID: projID}); err != nil {
		t.Fatalf("update echoing the same owner failed: %v", err)
	}
	// A DIFFERENT owner is rejected.
	if _, err := svc.Update(ctx, testTenant, mcpsettings.UpdateInput{ID: e.ID, ConversationID: convID}); err == nil {
		t.Error("an owner change was accepted; the scope must be immutable")
	}
}

// TestMCPDeleteIsUnguardedAndPurgesSecrets pins the removal of the reference
// guard: an owner-scoped definition deletes cleanly (its owner is its only
// reference) and its derived secrets go with it.
func TestMCPDeleteIsUnguardedAndPurgesSecrets(t *testing.T) {
	svc, pool := newTestService(t)
	ctx := context.Background()
	cleanupMCP(t, pool)
	projID, _ := seedOwner(t, pool)

	e, err := svc.Create(ctx, testTenant, mcpsettings.CreateInput{
		Name: "github", ProjectID: projID,
		Transport: mcpsettings.TransportStdio, Command: "npx",
		Args: []string{"-y", "@modelcontextprotocol/server-github"}, CatalogSlug: "github",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	secretName, err := svc.SetSecret(ctx, testTenant, e.ID, "GITHUB_PERSONAL_ACCESS_TOKEN", "ghp_test")
	if err != nil {
		t.Fatalf("set secret: %v", err)
	}
	if err := svc.Delete(ctx, testTenant, e.ID); err != nil {
		t.Fatalf("delete of an owned definition was refused: %v", err)
	}
	if _, err := svc.Get(ctx, testTenant, e.ID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("deleted definition still present: %v", err)
	}
	tx, err := pool.BeginTenantTx(ctx, testTenant)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := db.GetSecretByName(ctx, tx.Tx, testTenant, secretName); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("derived secret %q survived the delete: %v", secretName, err)
	}
}
