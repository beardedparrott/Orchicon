package workitem

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// readItem loads a work item in a short-lived transaction that is rolled
// back immediately, so tests never hold pool connections across steps.
func readItem(t *testing.T, pool *db.Pool, id string) db.WorkItemRow {
	t.Helper()
	ttx, err := pool.BeginTenantTx(context.Background(), validateParentTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = ttx.Rollback(context.Background()) }()
	w, err := db.GetWorkItem(context.Background(), ttx.Tx, validateParentTestTenant, id)
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return w
}

// writeItem applies a partial update in a committed transaction.
func writeItem(t *testing.T, pool *db.Pool, id string, version int, fields db.UpdateWorkItemFields) {
	t.Helper()
	ttx, err := pool.BeginTenantTx(context.Background(), validateParentTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = ttx.Rollback(context.Background()) }()
	if _, err := db.UpdateWorkItem(context.Background(), ttx.Tx, validateParentTestTenant, id, version, fields); err != nil {
		t.Fatalf("update %s: %v", id, err)
	}
	if err := ttx.Commit(context.Background()); err != nil {
		t.Fatalf("commit %s: %v", id, err)
	}
}

// readItemTx returns a short-lived tenant transaction for one-off reads
// (e.g. outbox assertions); the caller must roll it back.
func readItemTx(t *testing.T, pool *db.Pool) pgx.Tx {
	t.Helper()
	ttx, err := pool.BeginTenantTx(context.Background(), validateParentTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	t.Cleanup(func() { _ = ttx.Rollback(context.Background()) })
	return ttx.Tx
}

// TestKindSwitchPreconditions verifies the system-managed preconditions
// without needing a DB: a running item and an active workflow run must be
// rejected before any resolution happens. (The pool here only serves the
// helper's transaction plumbing — these checks return before touching the
// DB beyond BeginTenantTx.)
func TestKindSwitchPreconditions(t *testing.T) {
	pool := validateParentTestPool(t)
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)
	projectA := validateParentProject(t, ctx, pool)
	ttx, err := pool.BeginTenantTx(ctx, validateParentTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	// A running item must not be re-typed.
	running := db.WorkItemRow{Status: domain.WorkItemRunning}
	if _, err := ResolveKindSwitch(ctx, ttx.Tx, validateParentTestTenant, running, domain.WorkItemKindFeature, nil, projectA); !errors.Is(err, ErrKindSwitchRunning) {
		t.Fatalf("running item: want ErrKindSwitchRunning, got %v", err)
	}
	// A checkpointing/recovering item is equally unmovable.
	for _, st := range []string{domain.WorkItemCheckpointing, domain.WorkItemRecovering} {
		row := db.WorkItemRow{Status: st}
		if _, err := ResolveKindSwitch(ctx, ttx.Tx, validateParentTestTenant, row, domain.WorkItemKindFeature, nil, projectA); !errors.Is(err, ErrKindSwitchRunning) {
			t.Fatalf("%s item: want ErrKindSwitchRunning, got %v", st, err)
		}
	}

	// An invalid kind is rejected with a NormalizeWorkItemKind error.
	row := db.WorkItemRow{Status: domain.WorkItemPending}
	if _, err := ResolveKindSwitch(ctx, ttx.Tx, validateParentTestTenant, row, "banana", nil, projectA); err == nil {
		t.Fatal("invalid kind should be rejected")
	}
}

// TestResolveKindSwitchDB exercises the shared resolution helper against a
// real DB: parent walk-up, child reparenting, schedulability cleanup, and
// the explicit-parent path.
func TestResolveKindSwitchDB(t *testing.T) {
	pool := validateParentTestPool(t)
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)
	projectA := validateParentProject(t, ctx, pool)

	// Tree: epic → feature → task → subtask.
	epic := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindEpic, nil)
	feature := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindFeature, &epic.ID)
	task := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindTask, &feature.ID)
	subtask := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindSubtask, &task.ID)

	ttx, err := pool.BeginTenantTx(ctx, validateParentTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	// Task → Feature: the parent (Feature, depth 2) is no longer shallower
	// than a Feature (depth 2), so the parent walks up to the Epic.
	plan, err := ResolveKindSwitch(ctx, ttx.Tx, validateParentTestTenant, task, domain.WorkItemKindFeature, nil, projectA)
	if err != nil {
		t.Fatalf("task → feature: %v", err)
	}
	if plan.NewParentID == nil || *plan.NewParentID != epic.ID {
		t.Fatalf("task → feature parent = %v, want epic %s", plan.NewParentID, epic.ID)
	}
	// The feature is not schedulable: worker/schedule/status cleanup.
	if !plan.ClearWorkerRef || !plan.ClearScheduledStartAt {
		t.Fatal("task → feature should clear worker ref and scheduled start")
	}
	if plan.NewStatus != nil {
		t.Fatalf("pending task → feature should keep its status, got %v", *plan.NewStatus)
	}

	// Feature → Epic: the parent is cleared (epics are top-level).
	plan, err = ResolveKindSwitch(ctx, ttx.Tx, validateParentTestTenant, feature, domain.WorkItemKindEpic, nil, projectA)
	if err != nil {
		t.Fatalf("feature → epic: %v", err)
	}
	if plan.NewParentID != nil {
		t.Fatalf("feature → epic parent = %v, want nil", plan.NewParentID)
	}

	// Subtask → Task: parent (Task, depth 3) is not strictly shallower
	// than a Task (depth 3), so the walk-up finds the Feature (depth 2).
	plan, err = ResolveKindSwitch(ctx, ttx.Tx, validateParentTestTenant, subtask, domain.WorkItemKindTask, nil, projectA)
	if err != nil {
		t.Fatalf("subtask → task: %v", err)
	}
	if plan.NewParentID == nil || *plan.NewParentID != feature.ID {
		t.Fatalf("subtask → task parent = %v, want feature %s", plan.NewParentID, feature.ID)
	}

	// Epic → Feature without a parent: rejected (a parent must be chosen).
	_, err = ResolveKindSwitch(ctx, ttx.Tx, validateParentTestTenant, epic, domain.WorkItemKindFeature, nil, projectA)
	if err == nil || !strings.Contains(err.Error(), "must have a parent") {
		t.Fatalf("epic → feature without parent should be rejected, got %v", err)
	}
	// Epic → Feature WITH an explicit shallower parent (an epic) succeeds.
	epic2 := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindEpic, nil)
	plan, err = ResolveKindSwitch(ctx, ttx.Tx, validateParentTestTenant, epic2, domain.WorkItemKindFeature, strPtr(epic.ID), projectA)
	if err != nil {
		t.Fatalf("epic → feature with explicit parent: %v", err)
	}
	if plan.NewParentID == nil || *plan.NewParentID != epic.ID {
		t.Fatalf("epic → feature explicit parent = %v, want epic %s", plan.NewParentID, epic.ID)
	}

	// Explicit parent equal to the current parent is treated as "keep":
	// task → feature while naming its current parent (feature) must NOT
	// reject — the walk-up resolves it instead.
	plan, err = ResolveKindSwitch(ctx, ttx.Tx, validateParentTestTenant, task, domain.WorkItemKindFeature, strPtr(feature.ID), projectA)
	if err != nil {
		t.Fatalf("task → feature with same parent named: %v", err)
	}
	if plan.NewParentID == nil || *plan.NewParentID != epic.ID {
		t.Fatalf("task → feature (same parent named) resolved to %v, want epic %s", plan.NewParentID, epic.ID)
	}
}

// TestKindSwitchChildrenReparentDB verifies that direct children that can
// no longer sit under a switched item move under its resolved parent, and
// that an explicit invalid parent is rejected.
func TestKindSwitchChildrenReparentDB(t *testing.T) {
	pool := validateParentTestPool(t)
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)
	projectA := validateParentProject(t, ctx, pool)

	epic := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindEpic, nil)
	task := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindTask, &epic.ID)
	subtask := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindSubtask, &task.ID)

	ttx, err := pool.BeginTenantTx(ctx, validateParentTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	// Task (depth 3) with a Subtask child (depth 4) switched to Subtask
	// (depth 4): the child (depth 4 <= 4) must move under the item's
	// resolved parent. The item's parent is Epic (depth 1 < 4), so the
	// child becomes a sibling under the Epic.
	plan, err := ResolveKindSwitch(ctx, ttx.Tx, validateParentTestTenant, task, domain.WorkItemKindSubtask, nil, projectA)
	if err != nil {
		t.Fatalf("task → subtask: %v", err)
	}
	if len(plan.ReparentedChildren) != 1 {
		t.Fatalf("task → subtask should reparent 1 child, got %d", len(plan.ReparentedChildren))
	}
	cr := plan.ReparentedChildren[0]
	if cr.ChildID != subtask.ID || cr.NewParentID == nil || *cr.NewParentID != epic.ID {
		t.Fatalf("reparented child = %v → %v, want %s → %s", cr.ChildID, cr.NewParentID, subtask.ID, epic.ID)
	}

	// An explicit parent that is invalid for the NEW kind is rejected:
	// switching the task to a Feature with a Task parent (same depth).
	siblingTask := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindTask, &epic.ID)
	_, err = ResolveKindSwitch(ctx, ttx.Tx, validateParentTestTenant, task, domain.WorkItemKindFeature, strPtr(siblingTask.ID), projectA)
	if err == nil || !strings.Contains(err.Error(), "must be deeper than its parent") {
		t.Fatalf("explicit same-depth parent should be rejected, got %v", err)
	}

	// An explicit SELF parent must be rejected even when the depth rule
	// would pass (an epic switched to a feature has itself as a
	// shallower row until the kind actually changes).
	_, err = ResolveKindSwitch(ctx, ttx.Tx, validateParentTestTenant, epic, domain.WorkItemKindFeature, strPtr(epic.ID), projectA)
	if err == nil || !strings.Contains(err.Error(), "its own parent") {
		t.Fatalf("explicit self-parent should be rejected, got %v", err)
	}
}

// TestKindSwitchBlockedItemStaysBlocked is the regression test for the
// kind-switch cleanup edge: a BLOCKED item switched to a non-schedulable
// kind is not in the ready/assigned/scheduled/recurring demote list, so it
// keeps its blocked status. This is intentional and harmless — the
// reconcilers still process blocked items and clear them when the gate
// satisfies — but it must stay stable so operators never see the system
// status silently rewritten by a kind switch.
func TestKindSwitchBlockedItemStaysBlocked(t *testing.T) {
	pool := validateParentTestPool(t)
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)
	projectA := validateParentProject(t, ctx, pool)

	epic := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindEpic, nil)
	blockedTask := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindTask, &epic.ID)
	writeItem(t, pool, blockedTask.ID, blockedTask.Version, db.UpdateWorkItemFields{
		Status: strPtr(domain.WorkItemBlocked),
	})
	blockedTask = readItem(t, pool, blockedTask.ID)
	if blockedTask.Status != domain.WorkItemBlocked {
		t.Fatalf("fixture should be blocked, got %q", blockedTask.Status)
	}

	ttx, err := pool.BeginTenantTx(ctx, validateParentTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	plan, err := ResolveKindSwitch(ctx, ttx.Tx, validateParentTestTenant, blockedTask, domain.WorkItemKindFeature, nil, projectA)
	if err != nil {
		t.Fatalf("blocked task → feature: %v", err)
	}
	// The blocked status is NOT demoted (it is system-managed; the
	// reconcilers own it) — NewStatus must stay nil.
	if plan.NewStatus != nil {
		t.Fatalf("blocked → feature demoted status to %q, want untouched", *plan.NewStatus)
	}
	if !plan.ClearWorkerRef {
		t.Fatal("blocked → feature should still clear the worker ref (non-schedulable)")
	}

	// Apply the plan the way the UpdateWorkItem handler does (kind +
	// plan fields) and confirm the item keeps blocked end-to-end.
	apply := db.UpdateWorkItemFields{Kind: strPtr(domain.WorkItemKindFeature)}
	if plan.NewStatus != nil {
		apply.Status = plan.NewStatus
	}
	if plan.ClearWorkerRef {
		apply.ClearAssignedWorkerRef = true
	}
	applied := readItem(t, pool, blockedTask.ID)
	if _, err := db.UpdateWorkItem(ctx, ttx.Tx, validateParentTestTenant, applied.ID, applied.Version, apply); err != nil {
		t.Fatalf("apply switch: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	after := readItem(t, pool, blockedTask.ID)
	if after.Status != domain.WorkItemBlocked {
		t.Errorf("status after kind switch = %q, want %q (system-managed, untouched)", after.Status, domain.WorkItemBlocked)
	}
	if after.Kind != domain.WorkItemKindFeature {
		t.Errorf("kind after switch = %q, want feature", after.Kind)
	}
}

// TestKindSwitchProjectMoveDB verifies the interaction between a kind
// switch and a project move (ADR-WIT-1/2 + the carried-parent guard):
// moving to another project with a kind switch requires a parent in the
// target project, an explicit NEW parent is honored, a "keep the current
// parent" request is rejected when the current parent does not live in
// the target project, and the resolver's walk-up never crosses projects.
func TestKindSwitchProjectMoveDB(t *testing.T) {
	pool := validateParentTestPool(t)
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)
	s := New(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	projectA := validateParentProject(t, ctx, pool)
	projectB := validateParentProject(t, ctx, pool)

	epicA := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindEpic, nil)
	featureA := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindFeature, &epicA.ID)
	taskA := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindTask, &featureA.ID)
	epicB := validateParentItem(t, ctx, pool, projectB, domain.WorkItemKindEpic, nil)

	update := func(item db.WorkItemRow, mutate func(req *apiv1.UpdateWorkItemRequest)) error {
		req := connect.NewRequest(&apiv1.UpdateWorkItemRequest{Id: item.ID})
		mutate(req.Msg)
		_, err := s.UpdateWorkItem(ctx, req)
		return err
	}
	kind := func(k apiv1.WorkItemKind) *apiv1.WorkItemKind { return &k }

	// 1. Kind switch + project move + explicit NEW parent in the target
	// project succeeds and reparents to it. (Regression test: the
	// carried-parent guard used to run after ResolveKindSwitch had
	// already validated the explicit parent against the target project,
	// falsely rejecting the request with "parent must be in the same
	// project".)
	err := update(taskA, func(m *apiv1.UpdateWorkItemRequest) {
		m.Kind = kind(apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE)
		m.ProjectId = strPtr(projectB)
		m.ParentId = strPtr(epicB.ID)
	})
	if err != nil {
		t.Fatalf("kind switch + project move + explicit parent: %v", err)
	}
	cur := readItem(t, pool, taskA.ID)
	if cur.ProjectID != projectB || cur.Kind != domain.WorkItemKindFeature || cur.ParentID == nil || *cur.ParentID != epicB.ID {
		t.Fatalf("after switch+move = project %s kind %s parent %v, want project %s kind feature parent %s",
			cur.ProjectID, cur.Kind, cur.ParentID, projectB, epicB.ID)
	}

	// 2. Kind switch + project move + no explicit parent is rejected: the
	// walk-up would resolve an ancestor from the old project.
	taskNoParent := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindTask, &featureA.ID)
	err = update(taskNoParent, func(m *apiv1.UpdateWorkItemRequest) {
		m.Kind = kind(apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE)
		m.ProjectId = strPtr(projectB)
	})
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("switch+move without a parent should be rejected, got %v", err)
	}

	// 3. Kind switch + project move + "keep the current parent" (explicit
	// parent == current parent, which still lives in the OLD project) is
	// rejected — treating it as a reparent would let the walk-up write a
	// cross-project parent.
	taskKeep := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindTask, &featureA.ID)
	err = update(taskKeep, func(m *apiv1.UpdateWorkItemRequest) {
		m.Kind = kind(apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE)
		m.ProjectId = strPtr(projectB)
		m.ParentId = strPtr(featureA.ID) // == current parent, still in project A
	})
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("switch+move keeping a parent from the old project should be rejected, got %v", err)
	}

	// 4. Kind switch + project move + keep the current parent when it
	// ALREADY lives in the target project succeeds (the "parent moved
	// first" pattern): the walk-up resolves within the target project.
	epicC := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindEpic, nil)
	featureC := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindFeature, &epicC.ID)
	if err := update(epicC, func(m *apiv1.UpdateWorkItemRequest) { m.ProjectId = strPtr(projectB) }); err != nil {
		t.Fatalf("move epicC to B first: %v", err)
	}
	err = update(featureC, func(m *apiv1.UpdateWorkItemRequest) {
		m.Kind = kind(apiv1.WorkItemKind_WORK_ITEM_KIND_TASK)
		m.ProjectId = strPtr(projectB)
		m.ParentId = strPtr(epicC.ID) // == current parent, now in project B
	})
	if err != nil {
		t.Fatalf("switch+move keeping a parent already in the target project: %v", err)
	}
	cur = readItem(t, pool, featureC.ID)
	if cur.ProjectID != projectB || cur.Kind != domain.WorkItemKindTask || cur.ParentID == nil || *cur.ParentID != epicC.ID {
		t.Fatalf("after keep-parent switch+move = project %s kind %s parent %v, want project %s kind task parent %s",
			cur.ProjectID, cur.Kind, cur.ParentID, projectB, epicC.ID)
	}

	// 5. The resolver's walk-up backstop: a keep-parent resolution that
	// would cross back into the old project is rejected even when this
	// request does not move the project (the item already lives in B but
	// its parent chain was left behind when the parent moved back to A).
	epicD := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindEpic, nil)
	featureD := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindFeature, &epicD.ID)
	if err := update(epicD, func(m *apiv1.UpdateWorkItemRequest) { m.ProjectId = strPtr(projectB) }); err != nil {
		t.Fatalf("move epicD to B: %v", err)
	}
	if err := update(featureD, func(m *apiv1.UpdateWorkItemRequest) {
		m.ProjectId = strPtr(projectB)
		m.ParentId = strPtr(epicD.ID)
	}); err != nil {
		t.Fatalf("move featureD to B keeping parent: %v", err)
	}
	if err := update(epicD, func(m *apiv1.UpdateWorkItemRequest) { m.ProjectId = strPtr(projectA) }); err != nil {
		t.Fatalf("move epicD back to A: %v", err)
	}
	err = update(featureD, func(m *apiv1.UpdateWorkItemRequest) {
		m.Kind = kind(apiv1.WorkItemKind_WORK_ITEM_KIND_TASK)
		m.ParentId = strPtr(epicD.ID) // == current parent, but the chain now lives in A
	})
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("keep-parent walk-up crossing projects should be rejected, got %v", err)
	}
	cur = readItem(t, pool, featureD.ID)
	if cur.Kind != domain.WorkItemKindFeature || cur.ProjectID != projectB {
		t.Fatalf("item should be untouched after rejected switch, kind = %s project = %s", cur.Kind, cur.ProjectID)
	}
}

// TestKindSwitchAutoStartDB verifies that a kind switch never triggers the
// post-commit auto-start on its own — the user re-typed the item, they did
// not ask to start it now (ADR-WIT-2). This holds for switches to a
// non-schedulable kind (which clears the schedule) AND between schedulable
// kinds (Task → Subtask), where the destructive bug lived: nothing was
// cleared, so the post-commit auto-start fired whenever the item had
// auto_start_workflow=true stored (the old default) and no scheduled time.
// An explicit autoStartWorkflow=true in the same request still wins.
func TestKindSwitchAutoStartDB(t *testing.T) {
	pool := validateParentTestPool(t)
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)
	s := New(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	projectA := validateParentProject(t, ctx, pool)
	epic := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindEpic, nil)

	future := time.Now().Add(24 * time.Hour)
	// mkScheduled: auto-start task with a future scheduled start (the
	// switch to a non-schedulable kind clears the schedule).
	mkScheduled := func() db.WorkItemRow {
		t.Helper()
		item := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindTask, &epic.ID)
		writeItem(t, pool, item.ID, item.Version, db.UpdateWorkItemFields{
			WorkflowID:        strPtr("wf-kind-switch-test"),
			ScheduledStartAt:  &future,
			AutoStartWorkflow: boolPtr(true),
			Status:            strPtr(domain.WorkItemScheduled),
		})
		return readItem(t, pool, item.ID)
	}
	// mkImmediate: auto-start task with NO scheduled start — the setup
	// where a plain kind switch used to kick off a run immediately.
	mkImmediate := func() db.WorkItemRow {
		t.Helper()
		item := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindTask, &epic.ID)
		writeItem(t, pool, item.ID, item.Version, db.UpdateWorkItemFields{
			WorkflowID:        strPtr("wf-kind-switch-test"),
			AutoStartWorkflow: boolPtr(true),
			Status:            strPtr(domain.WorkItemReady),
		})
		return readItem(t, pool, item.ID)
	}

	var started []string
	s.SetStartWorkflowStarter(func(ctx context.Context, tenantID, workflowID, projectID, workItemID string) error {
		started = append(started, workItemID)
		return nil
	})

	update := func(item db.WorkItemRow, mutate func(req *apiv1.UpdateWorkItemRequest)) error {
		req := connect.NewRequest(&apiv1.UpdateWorkItemRequest{Id: item.ID})
		mutate(req.Msg)
		_, err := s.UpdateWorkItem(ctx, req)
		return err
	}
	kind := func(k apiv1.WorkItemKind) *apiv1.WorkItemKind { return &k }

	// 1. Scheduled, auto-start task switched to a feature: the switch
	// clears the schedule, so the post-commit auto-start must NOT fire.
	sched1 := mkScheduled()
	if err := update(sched1, func(m *apiv1.UpdateWorkItemRequest) {
		m.Kind = kind(apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE)
	}); err != nil {
		t.Fatalf("switch scheduled task → feature: %v", err)
	}
	cur := readItem(t, pool, sched1.ID)
	if cur.ScheduledStartAt != nil {
		t.Fatalf("scheduled_start_at should be cleared, got %v", cur.ScheduledStartAt)
	}
	if len(started) != 0 {
		t.Fatalf("auto-start fired after kind switch cleared the schedule: %v", started)
	}

	// 2. Same setup but the request explicitly asks to auto-start: the
	// explicit request wins and the run starts.
	sched2 := mkScheduled()
	if err := update(sched2, func(m *apiv1.UpdateWorkItemRequest) {
		m.Kind = kind(apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE)
		m.AutoStartWorkflow = boolPtr(true)
	}); err != nil {
		t.Fatalf("switch scheduled task → feature with explicit auto-start: %v", err)
	}
	if len(started) != 1 || started[0] != sched2.ID {
		t.Fatalf("explicit auto-start should fire once for sched2, got %v", started)
	}

	// 3. The reported bug: an IMMEDIATE auto-start task switched to a
	// schedulable kind (Task → Subtask) with NO explicit autoStartWorkflow
	// in the request must NOT auto-start. The old guard only suppressed
	// when the switch cleared the schedule (non-schedulable kinds); here
	// nothing is cleared and ScheduledStartAt is nil, so the post-commit
	// auto-start fired and kicked off a run the user never asked for.
	immediate1 := mkImmediate()
	if err := update(immediate1, func(m *apiv1.UpdateWorkItemRequest) {
		m.Kind = kind(apiv1.WorkItemKind_WORK_ITEM_KIND_SUBTASK)
	}); err != nil {
		t.Fatalf("switch immediate task → subtask: %v", err)
	}
	cur3 := readItem(t, pool, immediate1.ID)
	if cur3.Kind != domain.WorkItemKindSubtask {
		t.Fatalf("immediate1 kind = %s, want subtask", cur3.Kind)
	}
	if len(started) != 1 || started[0] != sched2.ID {
		t.Fatalf("schedulable kind switch must not auto-start: %v", started)
	}

	// 4. An explicit autoStartWorkflow=true on a schedulable switch still
	// wins (same contract as the non-schedulable case).
	immediate2 := mkImmediate()
	if err := update(immediate2, func(m *apiv1.UpdateWorkItemRequest) {
		m.Kind = kind(apiv1.WorkItemKind_WORK_ITEM_KIND_SUBTASK)
		m.AutoStartWorkflow = boolPtr(true)
	}); err != nil {
		t.Fatalf("switch immediate task → subtask with explicit auto-start: %v", err)
	}
	if len(started) != 2 || started[1] != immediate2.ID {
		t.Fatalf("explicit auto-start should fire once for immediate2, got %v", started)
	}
}

func boolPtr(b bool) *bool { return &b }

func statusPtr(s apiv1.WorkItemStatus) *apiv1.WorkItemStatus { return &s }

// TestUpdateWorkItemKindSwitchDB exercises the full UpdateWorkItem handler:
// kind switching with parent walk-up, child reparenting, schedulability
// cleanup, events, and the system-managed preconditions.
func TestUpdateWorkItemKindSwitchDB(t *testing.T) {
	pool := validateParentTestPool(t)
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)
	s := New(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	projectA := validateParentProject(t, ctx, pool)
	epic := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindEpic, nil)
	feature := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindFeature, &epic.ID)
	task := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindTask, &feature.ID)
	subtask := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindSubtask, &task.ID)

	update := func(item db.WorkItemRow, mutate func(req *apiv1.UpdateWorkItemRequest)) error {
		req := connect.NewRequest(&apiv1.UpdateWorkItemRequest{Id: item.ID})
		mutate(req.Msg)
		_, err := s.UpdateWorkItem(ctx, req)
		return err
	}
	kind := func(k apiv1.WorkItemKind) *apiv1.WorkItemKind { return &k }

	// 1. Task → Feature: parent walks up to the Epic, worker binding and
	// scheduled start are cleared, ready → pending. Give the task a worker
	// binding and a ready status first.
	workerRef := []byte(`{"worker_id":"w1","version":1}`)
	writeItem(t, pool, task.ID, task.Version, db.UpdateWorkItemFields{
		AssignedWorkerRef: &workerRef,
		Status:            strPtr(domain.WorkItemReady),
	})
	// A same-kind switch is a no-op and must not fail.
	err := update(feature, func(m *apiv1.UpdateWorkItemRequest) { m.Kind = kind(apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE) })
	if err != nil {
		t.Fatalf("switch feature → feature (same kind no-op): %v", err)
	}
	err = update(task, func(m *apiv1.UpdateWorkItemRequest) { m.Kind = kind(apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE) })
	if err != nil {
		t.Fatalf("switch task → feature: %v", err)
	}
	cur := readItem(t, pool, task.ID)
	if cur.Kind != domain.WorkItemKindFeature {
		t.Fatalf("task kind = %s, want feature", cur.Kind)
	}
	if cur.ParentID == nil || *cur.ParentID != epic.ID {
		t.Fatalf("task parent = %v, want epic %s", cur.ParentID, epic.ID)
	}
	if cur.Status != domain.WorkItemPending {
		t.Fatalf("task status = %s, want pending", cur.Status)
	}
	if cur.AssignedWorkerRef != nil {
		t.Fatalf("task worker ref should be cleared, got %s", string(cur.AssignedWorkerRef))
	}
	// The switch emits work_item.kind_changed with old_kind + new_kind.
	var evt []byte
	qerr := readItemTx(t, pool).QueryRow(ctx,
		`SELECT payload FROM outbox WHERE tenant_id = $1 AND event_type = 'work_item.kind_changed' AND aggregate_id = $2 ORDER BY occurred_at DESC LIMIT 1`,
		validateParentTestTenant, task.ID).Scan(&evt)
	if qerr != nil {
		t.Fatalf("read kind_changed event: %v", qerr)
	}
	if !strings.Contains(string(evt), "task") || !strings.Contains(string(evt), "feature") {
		t.Fatalf("kind_changed payload missing kinds: %s", string(evt))
	}

	// 2. Feature → Epic: parent cleared.
	err = update(feature, func(m *apiv1.UpdateWorkItemRequest) { m.Kind = kind(apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC) })
	if err != nil {
		t.Fatalf("switch feature → epic: %v", err)
	}
	cur = readItem(t, pool, feature.ID)
	if cur.Kind != domain.WorkItemKindEpic || cur.ParentID != nil {
		t.Fatalf("feature after epic switch = kind %s parent %v, want epic + nil", cur.Kind, cur.ParentID)
	}

	// 3. The (now feature) item with a Subtask child → Subtask: the child
	// moves under the item's parent (the Epic).
	item := readItem(t, pool, task.ID)
	child := readItem(t, pool, subtask.ID)
	if child.ParentID == nil || *child.ParentID != task.ID {
		t.Fatalf("precondition: subtask parent = %v, want task", child.ParentID)
	}
	err = update(item, func(m *apiv1.UpdateWorkItemRequest) { m.Kind = kind(apiv1.WorkItemKind_WORK_ITEM_KIND_SUBTASK) })
	if err != nil {
		t.Fatalf("switch feature → subtask: %v", err)
	}
	cur = readItem(t, pool, task.ID)
	if cur.Kind != domain.WorkItemKindSubtask {
		t.Fatalf("switched item kind = %s, want subtask", cur.Kind)
	}
	if cur.ParentID == nil || *cur.ParentID != epic.ID {
		t.Fatalf("switched item parent = %v, want epic", cur.ParentID)
	}
	child = readItem(t, pool, subtask.ID)
	if child.ParentID == nil || *child.ParentID != epic.ID {
		t.Fatalf("reparented subtask parent = %v, want epic", child.ParentID)
	}

	// 4. Running item → FailedPrecondition.
	running := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindTask, &epic.ID)
	writeItem(t, pool, running.ID, running.Version, db.UpdateWorkItemFields{
		Status: strPtr(domain.WorkItemRunning),
	})
	err = update(running, func(m *apiv1.UpdateWorkItemRequest) { m.Kind = kind(apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE) })
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("running switch should be FailedPrecondition, got %v", err)
	}

	// 5. Epic → Feature without a parent → InvalidArgument.
	err = update(epic, func(m *apiv1.UpdateWorkItemRequest) { m.Kind = kind(apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE) })
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("epic → feature without parent should be InvalidArgument, got %v", err)
	}

	// 6. Epic → Feature WITH an explicit parent succeeds.
	epic2 := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindEpic, nil)
	err = update(epic2, func(m *apiv1.UpdateWorkItemRequest) {
		m.Kind = kind(apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE)
		m.ParentId = strPtr(epic.ID)
	})
	if err != nil {
		t.Fatalf("epic → feature with explicit parent: %v", err)
	}
	cur = readItem(t, pool, epic2.ID)
	if cur.Kind != domain.WorkItemKindFeature || cur.ParentID == nil || *cur.ParentID != epic.ID {
		t.Fatalf("epic2 after switch = kind %s parent %v, want feature under %s", cur.Kind, cur.ParentID, epic.ID)
	}
}

// TestCreateWorkItemAutoStartDefaultDB verifies the bug-fix default: a new
// work item created without an explicit autoStartWorkflow must have
// auto_start_workflow = false ("Start immediately on save" is opt-in, never
// the default). An explicit true in the create request still wins.
func TestCreateWorkItemAutoStartDefaultDB(t *testing.T) {
	pool := validateParentTestPool(t)
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)
	s := New(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	projectA := validateParentProject(t, ctx, pool)

	create := func(mutate func(m *apiv1.CreateWorkItemRequest)) db.WorkItemRow {
		t.Helper()
		req := connect.NewRequest(&apiv1.CreateWorkItemRequest{
			ProjectId: projectA,
			Kind:      apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC,
			Title:     "Auto-start default " + strings.ToLower(db.NewID()),
			Priority:  0,
		})
		mutate(req.Msg)
		resp, err := s.CreateWorkItem(ctx, req)
		if err != nil {
			t.Fatalf("create work item: %v", err)
		}
		return readItem(t, pool, resp.Msg.WorkItem.Id)
	}

	// 1. Unset autoStartWorkflow → defaults to false (the fix).
	item := create(func(m *apiv1.CreateWorkItemRequest) {})
	if item.AutoStartWorkflow {
		t.Fatalf("new work item without explicit auto-start should default to false, got true")
	}

	// 2. Explicit true → stays true (opt-in honored).
	item2 := create(func(m *apiv1.CreateWorkItemRequest) {
		m.AutoStartWorkflow = boolPtr(true)
	})
	if !item2.AutoStartWorkflow {
		t.Fatalf("explicit auto-start true should be honored, got false")
	}

	// 3. Explicit false → false.
	item3 := create(func(m *apiv1.CreateWorkItemRequest) {
		m.AutoStartWorkflow = boolPtr(false)
	})
	if item3.AutoStartWorkflow {
		t.Fatalf("explicit auto-start false should be honored, got true")
	}
}

// TestUpdateWorkItemScheduleFlipsStatusDB verifies the bug-fix contract:
// saving a scheduled_start_at in UpdateWorkItem flips the edited item's
// status to "scheduled" (AC2), and ONLY the edited item — sibling
// scheduled items are untouched (no bulk flip). Guarded against in-flight
// runs (a running item keeps its run status so the reconciler cannot fire
// a duplicate run) and against a kind switch that clears the schedule.
// See architecture-notes/running-workflows-not-showing-in-schedules.md
// (ADR-001).
func TestUpdateWorkItemScheduleFlipsStatusDB(t *testing.T) {
	pool := validateParentTestPool(t)
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)
	s := New(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	projectA := validateParentProject(t, ctx, pool)
	epic := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindEpic, nil)

	future := time.Now().Add(24 * time.Hour)
	tspb := timestamppb.New(future)

	update := func(item db.WorkItemRow, mutate func(req *apiv1.UpdateWorkItemRequest)) (*connect.Response[apiv1.UpdateWorkItemResponse], error) {
		t.Helper()
		req := connect.NewRequest(&apiv1.UpdateWorkItemRequest{Id: item.ID})
		mutate(req.Msg)
		return s.UpdateWorkItem(ctx, req)
	}
	kind := func(k apiv1.WorkItemKind) *apiv1.WorkItemKind { return &k }

	// 1. Item in `ready` + schedule set → status becomes `scheduled`.
	// A leaf needs a workflow bound to be schedulable (a workflow-less leaf
	// is rejected below).
	wfID := seedPublishedWorkflowForTest(t, pool, projectA, true)
	readyItem := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindTask, &epic.ID)
	writeItem(t, pool, readyItem.ID, readyItem.Version, db.UpdateWorkItemFields{
		Status:     strPtr(domain.WorkItemReady),
		WorkflowID: strPtr(wfID),
	})
	resp, err := update(readyItem, func(m *apiv1.UpdateWorkItemRequest) {
		m.ScheduledStartAt = tspb
	})
	if err != nil {
		t.Fatalf("schedule a ready item: %v", err)
	}
	if resp.Msg.WorkItem.Status != apiv1.WorkItemStatus_WORK_ITEM_STATUS_SCHEDULED {
		t.Fatalf("status after scheduling = %v, want scheduled", resp.Msg.WorkItem.Status)
	}
	cur := readItem(t, pool, readyItem.ID)
	if cur.Status != domain.WorkItemScheduled {
		t.Fatalf("db status after scheduling = %s, want scheduled", cur.Status)
	}
	if cur.ScheduledStartAt == nil {
		t.Fatalf("scheduled_start_at should be persisted")
	}

	// 1b. A workflow-less LEAF cannot be scheduled — reject with a clear
	// error instead of silently storing a schedule that would never fire.
	noWF := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindTask, &epic.ID)
	_, err = update(noWF, func(m *apiv1.UpdateWorkItemRequest) {
		m.ScheduledStartAt = tspb
	})
	if err == nil || !strings.Contains(err.Error(), "no workflow is set") {
		t.Fatalf("scheduling a workflow-less leaf should be rejected with 'no workflow is set', got %v", err)
	}

	// 2. A sibling item with its own schedule is untouched (no bulk flip).
	sibling := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindTask, &epic.ID)
	writeItem(t, pool, sibling.ID, sibling.Version, db.UpdateWorkItemFields{
		Status:            strPtr(domain.WorkItemScheduled),
		ScheduledStartAt:  &future,
	})
	after := readItem(t, pool, sibling.ID)
	if after.Status != domain.WorkItemScheduled {
		t.Fatalf("sibling status changed to %s, want scheduled untouched", after.Status)
	}

	// 3. The form echoes the current status — the schedule flip must
	// override it (the dropdown would otherwise fight the flip).
	ready2 := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindTask, &epic.ID)
	writeItem(t, pool, ready2.ID, ready2.Version, db.UpdateWorkItemFields{
		Status:     strPtr(domain.WorkItemReady),
		WorkflowID: strPtr(wfID),
	})
	resp3, err := update(ready2, func(m *apiv1.UpdateWorkItemRequest) {
		m.ScheduledStartAt = tspb
		m.Status = statusPtr(apiv1.WorkItemStatus_WORK_ITEM_STATUS_READY)
	})
	if err != nil {
		t.Fatalf("schedule a ready item with echoed status: %v", err)
	}
	if resp3.Msg.WorkItem.Status != apiv1.WorkItemStatus_WORK_ITEM_STATUS_SCHEDULED {
		t.Fatalf("echoed status should lose to the schedule flip, got %v", resp3.Msg.WorkItem.Status)
	}

	// 4. Active-run guard: a `running` item keeps its status so
	// ScheduledRunReconciler cannot re-arm an in-flight run.
	runningItem := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindTask, &epic.ID)
	writeItem(t, pool, runningItem.ID, runningItem.Version, db.UpdateWorkItemFields{
		Status:     strPtr(domain.WorkItemRunning),
		WorkflowID: strPtr(wfID),
	})
	resp4, err := update(runningItem, func(m *apiv1.UpdateWorkItemRequest) {
		m.ScheduledStartAt = tspb
	})
	if err != nil {
		t.Fatalf("schedule a running item: %v", err)
	}
	if resp4.Msg.WorkItem.Status != apiv1.WorkItemStatus_WORK_ITEM_STATUS_RUNNING {
		t.Fatalf("running item status = %v, want running preserved", resp4.Msg.WorkItem.Status)
	}
	cur4 := readItem(t, pool, runningItem.ID)
	if cur4.Status != domain.WorkItemRunning {
		t.Fatalf("db running item status = %s, want running preserved", cur4.Status)
	}
	if cur4.ScheduledStartAt == nil {
		t.Fatalf("running item schedule should still be stored")
	}

	// 5. Kind-switch precedence: switching to a non-schedulable kind clears
	// the schedule and demotes the status — the flip must not win.
	switching := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindTask, &epic.ID)
	writeItem(t, pool, switching.ID, switching.Version, db.UpdateWorkItemFields{
		WorkflowID: strPtr(wfID),
	})
	resp5, err := update(switching, func(m *apiv1.UpdateWorkItemRequest) {
		m.Kind = kind(apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE)
		m.ScheduledStartAt = tspb
	})
	if err != nil {
		t.Fatalf("switch kind + schedule: %v", err)
	}
	if resp5.Msg.WorkItem.Status == apiv1.WorkItemStatus_WORK_ITEM_STATUS_SCHEDULED {
		t.Fatalf("kind switch that clears the schedule must not leave status scheduled")
	}
	cur5 := readItem(t, pool, switching.ID)
	if cur5.ScheduledStartAt != nil {
		t.Fatalf("schedule should be cleared on non-schedulable kind switch, got %v", cur5.ScheduledStartAt)
	}

	// 6. Clear path: an update without scheduledStartAt leaves status alone.
	clearItem := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindTask, &epic.ID)
	writeItem(t, pool, clearItem.ID, clearItem.Version, db.UpdateWorkItemFields{
		Status: strPtr(domain.WorkItemReady),
	})
	resp6, err := update(clearItem, func(m *apiv1.UpdateWorkItemRequest) {
		m.Title = strPtr("Renamed without schedule")
	})
	if err != nil {
		t.Fatalf("update without schedule: %v", err)
	}
	if resp6.Msg.WorkItem.Status != apiv1.WorkItemStatus_WORK_ITEM_STATUS_READY {
		t.Fatalf("status without schedule change = %v, want ready untouched", resp6.Msg.WorkItem.Status)
	}
}

// TestUpdateWorkItemDisableRecurringDemotesToPendingDB verifies the
// recurring-status invariant (AC: disabling recurring on a work item and
// saving cancels upcoming schedules AND returns the status to pending).
// The edit form unchecks the Recurring schedule toggle — an empty-but-present
// RecurringSchedule, proto clear semantics — while its status dropdown still
// reports "recurring", so the clear and the explicit recurring status arrive
// in the SAME request and the clear wins. Also covered: the inverse hole
// (a manual status=recurring pick with no schedule → pending), the keep path
// (setting a new schedule stays recurring), the echo path (a recurring item
// saved with status=recurring + schedule intact stays recurring), and the
// explicit-other-status path (clear schedule + status=scheduled → scheduled).
func TestUpdateWorkItemDisableRecurringDemotesToPendingDB(t *testing.T) {
	pool := validateParentTestPool(t)
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)
	s := New(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	projectA := validateParentProject(t, ctx, pool)
	epic := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindEpic, nil)

	update := func(item db.WorkItemRow, mutate func(req *apiv1.UpdateWorkItemRequest)) error {
		req := connect.NewRequest(&apiv1.UpdateWorkItemRequest{Id: item.ID})
		mutate(req.Msg)
		_, err := s.UpdateWorkItem(ctx, req)
		return err
	}
	// seedRecurring creates a task with a recurring schedule + a computed
	// next_run_at and status recurring (mirrors the Create path's flip).
	seedRecurring := func() db.WorkItemRow {
		t.Helper()
		item := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindTask, &epic.ID)
		scheduleJSON, err := ValidateRecurringSchedule(&apiv1.RecurringSchedule{
			Frequency: "daily",
			Interval:  1,
			StartDate: "2026-08-12",
			StartTime: "09:00",
		})
		if err != nil {
			t.Fatalf("validate recurring schedule: %v", err)
		}
		future := time.Now().Add(24 * time.Hour)
		writeItem(t, pool, item.ID, item.Version, db.UpdateWorkItemFields{
			Status:            strPtr(domain.WorkItemRecurring),
			RecurringSchedule: &scheduleJSON,
			NextRunAt:         &future,
		})
		return readItem(t, pool, item.ID)
	}

	// 1. The reported bug: disable the toggle while the dropdown still says
	// "recurring" → empty-but-present schedule + status=recurring. The clear
	// must win: status → pending, schedule + cursor NULL.
	item := seedRecurring()
	if err := update(item, func(m *apiv1.UpdateWorkItemRequest) {
		m.Status = statusPtr(apiv1.WorkItemStatus_WORK_ITEM_STATUS_RECURRING)
		m.RecurringSchedule = &apiv1.RecurringSchedule{}
	}); err != nil {
		t.Fatalf("disable recurring: %v", err)
	}
	cur := readItem(t, pool, item.ID)
	if cur.Status != domain.WorkItemPending {
		t.Fatalf("status after disable = %q, want pending", cur.Status)
	}
	if len(cur.RecurringSchedule) != 0 {
		t.Fatalf("recurring_schedule after disable should be NULL, got %q", string(cur.RecurringSchedule))
	}
	if cur.NextRunAt != nil {
		t.Fatalf("next_run_at after disable = %v, want NULL", cur.NextRunAt)
	}

	// 2. Inverse hole: a manual status=recurring pick on an item with NO
	// schedule → demoted to pending ("recurring" is derived, not settable).
	item2 := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindTask, &epic.ID)
	if err := update(item2, func(m *apiv1.UpdateWorkItemRequest) {
		m.Status = statusPtr(apiv1.WorkItemStatus_WORK_ITEM_STATUS_RECURRING)
	}); err != nil {
		t.Fatalf("status=recurring without schedule: %v", err)
	}
	if cur := readItem(t, pool, item2.ID); cur.Status != domain.WorkItemPending {
		t.Fatalf("status=recurring without schedule = %q, want pending", cur.Status)
	}

	// 3. Keep path: setting a NEW schedule keeps the item recurring.
	item3 := seedRecurring()
	if err := update(item3, func(m *apiv1.UpdateWorkItemRequest) {
		m.RecurringSchedule = &apiv1.RecurringSchedule{
			Frequency: "weekly",
			Interval:  1,
			Days:      []string{"Mon", "Fri"},
			StartDate: "2026-08-14",
			StartTime: "10:30",
		}
	}); err != nil {
		t.Fatalf("set new recurring schedule: %v", err)
	}
	cur3 := readItem(t, pool, item3.ID)
	if cur3.Status != domain.WorkItemRecurring {
		t.Fatalf("new schedule status = %q, want recurring", cur3.Status)
	}
	if len(cur3.RecurringSchedule) == 0 {
		t.Fatal("recurring_schedule should be persisted for the new schedule")
	}

	// 4. Echo path: the form always sends the dropdown status. A recurring
	// item saved with status=recurring and its schedule intact stays
	// recurring — the disable flow only demotes when the clear is present.
	item4 := seedRecurring()
	if err := update(item4, func(m *apiv1.UpdateWorkItemRequest) {
		m.Status = statusPtr(apiv1.WorkItemStatus_WORK_ITEM_STATUS_RECURRING)
		m.Title = strPtr("Renamed, still recurring")
	}); err != nil {
		t.Fatalf("echo status=recurring on recurring item: %v", err)
	}
	if cur := readItem(t, pool, item4.ID); cur.Status != domain.WorkItemRecurring {
		t.Fatalf("echoed recurring save = %q, want recurring (schedule intact)", cur.Status)
	}

	// 5. Explicit other status: clear the schedule AND pick "scheduled" →
	// the explicit status is honored, stays scheduled (no zombie).
	item5 := seedRecurring()
	if err := update(item5, func(m *apiv1.UpdateWorkItemRequest) {
		m.Status = statusPtr(apiv1.WorkItemStatus_WORK_ITEM_STATUS_SCHEDULED)
		m.RecurringSchedule = &apiv1.RecurringSchedule{}
	}); err != nil {
		t.Fatalf("clear schedule + status=scheduled: %v", err)
	}
	cur5 := readItem(t, pool, item5.ID)
	if cur5.Status != domain.WorkItemScheduled {
		t.Fatalf("clear + scheduled status = %q, want scheduled", cur5.Status)
	}
	if len(cur5.RecurringSchedule) != 0 {
		t.Fatalf("recurring_schedule should be NULL, got %q", string(cur5.RecurringSchedule))
	}
}

// TestBindingWorkflowClearsOneShotWorkerRef verifies the one-shot stale-data
// self-heal + the post-update validation:
//   - a leaf carrying a stale assigned_worker_ref (leftover from the one-shot
//     standalone path) that the user binds to a workflow in the SAME request
//     must have that worker ref cleared (it would otherwise flag the item as
//     a worker-assigned one-shot in sequence validation and make its parent
//     unschedulable), and
//   - scheduling with the workflow selected in this request must NOT be
//     rejected as "no workflow is set" (the validation uses the post-update
//     binding, not the stale pre-update row).
func TestBindingWorkflowClearsOneShotWorkerRef(t *testing.T) {
	pool := validateParentTestPool(t)
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)
	projectA := validateParentProject(t, ctx, pool)
	s := New(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	wfID := seedPublishedWorkflowForTest(t, pool, projectA, true)

	// A leaf with a stale one-shot worker assignment (the standalone path).
	leaf := createSequenceItem(t, pool, projectA, domain.WorkItemKindTask, "One-shot Leaf",
		nil, nil, []byte(`{"worker_id":"w_se_devops_engineer","version":0}`))
	got := readItem(t, pool, leaf.ID)
	if len(got.AssignedWorkerRef) == 0 {
		t.Fatalf("fixture should start one-shot (assigned_worker_ref set)")
	}

	// Bind a workflow AND start immediately in the same request.
	req := connect.NewRequest(&apiv1.UpdateWorkItemRequest{Id: leaf.ID})
	auto := true
	req.Msg.WorkflowId = strPtr(wfID)
	req.Msg.AutoStartWorkflow = &auto
	resp, err := s.UpdateWorkItem(ctx, req)
	if err != nil {
		t.Fatalf("update with workflow + auto-start should succeed, got %v", err)
	}
	if resp.Msg.WorkItem.WorkflowId != wfID {
		t.Fatalf("workflow_id = %q, want %q", resp.Msg.WorkItem.WorkflowId, wfID)
	}
	if resp.Msg.WorkItem.AssignedWorkerRef != "" {
		t.Fatalf("assigned_worker_ref should be cleared on workflow bind, got %q", resp.Msg.WorkItem.AssignedWorkerRef)
	}
	cur := readItem(t, pool, leaf.ID)
	if len(cur.AssignedWorkerRef) != 0 {
		t.Fatalf("db assigned_worker_ref should be cleared on workflow bind, got %q", cur.AssignedWorkerRef)
	}
}
