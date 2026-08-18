package scheduler

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// These tests exercise the WorktreeReconciler against a real Postgres and
// a real temp git repo. They are skipped unless ORCHICON_TEST_DSN points
// at a disposable database (see approval_no_clone_test.go for the DSN
// contract):
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/scheduler/ -run 'TestWorktree' -v
//
// They guard the acceptance criteria: worktree created at arm-time on the
// deterministic branch, collision-safe across concurrent runs of the same
// work item, idempotent (no re-create), result recorded on the run row,
// and non-repo projects skipped.

// gitRun runs git in dir and fails the test on error.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
	return string(out)
}

// newTestRepo creates a temp git repo with a develop branch and one commit.
func newTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-b", "develop")
	gitRun(t, dir, "config", "user.email", "worktree-test@orchicon.dev")
	gitRun(t, dir, "config", "user.name", "Worktree Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test repo\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "initial commit")
	return dir
}

// nonRepoDir creates a temp directory that is NOT inside any git work
// tree. t.TempDir() inherits GOTMPDIR, which for the test process lives
// inside the Orchicon checkout — git rev-parse would walk up and find the
// parent repo. /tmp/orchicon is the scratch mount, safely outside.
func nonRepoDir(t *testing.T) string {
	t.Helper()
	if err := os.MkdirAll("/tmp/orchicon", 0o755); err != nil {
		t.Fatalf("mkdir /tmp/orchicon: %v", err)
	}
	dir, err := os.MkdirTemp("/tmp/orchicon", "nonrepo-")
	if err != nil {
		t.Fatalf("mkdir non-repo dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// worktreeTestEnv is the shared fixture for the worktree integration tests.
type worktreeTestEnv struct {
	t      *testing.T
	pool   *db.Pool
	rec    *WorktreeReconciler
	repo   string
	proj   db.ProjectRow
	run    db.WorkflowRunRow
	itemID string
}

func newWorktreeTestEnv(t *testing.T) *worktreeTestEnv {
	t.Helper()
	pool := approvalTestPool(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	env := &worktreeTestEnv{t: t, pool: pool,
		rec: NewWorktreeReconciler(pool, logger),
	}
	env.repo = newTestRepo(t)

	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		Name: "Worktree Reconciler Project", Slug: "worktree-" + strings.ToLower(db.NewID()),
		Status: "active", Goals: []byte("[]"),
		ProjectDir: env.repo,
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	env.proj = proj

	const itemTitle = "Refactor Export Pipeline"
	item, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		ProjectID: proj.ID, Kind: domain.WorkItemKindTask,
		Title:              itemTitle,
		Description:        "reconcile worktrees",
		AcceptanceCriteria: "worktree created",
		Status:             domain.WorkItemRunning,
	})
	if err != nil {
		t.Fatalf("create work item: %v", err)
	}
	env.itemID = item.ID

	run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowID: "wf-worktree-test", WorkflowVersion: 1,
		ProjectID: proj.ID, Status: domain.WorkflowRunPending,
		RunContext: []byte("{}"), WorkItemID: item.ID,
	})
	if err != nil {
		t.Fatalf("create workflow run: %v", err)
	}
	env.run = run

	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit fixture: %v", err)
	}
	return env
}

func (e *worktreeTestEnv) expectedBranch() string {
	// The slug source is the bound work item's title (the most specific
	// deterministic source the reconciler picks), not the project name.
	return branchNameFor("Refactor Export Pipeline", e.run.ID)
}

func (e *worktreeTestEnv) expectedPath() string {
	return filepath.Join(e.repo, worktreeDirName, e.run.ID)
}

func (e *worktreeTestEnv) getRun(t *testing.T) db.WorkflowRunRow {
	t.Helper()
	ttx, err := e.pool.BeginTenantTx(context.Background(), approvalTestTenant)
	if err != nil {
		t.Fatalf("get run: begin tx: %v", err)
	}
	defer ttx.Rollback(context.Background())
	run, err := db.GetWorkflowRun(context.Background(), ttx.Tx, approvalTestTenant, e.run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	return run
}

func countWorktrees(t *testing.T, repo string) int {
	t.Helper()
	out := gitRun(t, repo, "worktree", "list")
	return strings.Count(out, "\n")
}

// TestWorktreeCreatedAtArmTime is the core acceptance test: reconcile a
// pending run against a git-backed project and verify the worktree + branch
// are created and recorded on the run row.
func TestWorktreeCreatedAtArmTime(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile: %v", res.Error)
	}

	// The worktree directory exists with the README checked out.
	if _, err := os.Stat(env.expectedPath()); err != nil {
		t.Fatalf("worktree not created at %s: %v", env.expectedPath(), err)
	}
	if _, err := os.Stat(filepath.Join(env.expectedPath(), "README.md")); err != nil {
		t.Fatalf("worktree does not contain the repo's files: %v", err)
	}

	// The deterministic branch exists and the worktree is attached to it.
	out := gitRun(t, env.repo, "branch", "--list", env.expectedBranch())
	if out == "" {
		t.Fatalf("branch %q was not created", env.expectedBranch())
	}
	if !strings.Contains(gitRun(t, env.repo, "worktree", "list"), filepath.Join(env.expectedPath())) {
		t.Fatalf("worktree list does not include %s", env.expectedPath())
	}

	// The result is recorded on the run row.
	run := env.getRun(t)
	if run.WorktreeStatus != domain.WorktreeReady {
		t.Fatalf("worktree_status = %q, want ready", run.WorktreeStatus)
	}
	if run.WorktreePath != env.expectedPath() {
		t.Errorf("worktree_path = %q, want %q", run.WorktreePath, env.expectedPath())
	}
	if run.WorktreeBranch != env.expectedBranch() {
		t.Errorf("worktree_branch = %q, want %q", run.WorktreeBranch, env.expectedBranch())
	}
}

// TestWorktreeReconcileIdempotent verifies the converge-don't-recreate
// contract: a second pass for a 'ready' run is a no-op and does not add a
// worktree.
func TestWorktreeReconcileIdempotent(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile pass 1: %v", res.Error)
	}
	before := countWorktrees(t, env.repo)
	if before != 2 { // main tree + the new worktree
		t.Fatalf("expected 2 worktrees after provisioning, got %d", before)
	}

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile pass 2: %v", res.Error)
	}
	if after := countWorktrees(t, env.repo); after != before {
		t.Fatalf("idempotency broken: worktrees before=%d after=%d", before, after)
	}

	// The scan pass also converges without creating anything new.
	if res := env.rec.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("reconcile scan: %v", res.Error)
	}
	if after := countWorktrees(t, env.repo); after != before {
		t.Fatalf("scan re-created a worktree: before=%d after=%d", before, after)
	}
}

// TestWorktreeConcurrentRunsDistinctBranches verifies the collision-safety
// acceptance criterion: two runs of the same work item get distinct
// branches and distinct worktrees.
func TestWorktreeConcurrentRunsDistinctBranches(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	// Second run bound to the SAME work item.
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	run2, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowID: "wf-worktree-test", WorkflowVersion: 1,
		ProjectID: env.proj.ID, Status: domain.WorkflowRunPending,
		RunContext: []byte("{}"), WorkItemID: env.itemID,
	})
	if err != nil {
		t.Fatalf("create second run: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit second run: %v", err)
	}

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile run 1: %v", res.Error)
	}
	if res := env.rec.Reconcile(ctx, run2.ID); res.Error != nil {
		t.Fatalf("reconcile run 2: %v", res.Error)
	}

	branch1 := branchNameFor("Refactor Export Pipeline", env.run.ID)
	branch2 := branchNameFor("Refactor Export Pipeline", run2.ID)
	if branch1 == branch2 {
		t.Fatalf("two runs of the same item collided on branch %q", branch1)
	}
	path2 := filepath.Join(env.repo, worktreeDirName, run2.ID)
	if _, err := os.Stat(path2); err != nil {
		t.Fatalf("second worktree not created: %v", err)
	}
	if count := countWorktrees(t, env.repo); count != 3 { // main + 2 worktrees
		t.Fatalf("expected 3 worktrees, got %d", count)
	}
}

// TestWorktreeNonRepoProjectSkipped verifies non-repo detection: a project
// whose directory is not a git work tree is marked 'skipped' and the run
// proceeds in place.
func TestWorktreeNonRepoProjectSkipped(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	plain := nonRepoDir(t)
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		Name: "Plain Directory Project", Slug: "plain-dir-" + strings.ToLower(db.NewID()),
		Status: "active", Goals: []byte("[]"),
		ProjectDir: plain,
	})
	if err != nil {
		t.Fatalf("create non-repo project: %v", err)
	}
	run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowID: "wf-worktree-test", WorkflowVersion: 1,
		ProjectID: proj.ID, Status: domain.WorkflowRunPending,
		RunContext: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit fixture: %v", err)
	}

	if res := env.rec.Reconcile(ctx, run.ID); res.Error != nil {
		t.Fatalf("reconcile: %v", res.Error)
	}

	ttx, err = env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	got, err := db.GetWorkflowRun(ctx, ttx.Tx, approvalTestTenant, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.WorktreeStatus != domain.WorktreeSkipped {
		t.Fatalf("worktree_status = %q, want skipped", got.WorktreeStatus)
	}
	if _, err := os.Stat(filepath.Join(plain, worktreeDirName, run.ID)); !os.IsNotExist(err) {
		t.Fatalf("a worktree was created for a non-repo project")
	}
}

// TestWorktreeEmptyProjectIDUntouched verifies the one-shot run guard: a
// run without a bound project is never provisioned.
func TestWorktreeEmptyProjectIDUntouched(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowID: "wf-worktree-test", WorkflowVersion: 1,
		Status:     domain.WorkflowRunPending,
		RunContext: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit fixture: %v", err)
	}

	if res := env.rec.Reconcile(ctx, run.ID); res.Error != nil {
		t.Fatalf("reconcile: %v", res.Error)
	}
	ttx, err = env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	row, err := db.GetWorkflowRun(ctx, ttx.Tx, approvalTestTenant, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if row.WorktreeStatus != domain.WorktreePending {
		t.Fatalf("run with empty project_id was touched: worktree_status = %q", row.WorktreeStatus)
	}
}

// TestWorktreeSkippedNotReprovisioned verifies a recorded 'skipped' decision
// is respected: pointing the project at a real git repo later does not make
// the loop revisit the run.
func TestWorktreeSkippedNotReprovisioned(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	plain := nonRepoDir(t)
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		Name: "Later Git Project", Slug: "later-git-" + strings.ToLower(db.NewID()),
		Status: "active", Goals: []byte("[]"),
		ProjectDir: plain,
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowID: "wf-worktree-test", WorkflowVersion: 1,
		ProjectID: proj.ID, Status: domain.WorkflowRunPending,
		RunContext: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit fixture: %v", err)
	}
	if res := env.rec.Reconcile(ctx, run.ID); res.Error != nil {
		t.Fatalf("reconcile: %v", res.Error)
	}

	// Now the project points at a real repo.
	ttx, err = env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	cur, err := db.GetProject(ctx, ttx.Tx, approvalTestTenant, proj.ID)
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	repo := newTestRepo(t)
	if _, err := db.UpdateProject(ctx, ttx.Tx, approvalTestTenant, proj.ID, cur.Version, db.UpdateProjectFields{
		ProjectDir: &repo,
	}); err != nil {
		t.Fatalf("update project dir: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit update: %v", err)
	}

	if res := env.rec.Reconcile(ctx, run.ID); res.Error != nil {
		t.Fatalf("reconcile after repo appears: %v", res.Error)
	}
	ttx, err = env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	got, err := db.GetWorkflowRun(ctx, ttx.Tx, approvalTestTenant, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.WorktreeStatus != domain.WorktreeSkipped {
		t.Fatalf("worktree_status = %q, want skipped (recorded decision must hold)", got.WorktreeStatus)
	}
}
