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

// TestWorktreeGitDetectionCached verifies the D1 cache write: after a
// reconcile pass over a git-backed project, the project row carries
// git_work_tree=true with a fresh git_detected_at, so later passes trust the
// cache instead of shelling out to git.
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

// TestWorktreeNonRepoProjectCachesFalse verifies a non-repo project's run
// proceeds in place (skipped) AND the cache records git_work_tree=false so
// the loop does not re-shell-out to git on every pass.
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

// TestWorktreeGitDetectionInvalidatedOnDirChange verifies the D1 invalidation
// choke point: changing a project's project_dir via UpdateProject resets the
// git-detection cache (git_detected_at -> NULL) so the reconciler re-detects
// for the new directory.
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

// TestWorktreeNonRepoSecondPassNoGitShellOut proves the cache avoids repeated
// shell-outs: after a non-repo project's detection is cached (false, fresh),
// a second reconcile pass must NOT invoke git. We remove git from PATH; if the
// reconciler still trusts the cache, the pass converges on 'skipped' without
// error. If it shelled out to git it would fail to find the binary.
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

// TestWorktreePrunedAtTerminal is the core pruning acceptance test: when a
// run reaches a terminal state, reconcile removes the worktree dir, runs
// `git worktree prune`, records 'pruned', and — AC4 — does NOT delete the
// branch (that stays with the DevOps merge step).
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

	// AC4: the branch must still exist — the control plane never deletes
	// branches.
	if out := gitRun(t, env.repo, "branch", "--list", env.expectedBranch()); out == "" {
		t.Fatalf("branch %q was deleted by pruning — branch deletion stays with the DevOps merge step", env.expectedBranch())
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

// TestWorktreePruneSkipsNonTerminal locks in AC3: a run that is still
// running with a ready worktree is never pruned — neither by the enqueue
// (Reconcile(key)) path nor by the scan discovery.
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

// TestWorktreePruneAlreadyGone covers the idempotent "missing worktree =
// already pruned = success" path: the worktree is removed out-of-band (e.g.
// by a crashed earlier pass) before the row is marked, and reconcile still
// converges to 'pruned' without error.
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
	assertPruned(t, env)
}

// TestWorktreePruneAbortedRun verifies the aborted terminal path is pruned
// exactly like completed/failed (a cancelled work item maps to an aborted
// run).
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

// TestWorktreePruneNonReadyTerminalUntouched verifies the idempotent guard:
// a terminal run in any worktree state other than 'ready' has nothing to
// prune and is never touched.
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

// TestWorktreePrunedNotReprovisioned locks in the recovery-adjacent guard:
// a run whose worktree was pruned is a recorded terminal decision — even if
// the run is later re-armed (status flipped back to running), the loop never
// re-provisions it (the deterministic branch already exists, so
// re-provisioning could never succeed).
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
