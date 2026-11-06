package scheduler

// verify_evidence.go — GATHERING the facts that internal/verify decides on.
//
// internal/verify is deliberately pure: it takes an Evidence struct and returns a verdict, so its
// rules are testable without a repository. This file is the other half — it reads the repository and
// the project's declared gate and fills that struct in. Split this way because the two failure modes
// are different: a rule bug makes the verdict WRONG, a collector bug makes it ABSTAIN. Keeping them
// apart means a test of one cannot accidentally mask the other.
//
// IT NEVER RUNS THE PROJECT'S GATE AT THE BASE COMMIT HERE. Baseline capture happens BEFORE the step
// runs (see CaptureBaseline) and is recorded on the step run; this file only re-measures at the
// step's HEAD and compares. That ordering is what makes the comparison meaningful: measuring the
// base AFTER the worker has run would measure the worker's own tree.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/verify"
)

// maxGateOutput bounds what we keep from a gate run for the record. The verdict needs the RESULT of
// each check, not the whole transcript; a runaway suite would otherwise write megabytes into a row.
const maxGateOutput = 64 * 1024

// completionGateTimeout bounds the gate run on the COMPLETION path, and it is deliberately much shorter
// than verifyCommandTimeout (10m).
//
// THE CONSTRAINTS ARE DIFFERENT AND THE DIFFERENCE MATTERS. local_integrate runs on its own queue with
// its own budget, where waiting ten minutes for a thorough check is the whole point. This one runs
// SYNCHRONOUSLY inside OnResult — the execution is still formally `running`, the operator's UI shows a
// worker that has visibly stopped, and every later reconcile pass for this row is behind it. A 10-minute
// wait there is not patience, it is a wedge, and a wedged completion is far worse than an unmeasured
// gate: a timeout ABSTAINS (the step lands `unverified`, which is honest) whereas a wedge strands the
// run.
//
// The budget is generous enough for a warm-cache suite on a real repository (this one turns its whole Go
// suite around in ~17s warm) and short enough that a pathological gate costs minutes, not a run.
const completionGateTimeout = 3 * time.Minute

// declaredGateCommand returns the project's declared verification command, or "".
//
// It reuses projectVerifyCommand — the SAME reader the local-integration path uses — so a project
// cannot end up with two different notions of "its gate" depending on which code path asked. The
// comment there explains why this is a file rather than a setting; the short version is that a
// command the platform invents would be worse than none, and a schema field for something most
// projects will not set is a schema field nobody maintains.
func declaredGateCommand(projectDir string) string {
	if strings.TrimSpace(projectDir) == "" {
		return ""
	}
	return projectVerifyCommand(projectDir)
}

// captureBaseline runs the project's gate at ref and records the per-check results.
//
// A BASELINE IS BEST-EFFORT AND ITS FAILURE IS NOT AN ERROR. If it cannot be taken (no gate, git
// unreadable, the tree is not checked out where we can reach it) the returned Baseline is nil and the
// differential abstains — which is the honest outcome, and lands the step in "unverified" rather than
// failing it. Failing a step because the PLATFORM could not measure something would be blaming the
// worker for an infrastructure problem.
func (r *TaskReconciler) captureBaseline(ctx context.Context, projectDir, ref, worktreePath string) *verify.Baseline {
	cmd := declaredGateCommand(projectDir)
	if cmd == "" {
		return nil
	}
	// The baseline runs in a scratch worktree of the base ref so it cannot disturb the run's own tree
	// or the operator's checkout. When that is not possible we abstain rather than running the gate in
	// a place whose state we do not control.
	dir, cleanup, err := r.scratchCheckout(ctx, projectDir, ref)
	if err != nil {
		r.log.Info("verify: no baseline (scratch checkout unavailable)",
			"project_dir", projectDir, "ref", ref, "error", err.Error())
		return nil
	}
	defer cleanup()

	out, runErr := r.runGateCommand(ctx, dir, cmd)
	results := parseGateResults(out)
	if len(results) == 0 {
		// Nothing parseable. Record why, so the differential says "the baseline is incomplete" and
		// names it rather than reporting an absence of failures.
		detail := "the baseline gate produced no parseable results"
		if runErr != nil {
			detail += ": " + truncateTail(strings.TrimSpace(out), 400)
		}
		return &verify.Baseline{Commit: ref, Detail: detail}
	}
	b := &verify.Baseline{Commit: ref, Results: results}
	if runErr != nil {
		// A non-zero exit with PARSEABLE results is expected: the gate ran and some checks failed.
		// That is a perfectly good baseline — it is how we learn what was ALREADY broken.
		return b
	}
	return b
}

// scratchCheckout materialises ref in a throwaway worktree under the project's own worktree dir.
//
// It returns the directory and a cleanup func. Failure is returned, never fatal: the caller abstains.
func (r *TaskReconciler) scratchCheckout(ctx context.Context, projectDir, ref string) (string, func(), error) {
	if strings.TrimSpace(ref) == "" {
		return "", func() {}, fmt.Errorf("no ref to check out")
	}
	base := filepath.Join(projectDir, worktreeDirName)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", func() {}, fmt.Errorf("create worktree base: %w", err)
	}
	dir, err := os.MkdirTemp(base, "_verify-baseline-")
	if err != nil {
		return "", func() {}, fmt.Errorf("create scratch dir: %w", err)
	}
	// The dir must NOT exist for `git worktree add` to create it in place.
	_ = os.RemoveAll(dir)
	if _, err := runGit(ctx, projectDir, "worktree", "add", "--detach", dir, ref); err != nil {
		return "", func() {}, fmt.Errorf("worktree add %s: %w", ref, err)
	}
	cleanup := func() {
		if _, err := runGit(ctx, projectDir, "worktree", "remove", "--force", dir); err != nil {
			r.log.Info("verify: scratch checkout cleanup failed (will be swept)", "dir", dir, "error", err.Error())
			_ = os.RemoveAll(dir)
		}
		_, _ = runGit(ctx, projectDir, "worktree", "prune")
	}
	return dir, cleanup, nil
}

// runGateCommand executes the gate with the shared bounded runner and returns combined output.
//
// It reports the error so the caller can distinguish "the gate failed" (a useful baseline) from "the
// gate could not run" (an abstention) — a distinction the previous integration-only check could not
// express.
func (r *TaskReconciler) runGateCommand(ctx context.Context, dir, command string) (string, error) {
	out, err := runVerifyCommand(ctx, dir, command)
	if len(out) > maxGateOutput {
		out = out[:maxGateOutput]
	}
	return out, err
}

// runCompletionGate runs the project's gate on the COMPLETION path under completionGateTimeout.
//
// A TIMEOUT IS AN ABSTENTION, NOT A GATE FAILURE. If the check cannot finish inside the budget the
// evidence says "not measured", which the decision function turns into `unverified` — never into a
// failure, because the worker is not responsible for the gate being slow, and never into a pass, because
// nothing was established.
func (r *TaskReconciler) runCompletionGate(ctx context.Context, dir, command string) (string, error) {
	gctx, cancel := context.WithTimeout(ctx, completionGateTimeout)
	defer cancel()
	done := make(chan struct{})
	var out string
	var err error
	go func() {
		defer close(done)
		out, err = runVerifyCommand(gctx, dir, command)
	}()
	select {
	case <-done:
	case <-gctx.Done():
		return "", fmt.Errorf("the project's gate did not finish within %s — not measured", completionGateTimeout)
	}
	<-done
	if len(out) > maxGateOutput {
		out = out[:maxGateOutput]
	}
	return out, err
}

// parseGateResults turns a test-runner transcript into a per-check result map.
//
// WHY A PARSER AND NOT JUST THE EXIT CODE. The exit code answers "is everything green", which is
// unanswerable on a repository with any pre-existing breakage — and every real repository has some.
// Per-check results let the comparison be a SET difference, so a step is measured only on what IT
// changed. It also means a project whose gate is already red can still have its steps verified, which
// matters: the alternative is that any project with one broken test can never certify anything, and
// the mechanism gets switched off.
//
// THE LINE FORMATS ARE THE COMMON ONES, not a Go-only contract:
//
//	go test        "ok  \tpkg", "FAIL\tpkg", "--- FAIL: TestX"
//	make/npm/any   anything the project's own command prints, plus a final exit status
//
// When nothing is parseable the caller records an incomplete baseline rather than an empty (i.e.
// "everything passed") one — an empty map must never be read as success.
func parseGateResults(out string) map[string]bool {
	results := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, passing, ok := parseResultLine(line)
		if !ok {
			continue
		}
		// A subject that appears twice (package + its tests) fails if ANY line fails.
		if prev, seen := results[key]; seen {
			results[key] = prev && passing
			continue
		}
		results[key] = passing
	}
	return results
}

// parseResultLine recognises the result lines this platform's projects produce.
func parseResultLine(line string) (key string, passing bool, ok bool) {
	lower := strings.ToLower(line)
	switch {
	case strings.HasPrefix(line, "--- FAIL:"):
		name := strings.TrimSpace(strings.TrimPrefix(line, "--- FAIL:"))
		return firstField(name), false, name != ""
	case strings.HasPrefix(line, "--- PASS:"):
		name := strings.TrimSpace(strings.TrimPrefix(line, "--- PASS:"))
		return firstField(name), true, name != ""
	case strings.HasPrefix(lower, "ok ") || strings.HasPrefix(lower, "ok\t"):
		f := strings.Fields(line)
		if len(f) >= 2 {
			return f[1], true, true
		}
	case strings.HasPrefix(line, "FAIL") || strings.HasPrefix(line, "FAIL\t"):
		f := strings.Fields(line)
		if len(f) >= 2 {
			return f[1], false, true
		}
	case strings.HasPrefix(line, "PASS") || strings.HasPrefix(line, "PASS\t"):
		// A bare "PASS" with no subject is go test's final summary line — no identity, so no result.
		f := strings.Fields(line)
		if len(f) >= 2 {
			return f[1], true, true
		}
	}
	// Runner-prefixed per-file lines: " FAIL  src/foo.test.ts > case" / " PASS  src/bar.test.ts".
	// Checked last so the exact-prefix cases above win, and matched case-insensitively because these
	// runners vary (vitest "FAIL", jest "FAIL", mocha "failing").
	if k, p, ok := parseRunnerPrefixed(lower, line); ok {
		return k, p, true
	}
	if strings.Contains(line, ": FAILED") || strings.HasSuffix(line, " FAILED") {
		f := strings.Fields(line)
		for i, tok := range f {
			if strings.EqualFold(tok, "FAIL") && i+1 < len(f) {
				return f[i+1], false, true
			}
		}
	}
	return "", false, false
}

// parseRunnerPrefixed handles the " STATUS <subject>" family (vitest/jest/mocha style), where the
// status word is padded with whitespace before the subject on the same line.
func parseRunnerPrefixed(lower, line string) (string, bool, bool) {
	f := strings.Fields(line)
	if len(f) < 2 {
		return "", false, false
	}
	switch strings.ToUpper(f[0]) {
	case "FAIL", "FAILED", "✗", "✕", "×":
		return f[1], false, true
	case "PASS", "PASSED", "✓", "√", "✔":
		return f[1], true, true
	}
	return "", false, false
}

// firstField returns the first whitespace-delimited field, or "".
func firstField(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// gatherEvidence assembles the verify.Evidence for a completed step.
//
// EVERY FIELD IS BEST-EFFORT AND EVERY FAILURE ABSTAINS. The only way this function may contribute a
// FAILURE is by observing a contradiction (HEAD did not move, a claimed file is absent, the tree is
// dirty, a check regressed); anything it cannot observe stays unmeasured, which the decision function
// turns into "unverified" rather than "broken".
func (r *TaskReconciler) gatherEvidence(ctx context.Context, in evidenceInput) verify.Evidence {
	ev := verify.Evidence{
		StepDeclaresDeliverable: in.stepDeclaresDeliverable,
		SkipReason:              in.skipReason,
		ClaimedFiles:            in.claimedFiles,
	}
	if strings.TrimSpace(in.worktreePath) == "" {
		ev.GitError = "this execution had no isolated worktree, so there is no repository state to check"
		return ev
	}

	// Base ref: what the step's work was cut from. Prefer the recorded branch point; fall back to the
	// merge-base with the integration branch, which is the same thing computed rather than stored.
	base := strings.TrimSpace(in.baseRef)
	if base == "" {
		if mb, err := runGit(ctx, in.worktreePath, "merge-base", "HEAD", in.integrationBranch); err == nil {
			base = strings.TrimSpace(mb)
		}
	}
	ev.BaseRef = base

	head, err := runGit(ctx, in.worktreePath, "rev-parse", "HEAD")
	if err != nil {
		ev.GitError = "the worktree's HEAD could not be read: " + err.Error()
		return ev
	}
	ev.HeadRef = strings.TrimSpace(head)
	ev.GitReadable = true

	// Commits ahead of the base — the "did any work happen" measurement.
	ev.CommitsAhead = -1
	if base != "" {
		if n, nerr := runGit(ctx, in.worktreePath, "rev-list", "--count", base+"..HEAD"); nerr == nil {
			var count int
			if _, serr := fmt.Sscanf(strings.TrimSpace(n), "%d", &count); serr == nil {
				ev.CommitsAhead = count
			}
		}
		// Changed files between the base and HEAD, as a SET.
		if out, gerr := runGit(ctx, in.worktreePath, "diff", "--name-only", base+"..HEAD"); gerr == nil {
			ev.ChangedFiles = pathSet(out)
		}
	}

	// Tree state. Only meaningful when a deliverable was expected; the decision function gates it.
	if out, gerr := runGit(ctx, in.worktreePath, "status", "--porcelain"); gerr == nil {
		clean := strings.TrimSpace(out) == ""
		ev.TreeClean = &clean
		if !clean {
			ev.DirtySummary = summariseDirty(out)
		}
	}

	// The project's declared gate, re-measured at HEAD.
	ev.DeclaredCommand = declaredGateCommand(in.projectDir)
	if ev.DeclaredCommand != "" {
		out, gerr := r.runCompletionGate(ctx, in.worktreePath, ev.DeclaredCommand)
		ev.DeclaredRan = true
		ev.DeclaredPassed = gerr == nil
		ev.DeclaredDetail = truncateTail(strings.TrimSpace(out), 600)
		// The HEAD result set feeds the differential.
		if res := parseGateResults(out); len(res) > 0 {
			ev.HeadResults = res
			ev.HeadMeasured = true
		} else {
			ev.HeadDetail = "the gate produced no parseable results"
			if gerr != nil {
				ev.HeadDetail += ": " + ev.DeclaredDetail
			}
		}
	}

	ev.Baseline = in.baseline
	return ev
}

// evidenceInput is the caller-supplied context for gathering evidence.
type evidenceInput struct {
	projectDir              string
	worktreePath            string
	baseRef                 string
	integrationBranch       string
	baseline                *verify.Baseline
	claimedFiles            map[string]struct{}
	stepDeclaresDeliverable bool
	skipReason              string
}

// pathSet splits newline-separated `git diff --name-only` output into a set.
func pathSet(out string) map[string]struct{} {
	set := map[string]struct{}{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		set[line] = struct{}{}
	}
	return set
}

// summariseDirty renders `git status --porcelain` as a short, single-line summary.
//
// It names up to four entries because the operator needs to know WHAT is uncommitted to judge whether
// it matters — "the tree is dirty" alone sends them back to a terminal.
func summariseDirty(out string) string {
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// porcelain v1: "XY path" — keep the status letters and the path.
		paths = append(paths, line)
		if len(paths) == 4 {
			break
		}
	}
	joined := strings.Join(paths, ", ")
	if strings.Count(strings.TrimSpace(out), "\n")+1 > len(paths) {
		joined += " …"
	}
	return joined
}

// errNoWorktree marks the "there is nothing to verify against" case distinctly from a read failure, so
// the abstention detail can be specific.
var errNoWorktree = errors.New("no isolated worktree for this step")
