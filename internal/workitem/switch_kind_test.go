package workitem

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/jackc/pgx/v5"
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
	_, err = ResolveKindSwitch(ctx, ttx.Tx, validateParentTestTenant, task, domain.WorkItemKindFeature, strPtr(task.ID), projectA)
	if err == nil || !strings.Contains(err.Error(), "must be deeper than its parent") {
		t.Fatalf("explicit same-depth parent should be rejected, got %v", err)
	}
}

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
