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
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// Sequence validation + reorder tests (architecture-notes/
// sequential-multi-workflow-runs.md §1, §3). DB-backed parts are skipped
// unless ORCHICON_TEST_DSN is set (same convention as validate_test.go):
//
//	export ORCHICON_TEST_DSN='postgres://orchicon@127.0.0.1:5432/orchicon?sslmode=disable'
//	go test ./internal/workitem/ -run 'TestSequence|TestReorder|TestValidateSequence' -v

// TestBuildSequenceValidationError verifies the schedule-time rejection
// message matches the design contract exactly.
func TestBuildSequenceValidationError(t *testing.T) {
	err := BuildSequenceValidationError("Parent Title", []string{"Feature A", "Task C"}, nil, nil)
	want := `Cannot schedule "Parent Title": 2 children have no workflow set — "Feature A", "Task C". Bind workflows or remove them from the sequence.`
	if err == nil || err.Error() != want {
		t.Fatalf("message mismatch:\n got: %v\nwant: %s", err, want)
	}
}

func TestBuildSequenceValidationErrorOneShot(t *testing.T) {
	err := BuildSequenceValidationError("Parent", nil, []string{"Worker Task"}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "Cannot schedule \"Parent\"") ||
		!strings.Contains(err.Error(), "worker-assigned (one-shot)") {
		t.Fatalf("one-shot message missing markers: %v", err)
	}
}

// TestValidateSequenceSubtreeDB walks a real subtree and reports
// workflow-less leaves, one-shot children, and unrunnable workflows.
func TestValidateSequenceSubtreeDB(t *testing.T) {
	pool := validateParentTestPool(t)
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)
	projID := validateParentProject(t, ctx, pool)

	// A published workflow (runnable) and a draft workflow (not runnable).
	publishedWF := seedPublishedWorkflowForTest(t, pool, projID, true)
	draftWF := seedPublishedWorkflowForTest(t, pool, projID, false)

	parent := createSequenceItem(t, pool, projID, domain.WorkItemKindEpic, "Parent", nil, nil, nil)
	// Leaf with no workflow → offender.
	leafNoWF := createSequenceItem(t, pool, projID, domain.WorkItemKindTask, "No Workflow Task", &parent.ID, nil, nil)
	// Leaf with a published workflow → OK.
	leafOK := createSequenceItem(t, pool, projID, domain.WorkItemKindTask, "Bound Task", &parent.ID, &publishedWF, nil)
	// Container child (has its own child) → exempt; its leaf is validated.
	feature := createSequenceItem(t, pool, projID, domain.WorkItemKindFeature, "Feature", &parent.ID, nil, nil)
	_ = createSequenceItem(t, pool, projID, domain.WorkItemKindTask, "Task under Feature", &feature.ID, &draftWF, nil)
	// One-shot leaf anywhere in the subtree → offender.
	leafOneShot := createSequenceItem(t, pool, projID, domain.WorkItemKindTask, "One-Shot Task", &parent.ID, &publishedWF, []byte(`{"worker_id":"w1"}`))

	ttx, err := pool.BeginTenantTx(ctx, validateParentTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	noWorkflow, oneShot, badWorkflow, err := ValidateSequenceSubtree(ctx, ttx.Tx, validateParentTestTenant, mustGetSequenceItem(t, pool, parent.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(noWorkflow) != 1 || noWorkflow[0] != "No Workflow Task" {
		t.Errorf("noWorkflow = %v, want [\"No Workflow Task\"]", noWorkflow)
	}
	if len(oneShot) != 1 || oneShot[0] != "One-Shot Task" {
		t.Errorf("oneShot = %v, want [\"One-Shot Task\"]", oneShot)
	}
	if len(badWorkflow) != 1 || badWorkflow[0] != "Task under Feature" {
		t.Errorf("badWorkflow = %v, want [\"Task under Feature\"]", badWorkflow)
	}
	_ = leafNoWF
	_ = leafOK
	_ = leafOneShot
}

// TestValidateSequenceScheduleDB drives the shared validation entry point:
// a valid subtree returns nil; a workflow-less leaf returns the rejection.
func TestValidateSequenceScheduleDB(t *testing.T) {
	pool := validateParentTestPool(t)
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)
	projID := validateParentProject(t, ctx, pool)
	wf := seedPublishedWorkflowForTest(t, pool, projID, true)

	parent := createSequenceItem(t, pool, projID, domain.WorkItemKindEpic, "Parent", nil, nil, nil)
	_ = createSequenceItem(t, pool, projID, domain.WorkItemKindTask, "Bound", &parent.ID, &wf, nil)

	ttx, err := pool.BeginTenantTx(ctx, validateParentTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	if err := ValidateSequenceSchedule(ctx, ttx.Tx, validateParentTestTenant, mustGetSequenceItem(t, pool, parent.ID)); err != nil {
		t.Fatalf("valid subtree should pass, got %v", err)
	}

	// Add an unbound leaf → reject.
	_ = createSequenceItem(t, pool, projID, domain.WorkItemKindTask, "Unbound", &parent.ID, nil, nil)
	if err := ValidateSequenceSchedule(ctx, ttx.Tx, validateParentTestTenant, mustGetSequenceItem(t, pool, parent.ID)); err == nil {
		t.Fatal("expected rejection for unbound leaf")
	} else if !strings.Contains(err.Error(), "1 child has no workflow set") {
		t.Fatalf("unexpected rejection message: %v", err)
	}
}

// TestReorderWorkItemsDB reorders siblings through the RPC in one
// transaction and rejects cross-parent / duplicate lists.
func TestReorderWorkItemsDB(t *testing.T) {
	pool := validateParentTestPool(t)
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)
	projID := validateParentProject(t, ctx, pool)

	s := New(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	parent := createSequenceItem(t, pool, projID, domain.WorkItemKindEpic, "Parent", nil, nil, nil)
	otherParent := createSequenceItem(t, pool, projID, domain.WorkItemKindEpic, "Other Parent", nil, nil, nil)
	c1 := createSequenceItem(t, pool, projID, domain.WorkItemKindTask, "C1", &parent.ID, nil, nil)
	c2 := createSequenceItem(t, pool, projID, domain.WorkItemKindTask, "C2", &parent.ID, nil, nil)
	c3 := createSequenceItem(t, pool, projID, domain.WorkItemKindTask, "C3", &parent.ID, nil, nil)
	foreign := createSequenceItem(t, pool, projID, domain.WorkItemKindTask, "Foreign", &otherParent.ID, nil, nil)

	// Reorder [c3, c1, c2] → persists.
	resp, err := s.ReorderWorkItems(ctx, connect.NewRequest(&apiv1.ReorderWorkItemsRequest{
		ProjectId: projID, ParentId: parent.ID,
		ChildIds: []string{c3.ID, c1.ID, c2.ID},
	}))
	if err != nil {
		t.Fatalf("reorder: %v", err)
	}
	if len(resp.Msg.WorkItems) != 3 {
		t.Fatalf("response has %d items, want 3", len(resp.Msg.WorkItems))
	}
	if resp.Msg.WorkItems[0].Id != c3.ID || resp.Msg.WorkItems[1].Id != c1.ID || resp.Msg.WorkItems[2].Id != c2.ID {
		t.Fatalf("reordered order = %s,%s,%s", resp.Msg.WorkItems[0].Id, resp.Msg.WorkItems[1].Id, resp.Msg.WorkItems[2].Id)
	}
	// Verify persisted sort_order.
	got := sequenceChildren(t, pool, parent.ID)
	if got[0].ID != c3.ID || got[1].ID != c1.ID || got[2].ID != c2.ID {
		t.Fatalf("persisted order = %s,%s,%s", got[0].ID, got[1].ID, got[2].ID)
	}
	// A partial reorder appends the unlisted siblings after the requested
	// ones.
	resp, err = s.ReorderWorkItems(ctx, connect.NewRequest(&apiv1.ReorderWorkItemsRequest{
		ProjectId: projID, ParentId: parent.ID, ChildIds: []string{c1.ID},
	}))
	if err != nil {
		t.Fatalf("partial reorder: %v", err)
	}
	got = sequenceChildren(t, pool, parent.ID)
	if got[0].ID != c1.ID || len(got) != 3 {
		t.Fatalf("after partial reorder: first=%s n=%d, want c1 first + all 3", got[0].ID, len(got))
	}

	// Cross-parent child → reject.
	if _, err := s.ReorderWorkItems(ctx, connect.NewRequest(&apiv1.ReorderWorkItemsRequest{
		ProjectId: projID, ParentId: parent.ID, ChildIds: []string{c1.ID, foreign.ID},
	})); err == nil {
		t.Fatal("expected rejection for a foreign child")
	}
	// Duplicate ids → reject.
	if _, err := s.ReorderWorkItems(ctx, connect.NewRequest(&apiv1.ReorderWorkItemsRequest{
		ProjectId: projID, ParentId: parent.ID, ChildIds: []string{c1.ID, c1.ID},
	})); err == nil {
		t.Fatal("expected rejection for duplicate ids")
	}
	// Empty list → reject.
	if _, err := s.ReorderWorkItems(ctx, connect.NewRequest(&apiv1.ReorderWorkItemsRequest{
		ProjectId: projID, ParentId: parent.ID, ChildIds: []string{},
	})); err == nil {
		t.Fatal("expected rejection for an empty list")
	}
}

// --- test helpers -----------------------------------------------------------

func seedPublishedWorkflowForTest(t *testing.T, pool *db.Pool, projID string, publish bool) string {
	t.Helper()
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)
	ttx, err := pool.BeginTenantTx(ctx, validateParentTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	wf, err := db.CreateWorkflow(ctx, ttx.Tx, db.WorkflowRow{
		ID: db.NewID(), TenantID: validateParentTestTenant, ProjectID: projID,
		Name: "wf-" + db.NewID()[:8], CurrentVersion: 0, Status: domain.WorkflowDraft,
		Type: domain.WorkflowTypeTemplate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateWorkflowVersion(ctx, ttx.Tx, db.WorkflowVersionRow{
		ID: db.NewID(), TenantID: validateParentTestTenant, WorkflowID: wf.ID,
		Version: 1, Status: domain.WorkflowVersionDraft,
		Steps: []byte(`[{"id":"step-1","name":"Task","kind":"task","ref":"w_se_devops_engineer","worker_version":0,"depends_on":[],"config":""}]`),
		Inputs: []byte("{}"), Outputs: []byte("{}"),
	}); err != nil {
		t.Fatal(err)
	}
	if publish {
		if _, err := db.PublishWorkflowVersion(ctx, ttx.Tx, validateParentTestTenant, wf.ID, 1); err != nil {
			t.Fatal(err)
		}
		// Mirror the workflow service's publish: bump the header's
		// current_version + status so GetWorkflow reports it runnable.
		if _, err := db.UpdateWorkflowCurrentVersion(ctx, ttx.Tx, validateParentTestTenant, wf.ID, wf.Version, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return wf.ID
}

// seedEmptyDagPublishedWorkflowForTest publishes a workflow whose current
// version has an EMPTY step DAG — a run started on it can never progress
// (the reconciler fails it at start), so a sequence leaf bound to one must
// be rejected at schedule time.
func seedEmptyDagPublishedWorkflowForTest(t *testing.T, pool *db.Pool, projID string) string {
	t.Helper()
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)
	ttx, err := pool.BeginTenantTx(ctx, validateParentTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	wf, err := db.CreateWorkflow(ctx, ttx.Tx, db.WorkflowRow{
		ID: db.NewID(), TenantID: validateParentTestTenant, ProjectID: projID,
		Name: "empty-" + db.NewID()[:8], CurrentVersion: 0, Status: domain.WorkflowDraft,
		Type: domain.WorkflowTypeTemplate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateWorkflowVersion(ctx, ttx.Tx, db.WorkflowVersionRow{
		ID: db.NewID(), TenantID: validateParentTestTenant, WorkflowID: wf.ID,
		Version: 1, Status: domain.WorkflowVersionDraft, Steps: []byte("[]"),
		Inputs: []byte("{}"), Outputs: []byte("{}"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PublishWorkflowVersion(ctx, ttx.Tx, validateParentTestTenant, wf.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateWorkflowCurrentVersion(ctx, ttx.Tx, validateParentTestTenant, wf.ID, wf.Version, 1); err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return wf.ID
}

// TestValidateSequenceRejectsEmptyStepDag verifies a sequence leaf bound to
// a published workflow with an EMPTY step DAG is rejected at schedule time
// (arming it would create a run that can never progress, stranding the
// chain and leaking a runtime container).
func TestValidateSequenceRejectsEmptyStepDag(t *testing.T) {
	pool := validateParentTestPool(t)
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)
	projID := validateParentProject(t, ctx, pool)
	emptyWF := seedEmptyDagPublishedWorkflowForTest(t, pool, projID)

	parent := createSequenceItem(t, pool, projID, domain.WorkItemKindEpic, "Parent", nil, nil, nil)
	_ = createSequenceItem(t, pool, projID, domain.WorkItemKindTask, "Empty Bound", &parent.ID, &emptyWF, nil)

	ttx, err := pool.BeginTenantTx(ctx, validateParentTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	noWorkflow, oneShot, badWorkflow, err := ValidateSequenceSubtree(ctx, ttx.Tx, validateParentTestTenant, mustGetSequenceItem(t, pool, parent.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(noWorkflow) != 0 || len(oneShot) != 0 {
		t.Errorf("unexpected offenders: noWorkflow=%v oneShot=%v", noWorkflow, oneShot)
	}
	if len(badWorkflow) != 1 || badWorkflow[0] != "Empty Bound" {
		t.Errorf("badWorkflow = %v, want [\"Empty Bound\"] (empty step DAG must be rejected)", badWorkflow)
	}
}

func createSequenceItem(t *testing.T, pool *db.Pool, projID, kind, title string, parent *string, workflowID *string, workerRef []byte) db.WorkItemRow {
	t.Helper()
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)
	ttx, err := pool.BeginTenantTx(ctx, validateParentTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	w, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: validateParentTestTenant, ProjectID: projID,
		ParentID: parent, Kind: kind, Title: title, Status: domain.WorkItemPending,
		WorkflowID: workflowID, AssignedWorkerRef: workerRef,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return w
}

func mustGetSequenceItem(t *testing.T, pool *db.Pool, id string) db.WorkItemRow {
	t.Helper()
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)
	ttx, err := pool.BeginTenantTx(ctx, validateParentTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	w, err := db.GetWorkItem(ctx, ttx.Tx, validateParentTestTenant, id)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func sequenceChildren(t *testing.T, pool *db.Pool, parentID string) []db.WorkItemRow {
	t.Helper()
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)
	ttx, err := pool.BeginTenantTx(ctx, validateParentTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	children, err := db.ListDirectChildren(ctx, ttx.Tx, validateParentTestTenant, parentID)
	if err != nil {
		t.Fatal(err)
	}
	return children
}
