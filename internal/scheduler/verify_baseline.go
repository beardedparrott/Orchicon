package scheduler

// verify_baseline.go — CAPTURE THE BASELINE BEFORE THE WORK, CACHE IT PER COMMIT. IT CANNOT BLOCK
// DISPATCH.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/verify"
)

// baselineCacheDir is where per-commit baselines live, under the project's own .orchicon tree so it
// sits beside the other run artifacts the operator already knows.
const baselineCacheDir = "baselines"

// baselineCaptureTimeout bounds a background capture. Longer than a single gate run because a cold
// cache is the expensive case, but still bounded so a wedged gate cannot leak a goroutine forever.
const baselineCaptureTimeout = 20 * time.Minute

// baselineInFlight is the single-flight guard: one capture per (project, commit) across the
// process.
var baselineInFlight sync.Map // key string -> struct{}

// baselineCachePath returns the cache file for a commit.
func baselineCachePath(projectDir, commit string) string {
	// A commit SHA is hex, so it is filesystem-safe as-is; the short form is enough to be unique and
	// keeps the directory readable.
	short := strings.TrimSpace(commit)
	if len(short) > 40 {
		short = short[:40]
	}
	return filepath.Join(projectDir, ".orchicon", baselineCacheDir, short+".json")
}

// loadCachedBaseline reads a previously captured baseline, or nil when absent/corrupt.
func loadCachedBaseline(projectDir, commit string) *verify.Baseline {
	if strings.TrimSpace(projectDir) == "" || strings.TrimSpace(commit) == "" {
		return nil
	}
	raw, err := os.ReadFile(baselineCachePath(projectDir, commit))
	if err != nil {
		return nil
	}
	var b verify.Baseline
	if jerr := json.Unmarshal(raw, &b); jerr != nil {
		return nil
	}
	if b.Incomplete() {
		return nil
	}
	return &b
}

// storeCachedBaseline writes a baseline for later runs cut from the same commit.
func storeCachedBaseline(projectDir string, b *verify.Baseline) error {
	if strings.TrimSpace(projectDir) == "" || b == nil || b.Incomplete() {
		return nil
	}
	path := baselineCachePath(projectDir, b.Commit)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	// Write-then-rename so a concurrent reader never sees a partial file.
	tmp := path + ".tmp"
	if werr := os.WriteFile(tmp, raw, 0o644); werr != nil {
		return werr
	}
	return os.Rename(tmp, path)
}

// startBaselineCapture kicks off a background capture for ref, unless one is already running or a
// cached result exists.
func (r *TaskReconciler) startBaselineCapture(projectDir, ref string) {
	if strings.TrimSpace(projectDir) == "" || strings.TrimSpace(ref) == "" {
		return
	}
	if loadCachedBaseline(projectDir, ref) != nil {
		return // already measured for this commit
	}
	// THE GATE IS ASKED OF THE REF, NOT OF THE OPERATOR'S CHECKOUT.
	if !r.refDeclaresGate(context.Background(), projectDir, ref) {
		return // nothing declared at the base commit; the differential says so, and it is the honest answer
	}
	key := projectDir + "\x00" + ref
	if _, loaded := baselineInFlight.LoadOrStore(key, struct{}{}); loaded {
		return // another dispatch is already measuring this commit
	}
	go func() {
		defer baselineInFlight.Delete(key)
		// A DETACHED CONTEXT, deliberately: the capture must outlive the dispatch call that started it.
		ctx, cancel := context.WithTimeout(context.Background(), baselineCaptureTimeout)
		defer cancel()
		b := r.captureBaseline(ctx, projectDir, ref, "")
		if b == nil || b.Incomplete() {
			r.log.Info("verify: baseline not captured (the differential will abstain)",
				"project_dir", projectDir, "ref", ref)
			return
		}
		if err := storeCachedBaseline(projectDir, b); err != nil {
			r.log.Info("verify: baseline captured but not cached", "ref", ref, "error", err.Error())
			return
		}
		r.log.Info("verify: baseline captured", "ref", ref, "checks", len(b.Results))
	}()
}

// captureBaselineForDispatch is the dispatch-path entry point.
func (r *TaskReconciler) captureBaselineForDispatch(ctx context.Context, projectDir, worktreePath, worktreeBranch string) {
	if strings.TrimSpace(projectDir) == "" {
		return
	}
	ref := ""
	if strings.TrimSpace(worktreePath) != "" {
		// The commit the step's work sits on top of.
		if out, err := runGit(ctx, worktreePath, "merge-base", "HEAD", defaultIntegrationBranch); err == nil {
			ref = strings.TrimSpace(out)
		}
		if ref == "" {
			// No merge-base (a detached or orphan branch): measure the current HEAD, which is still
			// the state before the worker runs.
			if out, err := runGit(ctx, worktreePath, "rev-parse", "HEAD"); err == nil {
				ref = strings.TrimSpace(out)
			}
		}
	}
	if ref == "" {
		// No worktree: fall back to the integration branch's tip in the project dir.
		branch := defaultIntegrationBranch
		if strings.TrimSpace(worktreeBranch) != "" {
			branch = worktreeBranch
		}
		if out, err := runGit(ctx, projectDir, "rev-parse", branch); err == nil {
			ref = strings.TrimSpace(out)
		}
	}
	if ref == "" {
		r.log.Info("verify: no base ref resolvable — baseline not scheduled", "project_dir", projectDir)
		return
	}
	r.startBaselineCapture(projectDir, ref)
}

// baselineForEvidence returns the cached baseline for a step's base ref, if one was captured.
func (r *TaskReconciler) baselineForEvidence(ctx context.Context, projectDir, worktreePath string) *verify.Baseline {
	if strings.TrimSpace(projectDir) == "" {
		return nil
	}
	ref := ""
	if strings.TrimSpace(worktreePath) != "" {
		if out, err := runGit(ctx, worktreePath, "merge-base", "HEAD", defaultIntegrationBranch); err == nil {
			ref = strings.TrimSpace(out)
		}
	}
	if ref == "" {
		if out, err := runGit(ctx, projectDir, "rev-parse", defaultIntegrationBranch); err == nil {
			ref = strings.TrimSpace(out)
		}
	}
	if ref == "" {
		return nil
	}
	return loadCachedBaseline(projectDir, ref)
}

// describeBaselineCache is a small diagnostic helper used by the log line above and by tests.
func describeBaselineCache(projectDir string) string {
	dir := filepath.Join(projectDir, ".orchicon", baselineCacheDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Sprintf("%s (unreadable: %v)", dir, err)
	}
	return fmt.Sprintf("%s (%d cached)", dir, len(entries))
}

// refDeclaresGate reports whether the commit at ref declares a gate.
func (r *TaskReconciler) refDeclaresGate(ctx context.Context, projectDir, ref string) bool {
	out, err := runGit(ctx, projectDir, "show", ref+":.orchicon/verify")
	if err != nil {
		return false
	}
	d, perr := verify.ParseDeclaration(out)
	if perr != nil {
		// A malformed declaration at the base commit is still A DECLARATION: let the capture run and report the
		// parse error in the baseline's detail, rather than silently treating a typo as "no gate".
		return true
	}
	return d.Declared()
}
