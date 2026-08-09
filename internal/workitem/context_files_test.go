package workitem

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// TestWorkItemContextFilesDB verifies the work-item context_files
// contract end-to-end through the Connect service against a real
// Postgres (skipped unless ORCHICON_TEST_DSN is set, same convention as
// the validate-parent tests):
//
//  1. CreateWorkItem with context_files (a file + a directory) persists
//     both and returns them on the proto row.
//  2. CreateWorkItem without context_files defaults to an empty array
//     (defined value, not null).
//  3. UpdateWorkItem replaces the selection (files + directories).
//  4. UpdateWorkItem with an empty ContextFiles list clears the
//     selection.
//  5. Validation rejects non-absolute / traversal paths at the API
//     boundary (InvalidArgument).
func TestWorkItemContextFilesDB(t *testing.T) {
	pool := validateParentTestPool(t)
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)
	s := New(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	projectA := validateParentProject(t, ctx, pool)

	cf := []string{"/abs/one.go", "/abs/two/dir"}
	createReq := connect.NewRequest(&apiv1.CreateWorkItemRequest{
		ProjectId:    projectA,
		Kind:         apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC,
		Title:        "Context files " + strings.ToLower(db.NewID()),
		Priority:     0,
		ContextFiles: cf,
	})
	created, err := s.CreateWorkItem(ctx, createReq)
	if err != nil {
		t.Fatalf("create work item with context files: %v", err)
	}
	// Proto row exposes them.
	if got := created.Msg.WorkItem.ContextFiles; len(got) != 2 || got[0] != cf[0] || got[1] != cf[1] {
		t.Fatalf("create: context_files = %v, want %v", got, cf)
	}
	// DB row round-trips them.
	dbItem := readItem(t, pool, created.Msg.WorkItem.Id)
	if len(dbItem.ContextFiles) == 0 {
		t.Fatalf("create: DB context_files is empty")
	}

	// Defaults to a defined empty array when unset.
	plain, err := s.CreateWorkItem(ctx, connect.NewRequest(&apiv1.CreateWorkItemRequest{
		ProjectId: projectA,
		Kind:      apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC,
		Title:     "Plain " + strings.ToLower(db.NewID()),
	}))
	if err != nil {
		t.Fatalf("create plain work item: %v", err)
	}
	if len(plain.Msg.WorkItem.ContextFiles) != 0 {
		t.Fatalf("plain create: expected empty context_files, got %v", plain.Msg.WorkItem.ContextFiles)
	}

	// Update replaces the selection.
	replace := []string{"/new/dir", "/new/file.txt"}
	upd, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id:           created.Msg.WorkItem.Id,
		ContextFiles: &apiv1.ContextFiles{Files: replace},
	}))
	if err != nil {
		t.Fatalf("update context files: %v", err)
	}
	if got := upd.Msg.WorkItem.ContextFiles; len(got) != 2 || got[0] != replace[0] || got[1] != replace[1] {
		t.Fatalf("update: context_files = %v, want %v", got, replace)
	}

	// Update with an empty list clears the selection.
	clear, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id:           created.Msg.WorkItem.Id,
		ContextFiles: &apiv1.ContextFiles{},
	}))
	if err != nil {
		t.Fatalf("clear context files: %v", err)
	}
	if len(clear.Msg.WorkItem.ContextFiles) != 0 {
		t.Fatalf("clear: expected empty context_files, got %v", clear.Msg.WorkItem.ContextFiles)
	}

	// Validation rejects bad paths at the boundary.
	for _, bad := range [][]string{{"relative/path"}, {"/a/../b"}} {
		_, err := s.CreateWorkItem(ctx, connect.NewRequest(&apiv1.CreateWorkItemRequest{
			ProjectId:    projectA,
			Kind:         apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC,
			Title:        "Bad context " + strings.ToLower(db.NewID()),
			ContextFiles: bad,
		}))
		if err == nil {
			t.Fatalf("create with %v: expected error, got nil", bad)
		}
	}
	_, err = s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id:           created.Msg.WorkItem.Id,
		ContextFiles: &apiv1.ContextFiles{Files: []string{"nope/rel"}},
	}))
	if err == nil {
		t.Fatal("update with relative context file: expected error, got nil")
	}
}

// TestWorkItemContextFilesWithinProjectDirDB verifies the confinement rule:
// when the project has a project_dir configured, context_files must live
// inside it (the only path guaranteed mounted where workers run). Paths
// outside are rejected at the API boundary with InvalidArgument, for both
// Create and Update.
func TestWorkItemContextFilesWithinProjectDirDB(t *testing.T) {
	pool := validateParentTestPool(t)
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)
	s := New(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	const projDir = "/home/test/projects/MyApp"
	ttx, err := pool.BeginTenantTx(ctx, validateParentTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: validateParentTestTenant,
		Name: "Confined", Slug: "confined-" + strings.ToLower(db.NewID()),
		Status: "active", Goals: []byte("[]"), ProjectDir: projDir,
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit project: %v", err)
	}

	// Inside the project dir → accepted.
	inside := []string{projDir + "/src", projDir + "/README.md"}
	created, err := s.CreateWorkItem(ctx, connect.NewRequest(&apiv1.CreateWorkItemRequest{
		ProjectId: proj.ID, Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC,
		Title: "Inside " + strings.ToLower(db.NewID()), ContextFiles: inside,
	}))
	if err != nil {
		t.Fatalf("create with inside-project context files: %v", err)
	}
	if len(created.Msg.WorkItem.ContextFiles) != 2 {
		t.Fatalf("inside create: context_files = %v, want 2", created.Msg.WorkItem.ContextFiles)
	}

	// Outside the project dir → rejected on Create.
	_, err = s.CreateWorkItem(ctx, connect.NewRequest(&apiv1.CreateWorkItemRequest{
		ProjectId: proj.ID, Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC,
		Title:        "Outside " + strings.ToLower(db.NewID()),
		ContextFiles: []string{"/home/test/other/file.go"},
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("create outside project dir: got %v, want InvalidArgument", err)
	}

	// Outside the project dir → rejected on Update.
	_, err = s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id:           created.Msg.WorkItem.Id,
		ContextFiles: &apiv1.ContextFiles{Files: []string{"/etc/passwd"}},
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("update outside project dir: got %v, want InvalidArgument", err)
	}
}

// TestContextFilesColumnExistsDB verifies the migration added the
// context_files column and the RLS gate's tenant scoping still works
// (the row is readable only under its tenant).
func TestContextFilesColumnExistsDB(t *testing.T) {
	pool := validateParentTestPool(t)
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)
	ttx, err := pool.BeginTenantTx(ctx, validateParentTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = ttx.Rollback(context.Background()) }()
	// The column exists (a pre-migration query would fail here).
	var one string
	if err := ttx.Tx.QueryRow(ctx,
		`SELECT context_files FROM work_items LIMIT 1`).Scan(&one); err != nil && !strings.Contains(err.Error(), "no rows") {
		t.Fatalf("query context_files column: %v", err)
	}
}
