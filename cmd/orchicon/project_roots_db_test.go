package main

// project_roots_db_test.go — the SOURCE must return real project directories, not compile-only.
//
// The daemon-side tests prove a source's roots are honoured; this proves the source the daemon is actually
// given reads project_dir from the database. Without it the wiring could be a function that always returns nil
// and every test above would still pass.

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

func TestProjectDirRootsReadsTheDatabase(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping the DB-backed project-roots test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	const tenant = "tnt_dev"
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	slug := "roots-" + db.NewID()
	dir := filepath.Join(t.TempDir(), "a-project-root")
	p, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: tenant, Name: "Roots", Slug: slug,
		Status: "active", Goals: []byte("[]"), ProjectDir: dir,
		ExecutionMode: db.ExecutionModeLocal,
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	got, err := projectDirRoots(ctx, pool, tenant, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("projectDirRoots: %v", err)
	}
	found := false
	for _, r := range got {
		if filepath.Clean(r) == filepath.Clean(dir) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the project's project_dir %q is NOT among the returned roots %v — a container for this "+
			"project would be refused with 'mount source outside allowed roots'", dir, got)
	}
	_ = p
}

// TestProjectDirRootsIncludesANonActiveProject is the "already set, still valid" case.
//
// The plane does not gate dispatch on project status when it resolves a project_dir
// (internal/scheduler/reconciler.go: SELECT project_dir FROM projects WHERE id = $1). A mount gate that DID
// gate on status would refuse containers for runs the plane is happy to start — stricter than the thing it is
// meant to protect, which is how a security check becomes a bug.
func TestProjectDirRootsIncludesANonActiveProject(t *testing.T) {
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

	const tenant = "tnt_dev"
	for _, status := range []string{"paused", "drafting"} {
		ttx, err := pool.BeginTenantTx(ctx, tenant)
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(t.TempDir(), "root-"+status)
		if _, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
			ID: db.NewID(), TenantID: tenant, Name: "P-" + status, Slug: "p-" + db.NewID(),
			Status: status, Goals: []byte("[]"), ProjectDir: dir,
			ExecutionMode: db.ExecutionModeLocal,
		}); err != nil {
			t.Fatalf("create %s project: %v", status, err)
		}
		if err := ttx.Commit(ctx); err != nil {
			t.Fatal(err)
		}

		got, err := projectDirRoots(ctx, pool, tenant, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatalf("projectDirRoots: %v", err)
		}
		found := false
		for _, r := range got {
			if filepath.Clean(r) == filepath.Clean(dir) {
				found = true
			}
		}
		if !found {
			t.Fatalf("a %q project's project_dir %q is not a mount root — the plane would happily dispatch a "+
				"run for it and then refuse its container", status, dir)
		}
	}
}

// TestProjectDirRootsIgnoresARelativeDir keeps the boundary honest: a relative dir has no meaning as a mount
// root, because it resolves against whatever directory the daemon happens to be in.
func TestProjectDirRootsIgnoresARelativeDir(t *testing.T) {
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

	const tenant = "tnt_dev"
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	slug := "relroots-" + db.NewID()
	if _, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: tenant, Name: "Rel", Slug: slug,
		Status: "active", Goals: []byte("[]"), ProjectDir: "relative/path",
		ExecutionMode: db.ExecutionModeLocal,
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	got, err := projectDirRoots(ctx, pool, tenant, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("projectDirRoots: %v", err)
	}
	for _, r := range got {
		if r == "relative/path" {
			t.Fatal("a RELATIVE project_dir was returned as a mount root — it would resolve against the " +
				"daemon's working directory, which is not a boundary anyone can reason about")
		}
	}
}
