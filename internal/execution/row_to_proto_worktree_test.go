package execution

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// TestRowToProtoWorktreeMapping verifies that rowToProto carries the
// worktree provisioning state onto the WorkerExecution wire type: nil
// columns map to empty strings (worktree absent, e.g. a non-repo run or
// a legacy execution), and populated columns map through verbatim. The
// execution detail view renders path/branch/status from these fields, so
// a dropped mapping would silently hide the worktree.
func TestRowToProtoWorktreeMapping(t *testing.T) {
	t.Run("nil worktree columns map to empty", func(t *testing.T) {
		row := db.ExecutionRow{ID: "e1", Status: "succeeded", HealthState: "healthy"}
		p := rowToProto(row)
		if p.WorktreeStatus != "" || p.WorktreeBranch != "" || p.WorktreePath != "" {
			t.Errorf("nil worktree columns must map to empty strings; got status=%q branch=%q path=%q",
				p.WorktreeStatus, p.WorktreeBranch, p.WorktreePath)
		}
	})

	t.Run("ready worktree maps all three fields", func(t *testing.T) {
		branch := "orchicon-run-abcd"
		path := "/srv/worktrees/abcd"
		status := "ready"
		row := db.ExecutionRow{
			ID:             "e2",
			Status:         "running",
			HealthState:    "healthy",
			WorktreeStatus: &status,
			WorktreeBranch: &branch,
			WorktreePath:   &path,
		}
		p := rowToProto(row)
		if p.WorktreeStatus != "ready" {
			t.Errorf("WorktreeStatus = %q, want %q", p.WorktreeStatus, "ready")
		}
		if p.WorktreeBranch != branch {
			t.Errorf("WorktreeBranch = %q, want %q", p.WorktreeBranch, branch)
		}
		if p.WorktreePath != path {
			t.Errorf("WorktreePath = %q, want %q", p.WorktreePath, path)
		}
	})

	t.Run("skipped non-repo run carries status only", func(t *testing.T) {
		status := "skipped"
		row := db.ExecutionRow{
			ID:             "e3",
			Status:         "succeeded",
			HealthState:    "healthy",
			WorktreeStatus: &status,
		}
		p := rowToProto(row)
		if p.WorktreeStatus != "skipped" {
			t.Errorf("WorktreeStatus = %q, want %q", p.WorktreeStatus, "skipped")
		}
		if p.WorktreeBranch != "" || p.WorktreePath != "" {
			t.Errorf("skipped run must not map branch/path; got branch=%q path=%q",
				p.WorktreeBranch, p.WorktreePath)
		}
	})
}
