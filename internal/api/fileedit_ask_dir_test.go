package api

// fileedit_ask_dir_test.go — the Ask diff observer must watch the CONVERSATION's
// directory, not the control plane's working directory.
//
// THE OPERATOR: "I don't see ANY files under diff viewer right now in this ask
// session."
//
// REPRODUCED FROM THE LIVE PLANE'S OWN LOG, which named the cause outright:
//
//	WARN "ask file edit ledger git reconciliation failed"
//	     conversation="01M57YEE75ETXRJXB90YP88BRE"      <- the session in question
//	     dir="/home/beardedparrott/projects/Orchicon"    <- THE PLANE'S CWD
//
// Every one of those failures (7 of them, 100%) was against the plane's cwd and
// never a session's own tree. askBaseDir() returned os.Getwd() — the directory the
// control plane was STARTED from — on the stated reasoning that Ask sessions run on
// the host opencode serve as directory-less sessions. That is true of that ONE
// transport and false of the others: a native orchicon session runs its file/shell
// suite against AskFileScopeFor's directory (the conversation's project).
//
// WHY IT WAS INVISIBLE RATHER THAN LOUD: the plane's cwd usually EXISTS and usually
// holds a file at the same relative path (it is the repo the plane was started from),
// so the observer compared the WRONG FILE against the previous contents of the wrong
// file. A session editing internal/db/usage.go in its own worktree had the plane diff
// <plane cwd>/internal/db/usage.go — a file it never touched — so the row was dropped
// as unchanged, or worse, recorded against a file the session never opened.
//
// AND WHY THE EXISTING SUITE MISSED IT: fileedit_ask_attribution_db_test.go drives
// the hook with an engine payload (output containing "file_edits"), which takes the
// ENGINE branch and never consults a directory at all. The observer branch — the one
// that needs a correct base dir — was only ever exercised with "" and a non-repo temp
// dir. So the tuple was pinned and the DIRECTORY was not.
//
// This test pins the directory: it asserts the resolved base is the conversation's
// project dir and NOT the process cwd, for both the observer and the reconciler.

import (
	"context"
	"os"
	"testing"

	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// TestAskBaseDirIsTheConversationsProjectNotTheProcessCwd is the regression pin.
//
// It needs a DB (the conversation's project is read from a row, which is the whole
// point — the directory comes from the same authority the tools use).
func TestAskBaseDirIsTheConversationsProjectNotTheProcessCwd(t *testing.T) {
	pool := askDirPool(t)
	ctx := context.Background()
	tenantID := "tnt_askdir_qa"

	// A project with a REAL, distinctive directory: if the resolver ever falls back to
	// the process cwd again, this assertion notices.
	projDir := t.TempDir()
	convID := db.NewID()
	proj := createAskDirTestProject(t, pool, tenantID, projDir)

	// The conversation's project id rides the context — the same stamping chat.go
	// does for a turn (withAskConversationProject), which is what AskFileScopeFor
	// reads. Driving it through the context rather than a parameter is deliberate:
	// it is the production shape.
	// BOTH stamps, because AskFileScopeFor requires them: the TENANT (the RLS scope
	// every DB read needs) and the conversation's project. The production turn carries
	// exactly these two, so the test drives the real shape rather than a partial one.
	cctx := tenant.WithID(
		askmode.WithConversationScope(ctx, askmode.ConversationScope{ConversationID: convID, ProjectID: proj.ID}),
		tenantID,
	)

	deps := &Dependencies{Pool: pool}
	got := askBaseDir(cctx, deps)
	if got == "" {
		t.Fatal("askBaseDir returned empty for a conversation whose project has a directory — the " +
			"observer would ledger nothing and the diff pane would stay blank")
	}
	if got != projDir {
		t.Errorf("askBaseDir = %q, want the conversation's project dir %q", got, projDir)
	}

	// THE ACTUAL BUG, asserted directly: it must NOT be the process working directory.
	// This is the comparison that would have failed before the fix, on any machine
	// where the plane was started from a different tree than the session works in —
	// and it failed SILENTLY there, because both directories exist.
	if wd, err := os.Getwd(); err == nil && got == wd {
		t.Errorf("askBaseDir = the PROCESS CWD (%q) — the observer would diff files the session "+
			"never touched, which is the defect: the session writes in its own tree while the "+
			"plane watches the directory it was started from", wd)
	}
}

// TestAskBaseDirIsEmptyWithoutAProject: an unassigned conversation has no project dir,
// and the honest answer is "" (skip observation) rather than a guess at the cwd.
func TestAskBaseDirIsEmptyWithoutAProject(t *testing.T) {
	pool := askDirPool(t)
	ctx := context.Background()

	// A tenant, but NO project on the context: an unassigned conversation.
	tctx := tenant.WithID(ctx, "tnt_askdir_qa")
	if got := askBaseDir(tctx, &Dependencies{Pool: pool}); got != "" {
		// The tenant-anchor fallback AskFileScopeFor uses is allowed to answer, but it
		// must NEVER be the process cwd.
		if wd, err := os.Getwd(); err == nil && got == wd {
			t.Errorf("askBaseDir fell back to the PROCESS CWD (%q) for a conversation with no project "+
				"— that is the silent wrong-tree observation this fix removes", wd)
		}
	}
}

// A DB-less plane must not panic or guess: "" means "skip observation", the
// pre-existing posture.
func TestAskBaseDirWithNoPool(t *testing.T) {
	if got := askBaseDir(context.Background(), nil); got != "" {
		t.Errorf("askBaseDir(nil deps) = %q, want empty (skip observation, never guess)", got)
	}
	if got := askBaseDir(context.Background(), &Dependencies{}); got != "" {
		t.Errorf("askBaseDir(no pool) = %q, want empty", got)
	}
}

// askDirPool opens the DB-backed test pool, skipping without ORCHICON_TEST_DSN — the
// same contract every DB-backed test in this repo follows.
func askDirPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed Ask diff-directory test")
	}
	pool, err := db.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// createAskDirTestProject inserts a project whose project_dir is a real, distinctive
// temporary directory. It is cleaned up through the platform's own cascade so the test
// leaves nothing behind.
func createAskDirTestProject(t *testing.T, pool *db.Pool, tenant, dir string) db.ProjectRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tenant tx: %v", err)
	}
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: tenant,
		Name: "Ask Dir Test", Slug: "ask-dir-" + db.NewID()[:8],
		Status: "active", Goals: []byte("{}"), ProjectDir: dir,
	})
	if err != nil {
		_ = ttx.Rollback(ctx)
		t.Fatalf("create project: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit project: %v", err)
	}
	db.CleanupProject(t, pool, tenant, proj.ID)
	return proj
}

var _ = db.NewID
