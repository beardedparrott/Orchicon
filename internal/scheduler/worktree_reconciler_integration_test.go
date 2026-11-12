package scheduler

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	// Disable git's BACKGROUND auto-maintenance for the fixture repo.
	gitRun(t, dir, "config", "gc.auto", "0")
	gitRun(t, dir, "config", "maintenance.auto", "false")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test repo\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "initial commit")
	return dir
}

// nonRepoDir creates a temp directory that is NOT inside any git work tree.
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

// purgeWorktreeSweepResidue clears the orphan-sweep's shared-tenant surface before each worktree
// fixture, mirroring blocked_reconciler_test.go's purgeScanResidue.
func purgeWorktreeSweepResidue(t *testing.T, pool *db.Pool) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		return
	}
	defer ttx.Rollback(ctx)
	// Cleared FIRST: the rows below stop describing a branch, and an obligation pointing at a branch that no
	// longer exists would make the queue hold a later fixture's run behind a phantom.
	if _, err := ttx.Tx.Exec(ctx, `DELETE FROM local_integrations WHERE tenant_id = $1`, approvalTestTenant); err != nil {
		return
	}
	if _, err := ttx.Tx.Exec(ctx, `UPDATE workflow_runs SET worktree_branch = ''
		WHERE tenant_id = $1 AND status IN ('completed', 'failed', 'aborted')
		  AND worktree_status = 'pruned' AND worktree_branch <> ''`, approvalTestTenant); err != nil {
		return
	}
	_ = ttx.Commit(ctx)
}

func newWorktreeTestEnv(t *testing.T) *worktreeTestEnv {
	t.Helper()
	pool := approvalTestPool(t)
	purgeWorktreeSweepResidue(t, pool)
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
		ProjectDir:    env.repo,
		ExecutionMode: db.ExecutionModeRuntime,
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

// TestWorktreeCreatedAtArmTime is the core acceptance test: reconcile a pending run against a
// git-backed project and verify the worktree + branch are created and recorded on the run row.
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

// TestWorktreeReconcileIdempotent verifies the converge-don't-recreate contract: a second pass for
// a 'ready' run is a no-op and does not add a worktree.
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

// TestWorktreeConcurrentRunsDistinctBranches verifies the collision-safety acceptance criterion:
// two runs of the same work item get distinct branches and distinct worktrees.
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

// TestWorktreeNonRepoProjectSkipped verifies non-repo detection: a project whose directory is not a
// git work tree is marked 'skipped' and the run proceeds in place.
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
		ProjectDir:    plain,
		ExecutionMode: db.ExecutionModeRuntime,
		// git_strategy=none, DELIBERATELY: a non-repo directory is a legitimate configuration only for the
		// ephemeral strategy.
		GitStrategy: "none",
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

// getProjectGitDetection reads a project's cached git-detection state.
func getProjectGitDetection(t *testing.T, env *worktreeTestEnv, projectID string) db.ProjectRow {
	t.Helper()
	ttx, err := env.pool.BeginTenantTx(context.Background(), approvalTestTenant)
	if err != nil {
		t.Fatalf("get project: begin tx: %v", err)
	}
	defer ttx.Rollback(context.Background())
	proj, err := db.GetProject(context.Background(), ttx.Tx, approvalTestTenant, projectID)
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	return proj
}

// TestWorktreeGitDetectionCached verifies the D1 cache write: after a reconcile pass over a
// git-backed project, the project row carries git_work_tree=true with a fresh git_detected_at.
func TestWorktreeGitDetectionCached(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile: %v", res.Error)
	}
	proj := getProjectGitDetection(t, env, env.proj.ID)
	if !proj.GitWorkTree {
		t.Errorf("git_work_tree = false, want true for a git-backed project")
	}
	if proj.GitDetectedAt == nil {
		t.Errorf("git_detected_at = nil, want a fresh detection timestamp")
	}
}

// TestWorktreeNonRepoProjectCachesFalse verifies a non-repo project's run proceeds in place
// (skipped) AND the cache records git_work_tree=false so the loop does not re-shell-out to git.
func TestWorktreeNonRepoProjectCachesFalse(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	plain := nonRepoDir(t)
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		Name: "Cache Non-Repo Project", Slug: "cache-nonrepo-" + strings.ToLower(db.NewID()),
		Status: "active", Goals: []byte("[]"),
		ProjectDir:    plain,
		ExecutionMode: db.ExecutionModeRuntime,
		// git_strategy=none, DELIBERATELY: a non-repo directory is a legitimate configuration only for the
		// ephemeral strategy.
		GitStrategy: "none",
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
		t.Fatalf("reconcile: %v", err)
	}
	got := getProjectGitDetection(t, env, proj.ID)
	if got.GitWorkTree {
		t.Errorf("git_work_tree = true, want false for a non-repo project")
	}
	if got.GitDetectedAt == nil {
		t.Errorf("git_detected_at = nil, want a fresh detection timestamp")
	}
}

// TestWorktreeGitDetectionInvalidatedOnDirChange verifies the D1 invalidation choke point: changing
// a project's project_dir via UpdateProject resets the git-detection cache (git_detected_at ->.
func TestWorktreeGitDetectionInvalidatedOnDirChange(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	// Populate the cache with a detection.
	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile: %v", res.Error)
	}
	proj := getProjectGitDetection(t, env, env.proj.ID)
	if proj.GitDetectedAt == nil {
		t.Fatalf("expected cache populated before dir change")
	}

	// Change project_dir → cache must be reset to undetermined.
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	cur, err := db.GetProject(ctx, ttx.Tx, approvalTestTenant, env.proj.ID)
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	repo := newTestRepo(t)
	if _, err := db.UpdateProject(ctx, ttx.Tx, approvalTestTenant, env.proj.ID, cur.Version, db.UpdateProjectFields{
		ProjectDir: &repo,
	}); err != nil {
		t.Fatalf("update project dir: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit update: %v", err)
	}

	got := getProjectGitDetection(t, env, env.proj.ID)
	if got.GitDetectedAt != nil {
		t.Errorf("git_detected_at = %v after dir change, want nil (cache invalidated)", got.GitDetectedAt)
	}
	if got.GitWorkTree {
		t.Errorf("git_work_tree = true after dir change, want false (reset)")
	}
}

// TestWorktreeNonRepoSecondPassNoGitShellOut proves the cache avoids repeated shell-outs: after a
// non-repo project's detection is cached (false, fresh).
func TestWorktreeNonRepoSecondPassNoGitShellOut(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	plain := nonRepoDir(t)
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		Name: "NoGit Second Pass", Slug: "nogit-second-" + strings.ToLower(db.NewID()),
		Status: "active", Goals: []byte("[]"),
		ProjectDir:    plain,
		ExecutionMode: db.ExecutionModeRuntime,
		// git_strategy=none, DELIBERATELY: a non-repo directory is a legitimate configuration only for the
		// ephemeral strategy.
		GitStrategy: "none",
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

	// First pass: detect + cache false.
	if res := env.rec.Reconcile(ctx, run.ID); res.Error != nil {
		t.Fatalf("reconcile pass 1: %v", res.Error)
	}
	if proj := getProjectGitDetection(t, env, proj.ID); proj.GitDetectedAt == nil {
		t.Fatalf("expected cache populated after pass 1")
	}

	// Second pass with git unavailable: the fresh cache must be trusted, so
	// the pass converges on 'skipped' without ever invoking git.
	oldPATH := os.Getenv("PATH")
	t.Setenv("PATH", "/nonexistent")
	if res := env.rec.Reconcile(ctx, run.ID); res.Error != nil {
		t.Errorf("second pass shelled out to git (or errored): %v", res.Error)
	}
	_ = os.Setenv("PATH", oldPATH)

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
		t.Errorf("worktree_status = %q, want skipped", got.WorktreeStatus)
	}
}

// TestWorktreeSkippedNotReprovisioned verifies a recorded 'skipped' decision is respected: pointing
// the project at a real git repo later does not make the loop revisit the run.
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
		ProjectDir:    plain,
		ExecutionMode: db.ExecutionModeRuntime,
		// git_strategy=none, DELIBERATELY: a non-repo directory is a legitimate configuration only for the
		// ephemeral strategy.
		GitStrategy: "none",
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

// setRunStatus flips a run to a terminal state (completed/failed/aborted)
// the way the workflow engine would.
func setRunStatus(t *testing.T, env *worktreeTestEnv, status string) {
	t.Helper()
	ctx := context.Background()
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("set status: begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	run, err := db.GetWorkflowRun(ctx, ttx.Tx, approvalTestTenant, env.run.ID)
	if err != nil {
		t.Fatalf("set status: get run: %v", err)
	}
	if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, approvalTestTenant, env.run.ID, run.Version, db.UpdateWorkflowRunFields{
		Status: &status,
	}); err != nil {
		t.Fatalf("set status: update run: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("set status: commit: %v", err)
	}
}

// setRunPrState writes the authoritative pr_state into the run's run_context JSONB.
func setRunPrState(t *testing.T, env *worktreeTestEnv, state string) {
	t.Helper()
	ctx := context.Background()
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("set pr_state: begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	run, err := db.GetWorkflowRun(ctx, ttx.Tx, approvalTestTenant, env.run.ID)
	if err != nil {
		t.Fatalf("set pr_state: get run: %v", err)
	}
	rc := []byte(`{"pr_state":"` + state + `"}`)
	if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, approvalTestTenant, env.run.ID, run.Version, db.UpdateWorkflowRunFields{
		RunContext: &rc,
	}); err != nil {
		t.Fatalf("set pr_state: update run: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("set pr_state: commit: %v", err)
	}
}

// getStepRun reads a step run row outside a transaction.
func getStepRun(t *testing.T, env *worktreeTestEnv, stepRunID string) db.WorkflowStepRunRow {
	t.Helper()
	ttx, err := env.pool.BeginTenantTx(context.Background(), approvalTestTenant)
	if err != nil {
		t.Fatalf("get step run: begin tx: %v", err)
	}
	defer ttx.Rollback(context.Background())
	sr, err := db.GetWorkflowStepRun(context.Background(), ttx.Tx, approvalTestTenant, stepRunID)
	if err != nil {
		t.Fatalf("get step run: %v", err)
	}
	return sr
}

// assertPruned verifies the AC1/AC2 postconditions on the run row: pruned
// status, cleared path, branch retained.
func assertPruned(t *testing.T, env *worktreeTestEnv) db.WorkflowRunRow {
	t.Helper()
	run := env.getRun(t)
	if run.WorktreeStatus != domain.WorktreePruned {
		t.Fatalf("worktree_status = %q, want pruned", run.WorktreeStatus)
	}
	if run.WorktreePath != "" {
		t.Errorf("worktree_path = %q, want empty after prune", run.WorktreePath)
	}
	if run.WorktreeBranch == "" {
		t.Errorf("worktree_branch was cleared; the DevOps merge step needs it (branch deletion stays there)")
	}
	return run
}

// TestWorktreePrunedAtTerminal is the core pruning acceptance test: when a run reaches a terminal
// state, reconcile removes the worktree dir, runs `git worktree prune`, records 'pruned'.
func TestWorktreePrunedAtTerminal(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (provision): %v", res.Error)
	}
	if _, err := os.Stat(env.expectedPath()); err != nil {
		t.Fatalf("worktree not created before terminal state: %v", err)
	}
	setRunStatus(t, env, domain.WorkflowRunCompleted)

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (prune): %v", res.Error)
	}

	// The worktree dir is gone and git no longer lists it.
	if _, err := os.Stat(env.expectedPath()); !os.IsNotExist(err) {
		t.Fatalf("worktree dir still exists after terminal prune: %v", err)
	}
	if strings.Contains(gitRun(t, env.repo, "worktree", "list"), env.expectedPath()) {
		t.Fatalf("worktree list still shows %s after prune", env.expectedPath())
	}

	// Row postconditions: pruned + cleared path + retained branch.
	run := assertPruned(t, env)
	if run.WorktreeBranch != env.expectedBranch() {
		t.Errorf("worktree_branch = %q, want %q (must survive pruning)", run.WorktreeBranch, env.expectedBranch())
	}

	// On SUCCESS the branch is deleted by the reconciler.
	if out := gitRun(t, env.repo, "branch", "--list", env.expectedBranch()); out != "" {
		t.Fatalf("branch %q was NOT deleted after a successful run", env.expectedBranch())
	}
}

// TestWorktreePruneKeepsBranchOnFailure locks in the success-only deletion contract: a FAILED run's
// branch survives pruning so a retry can re-attach to it (carry-over of partial work).
func TestWorktreePruneKeepsBranchOnFailure(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (provision): %v", res.Error)
	}
	setRunStatus(t, env, domain.WorkflowRunFailed)

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (prune): %v", res.Error)
	}
	assertPruned(t, env)

	// The branch must survive a failed run.
	if out := gitRun(t, env.repo, "branch", "--list", env.expectedBranch()); out == "" {
		t.Fatalf("branch %q was deleted after a FAILED run — success-only deletion violated", env.expectedBranch())
	}
}

// TestWorktreePruneKeepsBranchOnAborted locks in the success-only deletion
// contract for a cancelled run: the branch survives so a retry can reuse it.
func TestWorktreePruneKeepsBranchOnAborted(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (provision): %v", res.Error)
	}
	setRunStatus(t, env, domain.WorkflowRunAborted)

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (prune aborted): %v", res.Error)
	}
	assertPruned(t, env)

	if out := gitRun(t, env.repo, "branch", "--list", env.expectedBranch()); out == "" {
		t.Fatalf("branch %q was deleted after an ABORTED run — success-only deletion violated", env.expectedBranch())
	}
}

// TestWorktreeReattachToExistingBranch is the retry-after-prune acceptance test: a failed run's
// worktree is pruned but its branch survives.
func TestWorktreeReattachToExistingBranch(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	// Provision, then fail + prune: the worktree dir is removed but the
	// branch survives with whatever commits the worker made.
	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (provision): %v", res.Error)
	}
	// Simulate partial work: a commit on the branch.
	gitRun(t, env.expectedPath(), "config", "user.email", "worktree-test@orchicon.dev")
	gitRun(t, env.expectedPath(), "config", "user.name", "Worktree Test")
	if err := os.WriteFile(filepath.Join(env.expectedPath(), "partial.txt"), []byte("partial work\n"), 0o644); err != nil {
		t.Fatalf("write partial work: %v", err)
	}
	gitRun(t, env.expectedPath(), "add", ".")
	gitRun(t, env.expectedPath(), "commit", "-m", "partial work from failed run")
	setRunStatus(t, env, domain.WorkflowRunFailed)
	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (prune): %v", res.Error)
	}
	assertPruned(t, env)
	if out := gitRun(t, env.repo, "branch", "--list", env.expectedBranch()); out == "" {
		t.Fatalf("branch %q was deleted after a failed run", env.expectedBranch())
	}

	// Retry: re-arm the run (status back to running, worktree_status reset
	// to pending so the loop re-provisions).
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	cur, err := db.GetWorkflowRun(ctx, ttx.Tx, approvalTestTenant, env.run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, approvalTestTenant, env.run.ID, cur.Version, db.UpdateWorkflowRunFields{
		Status:         strPtr(domain.WorkflowRunRunning),
		WorktreeStatus: strPtr(domain.WorktreePending),
		WorktreePath:   strPtr(""),
	}); err != nil {
		t.Fatalf("re-arm run: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit re-arm: %v", err)
	}

	// Re-provision: must ATTACH to the existing branch, not fail with
	// "branch already exists".
	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (re-provision): %v", res.Error)
	}
	run := env.getRun(t)
	if run.WorktreeStatus != domain.WorktreeReady {
		t.Fatalf("re-provisioned worktree_status = %q, want ready", run.WorktreeStatus)
	}
	if run.WorktreeBranch != env.expectedBranch() {
		t.Errorf("re-provisioned worktree_branch = %q, want %q", run.WorktreeBranch, env.expectedBranch())
	}
	// The re-provisioned worktree picked up the branch's existing commit.
	if _, err := os.Stat(filepath.Join(env.expectedPath(), "partial.txt")); err != nil {
		t.Fatalf("re-provisioned worktree did not carry over the branch's partial work: %v", err)
	}
}

// TestWorktreePruneKeepsUnmergedBranchOnSuccess locks in the safety gate: a SUCCESSFUL run whose
// branch is NOT merged into the base (develop) is never deleted.
func TestWorktreePruneKeepsUnmergedBranchOnSuccess(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (provision): %v", res.Error)
	}
	// Add a commit to the branch that is NOT merged into develop.
	gitRun(t, env.expectedPath(), "config", "user.email", "worktree-test@orchicon.dev")
	gitRun(t, env.expectedPath(), "config", "user.name", "Worktree Test")
	if err := os.WriteFile(filepath.Join(env.expectedPath(), "unmerged.txt"), []byte("unmerged\n"), 0o644); err != nil {
		t.Fatalf("write unmerged file: %v", err)
	}
	gitRun(t, env.expectedPath(), "add", ".")
	gitRun(t, env.expectedPath(), "commit", "-m", "unmerged work")
	setRunStatus(t, env, domain.WorkflowRunCompleted)

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (prune): %v", res.Error)
	}
	assertPruned(t, env)

	// The unmerged branch must survive even though the run succeeded.
	if out := gitRun(t, env.repo, "branch", "--list", env.expectedBranch()); out == "" {
		t.Fatalf("unmerged branch %q was deleted — never delete unmerged work", env.expectedBranch())
	}
}

// TestWorktreePruneDeletesBranchMergedOnRemote locks in the stale-ref fix: the DevOps worker merges
// via `gh pr merge` on the REMOTE, which advances origin/develop but not the local refs.
func TestWorktreePruneDeletesBranchMergedOnRemote(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (provision): %v", res.Error)
	}
	// Worker work on the branch.
	gitRun(t, env.expectedPath(), "config", "user.email", "worktree-test@orchicon.dev")
	gitRun(t, env.expectedPath(), "config", "user.name", "Worktree Test")
	if err := os.WriteFile(filepath.Join(env.expectedPath(), "merged.txt"), []byte("merged work\n"), 0o644); err != nil {
		t.Fatalf("write merged file: %v", err)
	}
	gitRun(t, env.expectedPath(), "add", ".")
	gitRun(t, env.expectedPath(), "commit", "-m", "merged work")

	// Create a bare remote, push develop + the feature branch, then merge the feature branch into
	// develop ON THE REMOTE — exactly what the DevOps worker's `gh pr merge` does.
	bare := filepath.Join(t.TempDir(), "origin.git")
	gitRun(t, env.repo, "clone", "--bare", env.repo, bare)
	gitRun(t, env.repo, "remote", "add", "origin", bare)
	gitRun(t, env.repo, "push", "origin", "develop")
	gitRun(t, env.repo, "push", "origin", env.expectedBranch())

	// Perform the merge in a scratch checkout of the remote.
	scratch := t.TempDir()
	gitRun(t, scratch, "clone", bare, filepath.Join(scratch, "work"))
	work := filepath.Join(scratch, "work")
	gitRun(t, work, "merge", "origin/"+env.expectedBranch())
	gitRun(t, work, "push", "origin", "develop")

	// Guard: the LOCAL develop ref must not contain the branch — this is the
	// stale state the old gate wrongly rejected.
	if err := exec.Command("git", "-C", env.repo, "merge-base", "--is-ancestor", env.expectedBranch(), "develop").Run(); err == nil {
		t.Fatalf("test setup wrong: local develop already contains the branch; no stale-ref scenario to guard")
	}

	setRunStatus(t, env, domain.WorkflowRunCompleted)
	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (prune): %v", res.Error)
	}
	assertPruned(t, env)

	// The branch was merged on the remote and must be deleted even though
	// the LOCAL develop ref never advanced.
	if out := gitRun(t, env.repo, "branch", "--list", env.expectedBranch()); out != "" {
		t.Fatalf("merged-on-remote branch %q was NOT deleted — stale-ref gate kept it", env.expectedBranch())
	}
}

// TestWorktreePruneDeletesBranchSquashMergedOnRemote is the regression test for the squash-merge
// leak: the DevOps worker merges via `gh pr merge --squash`.
func TestWorktreePruneDeletesBranchSquashMergedOnRemote(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (provision): %v", res.Error)
	}
	// Worker work on the branch.
	gitRun(t, env.expectedPath(), "config", "user.email", "worktree-test@orchicon.dev")
	gitRun(t, env.expectedPath(), "config", "user.name", "Worktree Test")
	if err := os.WriteFile(filepath.Join(env.expectedPath(), "squash-me.txt"), []byte("squashed work\n"), 0o644); err != nil {
		t.Fatalf("write squashed file: %v", err)
	}
	gitRun(t, env.expectedPath(), "add", ".")
	gitRun(t, env.expectedPath(), "commit", "-m", "squashed work")

	// Create a bare remote, push develop + the feature branch, then SQUASH merge the feature branch
	// into develop ON THE REMOTE — exactly what the DevOps worker's `gh pr merge --squash` does.
	bare := filepath.Join(t.TempDir(), "origin.git")
	gitRun(t, env.repo, "clone", "--bare", env.repo, bare)
	gitRun(t, env.repo, "remote", "add", "origin", bare)
	gitRun(t, env.repo, "push", "origin", "develop")
	gitRun(t, env.repo, "push", "origin", env.expectedBranch())

	scratch := t.TempDir()
	gitRun(t, scratch, "clone", bare, filepath.Join(scratch, "work"))
	work := filepath.Join(scratch, "work")
	gitRun(t, work, "config", "user.email", "worktree-test@orchicon.dev")
	gitRun(t, work, "config", "user.name", "Worktree Test")
	gitRun(t, work, "merge", "--squash", "origin/"+env.expectedBranch())
	gitRun(t, work, "commit", "-m", "squash-merge PR")
	gitRun(t, work, "push", "origin", "develop")

	// Guard: the branch tip must NOT be an ancestor of the squashed base —
	// this is exactly the case the old ancestry-only gate wrongly rejected.
	gitRun(t, env.repo, "fetch", "origin", "develop")
	if err := exec.Command("git", "-C", env.repo, "merge-base", "--is-ancestor", env.expectedBranch(), "origin/develop").Run(); err == nil {
		t.Fatalf("setup wrong: branch tip IS an ancestor of the squashed base; not a squash scenario")
	}
	if err := exec.Command("git", "-C", env.repo, "merge-base", "--is-ancestor", env.expectedBranch(), "develop").Run(); err == nil {
		t.Fatalf("setup wrong: local develop already contains the branch; no squash gap to guard")
	}

	// The DevOps worker writes the authoritative merged state post-merge.
	setRunPrState(t, env, "merged")
	setRunStatus(t, env, domain.WorkflowRunCompleted)

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (prune): %v", res.Error)
	}
	assertPruned(t, env)

	// The squash-merged branch must be deleted even though its tip is not an
	// ancestor of the base — this is the leak fix.
	if out := gitRun(t, env.repo, "branch", "--list", env.expectedBranch()); out != "" {
		t.Fatalf("squash-merged branch %q was NOT deleted — squash-aware pruning failed", env.expectedBranch())
	}
	// The run worktree container is reaped too.
	if _, err := os.Stat(env.expectedPath()); !os.IsNotExist(err) {
		t.Fatalf("run worktree dir still exists after prune: %v", err)
	}
}

// TestWorktreePruneKeepsSquashWithoutPrState locks in the no-delete-on- uncertainty invariant for
// the squash-aware gate.
func TestWorktreePruneKeepsSquashWithoutPrState(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (provision): %v", res.Error)
	}
	gitRun(t, env.expectedPath(), "config", "user.email", "worktree-test@orchicon.dev")
	gitRun(t, env.expectedPath(), "config", "user.name", "Worktree Test")
	if err := os.WriteFile(filepath.Join(env.expectedPath(), "unprovable.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	gitRun(t, env.expectedPath(), "add", ".")
	gitRun(t, env.expectedPath(), "commit", "-m", "unprovable squashed work")

	bare := filepath.Join(t.TempDir(), "origin.git")
	gitRun(t, env.repo, "clone", "--bare", env.repo, bare)
	gitRun(t, env.repo, "remote", "add", "origin", bare)
	gitRun(t, env.repo, "push", "origin", "develop")
	gitRun(t, env.repo, "push", "origin", env.expectedBranch())

	scratch := t.TempDir()
	gitRun(t, scratch, "clone", bare, filepath.Join(scratch, "work"))
	work := filepath.Join(scratch, "work")
	gitRun(t, work, "config", "user.email", "worktree-test@orchicon.dev")
	gitRun(t, work, "config", "user.name", "Worktree Test")
	gitRun(t, work, "merge", "--squash", "origin/"+env.expectedBranch())
	gitRun(t, work, "commit", "-m", "squash-merge PR")
	gitRun(t, work, "push", "origin", "develop")

	setRunStatus(t, env, domain.WorkflowRunCompleted)

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (prune): %v", res.Error)
	}
	assertPruned(t, env)

	if out := gitRun(t, env.repo, "branch", "--list", env.expectedBranch()); out == "" {
		t.Fatalf("branch %q was deleted without pr_state — no delete on uncertainty", env.expectedBranch())
	}
}

// TestWorktreePruneStepBranchSquashMergedOnRemote extends the squash-aware proof to parallel-branch
// step runs: a step sub-branch whose run's PR was squash-merged (pr_state=merged) is reclaimed.
func TestWorktreePruneStepBranchSquashMergedOnRemote(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	// Seed a parallel-branch child step run for the run.
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	sr, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant, WorkflowRunID: env.run.ID,
		StepID: "step-branch-a", StepName: "QA", StepKind: "task",
		Status: domain.StepRunReady, Result: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create branch step run: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit fixture: %v", err)
	}

	// Provision the branch worktree FIRST — this creates <runID>/ as a plain container holding only the
	// nested <runID>/<stepRunID> worktree (the run worktree is never provisioned.
	key := env.run.ID + ":" + sr.ID
	if res := env.rec.Reconcile(ctx, key); res.Error != nil {
		t.Fatalf("provision branch worktree: %v", res.Error)
	}
	branchPath := filepath.Join(env.repo, worktreeDirName, env.run.ID, sr.ID)
	if _, err := os.Stat(branchPath); err != nil {
		t.Fatalf("branch worktree missing after provision: %v", err)
	}
	step := getStepRun(t, env, sr.ID)
	if step.WorktreeBranch == "" {
		t.Fatalf("step run was not recorded with a branch: %+v", step)
	}

	// Worker work on the step branch.
	gitRun(t, branchPath, "config", "user.email", "worktree-test@orchicon.dev")
	gitRun(t, branchPath, "config", "user.name", "Worktree Test")
	if err := os.WriteFile(filepath.Join(branchPath, "step-squash.txt"), []byte("step work\n"), 0o644); err != nil {
		t.Fatalf("write step file: %v", err)
	}
	gitRun(t, branchPath, "add", ".")
	gitRun(t, branchPath, "commit", "-m", "step squashed work")

	// Create a bare remote, push develop + the step branch, then SQUASH merge
	// the step branch into develop on the remote.
	bare := filepath.Join(t.TempDir(), "origin.git")
	gitRun(t, env.repo, "clone", "--bare", env.repo, bare)
	gitRun(t, env.repo, "remote", "add", "origin", bare)
	gitRun(t, env.repo, "push", "origin", "develop")
	gitRun(t, env.repo, "push", "origin", step.WorktreeBranch)

	scratch := t.TempDir()
	gitRun(t, scratch, "clone", bare, filepath.Join(scratch, "work"))
	work := filepath.Join(scratch, "work")
	gitRun(t, work, "config", "user.email", "worktree-test@orchicon.dev")
	gitRun(t, work, "config", "user.name", "Worktree Test")
	gitRun(t, work, "merge", "--squash", "origin/"+step.WorktreeBranch)
	gitRun(t, work, "commit", "-m", "squash-merge step PR")
	gitRun(t, work, "push", "origin", "develop")

	// Guard: the step branch tip must NOT be an ancestor of the squashed base.
	gitRun(t, env.repo, "fetch", "origin", "develop")
	if err := exec.Command("git", "-C", env.repo, "merge-base", "--is-ancestor", step.WorktreeBranch, "origin/develop").Run(); err == nil {
		t.Fatalf("setup wrong: step branch tip IS an ancestor of the squashed base")
	}

	// Authoritative merged state + terminal run.
	setRunPrState(t, env, "merged")
	setRunStatus(t, env, domain.WorkflowRunCompleted)

	if res := env.rec.Reconcile(ctx, key); res.Error != nil {
		t.Fatalf("reconcile (prune step): %v", res.Error)
	}

	// The squash-merged step sub-branch must be deleted under pr_state=merged.
	if out := gitRun(t, env.repo, "branch", "--list", step.WorktreeBranch); out != "" {
		t.Fatalf("squash-merged step branch %q was NOT deleted", step.WorktreeBranch)
	}
	// The step worktree is removed and the vacated <runID>/ container is swept.
	if _, err := os.Stat(branchPath); !os.IsNotExist(err) {
		t.Fatalf("step worktree dir still exists after prune: %v", err)
	}
	if _, err := os.Stat(env.expectedPath()); !os.IsNotExist(err) {
		t.Fatalf("empty run container was NOT swept: %v", err)
	}
}

// TestWorktreePruneOrphanContainerSweepKeepsForeign locks in the guard on the orphan sweep: an
// empty run-namespaced container is reaped.
func TestWorktreePruneOrphanContainerSweepKeepsForeign(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (provision): %v", res.Error)
	}
	// An empty orphan run-namespace (a past run whose worktrees were pruned).
	orphan := filepath.Join(env.repo, worktreeDirName, "01ORPHANRUNID000000000000")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatalf("mkdir orphan: %v", err)
	}
	// A foreign directory that must never be deleted.
	foreign := filepath.Join(env.repo, worktreeDirName, "user-data-notes")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatalf("mkdir foreign: %v", err)
	}
	if err := os.WriteFile(filepath.Join(foreign, "keep.txt"), []byte("do not delete"), 0o644); err != nil {
		t.Fatalf("write foreign file: %v", err)
	}

	setRunStatus(t, env, domain.WorkflowRunCompleted)
	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (prune): %v", res.Error)
	}

	// The empty orphan container is swept.
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("empty orphan container was NOT swept: %v", err)
	}
	// The foreign directory is untouched.
	if _, err := os.Stat(filepath.Join(foreign, "keep.txt")); err != nil {
		t.Fatalf("foreign directory was modified/deleted: %v", err)
	}
}

// TestWorktreePruneNeverDeletesProtectedBranches locks in the safety gate: the reconciler never
// deletes main/develop/current branch.
func TestWorktreePruneNeverDeletesProtectedBranches(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	// Point the run's recorded branch at a protected name (simulating a
	// mis-recorded row) and complete it.
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	cur, err := db.GetWorkflowRun(ctx, ttx.Tx, approvalTestTenant, env.run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	develop := "develop"
	if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, approvalTestTenant, env.run.ID, cur.Version, db.UpdateWorkflowRunFields{
		Status:         strPtr(domain.WorkflowRunCompleted),
		WorktreeStatus: strPtr(domain.WorktreeReady),
		WorktreePath:   strPtr(env.expectedPath()),
		WorktreeBranch: strPtr(develop),
	}); err != nil {
		t.Fatalf("update run: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (prune): %v", res.Error)
	}
	// develop must still exist.
	if out := gitRun(t, env.repo, "branch", "--list", "develop"); out == "" {
		t.Fatalf("develop branch was deleted — protected branches are never deleted")
	}
}

// TestWorktreePruneIdempotent verifies AC2: reconcile retries after a
// successful prune are safe — no error, no churn, no re-creation.
func TestWorktreePruneIdempotent(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (provision): %v", res.Error)
	}
	setRunStatus(t, env, domain.WorkflowRunFailed)

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (prune pass 1): %v", res.Error)
	}
	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (prune pass 2): %v", res.Error)
	}
	run := assertPruned(t, env)

	// The scan pass converges too: a 'pruned' row is not a prune candidate.
	if res := env.rec.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("reconcile (scan): %v", res.Error)
	}
	if _, err := os.Stat(env.expectedPath()); !os.IsNotExist(err) {
		t.Fatalf("worktree dir recreated after prune: %v", err)
	}
	after := env.getRun(t)
	if after.WorktreeStatus != domain.WorktreePruned {
		t.Fatalf("scan disturbed the pruned row: status = %q", after.WorktreeStatus)
	}
	if after.WorktreeBranch != run.WorktreeBranch {
		t.Errorf("scan changed worktree_branch: %q → %q", run.WorktreeBranch, after.WorktreeBranch)
	}
}

// TestWorktreePruneSkipsNonTerminal locks in AC3: a run that is still running with a ready worktree
// is never pruned — neither by the enqueue (Reconcile(key)) path nor by the scan discovery.
func TestWorktreePruneSkipsNonTerminal(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (provision): %v", res.Error)
	}
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	cur, err := db.GetWorkflowRun(ctx, ttx.Tx, approvalTestTenant, env.run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, approvalTestTenant, env.run.ID, cur.Version, db.UpdateWorkflowRunFields{
		Status: strPtr(domain.WorkflowRunRunning),
	}); err != nil {
		t.Fatalf("set run running: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Enqueue path: reconcileOne must route a running run to provisioning
	// convergence, never to pruning.
	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (enqueue): %v", res.Error)
	}
	// Scan path: the prune discovery query must not return a running run.
	if res := env.rec.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("reconcile (scan): %v", res.Error)
	}

	if _, err := os.Stat(env.expectedPath()); err != nil {
		t.Fatalf("running run's worktree was pruned: %v", err)
	}
	run := env.getRun(t)
	if run.WorktreeStatus != domain.WorktreeReady {
		t.Fatalf("running run's worktree_status = %q, want ready", run.WorktreeStatus)
	}
	if run.WorktreePath != env.expectedPath() {
		t.Errorf("running run's worktree_path = %q, want %q", run.WorktreePath, env.expectedPath())
	}
}

// TestWorktreePruneAlreadyGone covers the idempotent "missing worktree = already pruned = success"
// path: the worktree is removed out-of-band (e.g. by a crashed earlier pass) before the row.
func TestWorktreePruneAlreadyGone(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (provision): %v", res.Error)
	}
	// Remove the worktree out-of-band, exactly like a crashed prune pass
	// would (dir removed but row still 'ready').
	gitRun(t, env.repo, "worktree", "remove", "--force", env.expectedPath())
	gitRun(t, env.repo, "worktree", "prune")
	if _, err := os.Stat(env.expectedPath()); !os.IsNotExist(err) {
		t.Fatalf("worktree dir still exists after manual removal: %v", err)
	}
	setRunStatus(t, env, domain.WorkflowRunFailed)

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (prune already-gone): %v", res.Error)
	}
	assertPruned(t, env)
}

// TestWorktreePruneScanDiscovery verifies terminal runs with a ready
// worktree are picked up by the scan pass alone (Reconcile(ctx, "")).
func TestWorktreePruneScanDiscovery(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (provision): %v", res.Error)
	}
	setRunStatus(t, env, domain.WorkflowRunCompleted)

	// No enqueue: the scan must discover and prune the terminal run.
	if res := env.rec.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("reconcile (scan): %v", res.Error)
	}
	if _, err := os.Stat(env.expectedPath()); !os.IsNotExist(err) {
		t.Fatalf("worktree dir still exists after scan-discovered prune: %v", err)
	}
	// The full scan also runs the orphan sweep, which reclaims the provably-merged branch and clears
	// the swept row's recorded branch (the page-advance invariant) — unlike the direct-prune path.
	run := env.getRun(t)
	if run.WorktreeStatus != domain.WorktreePruned {
		t.Fatalf("worktree_status = %q, want pruned", run.WorktreeStatus)
	}
	if run.WorktreePath != "" {
		t.Errorf("worktree_path = %q, want empty after prune", run.WorktreePath)
	}
	if out := gitRun(t, env.repo, "branch", "--list", env.expectedBranch()); out != "" {
		t.Fatalf("branch %q was NOT deleted after a successful run", env.expectedBranch())
	}
	if run.WorktreeBranch != "" {
		t.Errorf("worktree_branch = %q, want empty (orphan sweep reclaims the merged branch on the scan path)", run.WorktreeBranch)
	}
}

// TestWorktreePruneAbortedRun verifies the aborted terminal path is pruned exactly like
// completed/failed (a cancelled work item maps to an aborted run).
func TestWorktreePruneAbortedRun(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (provision): %v", res.Error)
	}
	setRunStatus(t, env, domain.WorkflowRunAborted)

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (prune aborted): %v", res.Error)
	}
	if _, err := os.Stat(env.expectedPath()); !os.IsNotExist(err) {
		t.Fatalf("aborted run's worktree dir still exists: %v", err)
	}
	assertPruned(t, env)
}

// TestWorktreePruneNonReadyTerminalUntouched verifies the idempotent guard: a terminal run in any
// worktree state other than 'ready' has nothing to prune and is never touched.
func TestWorktreePruneNonReadyTerminalUntouched(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	// A terminal run that was never provisioned (worktree_status pending).
	setRunStatus(t, env, domain.WorkflowRunFailed)
	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (enqueue): %v", res.Error)
	}
	if res := env.rec.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("reconcile (scan): %v", res.Error)
	}
	run := env.getRun(t)
	if run.WorktreeStatus != domain.WorktreePending {
		t.Fatalf("non-provisioned terminal run was touched: worktree_status = %q", run.WorktreeStatus)
	}
}

// Re-provisioning after a prune happens ONLY through an explicit retry, which resets
// worktree_status to pending (see TestRetryAfterPruneProvisionsWorktree).
func TestWorktreePrunedNotReprovisioned(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (provision): %v", res.Error)
	}
	setRunStatus(t, env, domain.WorkflowRunCompleted)
	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (prune): %v", res.Error)
	}
	assertPruned(t, env)

	// Re-arm the run the way a recovery would (status back to running).
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	cur, err := db.GetWorkflowRun(ctx, ttx.Tx, approvalTestTenant, env.run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, approvalTestTenant, env.run.ID, cur.Version, db.UpdateWorkflowRunFields{
		Status: strPtr(domain.WorkflowRunRunning),
	}); err != nil {
		t.Fatalf("re-arm run: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit re-arm: %v", err)
	}

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (re-armed): %v", res.Error)
	}
	run := env.getRun(t)
	if run.WorktreeStatus != domain.WorktreePruned {
		t.Fatalf("re-armed run was re-provisioned: worktree_status = %q", run.WorktreeStatus)
	}
	if _, err := os.Stat(env.expectedPath()); !os.IsNotExist(err) {
		t.Fatalf("a worktree was re-created for the pruned run: %v", err)
	}
}

// TestWorktreeRunProvisionedAfterBranchWorktree is the regression test for the parallel-branch
// provisioning race: a parallel-branch step run can be provisioned FIRST.
func TestWorktreeRunProvisionedAfterBranchWorktree(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	// Seed a parallel-branch child step run for the run so we can provision
	// its branch worktree before the run worktree exists.
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	sr, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant, WorkflowRunID: env.run.ID,
		StepID: "step-branch-a", StepName: "PR Reviewer", StepKind: "task",
		Status: domain.StepRunReady, Result: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create branch step run: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit fixture: %v", err)
	}

	// Race order: provision the branch worktree FIRST.
	key := env.run.ID + ":" + sr.ID
	if res := env.rec.Reconcile(ctx, key); res.Error != nil {
		t.Fatalf("provision branch worktree (first): %v", res.Error)
	}
	branchPath := filepath.Join(env.repo, worktreeDirName, env.run.ID, sr.ID)
	if _, err := os.Stat(branchPath); err != nil {
		t.Fatalf("branch worktree missing after first provision: %v", err)
	}
	// The container now exists and is not itself a registered worktree.
	if _, err := os.Stat(env.expectedPath()); err != nil {
		t.Fatalf("run container missing after branch provision: %v", err)
	}
	wt, err := env.rec.worktreeAt(ctx, env.repo, env.expectedPath())
	if err != nil {
		t.Fatalf("worktreeAt(run path): %v", err)
	}
	if wt != nil {
		t.Fatalf("run path should not yet be a registered worktree")
	}

	// Now reconcile the RUN.
	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile run after branch provision: %v", res.Error)
	}

	run := env.getRun(t)
	if run.WorktreeStatus != domain.WorktreeReady {
		t.Fatalf("run worktree_status = %q, want ready (race must not fail-close)", run.WorktreeStatus)
	}
	if run.WorktreePath != env.expectedPath() {
		t.Errorf("run worktree_path = %q, want %q", run.WorktreePath, env.expectedPath())
	}

	// The run worktree is present at <runID>.
	if _, err := os.Stat(env.expectedPath()); err != nil {
		t.Fatalf("run worktree not created at %s: %v", env.expectedPath(), err)
	}
	wt, err = env.rec.worktreeAt(ctx, env.repo, env.expectedPath())
	if err != nil {
		t.Fatalf("worktreeAt(run path) after reconcile: %v", err)
	}
	if wt == nil || wt.branch != env.expectedBranch() {
		t.Fatalf("run worktree missing/wrong branch: %+v", wt)
	}

	// The branch worktree is intact: still registered at its recorded path
	// with the same branch, and its checked-out files survive.
	ttx, err = env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx (read sr): %v", err)
	}
	gotSR, err := db.GetWorkflowStepRun(ctx, ttx.Tx, approvalTestTenant, sr.ID)
	_ = ttx.Rollback(ctx)
	if err != nil {
		t.Fatalf("get step run: %v", err)
	}
	if gotSR.WorktreeStatus != domain.WorktreeReady {
		t.Fatalf("branch worktree_status = %q, want ready (must remain intact)", gotSR.WorktreeStatus)
	}
	if gotSR.WorktreePath != branchPath {
		t.Errorf("branch worktree_path = %q, want %q (must remain at its recorded path)", gotSR.WorktreePath, branchPath)
	}
	wt, err = env.rec.worktreeAt(ctx, env.repo, branchPath)
	if err != nil {
		t.Fatalf("worktreeAt(branch path) after reconcile: %v", err)
	}
	if wt == nil || wt.branch != gotSR.WorktreeBranch {
		t.Fatalf("branch worktree lost/misregistered after run adoption: %+v", wt)
	}
	if _, err := os.Stat(filepath.Join(branchPath, "README.md")); err != nil {
		t.Fatalf("branch worktree checked-out files lost after run adoption: %v", err)
	}

	// The container directory was never deleted (AC2) — it now IS the run
	// worktree holding the nested branch worktree.
	if _, err := os.Stat(env.expectedPath()); err != nil {
		t.Fatalf("run container was deleted: %v", err)
	}
}

// TestWorktreeForeignDirStillFailsClosed locks in AC3: a genuinely foreign directory occupying
// <runID>/ (content not provably ours) still fails closed via markFailed.
func TestWorktreeForeignDirStillFailsClosed(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	// Create a foreign, non-worktree directory at the run's expected path.
	if err := os.MkdirAll(env.expectedPath(), 0o755); err != nil {
		t.Fatalf("mkdir foreign dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(env.expectedPath(), "user-notes.txt"), []byte("do not delete"), 0o644); err != nil {
		t.Fatalf("write foreign file: %v", err)
	}

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile: %v", res.Error)
	}
	run := env.getRun(t)
	if run.WorktreeStatus != domain.WorktreeFailed {
		t.Fatalf("worktree_status = %q, want failed (foreign dir must fail closed)", run.WorktreeStatus)
	}
	// The foreign directory is untouched.
	if _, err := os.Stat(filepath.Join(env.expectedPath(), "user-notes.txt")); err != nil {
		t.Fatalf("foreign directory was modified/deleted: %v", err)
	}
}

// TestWorktreeOrphanBranchSweptOnCompletedRun is the regression test for the orphaned-branch ref
// leak.
func TestWorktreeSweepOrphanBranchOnCompletedRun(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	// Provision a real worktree + branch (deterministic name on the row).
	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (provision): %v", res.Error)
	}
	run := env.getRun(t)
	branch := run.WorktreeBranch
	if branch == "" {
		t.Fatal("run recorded no worktree_branch")
	}

	// Simulate the orphan class: the run's worktree was already pruned (status pruned, path cleared,
	// branch retained) AND the branch has NO live worktree anymore.
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, approvalTestTenant, env.run.ID, run.Version, db.UpdateWorkflowRunFields{
		Status:         strPtr(domain.WorkflowRunCompleted),
		WorktreeStatus: strPtr(domain.WorktreePruned),
		WorktreePath:   strPtr(""),
	}); err != nil {
		_ = ttx.Rollback(ctx)
		t.Fatalf("mark run completed+pruned: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// Detach the branch's worktree entirely: git worktree remove, so the
	// branch is a free ref (only the main checkout remains).
	gitRun(t, env.repo, "worktree", "remove", "--force", env.expectedPath())
	if strings.Contains(gitRun(t, env.repo, "worktree", "list"), env.expectedPath()) {
		t.Fatalf("worktree still listed after remove")
	}

	// The run's branch commits are already in develop (simulate the merge).
	// Put the branch tip on develop so branchProvablyMerged → P1 ancestry.
	gitRun(t, env.repo, "branch", "-f", branch, "develop")

	// Full scan (key "") triggers the orphan sweep at the end.
	if res := env.rec.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("reconcile scan: %v", res.Error)
	}

	// The orphaned (merged) branch must now be gone.
	if out := gitRun(t, env.repo, "branch", "--list", branch); out != "" {
		t.Fatalf("orphaned branch %q was NOT swept after completed run", branch)
	}

	// Regression for the stuck-backlog: the swept run's worktree_branch must be cleared in the DB, or
	// the orphan query (`pruned` + branch <> '') selects it forever and the per-scan page never.
	final := env.getRun(t)
	if final.WorktreeStatus != domain.WorktreePruned {
		t.Fatalf("run worktree_status = %q, want %q", final.WorktreeStatus, domain.WorktreePruned)
	}
	if final.WorktreeBranch != "" {
		t.Fatalf("run worktree_branch = %q after sweep, want \"\" (cleared so the orphan query advances)", final.WorktreeBranch)
	}
	// Assert the orphan query no longer returns the row.
	ttx, err = env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	rows, qerr := db.ListTerminalRunsWithPrunedBranches(ctx, ttx.Tx, approvalTestTenant, 10)
	_ = ttx.Rollback(ctx)
	if qerr != nil {
		t.Fatalf("list terminal runs with pruned branches: %v", qerr)
	}
	for _, r := range rows {
		if r.ID == env.run.ID {
			t.Fatalf("swept run still returned by orphan query (branch %q not cleared)", r.WorktreeBranch)
		}
	}
}

// TestWorktreeSweepSkipsFailedRunBranch pins the success-only sweep guard.
func TestWorktreeSweepSkipsFailedRunBranch(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (provision): %v", res.Error)
	}
	run := env.getRun(t)
	branch := run.WorktreeBranch

	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, approvalTestTenant, env.run.ID, run.Version, db.UpdateWorkflowRunFields{
		Status:         strPtr(domain.WorkflowRunFailed),
		WorktreeStatus: strPtr(domain.WorktreePruned),
		WorktreePath:   strPtr(""),
	}); err != nil {
		_ = ttx.Rollback(ctx)
		t.Fatalf("mark run failed+pruned: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	gitRun(t, env.repo, "worktree", "remove", "--force", env.expectedPath())
	gitRun(t, env.repo, "branch", "-f", branch, "develop")

	if res := env.rec.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("reconcile scan: %v", res.Error)
	}
	if out := gitRun(t, env.repo, "branch", "--list", branch); out == "" {
		t.Fatalf("failed run's branch %q was swept — success-only deletion violated", branch)
	}
}

// TestWorktreeSweepReclaimsAbortedRunBranch verifies Gate B: an ABORTED run whose bound work item
// is terminal (succeeded) has its orphaned branch reclaimed even though the branch is NOT provably.
func TestWorktreeSweepReclaimsAbortedRunBranch(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (provision): %v", res.Error)
	}
	run := env.getRun(t)
	branch := run.WorktreeBranch
	if branch == "" {
		t.Fatal("run recorded no worktree_branch")
	}
	// Make the branch unmerged (a commit not on develop) so Gate A would
	// refuse to delete it — Gate B must bypass the merged check.
	gitRun(t, env.expectedPath(), "config", "user.email", "worktree-test@orchicon.dev")
	gitRun(t, env.expectedPath(), "config", "user.name", "Worktree Test")
	if err := os.WriteFile(filepath.Join(env.expectedPath(), "aborted-unmerged.txt"), []byte("aborted\n"), 0o644); err != nil {
		t.Fatalf("write unmerged file: %v", err)
	}
	gitRun(t, env.expectedPath(), "add", ".")
	gitRun(t, env.expectedPath(), "commit", "-m", "aborted unmerged commit")

	// Mark work item terminal succeeded — aborted branch becomes reclaimable.
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	wi, err := db.GetWorkItem(ctx, ttx.Tx, approvalTestTenant, env.itemID)
	if err != nil {
		_ = ttx.Rollback(ctx)
		t.Fatalf("get work item: %v", err)
	}
	if _, err := db.UpdateWorkItem(ctx, ttx.Tx, approvalTestTenant, env.itemID, wi.Version, db.UpdateWorkItemFields{Status: strPtr(domain.WorkItemSucceeded)}); err != nil {
		_ = ttx.Rollback(ctx)
		t.Fatalf("update work item to succeeded: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit work item update: %v", err)
	}

	run = env.getRun(t)
	ttx, err = env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, approvalTestTenant, env.run.ID, run.Version, db.UpdateWorkflowRunFields{
		Status:         strPtr(domain.WorkflowRunAborted),
		WorktreeStatus: strPtr(domain.WorktreePruned),
		WorktreePath:   strPtr(""),
	}); err != nil {
		_ = ttx.Rollback(ctx)
		t.Fatalf("mark run aborted+pruned: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	gitRun(t, env.repo, "worktree", "remove", "--force", env.expectedPath())
	if strings.Contains(gitRun(t, env.repo, "worktree", "list"), env.expectedPath()) {
		t.Fatalf("worktree still listed after remove")
	}
	// Branch is unmerged (not ancestor of develop) — Gate A would keep it.
	// Gate B must delete it because work item is terminal.
	if out := gitRun(t, env.repo, "branch", "--list", branch); out == "" {
		t.Fatalf("branch %q missing before sweep", branch)
	}

	if res := env.rec.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("reconcile scan: %v", res.Error)
	}
	if out := gitRun(t, env.repo, "branch", "--list", branch); out != "" {
		t.Fatalf("aborted run branch %q was NOT swept (Gate B terminal work item)", branch)
	}
	final := env.getRun(t)
	if final.WorktreeBranch != "" {
		t.Fatalf("run worktree_branch = %q after sweep, want \"\" (cleared)", final.WorktreeBranch)
	}
}

// TestWorktreeSweepReclaimsFailedRunBranchWhenItemSucceeded verifies Gate B for failed runs: a
// FAILED run whose work item is terminal succeeded has its orphaned branch reclaimed even when not.
func TestWorktreeSweepReclaimsFailedRunBranchWhenItemSucceeded(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (provision): %v", res.Error)
	}
	run := env.getRun(t)
	branch := run.WorktreeBranch

	gitRun(t, env.expectedPath(), "config", "user.email", "worktree-test@orchicon.dev")
	gitRun(t, env.expectedPath(), "config", "user.name", "Worktree Test")
	if err := os.WriteFile(filepath.Join(env.expectedPath(), "failed-unmerged.txt"), []byte("failed\n"), 0o644); err != nil {
		t.Fatalf("write unmerged file: %v", err)
	}
	gitRun(t, env.expectedPath(), "add", ".")
	gitRun(t, env.expectedPath(), "commit", "-m", "failed unmerged commit")

	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	wi, err := db.GetWorkItem(ctx, ttx.Tx, approvalTestTenant, env.itemID)
	if err != nil {
		_ = ttx.Rollback(ctx)
		t.Fatalf("get work item: %v", err)
	}
	if _, err := db.UpdateWorkItem(ctx, ttx.Tx, approvalTestTenant, env.itemID, wi.Version, db.UpdateWorkItemFields{Status: strPtr(domain.WorkItemSucceeded)}); err != nil {
		_ = ttx.Rollback(ctx)
		t.Fatalf("update work item to succeeded: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit work item update: %v", err)
	}

	run = env.getRun(t)
	ttx, err = env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, approvalTestTenant, env.run.ID, run.Version, db.UpdateWorkflowRunFields{
		Status:         strPtr(domain.WorkflowRunFailed),
		WorktreeStatus: strPtr(domain.WorktreePruned),
		WorktreePath:   strPtr(""),
	}); err != nil {
		_ = ttx.Rollback(ctx)
		t.Fatalf("mark run failed+pruned: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	gitRun(t, env.repo, "worktree", "remove", "--force", env.expectedPath())

	if out := gitRun(t, env.repo, "branch", "--list", branch); out == "" {
		t.Fatalf("branch %q missing before sweep", branch)
	}
	if res := env.rec.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("reconcile scan: %v", res.Error)
	}
	if out := gitRun(t, env.repo, "branch", "--list", branch); out != "" {
		t.Fatalf("failed+succeeded run branch %q was NOT swept", branch)
	}
	final := env.getRun(t)
	if final.WorktreeBranch != "" {
		t.Fatalf("run worktree_branch = %q after sweep, want \"\"", final.WorktreeBranch)
	}
}

// TestWorktreeSweepRetainsFailedRunBranchWhenItemActive verifies that a FAILED run with an active
// work item (running) is NOT swept — it is a retry target.
func TestWorktreeSweepRetainsFailedRunBranchWhenItemActive(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (provision): %v", res.Error)
	}
	run := env.getRun(t)
	branch := run.WorktreeBranch

	// Work item stays at running (active) — do not transition it.
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, approvalTestTenant, env.run.ID, run.Version, db.UpdateWorkflowRunFields{
		Status:         strPtr(domain.WorkflowRunFailed),
		WorktreeStatus: strPtr(domain.WorktreePruned),
		WorktreePath:   strPtr(""),
	}); err != nil {
		_ = ttx.Rollback(ctx)
		t.Fatalf("mark run failed+pruned: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	gitRun(t, env.repo, "worktree", "remove", "--force", env.expectedPath())
	gitRun(t, env.repo, "branch", "-f", branch, "develop")

	if res := env.rec.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("reconcile scan: %v", res.Error)
	}
	if out := gitRun(t, env.repo, "branch", "--list", branch); out == "" {
		t.Fatalf("failed+active run branch %q was swept — must be retained for retry", branch)
	}
	// The orphan query must NOT return the row (filtered by LEFT JOIN), so a
	// later sweep can advance to newer orphans — the stuck-page fix.
	ttx, err = env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	rows, qerr := db.ListTerminalRunsWithPrunedBranchesInclusive(ctx, ttx.Tx, approvalTestTenant, 10)
	_ = ttx.Rollback(ctx)
	if qerr != nil {
		t.Fatalf("list inclusive: %v", qerr)
	}
	for _, r := range rows {
		if r.ID == env.run.ID {
			t.Fatalf("failed+active run still returned by inclusive orphan query (must be filtered)")
		}
	}
}

// TestWorktreeSweepReclaimsRunWithNoWorkItem verifies a run with no bound work item (work_item_id
// IS NULL/empty) is reclaimable even when failed.
func TestWorktreeSweepReclaimsRunWithNoWorkItem(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (provision): %v", res.Error)
	}
	// Create a second run with no bound work item, using the same project.
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	noItemRun, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowID: "wf-worktree-test", WorkflowVersion: 1,
		ProjectID: env.proj.ID, Status: domain.WorkflowRunPending,
		RunContext: []byte("{}"),
		// WorkItemID left empty — no bound work item.
	})
	if err != nil {
		_ = ttx.Rollback(ctx)
		t.Fatalf("create no-item run: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit no-item run: %v", err)
	}
	if res := env.rec.Reconcile(ctx, noItemRun.ID); res.Error != nil {
		t.Fatalf("reconcile no-item run (provision): %v", res.Error)
	}
	ttx, err = env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	cur, err := db.GetWorkflowRun(ctx, ttx.Tx, approvalTestTenant, noItemRun.ID)
	_ = ttx.Rollback(ctx)
	if err != nil {
		t.Fatalf("get no-item run: %v", err)
	}
	branch := cur.WorktreeBranch
	if branch == "" {
		t.Fatal("no-item run recorded no worktree_branch")
	}
	path := filepath.Join(env.repo, worktreeDirName, noItemRun.ID)
	gitRun(t, path, "config", "user.email", "worktree-test@orchicon.dev")
	gitRun(t, path, "config", "user.name", "Worktree Test")
	if err := os.WriteFile(filepath.Join(path, "noitem-unmerged.txt"), []byte("noitem\n"), 0o644); err != nil {
		t.Fatalf("write unmerged file: %v", err)
	}
	gitRun(t, path, "add", ".")
	gitRun(t, path, "commit", "-m", "no-item unmerged commit")

	ttx, err = env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, approvalTestTenant, noItemRun.ID, cur.Version, db.UpdateWorkflowRunFields{
		Status:         strPtr(domain.WorkflowRunFailed),
		WorktreeStatus: strPtr(domain.WorktreePruned),
		WorktreePath:   strPtr(""),
	}); err != nil {
		_ = ttx.Rollback(ctx)
		t.Fatalf("mark no-item run failed+pruned: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	gitRun(t, env.repo, "worktree", "remove", "--force", path)

	if out := gitRun(t, env.repo, "branch", "--list", branch); out == "" {
		t.Fatalf("no-item branch %q missing before sweep", branch)
	}
	if res := env.rec.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("reconcile scan: %v", res.Error)
	}
	if out := gitRun(t, env.repo, "branch", "--list", branch); out != "" {
		t.Fatalf("no-item failed branch %q was NOT swept", branch)
	}
	ttx, err = env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	final, err := db.GetWorkflowRun(ctx, ttx.Tx, approvalTestTenant, noItemRun.ID)
	_ = ttx.Rollback(ctx)
	if err != nil {
		t.Fatalf("get final no-item run: %v", err)
	}
	if final.WorktreeBranch != "" {
		t.Fatalf("no-item run worktree_branch = %q after sweep, want \"\"", final.WorktreeBranch)
	}
}

// TestWorktreeNoneStrategyDetachedProvisionAndPrune is acceptance criterion #3.
func TestWorktreeNoneStrategyDetachedProvisionAndPrune(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	// The run's WorkflowID is a dummy that does not exist, so the effective
	// strategy resolves from the project. Make it ephemeral (none).
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	proj, err := db.GetProject(ctx, ttx.Tx, approvalTestTenant, env.proj.ID)
	if err != nil {
		ttx.Rollback(ctx)
		t.Fatalf("get project: %v", err)
	}
	if _, err := db.UpdateProject(ctx, ttx.Tx, approvalTestTenant, env.proj.ID, proj.Version, db.UpdateProjectFields{GitStrategy: strPtr("none")}); err != nil {
		ttx.Rollback(ctx)
		t.Fatalf("set project git_strategy=none: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit project update: %v", err)
	}

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (provision): %v", res.Error)
	}

	run := env.getRun(t)
	if run.WorktreeStatus != domain.WorktreeReady {
		t.Fatalf("worktree_status = %q, want ready", run.WorktreeStatus)
	}
	if run.WorktreeBranch != "" {
		t.Fatalf("none run recorded a branch %q — a detached worktree must record no branch", run.WorktreeBranch)
	}
	// No named branch ref was created for the run.
	if out := gitRun(t, env.repo, "branch", "--list", env.expectedBranch()); out != "" {
		t.Fatalf("none run created a named branch %q — detached must create none", env.expectedBranch())
	}
	// The run worktree must be registered as detached.
	if out := gitRun(t, env.repo, "worktree", "list", "--porcelain"); !strings.Contains(out, "detached") {
		t.Fatalf("none run worktree is not detached; got:\n%s", out)
	}

	// Prune must reconcile the detached worktree: dir reaped, row pruned, worktree_path cleared, and no
	// branch ever recorded.
	setRunStatus(t, env, domain.WorkflowRunCompleted)
	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (prune): %v", res.Error)
	}
	run = env.getRun(t)
	if run.WorktreeStatus != domain.WorktreePruned {
		t.Fatalf("worktree_status = %q, want pruned", run.WorktreeStatus)
	}
	if run.WorktreePath != "" {
		t.Fatalf("worktree_path = %q, want empty after prune", run.WorktreePath)
	}
	if run.WorktreeBranch != "" {
		t.Fatalf("detached none run recorded a branch %q after prune", run.WorktreeBranch)
	}
	if _, err := os.Stat(env.expectedPath()); !os.IsNotExist(err) {
		t.Fatalf("detached worktree dir still exists after prune: %v", err)
	}
	if strings.Contains(gitRun(t, env.repo, "worktree", "list"), env.expectedPath()) {
		t.Fatalf("worktree list still shows %s after prune", env.expectedPath())
	}
}

// TestWorktreeDeleteBranchRemovesRemoteOnMerge is acceptance criterion #4: when a provably-merged
// branch clears the local proof gates (P1 ancestry here; P2/P3 + Gate B unchanged).
func TestWorktreeDeleteBranchRemovesRemoteOnMerge(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (provision): %v", res.Error)
	}
	// Worker work on the branch.
	gitRun(t, env.expectedPath(), "config", "user.email", "worktree-test@orchicon.dev")
	gitRun(t, env.expectedPath(), "config", "user.name", "Worktree Test")
	if err := os.WriteFile(filepath.Join(env.expectedPath(), "merged.txt"), []byte("merged work\n"), 0o644); err != nil {
		t.Fatalf("write merged file: %v", err)
	}
	gitRun(t, env.expectedPath(), "add", ".")
	gitRun(t, env.expectedPath(), "commit", "-m", "merged work")

	// Bare origin with develop + the feature branch, then merge the feature
	// branch into develop ON THE REMOTE (the DevOps worker's gh pr merge).
	bare := filepath.Join(t.TempDir(), "origin.git")
	gitRun(t, env.repo, "clone", "--bare", env.repo, bare)
	gitRun(t, env.repo, "remote", "add", "origin", bare)
	gitRun(t, env.repo, "push", "origin", "develop")
	gitRun(t, env.repo, "push", "origin", env.expectedBranch())

	scratch := t.TempDir()
	gitRun(t, scratch, "clone", bare, filepath.Join(scratch, "work"))
	work := filepath.Join(scratch, "work")
	gitRun(t, work, "merge", "origin/"+env.expectedBranch())
	gitRun(t, work, "push", "origin", "develop")

	setRunStatus(t, env, domain.WorkflowRunCompleted)
	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (prune): %v", res.Error)
	}
	assertPruned(t, env)

	// Local ref deleted through the proof gate.
	if out := gitRun(t, env.repo, "branch", "--list", env.expectedBranch()); out != "" {
		t.Fatalf("provably-merged branch %q was NOT deleted locally", env.expectedBranch())
	}
	// Remote ref deleted (L3 remote half of the prune).
	if out := gitRun(t, env.repo, "ls-remote", "--heads", "origin", env.expectedBranch()); out != "" {
		t.Fatalf("provably-merged branch %q was NOT deleted on origin (L3 remote prune); still present: %s", env.expectedBranch(), out)
	}
}

// TestWorktreeFastPassProvisionsWhileSlowGated is the 3-minute dispatch stall regression test: with
// the slow pass freshly run (gated), a full scan must still provision a pending run to ready.
func TestWorktreeFastPassProvisionsWhileSlowGated(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	// Pretend a slow pass just ran: scan("") must skip the slow pass and
	// still provision through the fast pass.
	env.rec.lastSlowPass = time.Now()

	if res := env.rec.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("reconcile scan (slow-gated): %v", res.Error)
	}
	run := env.getRun(t)
	if run.WorktreeStatus != domain.WorktreeReady {
		t.Fatalf("slow-gated scan left worktree_status = %q, want ready (provisioning must not wait behind the slow pass)", run.WorktreeStatus)
	}
	if run.WorktreePath == "" {
		t.Fatalf("slow-gated scan recorded no worktree_path")
	}
	// The slow gate must still be holding (scan must not have run/retamped
	// the slow pass while gated).
	if time.Since(env.rec.lastSlowPass) >= slowPassInterval {
		t.Fatalf("slow-gated scan unexpectedly ran the slow pass")
	}
}

// TestWorktreeScanRunsSlowPassAfterInterval verifies the sweep slow pass is not disabled, only
// deprioritized: once the interval has elapsed, a scan runs the orphan sweep (here.
func TestWorktreeScanRunsSlowPassAfterInterval(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (provision): %v", res.Error)
	}
	run := env.getRun(t)
	branch := run.WorktreeBranch
	if branch == "" {
		t.Fatal("run recorded no worktree_branch")
	}

	// Build the orphan class: completed + pruned + branch retained, branch
	// tip on develop (merged), worktree detached.
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, approvalTestTenant, env.run.ID, run.Version, db.UpdateWorkflowRunFields{
		Status:         strPtr(domain.WorkflowRunCompleted),
		WorktreeStatus: strPtr(domain.WorktreePruned),
		WorktreePath:   strPtr(""),
	}); err != nil {
		_ = ttx.Rollback(ctx)
		t.Fatalf("mark run completed+pruned: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	gitRun(t, env.repo, "worktree", "remove", "--force", env.expectedPath())
	gitRun(t, env.repo, "branch", "-f", branch, "develop")

	// Force the slow pass due: the sweep must reclaim the orphan branch and
	// restamp lastSlowPass.
	env.rec.lastSlowPass = time.Now().Add(-2 * slowPassInterval)
	before := env.rec.lastSlowPass
	if res := env.rec.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("reconcile scan (slow-due): %v", res.Error)
	}
	if out := gitRun(t, env.repo, "branch", "--list", branch); out != "" {
		t.Fatalf("slow-due scan did not sweep merged orphan branch %q (slow pass must still run cleanup)", branch)
	}
	if !env.rec.lastSlowPass.After(before) {
		t.Fatalf("slow-due scan did not restamp lastSlowPass")
	}
}

// TestWorktreeSlowPassBudgetExpiryResumesNextTick verifies the page-advance invariant under budget
// expiry: sweep funcs with an already-expired deadline return early without clearing anything.
func TestWorktreeSlowPassBudgetExpiryResumesNextTick(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile (provision): %v", res.Error)
	}
	run := env.getRun(t)
	branch := run.WorktreeBranch
	if branch == "" {
		t.Fatal("run recorded no worktree_branch")
	}

	// Build the orphan class: completed + pruned + branch retained, branch
	// tip on develop (merged), worktree detached.
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, approvalTestTenant, env.run.ID, run.Version, db.UpdateWorkflowRunFields{
		Status:         strPtr(domain.WorkflowRunCompleted),
		WorktreeStatus: strPtr(domain.WorktreePruned),
		WorktreePath:   strPtr(""),
	}); err != nil {
		_ = ttx.Rollback(ctx)
		t.Fatalf("mark run completed+pruned: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	gitRun(t, env.repo, "worktree", "remove", "--force", env.expectedPath())
	gitRun(t, env.repo, "branch", "-f", branch, "develop")

	// Expired-budget sweep must return early: branch retained AND row still
	// recorded (nothing cleared, resumable next tick).
	expired := time.Now().Add(-time.Second)
	env.rec.sweepOrphanBranches(ctx, approvalTestTenant, expired)
	if out := gitRun(t, env.repo, "branch", "--list", branch); out == "" {
		t.Fatalf("expired-budget sweep deleted the branch — it must return early and resume next tick")
	}
	run = env.getRun(t)
	if run.WorktreeBranch == "" {
		t.Fatalf("expired-budget sweep cleared the recorded branch — forward progress requires the row to still match next tick")
	}

	// Fresh-budget sweep reclaims it (same assertions as the orphan test).
	env.rec.sweepOrphanBranches(ctx, approvalTestTenant, time.Now().Add(30*time.Second))
	if out := gitRun(t, env.repo, "branch", "--list", branch); out != "" {
		t.Fatalf("fresh-budget sweep did not reclaim merged orphan branch %q", branch)
	}
}
