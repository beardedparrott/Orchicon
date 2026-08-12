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
	assets "github.com/beardedparrott/orchicon"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/jackc/pgx/v5"
)

// TestValidateTopLevelKind verifies the hierarchy invariant that only
// epics are top-level (shared by Create and Update/clear).
func TestValidateTopLevelKind(t *testing.T) {
	if err := validateTopLevelKind(domain.WorkItemKindEpic); err != nil {
		t.Errorf("epic should be allowed top-level, got %v", err)
	}
	for _, kind := range []string{
		domain.WorkItemKindFeature,
		domain.WorkItemKindTask,
		domain.WorkItemKindSubtask,
	} {
		if err := validateTopLevelKind(kind); err == nil {
			t.Errorf("%s should be rejected as top-level", kind)
		} else if msg := err.Error(); !strings.Contains(msg, "only epics can be top-level") {
			t.Errorf("top-level error for %s = %q, want 'only epics can be top-level'", kind, msg)
		}
	}
}

// TestValidateKindDepth verifies the depth rule that a child must be
// strictly deeper than its parent (Epic > Feature > Task > Subtask).
func TestValidateKindDepth(t *testing.T) {
	valid := []struct{ child, parent string }{
		{domain.WorkItemKindFeature, domain.WorkItemKindEpic},
		{domain.WorkItemKindTask, domain.WorkItemKindFeature},
		{domain.WorkItemKindTask, domain.WorkItemKindEpic},
		{domain.WorkItemKindSubtask, domain.WorkItemKindTask},
		{domain.WorkItemKindSubtask, domain.WorkItemKindFeature},
		{domain.WorkItemKindSubtask, domain.WorkItemKindEpic},
	}
	for _, tc := range valid {
		if err := validateKindDepth(tc.child, tc.parent); err != nil {
			t.Errorf("validateKindDepth(%s, %s) unexpected error: %v", tc.child, tc.parent, err)
		}
	}

	invalid := []struct{ child, parent string }{
		{domain.WorkItemKindEpic, domain.WorkItemKindEpic},       // same depth
		{domain.WorkItemKindEpic, domain.WorkItemKindFeature},    // parent deeper than child
		{domain.WorkItemKindFeature, domain.WorkItemKindFeature}, // same depth
		{domain.WorkItemKindFeature, domain.WorkItemKindTask},    // parent deeper than child
		{domain.WorkItemKindTask, domain.WorkItemKindTask},       // same depth
		{domain.WorkItemKindTask, domain.WorkItemKindSubtask},    // parent deeper than child
		{domain.WorkItemKindSubtask, domain.WorkItemKindSubtask}, // same depth
	}
	for _, tc := range invalid {
		if err := validateKindDepth(tc.child, tc.parent); err == nil {
			t.Errorf("validateKindDepth(%s, %s) should have been rejected", tc.child, tc.parent)
		}
	}
}

// TestValidateKindDepthSelfParent verifies the cycle-safety property: a
// node can never be its own parent or its own descendant's child because
// the parent chain is strictly ordered by depth.
func TestValidateKindDepthSelfParent(t *testing.T) {
	for _, kind := range []string{
		domain.WorkItemKindEpic,
		domain.WorkItemKindFeature,
		domain.WorkItemKindTask,
		domain.WorkItemKindSubtask,
	} {
		if err := validateKindDepth(kind, kind); err == nil {
			t.Errorf("a %s must not be its own parent", kind)
		}
	}
}

// TestValidateAcceptanceReview verifies the acceptance review validator
// trims whitespace, accepts empty strings (a work item without a
// completed run has no review), and bounds the length like the other
// markdown fields (maxDescLen).
func TestValidateAcceptanceReview(t *testing.T) {
	if got, err := ValidateAcceptanceReview(""); err != nil {
		t.Fatalf("empty should be allowed, got %v", err)
	} else if got != "" {
		t.Fatalf("empty should stay empty, got %q", got)
	}
	got, err := ValidateAcceptanceReview("  ## Done\n  finished the thing  ")
	if err != nil {
		t.Fatalf("valid markdown rejected: %v", err)
	}
	if got != "## Done\n  finished the thing" {
		t.Fatalf("trim result = %q, want leading/trailing space trimmed", got)
	}
	over := strings.Repeat("x", maxDescLen+1)
	if _, err := ValidateAcceptanceReview(over); err == nil {
		t.Fatal("over-long review should be rejected")
	}
	if _, err := ValidateAcceptanceReview(strings.Repeat("x", maxDescLen)); err != nil {
		t.Fatalf("max-length review should be accepted, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// DB-backed integration test for ValidateParent (skipped unless
// ORCHICON_TEST_DSN points at a disposable database, same convention as
// internal/askorchicon/tool_workitems_test.go):
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/workitem/ -run TestValidateParent -v
// ---------------------------------------------------------------------------

const validateParentTestTenant = "tnt_validate_parent"

func validateParentTestPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed workitem tests")
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
	return pool
}

func validateParentProject(t *testing.T, ctx context.Context, pool *db.Pool) string {
	t.Helper()
	ttx, err := pool.BeginTenantTx(ctx, validateParentTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: validateParentTestTenant,
		Name: "Parent Test", Slug: "parent-test-" + strings.ToLower(db.NewID()),
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

func validateParentItem(t *testing.T, ctx context.Context, pool *db.Pool, projectID, kind string, parentID *string) db.WorkItemRow {
	t.Helper()
	ttx, err := pool.BeginTenantTx(ctx, validateParentTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	item, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: validateParentTestTenant,
		ProjectID: projectID, ParentID: parentID, Kind: kind,
		Title:  "Parent test " + strings.ToLower(db.NewID()),
		Status: domain.WorkItemPending,
	})
	if err != nil {
		t.Fatalf("create %s: %v", kind, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit item: %v", err)
	}
	return item
}

// TestValidateParentDB exercises the shared helper against a real DB:
// valid reparenting, depth violations, cross-project parents, clearing
// rules, and the cross-tenant NotFound guarantee.
func TestValidateParentDB(t *testing.T) {
	pool := validateParentTestPool(t)
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)

	projectA := validateParentProject(t, ctx, pool)
	projectB := validateParentProject(t, ctx, pool)

	epic := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindEpic, nil)
	feature := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindFeature, &epic.ID)
	task := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindTask, &feature.ID)
	subtask := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindSubtask, &task.ID)
	otherProjectEpic := validateParentItem(t, ctx, pool, projectB, domain.WorkItemKindEpic, nil)

	ttx, err := pool.BeginTenantTx(ctx, validateParentTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	// Valid: same project + strictly shallower kinds (skip-level ok).
	for _, tc := range []struct {
		child  db.WorkItemRow
		parent db.WorkItemRow
	}{
		{task, feature},
		{task, epic},
		{subtask, epic},
		{subtask, feature},
	} {
		if err := ValidateParent(ctx, ttx.Tx, validateParentTestTenant, tc.parent.ID, tc.child.Kind, projectA); err != nil {
			t.Errorf("ValidateParent(%s under %s) unexpected error: %v", tc.child.ID, tc.parent.ID, err)
		}
	}

	// Invalid: same-depth or deeper parent.
	if err := ValidateParent(ctx, ttx.Tx, validateParentTestTenant, task.ID, domain.WorkItemKindFeature, projectA); err == nil {
		t.Error("feature under task (deeper parent) should be rejected")
	}
	if err := ValidateParent(ctx, ttx.Tx, validateParentTestTenant, feature.ID, domain.WorkItemKindFeature, projectA); err == nil {
		t.Error("feature under feature (same depth) should be rejected")
	}
	// Self-parent is rejected by the depth check.
	if err := ValidateParent(ctx, ttx.Tx, validateParentTestTenant, task.ID, domain.WorkItemKindTask, projectA); err == nil {
		t.Error("task as its own parent should be rejected")
	}

	// Invalid: cross-project parent.
	if err := ValidateParent(ctx, ttx.Tx, validateParentTestTenant, otherProjectEpic.ID, domain.WorkItemKindFeature, projectA); err == nil {
		t.Error("cross-project parent should be rejected")
	} else if msg := err.Error(); !strings.Contains(msg, "same project") {
		t.Errorf("cross-project error = %q, want 'same project'", msg)
	}

	// Clearing: only epics may go top-level.
	if err := ValidateParent(ctx, ttx.Tx, validateParentTestTenant, "", domain.WorkItemKindTask, projectA); err == nil {
		t.Error("clearing a task's parent should be rejected")
	}
	if err := ValidateParent(ctx, ttx.Tx, validateParentTestTenant, "", domain.WorkItemKindEpic, projectA); err != nil {
		t.Errorf("clearing an epic's (absent) parent should be a no-op, got %v", err)
	}

	// Unknown parent id → ErrNotFound (cross-tenant parents are the same
	// shape: the row never resolves inside the tenant tx).
	if err := ValidateParent(ctx, ttx.Tx, validateParentTestTenant, "does-not-exist", domain.WorkItemKindFeature, projectA); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("unknown parent should be ErrNotFound, got %v", err)
	}
}

// TestUpdateWorkItemReparent exercises the full UpdateWorkItem handler:
// explicit reparenting (same-project, depth rules), clearing rules, and
// the carried-parent guard that rejects moving a child to another project
// when its parent does not already live there.
func TestUpdateWorkItemReparent(t *testing.T) {
	pool := validateParentTestPool(t)
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)
	s := New(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	projectA := validateParentProject(t, ctx, pool)
	projectB := validateParentProject(t, ctx, pool)

	epicA1 := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindEpic, nil)
	epicA2 := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindEpic, nil)
	feature := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindFeature, &epicA1.ID)
	task := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindTask, &feature.ID)
	epicB := validateParentItem(t, ctx, pool, projectB, domain.WorkItemKindEpic, nil)

	update := func(item db.WorkItemRow, mutate func(req *apiv1.UpdateWorkItemRequest)) error {
		req := connect.NewRequest(&apiv1.UpdateWorkItemRequest{Id: item.ID})
		mutate(req.Msg)
		_, err := s.UpdateWorkItem(ctx, req)
		return err
	}

	// Explicit reparent within the same project: feature moves from
	// epicA1 to epicA2.
	err := update(feature, func(m *apiv1.UpdateWorkItemRequest) {
		m.ParentId = strPtr(epicA2.ID)
	})
	if err != nil {
		t.Fatalf("reparent feature to epicA2: %v", err)
	}
	cur, err := db.GetWorkItem(ctx, mustTx(t, pool), validateParentTestTenant, feature.ID)
	if err != nil {
		t.Fatalf("reload feature: %v", err)
	}
	if cur.ParentID == nil || *cur.ParentID != epicA2.ID {
		t.Fatalf("feature parent = %v, want %s", cur.ParentID, epicA2.ID)
	}

	// A deeper parent (feature under its own task) is rejected — this is
	// also the cycle-safety check (feature → task → feature would be a
	// 2-cycle, impossible because the depth rule forbids it).
	err = update(feature, func(m *apiv1.UpdateWorkItemRequest) { m.ParentId = strPtr(task.ID) })
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("feature under task (deeper parent) should be rejected, got %v", err)
	}

	// Clearing a non-epic's parent is rejected.
	err = update(feature, func(m *apiv1.UpdateWorkItemRequest) { m.ParentId = strPtr("") })
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("clearing a feature's parent should be rejected, got %v", err)
	}

	// Moving a child cross-project WITHOUT reparenting is rejected by the
	// carried-parent guard (the parent still lives in project A).
	err = update(task, func(m *apiv1.UpdateWorkItemRequest) { m.ProjectId = strPtr(projectB) })
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("cross-project move without reparent should be rejected, got %v", err)
	}

	// Moving a child cross-project WITH an explicit parent in the target
	// project succeeds.
	err = update(task, func(m *apiv1.UpdateWorkItemRequest) {
		m.ProjectId = strPtr(projectB)
		m.ParentId = strPtr(epicB.ID)
	})
	if err != nil {
		t.Fatalf("cross-project move with reparent: %v", err)
	}
	cur, err = db.GetWorkItem(ctx, mustTx(t, pool), validateParentTestTenant, task.ID)
	if err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if cur.ProjectID != projectB || cur.ParentID == nil || *cur.ParentID != epicB.ID {
		t.Fatalf("task after cross-project move = project %s parent %v, want project %s parent %s",
			cur.ProjectID, cur.ParentID, projectB, epicB.ID)
	}

	// Moving an epic (no parent) cross-project succeeds without a parent.
	err = update(epicA1, func(m *apiv1.UpdateWorkItemRequest) { m.ProjectId = strPtr(projectB) })
	if err != nil {
		t.Fatalf("move epic cross-project: %v", err)
	}

	// Unknown parent id → NotFound.
	err = update(feature, func(m *apiv1.UpdateWorkItemRequest) { m.ParentId = strPtr("does-not-exist") })
	if err == nil || connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("reparent to unknown parent should be NotFound, got %v", err)
	}
}

func mustTx(t *testing.T, pool *db.Pool) pgx.Tx {
	t.Helper()
	ttx, err := pool.BeginTenantTx(context.Background(), validateParentTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	t.Cleanup(func() { _ = ttx.Rollback(context.Background()) })
	return ttx.Tx
}

// --- Recurring schedule tests -----------------------------------------------

func TestValidateRecurringSchedule_Nil(t *testing.T) {
	b, err := ValidateRecurringSchedule(nil)
	if err != nil {
		t.Fatalf("nil schedule: unexpected error: %v", err)
	}
	if b != nil {
		t.Fatalf("nil schedule: expected nil bytes, got %s", b)
	}
}

func TestValidateRecurringSchedule_ValidDaily(t *testing.T) {
	msg := &apiv1.RecurringSchedule{
		Frequency: "daily",
		Interval:  1,
		StartDate: "2026-08-15",
		StartTime: "09:00",
	}
	b, err := ValidateRecurringSchedule(msg)
	if err != nil {
		t.Fatalf("valid daily schedule: unexpected error: %v", err)
	}
	if b == nil {
		t.Fatal("valid daily schedule: expected non-nil bytes")
	}
	if !strings.Contains(string(b), `"daily"`) {
		t.Errorf("expected daily frequency in JSON, got %s", b)
	}
}

func TestValidateRecurringSchedule_ValidWeeklyWithDays(t *testing.T) {
	msg := &apiv1.RecurringSchedule{
		Frequency: "weekly",
		Interval:  2,
		Days:      []string{"Mon", "Wed", "Fri"},
		StartDate: "2026-08-15",
		StartTime: "14:30",
	}
	b, err := ValidateRecurringSchedule(msg)
	if err != nil {
		t.Fatalf("valid weekly schedule: unexpected error: %v", err)
	}
	if b == nil {
		t.Fatal("valid weekly schedule: expected non-nil bytes")
	}
}

func TestValidateRecurringSchedule_InvalidFrequency(t *testing.T) {
	msg := &apiv1.RecurringSchedule{
		Frequency: "yearly",
		Interval:  1,
		StartDate: "2026-08-15",
		StartTime: "09:00",
	}
	_, err := ValidateRecurringSchedule(msg)
	if err == nil {
		t.Fatal("invalid frequency should be rejected")
	}
	if !strings.Contains(err.Error(), "frequency") {
		t.Errorf("error should mention frequency, got: %v", err)
	}
}

func TestValidateRecurringSchedule_ZeroInterval(t *testing.T) {
	msg := &apiv1.RecurringSchedule{
		Frequency: "daily",
		Interval:  0,
		StartDate: "2026-08-15",
		StartTime: "09:00",
	}
	_, err := ValidateRecurringSchedule(msg)
	if err == nil {
		t.Fatal("interval < 1 should be rejected")
	}
	if !strings.Contains(err.Error(), "interval") {
		t.Errorf("error should mention interval, got: %v", err)
	}
}

func TestValidateRecurringSchedule_NegativeInterval(t *testing.T) {
	msg := &apiv1.RecurringSchedule{
		Frequency: "hourly",
		Interval:  -5,
		StartDate: "2026-08-15",
		StartTime: "09:00",
	}
	_, err := ValidateRecurringSchedule(msg)
	if err == nil {
		t.Fatal("negative interval should be rejected")
	}
}

func TestValidateRecurringSchedule_InvalidDay(t *testing.T) {
	msg := &apiv1.RecurringSchedule{
		Frequency: "weekly",
		Interval:  1,
		Days:      []string{"Mon", "Funday"},
		StartDate: "2026-08-15",
		StartTime: "09:00",
	}
	_, err := ValidateRecurringSchedule(msg)
	if err == nil {
		t.Fatal("invalid day should be rejected")
	}
	if !strings.Contains(err.Error(), "Funday") {
		t.Errorf("error should mention the invalid day, got: %v", err)
	}
}

func TestValidateRecurringSchedule_EmptyStartDate(t *testing.T) {
	msg := &apiv1.RecurringSchedule{
		Frequency: "daily",
		Interval:  1,
		StartTime: "09:00",
	}
	_, err := ValidateRecurringSchedule(msg)
	if err == nil {
		t.Fatal("empty start_date should be rejected")
	}
}

func TestValidateRecurringSchedule_BadStartDate(t *testing.T) {
	msg := &apiv1.RecurringSchedule{
		Frequency: "daily",
		Interval:  1,
		StartDate: "2026/08/15",
		StartTime: "09:00",
	}
	_, err := ValidateRecurringSchedule(msg)
	if err == nil {
		t.Fatal("bad start_date format should be rejected")
	}
	if !strings.Contains(err.Error(), "YYYY-MM-DD") {
		t.Errorf("error should mention YYYY-MM-DD, got: %v", err)
	}
}

func TestValidateRecurringSchedule_EmptyStartTime(t *testing.T) {
	msg := &apiv1.RecurringSchedule{
		Frequency: "daily",
		Interval:  1,
		StartDate: "2026-08-15",
	}
	_, err := ValidateRecurringSchedule(msg)
	if err == nil {
		t.Fatal("empty start_time should be rejected")
	}
}

func TestValidateRecurringSchedule_BadStartTime(t *testing.T) {
	msg := &apiv1.RecurringSchedule{
		Frequency: "daily",
		Interval:  1,
		StartDate: "2026-08-15",
		StartTime: "9am",
	}
	_, err := ValidateRecurringSchedule(msg)
	if err == nil {
		t.Fatal("bad start_time format should be rejected")
	}
	if !strings.Contains(err.Error(), "HH:MM") {
		t.Errorf("error should mention HH:MM, got: %v", err)
	}
}

func TestComputeNextRunAt_Nil(t *testing.T) {
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	got := ComputeNextRunAt(nil, now)
	if got != nil {
		t.Fatalf("nil schedule: expected nil, got %v", got)
	}
}

func TestComputeNextRunAt_DailyInFuture(t *testing.T) {
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	msg := &apiv1.RecurringSchedule{
		Frequency: "daily",
		Interval:  1,
		StartDate: "2026-08-15",
		StartTime: "09:00",
	}
	got := ComputeNextRunAt(msg, now)
	if got == nil {
		t.Fatal("expected non-nil next run")
	}
	expected := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	if !got.Equal(expected) {
		t.Errorf("expected %v, got %v", expected, got)
	}
}

func TestComputeNextRunAt_DailyInPast(t *testing.T) {
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	msg := &apiv1.RecurringSchedule{
		Frequency: "daily",
		Interval:  1,
		StartDate: "2026-08-10",
		StartTime: "09:00",
	}
	got := ComputeNextRunAt(msg, now)
	if got == nil {
		t.Fatal("expected non-nil next run")
	}
	// Start date is 2 days ago at 09:00, so next occurrence >= now is
	// 2026-08-12 09:00 (yesterday) — no, that's before now (10:00).
	// Next is 2026-08-13 09:00.
	expected := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	if !got.Equal(expected) {
		t.Errorf("expected %v, got %v", expected, got)
	}
}

func TestComputeNextRunAt_WeeklyWithDays(t *testing.T) {
	// 2026-08-12 is a Wednesday. Start is Monday 09:00. Every week.
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	msg := &apiv1.RecurringSchedule{
		Frequency: "weekly",
		Interval:  1,
		Days:      []string{"Mon", "Wed"},
		StartDate: "2026-08-10",
		StartTime: "09:00",
	}
	got := ComputeNextRunAt(msg, now)
	if got == nil {
		t.Fatal("expected non-nil next run")
	}
	// Today is Wed 10:00, start is Mon 09:00. Wed 09:00 is before now.
	// Next Mon is 2026-08-17 09:00.
	expected := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	if !got.Equal(expected) {
		t.Errorf("expected %v, got %v", expected, got)
	}
}

func TestComputeNextRunAt_Hourly(t *testing.T) {
	now := time.Date(2026, 8, 12, 14, 30, 0, 0, time.UTC)
	msg := &apiv1.RecurringSchedule{
		Frequency: "hourly",
		Interval:  2,
		StartDate: "2026-08-12",
		StartTime: "08:00",
	}
	got := ComputeNextRunAt(msg, now)
	if got == nil {
		t.Fatal("expected non-nil next run")
	}
	// Anchor 08:00. Steps: 08:00, 10:00, 12:00, 14:00, 16:00.
	// 14:00 is before now (14:30). Next is 16:00.
	expected := time.Date(2026, 8, 12, 16, 0, 0, 0, time.UTC)
	if !got.Equal(expected) {
		t.Errorf("expected %v, got %v", expected, got)
	}
}

func TestComputeNextRunAt_Monthly(t *testing.T) {
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	msg := &apiv1.RecurringSchedule{
		Frequency: "monthly",
		Interval:  1,
		StartDate: "2026-08-01",
		StartTime: "09:00",
	}
	got := ComputeNextRunAt(msg, now)
	if got == nil {
		t.Fatal("expected non-nil next run")
	}
	// Anchor 2026-08-01 09:00. That's before now. Next is 2026-09-01 09:00.
	expected := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	if !got.Equal(expected) {
		t.Errorf("expected %v, got %v", expected, got)
	}
}

func TestComputeNextRunAt_MinuteInterval(t *testing.T) {
	now := time.Date(2026, 8, 12, 14, 30, 15, 0, time.UTC)
	msg := &apiv1.RecurringSchedule{
		Frequency: "minute",
		Interval:  5,
		StartDate: "2026-08-12",
		StartTime: "14:00",
	}
	got := ComputeNextRunAt(msg, now)
	if got == nil {
		t.Fatal("expected non-nil next run")
	}
	// Steps: 14:00, 14:05, 14:10, ..., 14:30, 14:35.
	// 14:30 is before now (14:30:15). Next is 14:35.
	expected := time.Date(2026, 8, 12, 14, 35, 0, 0, time.UTC)
	if !got.Equal(expected) {
		t.Errorf("expected %v, got %v", expected, got)
	}
}

func TestDayMatches(t *testing.T) {
	// 2026-08-12 is a Wednesday.
	wed := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	if !dayMatches(wed, []string{"Mon", "Wed", "Fri"}) {
		t.Error("Wed should match [Mon, Wed, Fri]")
	}
	if dayMatches(wed, []string{"Mon", "Tue"}) {
		t.Error("Wed should not match [Mon, Tue]")
	}
	if dayMatches(wed, []string{}) {
		t.Error("Wed should not match empty slice")
	}
}

// --- Blocker 2 regression tests: weekly-with-days must find same-week matches ---

func TestComputeNextRunAt_WeeklyWithDays_SameWeek(t *testing.T) {
	// 2026-08-10 is a Monday. days=[Mon, Wed], now=Wed 08:00.
	// Wed 09:00 is in the future and in the same cadence week → should return it.
	now := time.Date(2026, 8, 12, 8, 0, 0, 0, time.UTC) // Wed 08:00
	msg := &apiv1.RecurringSchedule{
		Frequency: "weekly",
		Interval:  1,
		Days:      []string{"Mon", "Wed"},
		StartDate: "2026-08-10",
		StartTime: "09:00",
	}
	got := ComputeNextRunAt(msg, now)
	if got == nil {
		t.Fatal("expected non-nil next run")
	}
	// Wed 08-12 09:00 is the same-week Wednesday, in the future.
	expected := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	if !got.Equal(expected) {
		t.Errorf("expected %v, got %v", expected, got)
	}
}

func TestComputeNextRunAt_WeeklyWithDays_AnchorNotInDays(t *testing.T) {
	// Anchor is Monday, days=[Wed] only. now=Wed 08:00.
	// Should return Wed 08-12 09:00, not the anchor Mon 08-10.
	now := time.Date(2026, 8, 12, 8, 0, 0, 0, time.UTC) // Wed 08:00
	msg := &apiv1.RecurringSchedule{
		Frequency: "weekly",
		Interval:  1,
		Days:      []string{"Wed"},
		StartDate: "2026-08-10",
		StartTime: "09:00",
	}
	got := ComputeNextRunAt(msg, now)
	if got == nil {
		t.Fatal("expected non-nil next run")
	}
	expected := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC) // Wed 09:00
	if !got.Equal(expected) {
		t.Errorf("expected %v, got %v", expected, got)
	}
}

func TestComputeNextRunAt_WeeklyWithDays_Biweekly(t *testing.T) {
	// Anchor Mon 08-10, interval=2 (every 2 weeks), days=[Mon, Wed].
	// now=Thu 08-13 08:00. The current cadence week is 08-10 to 08-16.
	// Wed 08-12 09:00 is past. Next cadence week starts 08-24.
	// Mon 08-24 09:00 should be returned.
	now := time.Date(2026, 8, 13, 8, 0, 0, 0, time.UTC) // Thu 08:00
	msg := &apiv1.RecurringSchedule{
		Frequency: "weekly",
		Interval:  2,
		Days:      []string{"Mon", "Wed"},
		StartDate: "2026-08-10",
		StartTime: "09:00",
	}
	got := ComputeNextRunAt(msg, now)
	if got == nil {
		t.Fatal("expected non-nil next run")
	}
	expected := time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC) // Mon 08-24
	if !got.Equal(expected) {
		t.Errorf("expected %v, got %v", expected, got)
	}
}

func TestComputeNextRunAt_WeeklyWithDays_WrongAnchorDay(t *testing.T) {
	// Anchor Mon 08-10, days=[Wed] only, interval=2.
	// now=Tue 08-11 08:00. Wed 08-12 is in the future AND in the same
	// cadence week as the anchor (offset 2 from Mon, mod 14 = 2, valid).
	now := time.Date(2026, 8, 11, 8, 0, 0, 0, time.UTC) // Tue 08:00
	msg := &apiv1.RecurringSchedule{
		Frequency: "weekly",
		Interval:  2,
		Days:      []string{"Wed"},
		StartDate: "2026-08-10",
		StartTime: "09:00",
	}
	got := ComputeNextRunAt(msg, now)
	if got == nil {
		t.Fatal("expected non-nil next run")
	}
	expected := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC) // Wed 08-12
	if !got.Equal(expected) {
		t.Errorf("expected %v, got %v", expected, got)
	}
}

// --- Blocker 1 regression test: IsRecurringScheduleEmpty ---

func TestIsRecurringScheduleEmpty(t *testing.T) {
	if !IsRecurringScheduleEmpty(&apiv1.RecurringSchedule{}) {
		t.Error("empty message should be detected as empty")
	}
	if !IsRecurringScheduleEmpty(&apiv1.RecurringSchedule{Frequency: "", Interval: 0}) {
		t.Error("all-zero message should be detected as empty")
	}
	if IsRecurringScheduleEmpty(nil) {
		t.Error("nil should not be empty")
	}
	if IsRecurringScheduleEmpty(&apiv1.RecurringSchedule{Frequency: "daily"}) {
		t.Error("message with frequency should not be empty")
	}
	if IsRecurringScheduleEmpty(&apiv1.RecurringSchedule{Interval: 1}) {
		t.Error("message with interval should not be empty")
	}
	if IsRecurringScheduleEmpty(&apiv1.RecurringSchedule{Days: []string{"Mon"}}) {
		t.Error("message with days should not be empty")
	}
	if IsRecurringScheduleEmpty(&apiv1.RecurringSchedule{StartDate: "2026-01-01"}) {
		t.Error("message with start_date should not be empty")
	}
	if IsRecurringScheduleEmpty(&apiv1.RecurringSchedule{StartTime: "09:00"}) {
		t.Error("message with start_time should not be empty")
	}
}
