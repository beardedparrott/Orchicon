package askorchicon

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// These tests exercise the Ask Orchicon create_work_item tool against a
// real Postgres. They are skipped unless ORCHICON_TEST_DSN points at a
// disposable database (the migrations are applied on every run, so the
// database must be safe to re-seed):
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/askorchicon/ -run TestCreateWorkItemKind -v
//
// They guard the acceptance criteria "no more `kind: unknown` for Ask
// Orchicon-created items": the tool must normalize and validate `kind`
// (trim, lowercase, accept only epic/feature/task/subtask) so every
// future create stores a canonical lowercase value that the Connect API
// maps correctly.

func workItemKindTestPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed ask-orchicon tests")
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
	if err := db.SeedDevWorkers(ctx, pool); err != nil {
		t.Fatalf("seed dev workers: %v", err)
	}
	// Tools called directly (not via NewToolRegistry) need the package
	// logger initialized for post-commit side effects.
	if toolLogger == nil {
		toolLogger = slog.Default()
	}
	return pool
}

const workItemKindTestTenant = "tnt_dev"

// createProjectForTest creates a throwaway project in the test tenant so
// the tool's project_id FK resolves.
func createProjectForTest(t *testing.T, ctx context.Context, pool *db.Pool) string {
	t.Helper()
	ttx, err := pool.BeginTenantTx(ctx, workItemKindTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: workItemKindTestTenant,
		Name: "Kind Test", Slug: "kind-test-" + strings.ToLower(db.NewID()),
		Status: "active", Goals: []byte("[]"),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit project: %v", err)
	}
	return proj.ID
}

// createWorkItemViaTool invokes the real toolCreateWorkItem with the given
// raw kind value and returns the created row's kind. The tool enforces the
// same hierarchy rule as the API (only epics are top-level), so a parent
// epic is created first when the requested kind is not epic.
func createWorkItemViaTool(t *testing.T, ctx context.Context, pool *db.Pool, projectID, kind string) (db.WorkItemRow, error) {
	t.Helper()
	var parentID string
	effectiveKind := kind
	if kind == "" {
		effectiveKind = domain.WorkItemKindTask // tool default
	}
	if normalized, err := domain.NormalizeWorkItemKind(effectiveKind); err == nil && normalized != domain.WorkItemKindEpic {
		parent, err := createWorkItemViaTool(t, ctx, pool, projectID, domain.WorkItemKindEpic)
		if err != nil {
			return db.WorkItemRow{}, err
		}
		parentID = parent.ID
	}
	args := map[string]any{
		"project_id": projectID,
		"title":      "Kind normalization test " + strings.ToLower(db.NewID()),
	}
	if kind != "" {
		args["kind"] = kind
	}
	if parentID != "" {
		args["parent_id"] = parentID
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res, err := toolCreateWorkItem(ctx, pool, raw)
	if err != nil {
		return db.WorkItemRow{}, err
	}
	var item db.WorkItemRow
	if err := json.Unmarshal(res, &item); err != nil {
		t.Fatalf("unmarshal tool result: %v", err)
	}
	return item, nil
}

// TestCreateWorkItemKindCanonicalizes verifies the root-cause fix: any
// casing/whitespace variant of a canonical kind is stored lowercase.
func TestCreateWorkItemKindCanonicalizes(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	projectID := createProjectForTest(t, ctx, pool)

	for _, tc := range []struct {
		input string
		want  string
	}{
		{"Epic", domain.WorkItemKindEpic},
		{"Task", domain.WorkItemKindTask},
		{"EPIC", domain.WorkItemKindEpic},
		{"  task  ", domain.WorkItemKindTask},
		{"feature", domain.WorkItemKindFeature},
		{"Subtask", domain.WorkItemKindSubtask},
	} {
		item, err := createWorkItemViaTool(t, ctx, pool, projectID, tc.input)
		if err != nil {
			t.Fatalf("create with kind %q: %v", tc.input, err)
		}
		if item.Kind != tc.want {
			t.Errorf("create with kind %q stored %q, want %q", tc.input, item.Kind, tc.want)
		}
	}
}

// TestCreateWorkItemKindDefaultsToTask verifies an omitted kind defaults
// to the canonical task constant.
func TestCreateWorkItemKindDefaultsToTask(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	projectID := createProjectForTest(t, ctx, pool)

	item, err := createWorkItemViaTool(t, ctx, pool, projectID, "")
	if err != nil {
		t.Fatalf("create without kind: %v", err)
	}
	if item.Kind != domain.WorkItemKindTask {
		t.Errorf("create without kind stored %q, want %q", item.Kind, domain.WorkItemKindTask)
	}
}

// TestCreateWorkItemKindRejectsUnknown verifies unknown kinds are
// rejected with an error that names the four canonical kinds — the tool
// never stores a value the strict read path would map to UNSPECIFIED.
func TestCreateWorkItemKindRejectsUnknown(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	projectID := createProjectForTest(t, ctx, pool)

	for _, input := range []string{"Story", "bug", "epic2"} {
		if _, err := createWorkItemViaTool(t, ctx, pool, projectID, input); err == nil {
			t.Errorf("create with kind %q should have been rejected", input)
		} else if msg := err.Error(); !strings.Contains(msg, "epic") || !strings.Contains(msg, "subtask") {
			t.Errorf("create with kind %q error %q does not name the four canonical kinds", input, msg)
		}
	}
}

// TestNormalizeWorkItemKindUnit exercises the domain helper directly for
// the exact call shapes the tool produces.
func TestNormalizeWorkItemKindUnit(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
		ok    bool
	}{
		{"Epic", domain.WorkItemKindEpic, true},
		{"epic", domain.WorkItemKindEpic, true},
		{"EPIC", domain.WorkItemKindEpic, true},
		{"  Task  ", domain.WorkItemKindTask, true},
		{"", "", false},
		{"Story", "", false},
		{"recovery_stop", "", false},
	} {
		got, err := domain.NormalizeWorkItemKind(tc.input)
		if tc.ok && err != nil {
			t.Errorf("NormalizeWorkItemKind(%q) unexpected error: %v", tc.input, err)
		}
		if tc.ok && got != tc.want {
			t.Errorf("NormalizeWorkItemKind(%q) = %q, want %q", tc.input, got, tc.want)
		}
		if !tc.ok && err == nil {
			t.Errorf("NormalizeWorkItemKind(%q) expected error, got %q", tc.input, got)
		}
	}
}

// callToolCreate invokes toolCreateWorkItem with arbitrary args and returns
// the decoded row, so tests can exercise every field.
func callToolCreate(t *testing.T, ctx context.Context, pool *db.Pool, args map[string]any) (db.WorkItemRow, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res, err := toolCreateWorkItem(ctx, pool, raw)
	if err != nil {
		return db.WorkItemRow{}, err
	}
	var item db.WorkItemRow
	if err := json.Unmarshal(res, &item); err != nil {
		t.Fatalf("unmarshal tool result: %v", err)
	}
	return item, nil
}

// createParentEpicForTest creates a top-level epic under the given project
// so tests can create task-kind items (only epics are top-level).
func createParentEpicForTest(t *testing.T, ctx context.Context, pool *db.Pool, projectID string) string {
	t.Helper()
	item, err := callToolCreate(t, ctx, pool, map[string]any{
		"project_id": projectID,
		"title":      "Parent epic " + strings.ToLower(db.NewID()),
		"kind":       domain.WorkItemKindEpic,
	})
	if err != nil {
		t.Fatalf("create parent epic: %v", err)
	}
	return item.ID
}

// callToolUpdate invokes toolUpdateWorkItem with arbitrary args and returns
// the decoded row.
func callToolUpdate(t *testing.T, ctx context.Context, pool *db.Pool, args map[string]any) (db.WorkItemRow, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res, err := toolUpdateWorkItem(ctx, pool, raw)
	if err != nil {
		return db.WorkItemRow{}, err
	}
	var item db.WorkItemRow
	if err := json.Unmarshal(res, &item); err != nil {
		t.Fatalf("unmarshal tool result: %v", err)
	}
	return item, nil
}

// TestCreateWorkItemAllFieldsDB verifies create exposes the full mutable
// field set: budgets, context_window, workflow_id, scheduled_start_at
// (status flips to scheduled), auto_start_workflow, runtime_image.
func TestCreateWorkItemAllFieldsDB(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	projectID := createProjectForTest(t, ctx, pool)

	item, err := callToolCreate(t, ctx, pool, map[string]any{
		"project_id":          projectID,
		"title":               "Full field create",
		"kind":                "task",
		"parent_id":           createParentEpicForTest(t, ctx, pool, projectID),
		"description":         "desc",
		"acceptance_criteria": "ac",
		"priority":            3,
		"budgets":             `{"max_steps": 10}`,
		"context_window":      20000,
		"workflow_id":         "wf_test_1",
		"scheduled_start_at":  "2030-01-01T10:00:00Z",
		"auto_start_workflow": true,
		"runtime_image":       "base:latest ",
		"context_files":       []string{"/abs/dir", "/abs/one.go"},
	})
	if err != nil {
		t.Fatalf("create with all fields: %v", err)
	}
	if item.Priority != 3 {
		t.Errorf("priority = %d, want 3", item.Priority)
	}
	if string(item.Budgets) != `{"max_steps": 10}` {
		t.Errorf("budgets = %s", item.Budgets)
	}
	if item.ContextWindow != 20000 {
		t.Errorf("context_window = %d, want 20000", item.ContextWindow)
	}
	if item.WorkflowID == nil || *item.WorkflowID != "wf_test_1" {
		t.Errorf("workflow_id = %v, want wf_test_1", item.WorkflowID)
	}
	if item.ScheduledStartAt == nil {
		t.Error("scheduled_start_at not set")
	}
	// ADR-001: a supplied start time flips the status to scheduled.
	if item.Status != domain.WorkItemScheduled {
		t.Errorf("status = %q, want scheduled", item.Status)
	}
	if !item.AutoStartWorkflow {
		t.Error("auto_start_workflow not stored true")
	}
	// Runtime image is trimmed.
	if item.RuntimeImage != "base:latest" {
		t.Errorf("runtime_image = %q, want base:latest (trimmed)", item.RuntimeImage)
	}
	// Context files (files + directories) round-trip through the DB.
	var cf []string
	if err := json.Unmarshal(item.ContextFiles, &cf); err != nil {
		t.Fatalf("unmarshal context_files %q: %v", item.ContextFiles, err)
	}
	if len(cf) != 2 || cf[0] != "/abs/dir" || cf[1] != "/abs/one.go" {
		t.Errorf("context_files = %v, want the directory + file persisted", cf)
	}
}

// TestCreateWorkItemInvalidBudgetsRejectedDB verifies the create tool
// enforces the API's JSON validation for budgets at the MCP boundary.
func TestCreateWorkItemInvalidBudgetsRejectedDB(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	projectID := createProjectForTest(t, ctx, pool)

	item, err := callToolCreate(t, ctx, pool, map[string]any{
		"project_id": projectID,
		"title":      "Bad budgets",
		"budgets":    `{not json`,
	})
	if err == nil {
		t.Fatalf("create with invalid budgets should be rejected, got item %+v", item)
	}
	if !strings.Contains(err.Error(), "valid JSON") {
		t.Errorf("error %q does not mention valid JSON", err.Error())
	}
}

// TestCreateWorkItemOverlongTitleRejectedDB verifies the size bounds from
// the API boundary apply at the MCP boundary too.
func TestCreateWorkItemOverlongTitleRejectedDB(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	projectID := createProjectForTest(t, ctx, pool)

	if _, err := callToolCreate(t, ctx, pool, map[string]any{
		"project_id": projectID,
		"title":      strings.Repeat("x", 501),
	}); err == nil {
		t.Fatal("create with overlong title should be rejected")
	}
}

// TestUpdateWorkItemAllFieldsDB verifies update exposes the full mutable
// field set: budgets, context_window, workflow_id, scheduled_start_at
// (status flips to scheduled), runtime_image, workflow_run_id, project_id.
func TestUpdateWorkItemAllFieldsDB(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	projectID := createProjectForTest(t, ctx, pool)

	item, err := callToolCreate(t, ctx, pool, map[string]any{
		"project_id": projectID,
		"title":      "To update",
		"kind":       "task",
		"parent_id":  createParentEpicForTest(t, ctx, pool, projectID),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	updated, err := callToolUpdate(t, ctx, pool, map[string]any{
		"id":                  item.ID,
		"title":               "Updated title",
		"description":         "updated desc",
		"acceptance_criteria": "updated ac",
		"priority":            5,
		"budgets":             `{"max_steps": 3}`,
		"context_window":      50000,
		"workflow_id":         "wf_test_2",
		"scheduled_start_at":  "2030-02-01T10:00:00Z",
		"runtime_image":       "dev:latest",
		"context_files":       []string{"/new/dir", "/new/file.md"},
	})
	if err != nil {
		t.Fatalf("update all fields: %v", err)
	}
	if updated.Title != "Updated title" {
		t.Errorf("title = %q", updated.Title)
	}
	if string(updated.Budgets) != `{"max_steps": 3}` {
		t.Errorf("budgets = %s", updated.Budgets)
	}
	if updated.ContextWindow != 50000 {
		t.Errorf("context_window = %d", updated.ContextWindow)
	}
	if updated.WorkflowID == nil || *updated.WorkflowID != "wf_test_2" {
		t.Errorf("workflow_id = %v", updated.WorkflowID)
	}
	if updated.RuntimeImage != "dev:latest" {
		t.Errorf("runtime_image = %q", updated.RuntimeImage)
	}
	// Context files replace the previous selection.
	var cf []string
	if err := json.Unmarshal(updated.ContextFiles, &cf); err != nil {
		t.Fatalf("unmarshal context_files %q: %v", updated.ContextFiles, err)
	}
	if len(cf) != 2 || cf[0] != "/new/dir" || cf[1] != "/new/file.md" {
		t.Errorf("context_files = %v, want the replacement paths", cf)
	}
	// ADR-001: a supplied start time flips status to scheduled.
	if updated.Status != domain.WorkItemScheduled {
		t.Errorf("status = %q, want scheduled (ADR-001 flip)", updated.Status)
	}
}

// TestUpdateWorkItemClearContextFilesDB verifies an empty context_files
// list clears the selection (empty list = clear, unset = unchanged).
func TestUpdateWorkItemClearContextFilesDB(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	projectID := createProjectForTest(t, ctx, pool)

	item, err := callToolCreate(t, ctx, pool, map[string]any{
		"project_id":    projectID,
		"title":         "Clear context",
		"kind":          "task",
		"parent_id":     createParentEpicForTest(t, ctx, pool, projectID),
		"context_files": []string{"/some/dir"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(item.ContextFiles) == 0 {
		t.Fatal("create did not persist context_files")
	}
	cleared, err := callToolUpdate(t, ctx, pool, map[string]any{
		"id":            item.ID,
		"context_files": []string{},
	})
	if err != nil {
		t.Fatalf("clear context files: %v", err)
	}
	var clearedCF []string
	if err := json.Unmarshal(cleared.ContextFiles, &clearedCF); err != nil {
		t.Fatalf("unmarshal cleared context_files %q: %v", cleared.ContextFiles, err)
	}
	if len(clearedCF) != 0 {
		t.Errorf("clear: context_files = %v, want empty", clearedCF)
	}
}

// TestUpdateWorkItemAutoStartClearsScheduleDB verifies setting
// auto_start_workflow=true without a scheduled time clears any prior
// schedule and stores the flag (service.go behavior replicated).
func TestUpdateWorkItemAutoStartClearsScheduleDB(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	projectID := createProjectForTest(t, ctx, pool)

	item, err := callToolCreate(t, ctx, pool, map[string]any{
		"project_id":         projectID,
		"title":              "Auto start",
		"kind":               "task",
		"parent_id":          createParentEpicForTest(t, ctx, pool, projectID),
		"workflow_id":        "wf_test_3",
		"scheduled_start_at": "2030-03-01T10:00:00Z",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	updated, err := callToolUpdate(t, ctx, pool, map[string]any{
		"id":                  item.ID,
		"auto_start_workflow": true,
	})
	if err != nil {
		t.Fatalf("update auto-start: %v", err)
	}
	if !updated.AutoStartWorkflow {
		t.Error("auto_start_workflow not stored true")
	}
	if updated.ScheduledStartAt != nil {
		t.Errorf("scheduled_start_at should be cleared, got %v", updated.ScheduledStartAt)
	}
}

// TestUpdateWorkItemProjectReassignDB verifies project_id reassignment is
// settable via the MCP and the target must be active.
func TestUpdateWorkItemProjectReassignDB(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	projectID := createProjectForTest(t, ctx, pool)
	targetID := createProjectForTest(t, ctx, pool)

	item, err := callToolCreate(t, ctx, pool, map[string]any{
		"project_id": projectID,
		"title":      "Move me",
		"kind":       "epic",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	updated, err := callToolUpdate(t, ctx, pool, map[string]any{
		"id":         item.ID,
		"project_id": targetID,
	})
	if err != nil {
		t.Fatalf("reassign project: %v", err)
	}
	if updated.ProjectID != targetID {
		t.Errorf("project_id = %q, want %q", updated.ProjectID, targetID)
	}
}

// TestAssignUnassignWorkerDB verifies the assign_worker / unassign_worker
// tools persist the worker ref and clear it, matching AssignWorker /
// UnassignWorker.
func TestAssignUnassignWorkerDB(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	projectID := createProjectForTest(t, ctx, pool)

	item, err := callToolCreate(t, ctx, pool, map[string]any{
		"project_id": projectID,
		"title":      "Worker assign",
		"kind":       "task",
		"parent_id":  createParentEpicForTest(t, ctx, pool, projectID),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	assignArgs, _ := json.Marshal(map[string]any{"id": item.ID, "worker_id": "w_se_senior_software_engineer", "version": 1})
	assignRes, err := toolAssignWorker(ctx, pool, assignArgs)
	if err != nil {
		t.Fatalf("assign worker: %v", err)
	}
	var assigned db.WorkItemRow
	if err := json.Unmarshal(assignRes, &assigned); err != nil {
		t.Fatalf("unmarshal assign result: %v", err)
	}
	if assigned.AssignedWorkerRef == nil {
		t.Fatal("assigned_worker_ref not set")
	}
	var ref struct {
		WorkerID string `json:"worker_id"`
		Version  int    `json:"version"`
	}
	if err := json.Unmarshal(assigned.AssignedWorkerRef, &ref); err != nil {
		t.Fatalf("unmarshal worker ref: %v", err)
	}
	if ref.WorkerID != "w_se_senior_software_engineer" || ref.Version != 1 {
		t.Errorf("worker ref = %+v", ref)
	}

	unassignArgs, _ := json.Marshal(map[string]any{"id": item.ID})
	unassignRes, err := toolUnassignWorker(ctx, pool, unassignArgs)
	if err != nil {
		t.Fatalf("unassign worker: %v", err)
	}
	var unassigned db.WorkItemRow
	if err := json.Unmarshal(unassignRes, &unassigned); err != nil {
		t.Fatalf("unmarshal unassign result: %v", err)
	}
	if unassigned.AssignedWorkerRef != nil {
		t.Errorf("assigned_worker_ref should be nil after unassign, got %s", unassigned.AssignedWorkerRef)
	}
}

// TestUpdateWorkItemWorkflowRunIDDB verifies workflow_run_id is settable.
func TestUpdateWorkItemWorkflowRunIDDB(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	projectID := createProjectForTest(t, ctx, pool)

	item, err := callToolCreate(t, ctx, pool, map[string]any{
		"project_id": projectID,
		"title":      "Run id",
		"kind":       "task",
		"parent_id":  createParentEpicForTest(t, ctx, pool, projectID),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	updated, err := callToolUpdate(t, ctx, pool, map[string]any{
		"id":               item.ID,
		"workflow_run_id":  "run_test_1",
	})
	if err != nil {
		t.Fatalf("update workflow_run_id: %v", err)
	}
	if updated.WorkflowRunID != "run_test_1" {
		t.Errorf("workflow_run_id = %q, want run_test_1", updated.WorkflowRunID)
	}
}
