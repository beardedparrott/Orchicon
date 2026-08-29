package askorchicon

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// DB-backed tests for the create_worker / update_worker tool paths. They
// guard the ghost-record fix: toolCreateWorker must produce a draft
// version-1 row (with model_ref/runtime_ref persisted), a worker.created
// audit row, and a publishable worker — not the header-only husk the old
// implementation created. Skipped unless ORCHICON_TEST_DSN points at a
// disposable database (the migrations are applied on every run):
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/askorchicon/ -run 'TestToolCreateWorker|TestToolUpdateWorker' -v

func toolGhostTestPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed ask-orchicon worker/workflow tool tests")
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
	// Tools called directly (not via NewToolRegistry) need the package
	// logger initialized for post-commit side effects.
	if toolLogger == nil {
		toolLogger = slog.Default()
	}
	return pool
}

// toolGhostEnv returns a fresh tenant, a tenant-scoped ctx carrying a
// seeded identity (the audit FK target), and the pool — one tenant per
// test so audit rows from sibling tests never bleed across.
func toolGhostEnv(t *testing.T) (*db.Pool, context.Context, string) {
	t.Helper()
	pool := toolGhostTestPool(t)
	tenantID := "tnt_tool_" + strings.ToLower(db.NewID())
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	ident, _, err := db.GetOrCreateIdentity(ctx, ttx.Tx, tenantID,
		"tool-ghost-test-"+strings.ToLower(db.NewID()), "Tool Ghost Test", "user")
	if err != nil {
		t.Fatalf("ensure identity: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit identity: %v", err)
	}
	ctx = tenant.WithID(context.Background(), tenantID)
	ctx = auth.WithIdentity(ctx, auth.ResolvedIdentity{
		IdentityID: ident.ID,
		TenantID:   tenantID,
		Subject:    ident.Subject,
		AuthMethod: "oidc",
		IsAdmin:    true,
	})
	return pool, ctx, tenantID
}

// auditRowCount counts audit rows for (action, targetID) in the tenant.
func auditRowCount(t *testing.T, pool *db.Pool, tenantID, action, targetID string) int {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	var n int
	if err := ttx.Tx.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE action = $1 AND target_id = $2`,
		action, targetID).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}

// createWorkerViaTool invokes the real toolCreateWorker and decodes the
// flat response (worker-row JSON + version/version_id extras).
func createWorkerViaTool(t *testing.T, ctx context.Context, pool *db.Pool, args string) (db.WorkerRow, map[string]any) {
	t.Helper()
	raw, err := toolCreateWorker(ctx, pool, json.RawMessage(args))
	if err != nil {
		t.Fatalf("toolCreateWorker: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var w db.WorkerRow
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("re-encode response: %v", err)
	}
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatalf("decode worker row: %v", err)
	}
	return w, resp
}

func TestToolCreateWorkerCreatesDraftV1(t *testing.T) {
	pool, ctx, tenantID := toolGhostEnv(t)
	w, resp := createWorkerViaTool(t, ctx, pool,
		`{"name":"Ghost Buster","purpose":"retire ghost records","model_ref":"opencode-go/deepseek-v4-flash","runtime_ref":"opencode","description":"tool created","version_note":"initial draft"}`)
	if w.ID == "" {
		t.Fatal("response missing worker ID")
	}
	if v, _ := resp["version"].(float64); int(v) != 1 {
		t.Fatalf("response version = %v, want 1", resp["version"])
	}
	if vid, _ := resp["version_id"].(string); vid == "" {
		t.Fatal("response missing version_id")
	}
	if w.Status != "draft" || w.CurrentVersion != 0 {
		t.Fatalf("worker = %s/%d, want draft/0", w.Status, w.CurrentVersion)
	}
	if w.Slug != "ghost-buster" {
		t.Fatalf("slug = %q, want ghost-buster", w.Slug)
	}
	if w.Purpose != "retire ghost records" {
		t.Fatalf("purpose = %q", w.Purpose)
	}

	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	ver, err := db.GetLatestWorkerVersion(ctx, ttx.Tx, tenantID, w.ID, false)
	if err != nil {
		t.Fatalf("get latest version: %v", err)
	}
	if ver.Version != 1 || ver.Status != "draft" {
		t.Fatalf("version = %d (%s), want 1 (draft)", ver.Version, ver.Status)
	}
	if ver.ModelRef != "opencode-go/deepseek-v4-flash" || ver.RuntimeRef != "opencode" {
		t.Fatalf("refs not persisted: model=%q runtime=%q", ver.ModelRef, ver.RuntimeRef)
	}
	if string(ver.ContextSources) != "[]" || string(ver.Permissions) != "{}" {
		t.Fatalf("json defaults not canonical: context_sources=%s permissions=%s", ver.ContextSources, ver.Permissions)
	}
	ttx.Rollback(ctx)

	if n := auditRowCount(t, pool, tenantID, "worker.created", w.ID); n != 1 {
		t.Fatalf("worker.created audit rows = %d, want 1", n)
	}
}

func TestToolCreateWorkerComposesPrompt(t *testing.T) {
	pool, ctx, tenantID := toolGhostEnv(t)
	w, _ := createWorkerViaTool(t, ctx, pool,
		`{"name":"Prompted One","role":"You plan research.","skills":"Go, SQL"}`)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	ver, err := db.GetLatestWorkerVersion(ctx, ttx.Tx, tenantID, w.ID, false)
	if err != nil {
		t.Fatalf("get latest version: %v", err)
	}
	// The structured prompt fields are the persisted source of truth;
	// dispatch composes the system prompt from them (composeWorkerPrompt).
	// Note: worker_versions.system_prompt is not written by ANY create
	// path (db.CreateWorkerVersion omits the column), so parity means the
	// structured fields land verbatim.
	if ver.Role != "You plan research." || ver.Skills != "Go, SQL" {
		t.Fatalf("structured prompt fields not persisted: role=%q skills=%q", ver.Role, ver.Skills)
	}
}

func TestToolCreateWorkerSlugDedupe(t *testing.T) {
	pool, ctx, _ := toolGhostEnv(t)
	first, _ := createWorkerViaTool(t, ctx, pool, `{"name":"Dup Name"}`)
	second, _ := createWorkerViaTool(t, ctx, pool, `{"name":"Dup Name"}`)
	if second.Slug != first.Slug+"-2" {
		t.Fatalf("second slug = %q, want %q", second.Slug, first.Slug+"-2")
	}
}

func TestToolCreateWorkerPublishable(t *testing.T) {
	pool, ctx, tenantID := toolGhostEnv(t)
	w, _ := createWorkerViaTool(t, ctx, pool, `{"name":"Publish Me","model_ref":"m","runtime_ref":"opencode"}`)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	ver, err := db.PublishWorkerVersion(ctx, ttx.Tx, tenantID, w.ID, 1)
	if err != nil {
		t.Fatalf("publish draft v1: %v", err)
	}
	if ver.Status != "published" {
		t.Fatalf("version status = %s, want published", ver.Status)
	}
}

func TestToolCreateWorkerValidation(t *testing.T) {
	pool, ctx, _ := toolGhostEnv(t)
	cases := []string{
		`{"name":"   "}`, // empty name
		`{"name":"` + strings.Repeat("x", 501) + `"}`, // name over the 500-char bound
	}
	for _, args := range cases {
		if _, err := toolCreateWorker(ctx, pool, json.RawMessage(args)); err == nil {
			t.Errorf("toolCreateWorker(%s) succeeded, want error", args)
		}
	}
}

func TestToolUpdateWorkerHeaderAndAudit(t *testing.T) {
	pool, ctx, tenantID := toolGhostEnv(t)
	w, _ := createWorkerViaTool(t, ctx, pool, `{"name":"Before Name"}`)
	raw, err := toolUpdateWorker(ctx, pool, json.RawMessage(
		`{"id":"`+w.ID+`","name":"After Name","description":"new desc","purpose":"new purpose"}`))
	if err != nil {
		t.Fatalf("toolUpdateWorker: %v", err)
	}
	var u db.WorkerRow
	if err := json.Unmarshal(raw, &u); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if u.Name != "After Name" || u.Description != "new desc" || u.Purpose != "new purpose" {
		t.Fatalf("update not persisted: %+v", u)
	}
	if n := auditRowCount(t, pool, tenantID, "worker.updated", w.ID); n != 1 {
		t.Fatalf("worker.updated audit rows = %d, want 1", n)
	}
}
