package scheduler

// verify_baseline.go — CAPTURE THE BASELINE BEFORE THE WORK, CACHE IT PER COMMIT.
//
// THE ORDERING IS THE WHOLE POINT. "Is this test failure pre-existing?" is only answerable by measuring
// the gate at the commit the work was cut from — BEFORE the worker touches anything. Measured
// afterwards, it measures the worker's own tree and the question becomes circular. This is why the
// capture is kicked off at DISPATCH, not at completion.
//
// IT CANNOT BLOCK DISPATCH. A project's gate can take many minutes (this repository's own go-ci job
// runs ~13 minutes), and holding a dispatch behind it would delay every run in the queue. So the
// capture runs in the background and writes a cache file keyed by commit SHA. The completion path
// reads that cache; if the capture has not finished (or never started), the differential ABSTAINS —
// which lands the step in `unverified`, which is honest, rather than guessing.
//
// CACHED PER COMMIT because the answer for a commit never changes. The first run cut from a given
// develop tip pays for the capture; every sibling run cut from the same tip reuses it. On a busy
// repository that turns an N× cost into roughly 1× per commit.
//
// FAILURE IS SILENT-AND-VISIBLE: a failed capture leaves no cache entry, the differential abstains,
// and one Info line records why. It never fails a dispatch and never fails a step.

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

// baselineInFlight is the single-flight guard: one capture per (project, commit) across the process.
// Without it, ten runs dispatched from the same tip would each spawn a capture of the whole gate — the
// exact thundering herd that makes people disable this kind of measurement.
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
//
// A CORRUPT FILE IS TREATED AS ABSENT, never as "everything passed". The distinction matters: a
// half-written JSON file that decoded to an empty result set would read as a clean baseline and make
// every subsequent failure look like a regression.
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
//
// It returns immediately. The caller is a dispatch path and must not wait for a gate to run.
func (r *TaskReconciler) startBaselineCapture(projectDir, ref string) {
	if strings.TrimSpace(projectDir) == "" || strings.TrimSpace(ref) == "" {
		return
	}
	if loadCachedBaseline(projectDir, ref) != nil {
		return // already measured for this commit
	}
	if declaredGateCommand(projectDir) == "" {
		return // nothing declared; the differential will abstain, which is correct
	}
	key := projectDir + "\x00" + ref
	if _, loaded := baselineInFlight.LoadOrStore(key, struct{}{}); loaded {
		return // another dispatch is already measuring this commit
	}
	go func() {
		defer baselineInFlight.Delete(key)
		// A DETACHED CONTEXT, deliberately: the capture must outlive the dispatch call that started
		// it. Deriving from the request context would cancel the capture the moment dispatch returns,
		// which is exactly the wrong lifetime. Bounded by its own timeout so it cannot run forever.
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

// captureBaselineForDispatch is the dispatch-path entry point. It records the BASE REF on the step run
// so the completion path knows what to compare against, and starts the capture.
//
// The base ref is the run branch's own start point: for a git-backed run the worktree was cut from the
// integration branch, so the merge-base of the worktree with that branch is the honest answer.
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
//
// This is the read side used by the completion path. It looks up the base ref the same way the capture
// did, so the two agree; a miss abstains.
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
