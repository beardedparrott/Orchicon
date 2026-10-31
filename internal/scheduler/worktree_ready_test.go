package scheduler

// worktree_ready_test.go — "READY" MEANS THE DIRECTORY IS THERE.
//
// REPORTED: a run recorded WorktreePath=<path> / WorktreeStatus="ready" for a directory that did not exist, and
// every bash call in it then died with
//
//	runtime exec: chdir C:\projects\weatherapp\.orchicon-worktrees\01M69SPR...:
//	no such file or directory
//
// The provisioning code trusted `git worktree list`: git keeps .git/worktrees/<id> after the working directory
// is deleted, so it lists an entry that no longer exists. Recording that as ready is what turned a missing
// directory into a whole run of failures whose cause was invisible — the run believed it had a worktree.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// _ keeps the strings import honest if both tests below are ever reduced to one.
var _ = strings.Builder{}

// TestRecordReadyRefusesAMissingDirectory is the invariant, asserted at the one place the row is written.
//
// recordReady stats BEFORE opening a transaction, so a reconciler with no pool is enough: reaching the pool at
// all would be a nil dereference, which is how this proves the check comes first.
func TestRecordReadyRefusesAMissingDirectory(t *testing.T) {
	r := &WorktreeReconciler{log: testLoggerTo(&strings.Builder{})}
	missing := filepath.Join(t.TempDir(), "01M69SPRKBJ5NM10W7M3V86SMH")

	err := r.recordReady(context.Background(), "tnt", "run1", missing, "run-branch")
	if err == nil {
		t.Fatal("a worktree path that does not exist was recorded as READY — the run then reports a worktree " +
			"it never had, and every exec into it fails with the reported chdir error")
	}
	for _, want := range []string{"not a directory", missing} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %q so the cause is readable; got: %v", want, err)
		}
	}
}

// TestRecordReadyRefusesAFile — the other way a path can be wrong.
func TestRecordReadyRefusesAFile(t *testing.T) {
	r := &WorktreeReconciler{log: testLoggerTo(&strings.Builder{})}
	p := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.recordReady(context.Background(), "tnt", "run1", p, "b"); err == nil {
		t.Fatal("a FILE was recorded as a ready worktree")
	}
}
