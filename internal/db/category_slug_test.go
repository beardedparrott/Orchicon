package db

// category_slug_test.go — the slug-collision RETRY in CreateCategory / UpdateCategory.
//
// WHAT THESE PIN. `Slugify` lowercases, so two groupings whose names differ only in case ("Frontend"
// and "FrontEnd") pass the NAME unique constraint and collide on the SLUG one. Both writers are built
// to absorb that: they retry with a suffixed slug. The retry could never run — Postgres aborts a
// transaction on the first failed statement, so the second INSERT was refused by the ABORT rather than
// by the constraint, and the operator got
//
//	db: create category: ERROR: current transaction is aborted, commands ignored until end of transaction block
//
// instead of a grouping. These tests fail on that, and only pass when the attempt is wrapped in a
// savepoint (see withSavepoint).
//
// THE DATABASE IS LEFT EXACTLY AS FOUND. The tenant, the groupings and any rename all happen inside one
// transaction that is NEVER committed, so there is no cleanup step that can be forgotten. Guarded by
// ORCHICON_TEST_DSN like every other DB-backed test in this package:
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon_scratch?sslmode=disable'
//	go test ./internal/db/ -run TestCategorySlug -v
//
// Point it at a DISPOSABLE database. The live plane is refused by the dispatcher (internal/db/prompt.go)
// and must never be used for tests.
import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// categoryTestTx opens a throwaway tenant inside an uncommitted transaction.
func categoryTestTx(t *testing.T) (context.Context, pgx.Tx, string) {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed category slug test")
	}
	ctx := context.Background()
	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pool.Close)

	tenant := "tnt_catslug_" + NewID()[:8]
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tenant tx: %v", err)
	}
	// ROLLED BACK, never committed: the tenant and every grouping below disappear with the transaction,
	// so the shared instance is left byte-for-byte as it was found.
	t.Cleanup(func() { _ = ttx.Rollback(ctx) })
	if _, err := ttx.Exec(ctx, `INSERT INTO tenants (id, slug, name, status) VALUES ($1,$2,$3,'active')`,
		tenant, "catslug-"+NewID()[:8], "Category Slug Test"); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	return ctx, ttx.Tx, tenant
}

// TestCategorySlugCreateRetriesPastACollision is the operator-visible defect: creating a grouping whose
// name differs from an existing one only by CASE produced an opaque "transaction is aborted" error
// instead of a grouping (and it is reachable from the assign modal's "＋ new category…").
func TestCategorySlugCreateRetriesPastACollision(t *testing.T) {
	ctx, tx, tenant := categoryTestTx(t)

	first, err := CreateCategory(ctx, tx, tenant, "worker", "Frontend", "")
	if err != nil {
		t.Fatalf("fixture: the first grouping must be created: %v", err)
	}
	if first.Slug != "frontend" {
		t.Fatalf("first slug = %q, want %q", first.Slug, "frontend")
	}

	// SAME slug, DIFFERENT name (case only): the NAME constraint passes and the SLUG constraint fires.
	// This is the retry path.
	second, err := CreateCategory(ctx, tx, tenant, "worker", "FrontEnd", "")
	if err != nil {
		t.Fatalf("the retry must survive a slug collision, got: %v", err)
	}
	if second.Name != "FrontEnd" {
		t.Fatalf("created name %q, want the name that was typed", second.Name)
	}
	if second.Slug == first.Slug {
		t.Fatalf("the retry reused the COLLIDING slug %q — the second grouping would be unaddressable", second.Slug)
	}
	if !strings.HasPrefix(second.Slug, "frontend_") {
		t.Fatalf("retry slug = %q, want a %q prefix plus a suffix", second.Slug, "frontend_")
	}

	// THE TRANSACTION IS STILL USABLE. Before the savepoint, the collision left the transaction
	// aborted, so every statement the caller ran afterwards in the same unit of work failed too.
	rows, err := ListCategories(ctx, tx, tenant, "worker")
	if err != nil {
		t.Fatalf("the transaction must survive a slug collision: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want both groupings, got %d", len(rows))
	}
}

// TestCategorySlugCreateStillRefusesADuplicateName: the fix must not turn the NAME guard into a retry
// loop. An exact duplicate is the one case the operator can act on, so it must keep saying so.
func TestCategorySlugCreateStillRefusesADuplicateName(t *testing.T) {
	ctx, tx, tenant := categoryTestTx(t)

	if _, err := CreateCategory(ctx, tx, tenant, "worker", "Frontend", ""); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	_, err := CreateCategory(ctx, tx, tenant, "worker", "Frontend", "")
	if err == nil {
		t.Fatal("an exact duplicate name must be refused, not suffixed")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("refusal %q must be the operator-facing message", err)
	}
	// The friendly refusal must NOT be reachable as a raw constraint dump, and the transaction must
	// still work afterwards.
	if strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("the raw constraint error leaked to the caller: %v", err)
	}
	if _, err := CreateCategory(ctx, tx, tenant, "worker", "Backend", ""); err != nil {
		t.Fatalf("the transaction must survive a refused duplicate name: %v", err)
	}
}

// TestCategorySlugUpdateRetriesPastACollision: RENAME has the same defect and the same fix. Renaming a
// grouping to a case-variant of another one collides on the slug.
func TestCategorySlugUpdateRetriesPastACollision(t *testing.T) {
	ctx, tx, tenant := categoryTestTx(t)

	front, err := CreateCategory(ctx, tx, tenant, "workflow", "Frontend", "")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	back, err := CreateCategory(ctx, tx, tenant, "workflow", "Backend", "")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}

	// "Backend" → "FrontEnd": a DIFFERENT name from "Frontend" (so the NAME constraint passes) whose
	// slug is the same (so the SLUG constraint fires).
	rename := "FrontEnd"
	updated, err := UpdateCategory(ctx, tx, tenant, back.ID, &rename, nil)
	if err != nil {
		t.Fatalf("the rename retry must survive a slug collision, got: %v", err)
	}
	if updated.Name != "FrontEnd" {
		t.Fatalf("renamed to %q, want %q", updated.Name, "FrontEnd")
	}
	if updated.Slug == front.Slug {
		t.Fatalf("the retry reused the COLLIDING slug %q", updated.Slug)
	}
	if !strings.HasPrefix(updated.Slug, "frontend_") {
		t.Fatalf("retry slug = %q, want a %q prefix plus a suffix", updated.Slug, "frontend_")
	}
	// And the OTHER grouping is untouched — a retry that stole the slug would have overwritten it.
	got, err := GetCategory(ctx, tx, tenant, front.ID)
	if err != nil {
		t.Fatalf("the first grouping must still exist: %v", err)
	}
	if got.Slug != "frontend" || got.Name != "Frontend" {
		t.Fatalf("the existing grouping was disturbed: %+v", got)
	}
}

// TestCategorySlugCreateIsScopedToItsTargetType: verified while answering "do the three share names?".
// They do NOT — the constraints are per (tenant, target_type) — so a worker grouping named
// "Automation" and a workflow grouping named "Automation" coexist, and neither is suffixed.
func TestCategorySlugCreateIsScopedToItsTargetType(t *testing.T) {
	ctx, tx, tenant := categoryTestTx(t)

	for _, tt := range []string{"worker", "workflow", "conversation"} {
		got, err := CreateCategory(ctx, tx, tenant, tt, "Automation", "")
		if err != nil {
			t.Fatalf("%s grouping: %v", tt, err)
		}
		if got.Slug != "automation" {
			t.Fatalf("%s grouping got slug %q — the three target types must not collide with each other",
				tt, got.Slug)
		}
	}
}
