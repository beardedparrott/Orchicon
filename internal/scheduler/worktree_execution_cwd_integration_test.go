package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// These tests verify the execution-cwd acceptance criteria against a real
// Postgres and a real temp git repo (skipped unless ORCHICON_TEST_DSN is set
// — see approval_no_clone_test.go for the DSN contract):
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/scheduler/ -run 'TestWorktreeExecutionCwd' -v
//
// They guard: a run with a provisioned worktree dispatches with
// manifest.WorktreePath set (and ProjectDir unchanged as the mount/guard
// root), while a run without a provisioned worktree keeps an empty
// WorktreePath (execution cwd = project_dir).

// manifestCaptureBridge is a test double for AdapterBridge that records the
// manifests passed to Start. It is safe for concurrent use: parallel dispatch
// fans out startExecution across goroutines, so Start may be invoked from
// several of them at once.
type manifestCaptureBridge struct {
	mu        sync.Mutex
	manifests []ExecutionManifest
}

func (b *manifestCaptureBridge) Start(_ context.Context, _ db.ExecutionRow, manifest ExecutionManifest, _ ExecutionCallbacks) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.manifests = append(b.manifests, manifest)
	return nil
}

// testDispatcher builds a Dispatcher with the bridge registered under the
// opencode default kind, so the reconciler's model_ref → kind resolution
// (ParseModelRef) routes to it. Tests that need an unknown-kind path build
// their own dispatcher.
func testDispatcher(bridge AdapterBridge) *Dispatcher {
	d := NewDispatcher()
	d.Register(adapter.DefaultAdapterKind, bridge)
	return d
}

// newCwdTestTask builds the minimal task + worker version + execution rows
// startExecution needs, bound to the given workflow run and project.
func newCwdTestTask(projectID, runID, itemID string) (db.WorkItemRow, db.WorkerVersionRow, db.ExecutionRow) {
	task := db.WorkItemRow{
		ID: itemID, TenantID: approvalTestTenant,
		Title: "Refactor Export Pipeline", WorkflowRunID: runID,
	}
	version := db.WorkerVersionRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkerID: "w-cwd-test", Version: 1, Status: "published",
		ModelRef: "test-model",
	}
	exec := db.ExecutionRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		ProjectID: projectID, TaskID: itemID,
		Status: domain.ExecutionDispatching, WorkflowRunID: runID,
	}
	return task, version, exec
}

// TestWorktreeExecutionCwdReady verifies a provisioned worktree run
// dispatches with manifest.WorktreePath = the worktree path and
// manifest.ProjectDir unchanged (the mount/guard root).
func TestWorktreeExecutionCwdReady(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile worktree: %v", res.Error)
	}
	run := env.getRun(t)
	if run.WorktreeStatus != domain.WorktreeReady {
		t.Fatalf("worktree_status = %q, want ready", run.WorktreeStatus)
	}

	task, version, exec := newCwdTestTask(env.proj.ID, run.ID, env.itemID)
	bridge := &manifestCaptureBridge{}
	rec := NewTaskReconciler(env.pool, slog.Default(), testDispatcher(bridge))
	rec.startExecution(ctx, exec, task, version, db.AdapterRow{})

	if len(bridge.manifests) != 1 {
		t.Fatalf("startExecution produced %d manifests, want 1", len(bridge.manifests))
	}
	man := bridge.manifests[0]
	if man.WorktreePath != env.expectedPath() {
		t.Errorf("manifest.WorktreePath = %q, want %q", man.WorktreePath, env.expectedPath())
	}
	if man.ProjectDir != env.repo {
		t.Errorf("manifest.ProjectDir = %q, want %q (project root must stay the mount/guard root)", man.ProjectDir, env.repo)
	}
}

// TestWorktreeExecutionCwdFallback verifies a run without a provisioned
// worktree keeps an empty WorktreePath (execution cwd = project_dir).
func TestWorktreeExecutionCwdFallback(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	// The run stays 'pending' — no worktree provisioned (the reconciler was
	// never run for it).
	task, version, exec := newCwdTestTask(env.proj.ID, env.run.ID, env.itemID)
	bridge := &manifestCaptureBridge{}
	rec := NewTaskReconciler(env.pool, slog.Default(), testDispatcher(bridge))
	rec.startExecution(ctx, exec, task, version, db.AdapterRow{})

	if len(bridge.manifests) != 1 {
		t.Fatalf("startExecution produced %d manifests, want 1", len(bridge.manifests))
	}
	man := bridge.manifests[0]
	if man.WorktreePath != "" {
		t.Errorf("manifest.WorktreePath = %q, want empty (no provisioned worktree)", man.WorktreePath)
	}
	if man.ProjectDir != env.repo {
		t.Errorf("manifest.ProjectDir = %q, want %q", man.ProjectDir, env.repo)
	}
}
