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
//
// baseRef IS THE DIFFERENCE BETWEEN A RUN AND A CONVERSATION, and it is passed
// rather than inferred because the two owners need opposite answers:
//
//   - a RUN writes its worktree on a branch forked from a base, then COMMITS
//     (its brief orders it: "commit ALL changes ... verify `git status
//     --porcelain` is clean"). `git status` is therefore EMPTY by the time this
//     runs, and the run's whole contribution is invisible. Passing the base ref
//     adds the branch's committed changes and reads the before-side from the
//     fork point, which is the only way a committed run ever produces a diff.
//   - an ASK conversation edits the project dir on whatever branch it happens to
//     be on. Its own work is uncommitted, so `git status` is exactly right — and
//     passing a base there would attribute the WHOLE BRANCH's history to the
//     conversation. So the Ask path passes "" and keeps working-tree semantics.
//
// Empty baseRef = working-tree only (today's behaviour, unchanged).
func ReconcileGit(ctx context.Context, store Store, worktreeDir, tenantID, ownerKind, ownerID, baseRef string, log *slog.Logger) error {
	// Resolve the base to a MERGE BASE once, not per path: 'develop...HEAD' is
	// only the branch's own delta when the fork point is used, and one git call
	// beats one per changed file.
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
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// gitChangedPaths lists the worktree-changed paths from
// `git status --porcelain=v1` (tracked + untracked, renames surfaced as
// delete+add of their old/new paths).
func gitChangedPaths(dir, mergeBase string) ([]string, error) {
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
	// COMMITTED WORK, which `git status` CANNOT SEE.
	//
	// The worker's contract is to commit and leave the tree clean, so for a RUN
	// the working-tree list above is empty while the entire contribution sits in
	// commits on the run branch. Diffing from the FORK POINT (the merge base,
	// never the base ref itself: the base may have moved on since the branch was
	// cut, and `base..HEAD` would then report every unrelated merge) yields
	// exactly the branch's own delta.
	//
	// This is additive: an empty mergeBase — every Ask conversation, and any
	// caller that did not ask for branch semantics — leaves the list as it was.
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

// gitEntryFor builds a corrective ledger entry for one git-changed path:
// "after" is the live file content (nil when deleted), "before" is
// `git show HEAD:<path>` (nil for untracked/new files). The diff is computed
// by the same engine as every other entry — final truth == git truth.
func gitEntryFor(worktreeDir, relPath, mergeBase string) (Entry, bool) {
	abs := filepath.Join(worktreeDir, filepath.FromSlash(relPath))
	var after []byte
	if data, err := os.ReadFile(abs); err == nil {
		after = data
	} else if !os.IsNotExist(err) {
		return Entry{}, false
	}
	// THE BEFORE-SIDE IS THE FORK POINT, not HEAD.
	//
	// For a committed run HEAD *is* the commit, so a HEAD-seeded before is the
	// SAME BYTES as the after: the row would be written and the diff would be
	// empty, which is how a ledger with the right paths still renders nothing.
	// The fork point is the last state before this run touched the file, so
	// mb:path vs the working copy is the run's actual change — and a path absent
	// at mb yields a nil before, i.e. a correct NEW-FILE diff.
	before := gitShowHEAD(worktreeDir, relPath)
	if mergeBase != "" {
		before = gitShowAt(worktreeDir, mergeBase, relPath)
	}
	e := EntryFromSnapshots(before, after, relPath, ToolReconcileGit)
	return e, true
}

// committedPaths lists the paths this branch changed relative to the merge base
// — the changes `git status` cannot report because they are committed.
//
// Rename detection is DISABLED (--no-renames) so a rename surfaces as the plain
// old and new paths rather than as a paired record: the ledger keys on paths,
// and a rename pair would need both to be tracked anyway. Each is then a normal
// entry — a delete on one path and a create on the other.
func committedPaths(dir, mergeBase string) ([]string, error) {
	cmd := exec.Command("git", "-C", dir, "diff", "--name-only", "-z", "--no-renames", mergeBase, "HEAD")
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

// gitShowAt returns a file's content at rev, or nil when it does not exist
// there (a new file) or the ref cannot be read. A nil before is meaningful:
// EntryFromSnapshots renders it as a creation.
func gitShowAt(dir, rev, rel string) []byte {
	cmd := exec.Command("git", "-C", dir, "show", rev+":"+rel)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return out
}

// mergeBaseRef resolves the fork point of HEAD and base, or "" when it cannot
// be resolved (unknown ref, no history, not a repo) — in which case the caller
// keeps working-tree-only semantics rather than guessing.
func mergeBaseRef(dir, base string) string {
	cmd := exec.Command("git", "-C", dir, "merge-base", "HEAD", base)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// BaseRefFor resolves the ref a RUN's worktree branch was cut from.
//
// IT MIRRORS THE WORKTREE RECONCILER'S OWN RESOLUTION
// (scheduler.resolveBaseRef: develop → origin/develop → origin/HEAD → current
// HEAD) ON PURPOSE. The branch was CREATED from whatever that resolves to, so
// resolving it the same way here means the reconcile anchors on the branch's
// real fork point by construction rather than by a second, drifting guess.
//
// Returns "" when nothing resolves; callers then get working-tree semantics.
func BaseRefFor(dir string) string {
	for _, ref := range []string{"develop", "origin/develop"} {
		if exec.Command("git", "-C", dir, "rev-parse", "--verify", "--quiet", ref).Run() == nil {
			return ref
		}
	}
	if out, err := exec.Command("git", "-C", dir, "symbolic-ref", "refs/remotes/origin/HEAD").Output(); err == nil {
		if ref := strings.TrimPrefix(strings.TrimSpace(string(out)), "refs/remotes/"); ref != "" {
			return ref
		}
	}
	if out, err := exec.Command("git", "-C", dir, "symbolic-ref", "--short", "HEAD").Output(); err == nil {
		if ref := strings.TrimSpace(string(out)); ref != "" {
			return ref
		}
	}
	return ""
}
