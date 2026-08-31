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

// Update-path auto-start precondition tests (architecture-notes/
// fix-update-path-auto-start-never-re-arm-cancelled-terminal-items-warn-
// on-non-startable-explicit-start.md). The post-commit auto-start in
// UpdateWorkItem must only fire when the item's CURRENT status is
// startable (pending/scheduled/ready/assigned) — an edit must never
// resurrect or duplicate work, no matter what stored auto_start_workflow
// flag says. DB-backed parts are skipped unless ORCHICON_TEST_DSN is set
// (same convention as validate_test.go):
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/workitem/ -run 'TestIsStartableForAutoStart|TestUpdateAutoStart' -v

// autoStartTestEnv sets up the service + a fresh project, mirroring the
// other DB-backed workitem tests.
func autoStartTestEnv(t *testing.T) (*db.Pool, *Service, context.Context, string) {
	t.Helper()
	pool := validateParentTestPool(t)
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)
	s := New(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	proj := validateParentProject(t, ctx, pool)
	return pool, s, ctx, proj
}

// forceAutoStartState pins a row's status + auto_start_workflow directly,
// simulating both terminal statuses and the legacy stale rows (stored
// auto_start_workflow=true created before migration 20260807120000 set
// the create default to false) that the API cannot produce directly.
func forceAutoStartState(t *testing.T, pool *db.Pool, itemID, status string, autoStart bool) {
	t.Helper()
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)
	ttx, err := pool.BeginTenantTx(ctx, validateParentTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	if _, err := ttx.Tx.Exec(ctx,
		`UPDATE work_items SET status = $1, auto_start_workflow = $2 WHERE id = $3`,
		status, autoStart, itemID); err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// installAutoStartSpies injects counting stubs for the workflow/sequence
// starters so tests can assert whether the post-commit path fired.
func installAutoStartSpies(s *Service) (started *int, seqStarted *int) {
	n, m := 0, 0
	s.SetStartWorkflowStarter(func(ctx context.Context, tenantID, workflowID, projectID, workItemID string) error {
		n++
		return nil
	})
	s.SetStartSequenceStarter(func(ctx context.Context, tenantID, parentID string) error {
		m++
		return nil
	})
	return &n, &m
}

// TestIsStartableForAutoStart table-tests every canonical status: only
// pending/scheduled/ready/assigned may be armed by an update.
func TestIsStartableForAutoStart(t *testing.T) {
	cases := map[string]bool{
		domain.WorkItemPending:       true,
		domain.WorkItemScheduled:     true,
		domain.WorkItemReady:         true,
		domain.WorkItemAssigned:      true,
		domain.WorkItemRunning:       false,
		domain.WorkItemCheckpointing: false,
		domain.WorkItemRecovering:    false,
		domain.WorkItemSucceeded:     false,
		domain.WorkItemFailed:        false,
		domain.WorkItemCancelled:     false,
		domain.WorkItemSkipped:       false,
		domain.WorkItemRecurring:     false,
		domain.WorkItemBlocked:       false,
		domain.WorkItemArchived:      false,
	}
	for status, want := range cases {
		if got := IsStartableForAutoStart(status); got != want {
			t.Errorf("IsStartableForAutoStart(%q) = %v, want %v", status, got, want)
		}
	}
}

// TestUpdateAutoStartDeclinedOnCancelledLeafDB — the reported bug: a
// legacy row with stored auto_start_workflow=true whose edit binds a
// workflow_id must NOT be re-armed from cancelled. The binding is saved;
// nothing starts; no warning (the user never asked).
func TestUpdateAutoStartDeclinedOnCancelledLeafDB(t *testing.T) {
	pool, s, ctx, proj := autoStartTestEnv(t)
	wf := seedPublishedWorkflowForTest(t, pool, proj, true)
	item := createSequenceItem(t, pool, proj, domain.WorkItemKindTask, "Cancelled Leaf", nil, nil, nil)
	forceAutoStartState(t, pool, item.ID, domain.WorkItemCancelled, true) // legacy stale flag
	started, seqStarted := installAutoStartSpies(s)

	resp, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id: item.ID, WorkflowId: &wf,
	}))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if *started != 0 || *seqStarted != 0 {
		t.Fatalf("cancelled item re-armed by edit: workflow starts=%d sequence starts=%d, want 0/0", *started, *seqStarted)
	}
	if resp.Msg.Warning != "" {
		t.Fatalf("non-explicit decline must stay silent, got warning %q", resp.Msg.Warning)
	}
	got := mustGetSequenceItem(t, pool, item.ID)
	if got.Status != domain.WorkItemCancelled {
		t.Fatalf("status = %q, want cancelled", got.Status)
	}
	if got.WorkflowID == nil || *got.WorkflowID != wf {
		t.Fatalf("workflow_id edit was not saved: %v", got.WorkflowID)
	}
}

// TestUpdateAutoStartExplicitNonStartableWarnsDB — an EXPLICIT
// auto_start_workflow=true on a non-startable item saves the edit but
// returns a warning naming the required statuses, and starts nothing.
func TestUpdateAutoStartExplicitNonStartableWarnsDB(t *testing.T) {
	pool, s, ctx, proj := autoStartTestEnv(t)
	wf := seedPublishedWorkflowForTest(t, pool, proj, true)
	item := createSequenceItem(t, pool, proj, domain.WorkItemKindTask, "Failed Leaf", nil, &wf, nil)
	forceAutoStartState(t, pool, item.ID, domain.WorkItemFailed, false)
	started, seqStarted := installAutoStartSpies(s)

	truthy := true
	resp, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id: item.ID, AutoStartWorkflow: &truthy,
	}))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if *started != 0 || *seqStarted != 0 {
		t.Fatalf("failed item started: %d/%d, want 0/0", *started, *seqStarted)
	}
	for _, marker := range []string{"NOT applied", "pending, scheduled, ready, or assigned", "saved"} {
		if !strings.Contains(resp.Msg.Warning, marker) {
			t.Errorf("warning %q missing marker %q", resp.Msg.Warning, marker)
		}
	}
	if !resp.Msg.WorkItem.GetAutoStartWorkflow() {
		t.Fatal("explicit auto_start_workflow=true must still be SAVED (edit persists)")
	}
}

// TestUpdateAutoStartStaleFlagSilentNoFireDB — a stale stored flag on a
// succeeded row with a bound workflow stays inert under an unrelated edit:
// silent no-fire, empty warning.
func TestUpdateAutoStartStaleFlagSilentNoFireDB(t *testing.T) {
	pool, s, ctx, proj := autoStartTestEnv(t)
	wf := seedPublishedWorkflowForTest(t, pool, proj, true)
	item := createSequenceItem(t, pool, proj, domain.WorkItemKindTask, "Stale Flag", nil, &wf, nil)
	forceAutoStartState(t, pool, item.ID, domain.WorkItemSucceeded, true) // legacy stale flag
	started, seqStarted := installAutoStartSpies(s)

	newTitle := "Renamed " + db.NewID()
	resp, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id: item.ID, Title: &newTitle,
	}))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if *started != 0 || *seqStarted != 0 {
		t.Fatalf("stale-flag row fired: %d/%d, want 0/0", *started, *seqStarted)
	}
	if resp.Msg.Warning != "" {
		t.Fatalf("stale-flag decline is silent, got warning %q", resp.Msg.Warning)
	}
	got := mustGetSequenceItem(t, pool, item.ID)
	if got.Title != newTitle || got.Status != domain.WorkItemSucceeded || !got.AutoStartWorkflow {
		t.Fatalf("edit not saved as-is: title=%q status=%q flag=%v", got.Title, got.Status, got.AutoStartWorkflow)
	}
}

// TestUpdateAutoStartDeclinedWhenRequestCancelsDB — the same-request edge:
// an explicit auto_start=true together with a non-startable target status
// (status=cancelled in this very request) must also decline, with warning.
func TestUpdateAutoStartDeclinedWhenRequestCancelsDB(t *testing.T) {
	pool, s, ctx, proj := autoStartTestEnv(t)
	wf := seedPublishedWorkflowForTest(t, pool, proj, true)
	item := createSequenceItem(t, pool, proj, domain.WorkItemKindTask, "Cancel Same Request", nil, &wf, nil)
	started, seqStarted := installAutoStartSpies(s)

	truthy := true
	cancelled := apiv1.WorkItemStatus_WORK_ITEM_STATUS_CANCELLED
	resp, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id: item.ID, Status: &cancelled, AutoStartWorkflow: &truthy,
	}))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if *started != 0 || *seqStarted != 0 {
		t.Fatalf("same-request cancel started: %d/%d, want 0/0", *started, *seqStarted)
	}
	if !strings.Contains(resp.Msg.Warning, "NOT applied") {
		t.Fatalf("warning missing on explicit decline: %q", resp.Msg.Warning)
	}
	got := mustGetSequenceItem(t, pool, item.ID)
	if got.Status != domain.WorkItemCancelled {
		t.Fatalf("status = %q, want cancelled", got.Status)
	}
}

// TestUpdateAutoStartSequenceParentStillStartsDB — preserved behavior: a
// startable parent-with-children explicitly armed still fires its SEQUENCE
// (not a bound-run leaf start).
func TestUpdateAutoStartSequenceParentStillStartsDB(t *testing.T) {
	pool, s, ctx, proj := autoStartTestEnv(t)
	wf := seedPublishedWorkflowForTest(t, pool, proj, true)
	parent := createSequenceItem(t, pool, proj, domain.WorkItemKindEpic, "Seq Parent", nil, nil, nil)
	_ = createSequenceItem(t, pool, proj, domain.WorkItemKindTask, "Child", &parent.ID, &wf, nil)
	started, seqStarted := installAutoStartSpies(s)

	truthy := true
	resp, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id: parent.ID, AutoStartWorkflow: &truthy,
	}))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if *seqStarted != 1 || *started != 0 {
		t.Fatalf("sequence starts=%d workflow starts=%d, want 1/0", *seqStarted, *started)
	}
	if resp.Msg.Warning != "" {
		t.Fatalf("startable parent must not warn: %q", resp.Msg.Warning)
	}
}

// TestUpdateAutoStartGoodPathPendingLeafDB — regression guard: a pending
// leaf explicitly armed still starts its bound workflow exactly as before.
func TestUpdateAutoStartGoodPathPendingLeafDB(t *testing.T) {
	pool, s, ctx, proj := autoStartTestEnv(t)
	wf := seedPublishedWorkflowForTest(t, pool, proj, true)
	item := createSequenceItem(t, pool, proj, domain.WorkItemKindTask, "Good Path", nil, &wf, nil)
	started, seqStarted := installAutoStartSpies(s)

	truthy := true
	resp, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id: item.ID, AutoStartWorkflow: &truthy,
	}))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if *started != 1 || *seqStarted != 0 {
		t.Fatalf("workflow starts=%d sequence starts=%d, want 1/0", *started, *seqStarted)
	}
	if resp.Msg.Warning != "" {
		t.Fatalf("good path must not warn: %q", resp.Msg.Warning)
	}
}
