package scheduler

import (
	"context"
	"os"
	"testing"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/migrate"
)

// scratchPool opens a disposable scratch database (never the live plane)
// and applies all migrations including the always-container columns.
func scratchPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed resolution test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open scratch pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := migrate.Run(ctx, pool, assets.MigrationsFS, assets.MigrationsDir); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	if err := db.SeedDevWorkers(ctx, pool); err != nil {
		t.Fatalf("seed dev workers: %v", err)
	}
	return pool
}

func strptr(s string) *string { return &s }

// TestProjectRuntimeDefaults pins acceptance A: new projects default to
// runtime mode with a NULL default image; updates set both fields;
// optimistic concurrency is preserved.
func TestProjectRuntimeDefaults(t *testing.T) {
	pool := scratchPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	p, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: "prj_rt_default", TenantID: approvalTestTenant,
		Name: "rt default", Slug: "rt-default", Status: "active", Goals: []byte("[]"),
		ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if p.ExecutionMode != db.ExecutionModeRuntime {
		t.Errorf("new project mode = %q, want runtime", p.ExecutionMode)
	}
	if p.DefaultRuntimeImage != nil {
		t.Errorf("new project default image = %q, want NULL", *p.DefaultRuntimeImage)
	}
	img := "orchicon-runtime:orchicon-dev"
	mode := db.ExecutionModeLocal
	u, err := db.UpdateProject(ctx, ttx.Tx, approvalTestTenant, p.ID, p.Version, db.UpdateProjectFields{
		DefaultRuntimeImage: &img, ExecutionMode: &mode,
	})
	if err != nil {
		t.Fatalf("update project: %v", err)
	}
	if u.DefaultRuntimeImage == nil || *u.DefaultRuntimeImage != img {
		t.Errorf("updated default image = %v, want %q", u.DefaultRuntimeImage, img)
	}
	if u.ExecutionMode != db.ExecutionModeLocal {
		t.Errorf("updated mode = %q, want local", u.ExecutionMode)
	}
	if _, err := db.UpdateProject(ctx, ttx.Tx, approvalTestTenant, p.ID, p.Version, db.UpdateProjectFields{}); err != db.ErrNotFound && err != db.ErrVersionConflict {
		t.Errorf("stale version update err = %v, want version conflict/not-found", err)
	}
}

// TestWorkItemAutofill pins acceptance A: a bare create inherits the
// project default; an explicit value wins; updating the project default
// does NOT retro-mutate existing items.
func TestWorkItemAutofill(t *testing.T) {
	pool := scratchPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	p, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: "prj_rt_autofill", TenantID: approvalTestTenant,
		Name: "rt autofill", Slug: "rt-autofill", Status: "active", Goals: []byte("[]"),
		ProjectDir:          t.TempDir(),
		DefaultRuntimeImage: strptr("orchicon-runtime:orchicon-dev"),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	mkItem := func(id, img string) db.WorkItemRow {
		wi, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
			ID: id, TenantID: approvalTestTenant, ProjectID: p.ID,
			Kind:  domain.WorkItemKindTask,
			Title: "t", Status: domain.WorkItemReady, RuntimeImage: img,
		})
		if err != nil {
			t.Fatalf("create work item %s: %v", id, err)
		}
		return wi
	}
	bare := mkItem("wi_bare", "")
	if bare.RuntimeImage != "orchicon-runtime:orchicon-dev" {
		t.Errorf("bare item image = %q, want project default", bare.RuntimeImage)
	}
	explicit := mkItem("wi_explicit", "orchicon-runtime:base")
	if explicit.RuntimeImage != "orchicon-runtime:base" {
		t.Errorf("explicit item image = %q, want preserved base", explicit.RuntimeImage)
	}
	// Update the project default: existing items must NOT change.
	other := "orchicon-runtime:other"
	if _, err := db.UpdateProject(ctx, ttx.Tx, approvalTestTenant, p.ID, p.Version, db.UpdateProjectFields{
		DefaultRuntimeImage: &other,
	}); err != nil {
		t.Fatalf("update project default: %v", err)
	}
	still, err := db.GetWorkItem(ctx, ttx.Tx, approvalTestTenant, bare.ID)
	if err != nil {
		t.Fatalf("get bare item: %v", err)
	}
	if still.RuntimeImage != "orchicon-runtime:orchicon-dev" {
		t.Errorf("existing item image = %q after default change, want unchanged", still.RuntimeImage)
	}
	future := mkItem("wi_future", "")
	if future.RuntimeImage != "orchicon-runtime:other" {
		t.Errorf("future item image = %q, want new default", future.RuntimeImage)
	}
}

// TestResolutionChain pins acceptance A: explicit -> project default ->
// base, never empty, never no-serve. resolveProjectDefaultImage is the
// project leg; resolveImageFromValues is the agreement leg.
func TestResolutionChain(t *testing.T) {
	pool := scratchPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	p, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: "prj_rt_resolve", TenantID: approvalTestTenant,
		Name: "rt resolve", Slug: "rt-resolve", Status: "active", Goals: []byte("[]"),
		ProjectDir:          t.TempDir(),
		DefaultRuntimeImage: strptr("orchicon-runtime:orchicon-dev"),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if got := resolveProjectDefaultImage(ctx, ttx.Tx, approvalTestTenant, p.ID); got != "orchicon-runtime:orchicon-dev" {
		t.Errorf("project leg = %q, want dev default", got)
	}
	if got := resolveProjectDefaultImage(ctx, ttx.Tx, approvalTestTenant, "prj_missing"); got != db.BaseRuntimeImage {
		t.Errorf("missing project leg = %q, want base", got)
	}
	if got := resolveProjectDefaultImage(ctx, ttx.Tx, approvalTestTenant, ""); got != db.BaseRuntimeImage {
		t.Errorf("empty project leg = %q, want base", got)
	}
	// Agreement leg: all-empty -> "", single -> image, conflict -> error.
	if got, err := resolveImageFromValues(nil); err != nil || got != "" {
		t.Errorf("empty values = %q,%v; want empty,nil (arm site maps to base)", got, err)
	}
	if got, err := resolveImageFromValues([]string{"", "orchicon-runtime:base"}); err != nil || got != "orchicon-runtime:base" {
		t.Errorf("single value = %q,%v; want base,nil", got, err)
	}
	if _, err := resolveImageFromValues([]string{"orchicon-runtime:base", "orchicon-runtime:orchicon-dev"}); err == nil {
		t.Error("conflicting values must error")
	}
	// Full chain through imageForRun: never empty, never no-serve.
	for _, resolved := range []string{"", "orchicon-runtime:orchicon-dev", db.BaseRuntimeImage} {
		for _, serve := range []bool{true, false} {
			got := imageForRun(resolved, serve)
			if got == "" || got == "no-serve" {
				t.Errorf("imageForRun(%q,%v) = %q: never empty, never no-serve", resolved, serve, got)
			}
		}
	}
}
