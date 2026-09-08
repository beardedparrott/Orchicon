package fileedit

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ReconcileGit reconciles an owner's ledger against the run worktree's git
// state (AC 4). Called once on the owner's terminal state, best-effort:
//   - ledger paths present in `git status --porcelain` → git_confirmed=TRUE
//   - paths changed in git but absent from the ledger → corrective rows
//     (tool=reconcile:git) carrying git's final diff
//   - the final diff for any path then matches `git diff` by construction
//
// worktreeDir may be empty / non-git / missing (in-place non-repo runs) —
// the call is a no-op there. dirEmpty distinguishes "no worktree" from an
// empty-but-valid dir so callers pass what they have without branching.
func ReconcileGit(ctx context.Context, store Store, worktreeDir, tenantID, ownerKind, ownerID string, log *slog.Logger) error {
	if store == nil || worktreeDir == "" || ownerID == "" {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	if !isGitRepo(worktreeDir) {
		return nil
	}
	extras, ok := store.(StoreExtras)
	if !ok {
		return nil // store lacks reconciliation support
	}

	changed, err := gitChangedPaths(worktreeDir)
	if err != nil {
		return fmt.Errorf("reconcile: git status: %w", err)
	}

	// 1) Confirm every ledger row whose path git reports as changed.
	if len(changed) > 0 {
		if err := extras.MarkGitConfirmed(ctx, tenantID, ownerKind, ownerID, changed); err != nil {
			return fmt.Errorf("reconcile: mark confirmed: %w", err)
		}
	}

	// 2) Corrective rows for git-changed paths the ledger never saw
	// (bash-made edits, missed events, older sessions predating the ledger).
	known, err := extras.KnownPaths(ctx, tenantID, ownerKind, ownerID)
	if err != nil {
		return fmt.Errorf("reconcile: known paths: %w", err)
	}
	knownSet := make(map[string]bool, len(known))
	for _, k := range known {
		knownSet[NormalizePath(k)] = true
	}
	var corrective []Entry
	for _, p := range changed {
		if knownSet[NormalizePath(p)] {
			continue
		}
		e, ok := gitEntryFor(worktreeDir, p)
		if !ok {
			continue
		}
		corrective = append(corrective, e)
	}
	if len(corrective) > 0 {
		if _, err := store.Append(ctx, tenantID, ownerKind, ownerID, corrective); err != nil {
			return fmt.Errorf("reconcile: append corrective rows: %w", err)
		}
		log.Info("file edit ledger reconciled (corrective rows)", "owner", ownerID, "paths", len(corrective))
	}
	return nil
}

// StoreExtras is the reconciliation-facing extension of Store (kept separate
// so the core Store stays minimal for the record path).
type StoreExtras interface {
	MarkGitConfirmed(ctx context.Context, tenantID, ownerKind, ownerID string, paths []string) error
	KnownPaths(ctx context.Context, tenantID, ownerKind, ownerID string) ([]string, error)
}

// isGitRepo reports whether dir is inside a git work tree.
func isGitRepo(dir string) bool {
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return false
	}
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// gitChangedPaths lists the worktree-changed paths from
// `git status --porcelain=v1` (tracked + untracked, renames surfaced as
// delete+add of their old/new paths).
func gitChangedPaths(dir string) ([]string, error) {
	cmd := exec.Command("git", "-C", dir, "status", "--porcelain=v1", "-z")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	// -z format: XY <path>\0[<origPath>\0 for renames]
	fields := strings.Split(string(out), "\x00")
	var paths []string
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 4 {
			continue
		}
		status, path := f[:2], strings.TrimSpace(f[3:])
		if path == "" {
			continue
		}
		paths = append(paths, NormalizePath(path))
		if status[0] == 'R' || status[1] == 'R' {
			if i+1 < len(fields) {
				i++
				if orig := strings.TrimSpace(fields[i]); orig != "" {
					paths = append(paths, NormalizePath(orig))
				}
			}
		}
	}
	return paths, nil
}

// gitEntryFor builds a corrective ledger entry for one git-changed path:
// "after" is the live file content (nil when deleted), "before" is
// `git show HEAD:<path>` (nil for untracked/new files). The diff is computed
// by the same engine as every other entry — final truth == git truth.
func gitEntryFor(worktreeDir, relPath string) (Entry, bool) {
	abs := filepath.Join(worktreeDir, filepath.FromSlash(relPath))
	var after []byte
	if data, err := os.ReadFile(abs); err == nil {
		after = data
	} else if !os.IsNotExist(err) {
		return Entry{}, false
	}
	before := gitShowHEAD(worktreeDir, relPath)
	e := EntryFromSnapshots(before, after, relPath, ToolReconcileGit)
	return e, true
}
