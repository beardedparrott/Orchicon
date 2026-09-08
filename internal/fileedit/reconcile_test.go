package fileedit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// fakeReconcileStore is a Store+StoreExtras fake capturing the
// reconciliation touches (mark-confirmed paths, known paths, appended
// corrective rows) so the git reconciliation can be asserted without a DB.
type fakeReconcileStore struct {
	known      []string
	marked     []string
	appended   []Entry
	appendedOK bool
}

func (f *fakeReconcileStore) Append(ctx context.Context, tenantID, ownerKind, ownerID string, entries []Entry) ([]*db.FileEditLedgerRow, error) {
	f.appended = append(f.appended, entries...)
	f.appendedOK = true
	return nil, nil
}

func (f *fakeReconcileStore) MarkGitConfirmed(ctx context.Context, tenantID, ownerKind, ownerID string, paths []string) error {
	f.marked = append(f.marked, paths...)
	return nil
}

func (f *fakeReconcileStore) KnownPaths(ctx context.Context, tenantID, ownerKind, ownerID string) ([]string, error) {
	return f.known, nil
}

// gitRepo creates a temp git repo with an initial tracked file so the
// "changed" state is observable via `git status`.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("orig\n"), 0o644)
	run("add", "tracked.txt")
	run("commit", "-qm", "init")
	return dir
}

// TestReconcileGitConfirmedAndCorrective covers the two AC4 behaviours:
//  1. a ledger-known path changed in git → marked git_confirmed;
//  2. a git-changed path the ledger never saw → a corrective (reconcile:git)
//     entry carrying git's final diff.
func TestReconcileGitConfirmedAndCorrective(t *testing.T) {
	dir := gitRepo(t)
	ctx := context.Background()
	store := &fakeReconcileStore{known: []string{"tracked.txt"}}

	// Mutate the tracked file (git-changed, ledger known) AND create a new
	// untracked file (git-changed, ledger unknown → corrective).
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("orig\nchanged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ReconcileGit(ctx, store, dir, "tnt", db.FileEditOwnerExecution, "exec-1", nil); err != nil {
		t.Fatalf("ReconcileGit: %v", err)
	}

	// Both git-changed paths (the modified tracked file AND the new untracked
	// file) were marked git_confirmed — git truth confirms each.
	if len(store.marked) != 2 {
		t.Fatalf("mark-confirmed paths = %v, want [tracked.txt new.txt]", store.marked)
	}
	// The unknown git-changed path got a corrective entry.
	if !store.appendedOK {
		t.Fatalf("no corrective rows appended")
	}
	var correction *Entry
	for i := range store.appended {
		if store.appended[i].Path == "new.txt" {
			correction = &store.appended[i]
			break
		}
	}
	if correction == nil {
		t.Fatalf("no corrective entry for new.txt; appended=%+v", store.appended)
	}
	if correction.Kind != "create" || correction.Tool != ToolReconcileGit {
		t.Fatalf("corrective entry wrong: %+v", correction)
	}
	if !strings.Contains(correction.UnifiedDiff, "+hello\n") {
		t.Fatalf("corrective diff missing content: %q", correction.UnifiedDiff)
	}
	// No corrective for the known path (it already has a ledger entry).
	for _, e := range store.appended {
		if e.Path == "tracked.txt" {
			t.Fatalf("known path should not get a corrective row: %+v", e)
		}
	}
}

// TestReconcileGitNonRepoNoOp: a non-git dir (no worktree) is a no-op — no
// git shell-out, no store touch.
func TestReconcileGitNonRepoNoOp(t *testing.T) {
	dir := t.TempDir()
	store := &fakeReconcileStore{}
	if err := ReconcileGit(context.Background(), store, dir, "tnt", db.FileEditOwnerExecution, "exec-1", nil); err != nil {
		t.Fatalf("non-repo reconcile should be a no-op, got %v", err)
	}
	if store.appendedOK || len(store.marked) != 0 {
		t.Fatalf("non-repo reconcile touched the store: appended=%v marked=%v", store.appended, store.marked)
	}
}
