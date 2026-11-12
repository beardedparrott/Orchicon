package fileedit

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/beardedparrott/orchicon/internal/procwin"
)

// ReconcileGit reconciles an owner's ledger against the run worktree's git state (AC 4).
func ReconcileGit(ctx context.Context, store Store, worktreeDir, tenantID, ownerKind, ownerID, baseRef string, log *slog.Logger) error {
	// Resolve the base to a MERGE BASE once, not per path: 'develop...HEAD' is only the branch's own
	// delta when the fork point is used, and one git call beats one per changed file.
	mergeBase := ""
	if baseRef != "" {
		mergeBase = mergeBaseRef(worktreeDir, baseRef)
	}
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

	changed, err := gitChangedPaths(worktreeDir, mergeBase)
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
		e, ok := gitEntryFor(worktreeDir, p, mergeBase)
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
	cmd := procwin.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// gitChangedPaths lists the worktree-changed paths from `git status --porcelain=v1` (tracked +
// untracked, renames surfaced as delete+add of their old/new paths).
func gitChangedPaths(dir, mergeBase string) ([]string, error) {
	cmd := procwin.Command("git", "-C", dir, "status", "--porcelain=v1", "-z")
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
	// COMMITTED WORK, which `git status` CANNOT SEE.
	if mergeBase != "" {
		committed, cerr := committedPaths(dir, mergeBase)
		if cerr == nil {
			seen := make(map[string]bool, len(paths))
			for _, p := range paths {
				seen[p] = true
			}
			for _, p := range committed {
				if !seen[p] {
					seen[p] = true
					paths = append(paths, p)
				}
			}
		}
	}
	return paths, nil
}

// gitEntryFor builds a corrective ledger entry for one git-changed path: "after" is the live file
// content (nil when deleted), "before" is `git show HEAD:<path>` (nil for untracked/new files).
func gitEntryFor(worktreeDir, relPath, mergeBase string) (Entry, bool) {
	abs := filepath.Join(worktreeDir, filepath.FromSlash(relPath))
	var after []byte
	if data, err := os.ReadFile(abs); err == nil {
		after = data
	} else if !os.IsNotExist(err) {
		return Entry{}, false
	}
	// THE BEFORE-SIDE IS THE FORK POINT, not HEAD.
	before := gitShowHEAD(worktreeDir, relPath)
	if mergeBase != "" {
		before = gitShowAt(worktreeDir, mergeBase, relPath)
	}
	e := EntryFromSnapshots(before, after, relPath, ToolReconcileGit)
	return e, true
}

// committedPaths lists the paths this branch changed relative to the merge base — the changes `git
// status` cannot report because they are committed.
func committedPaths(dir, mergeBase string) ([]string, error) {
	cmd := procwin.Command("git", "-C", dir, "diff", "--name-only", "-z", "--no-renames", mergeBase, "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, f := range strings.Split(string(out), "\x00") {
		if p := strings.TrimSpace(f); p != "" {
			paths = append(paths, NormalizePath(p))
		}
	}
	return paths, nil
}

// gitShowAt returns a file's content at rev, or nil when it does not exist there (a new file) or
// the ref cannot be read.
func gitShowAt(dir, rev, rel string) []byte {
	cmd := procwin.Command("git", "-C", dir, "show", rev+":"+rel)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return out
}

// mergeBaseRef resolves the fork point of HEAD and base, or "" when it cannot be resolved (unknown
// ref, no history, not a repo).
func mergeBaseRef(dir, base string) string {
	cmd := procwin.Command("git", "-C", dir, "merge-base", "HEAD", base)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// BaseRefFor resolves the ref a RUN's worktree branch was cut from.
func BaseRefFor(dir string) string {
	for _, ref := range []string{"develop", "origin/develop"} {
		if procwin.Command("git", "-C", dir, "rev-parse", "--verify", "--quiet", ref).Run() == nil {
			return ref
		}
	}
	if out, err := procwin.Command("git", "-C", dir, "symbolic-ref", "refs/remotes/origin/HEAD").Output(); err == nil {
		if ref := strings.TrimPrefix(strings.TrimSpace(string(out)), "refs/remotes/"); ref != "" {
			return ref
		}
	}
	if out, err := procwin.Command("git", "-C", dir, "symbolic-ref", "--short", "HEAD").Output(); err == nil {
		if ref := strings.TrimSpace(string(out)); ref != "" {
			return ref
		}
	}
	return ""
}
