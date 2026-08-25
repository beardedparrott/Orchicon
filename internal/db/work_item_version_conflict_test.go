package db_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/migrate"
	assets "github.com/beardedparrott/orchicon"
)

func TestUpdateWorkItemVersionConflictDistinct(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := migrate.Run(ctx, pool, assets.MigrationsFS, assets.MigrationsDir); err != nil {
		t.Fatal(err)
	}
	tenant := "tnt_dev"
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: tenant, Name: "vc-test", Slug: "vc-test-" + db.NewID(), Status: domain.ProjectActive, Goals: []byte("[]"), ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	item, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: tenant, ProjectID: proj.ID, Kind: domain.WorkItemKindTask, Title: "vc", Description: "d", AcceptanceCriteria: "ac", Status: domain.WorkItemPending, Priority: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	// stale version update should be version conflict, not not-found
	stale := item.Version
	title := "bumped"
	bumped, err := db.UpdateWorkItem(ctx, ttx.Tx, tenant, item.ID, stale, db.UpdateWorkItemFields{Title: &title})
	if err != nil {
		t.Fatal(err)
	}
	if bumped.Version != stale+1 {
		t.Fatalf("version not bumped")
	}
	// second update with stale version must be ErrVersionConflict
	_, err = db.UpdateWorkItem(ctx, ttx.Tx, tenant, item.ID, stale, db.UpdateWorkItemFields{Title: &title})
	if !errors.Is(err, db.ErrVersionConflict) {
		t.Fatalf("stale update = %v, want ErrVersionConflict", err)
	}
	if errors.Is(err, db.ErrNotFound) {
		t.Fatalf("stale update incorrectly maps to ErrNotFound")
	}
	// missing row must be ErrNotFound distinct
	_, err = db.UpdateWorkItem(ctx, ttx.Tx, tenant, "01MISSING", 1, db.UpdateWorkItemFields{Title: &title})
	if !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("missing row = %v, want ErrNotFound", err)
	}
	if errors.Is(err, db.ErrVersionConflict) {
		t.Fatalf("missing row incorrectly maps to ErrVersionConflict")
	}
	// tolerant ParkWorkItem should succeed even after version drift
	if _, err := db.ParkWorkItem(ctx, ttx.Tx, tenant, item.ID); err != nil {
		t.Fatalf("ParkWorkItem: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
