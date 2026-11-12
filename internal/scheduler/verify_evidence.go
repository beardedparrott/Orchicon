package scheduler

// verify_evidence.go — GATHERING the facts that internal/verify decides on. IT NEVER RUNS THE
// PROJECT'S GATE AT THE BASE COMMIT HERE.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/beardedparrott/orchicon/internal/verify"
)

// maxGateOutput bounds what we keep from a gate run for the record. The verdict needs the RESULT of
// each check, not the whole transcript; a runaway suite would otherwise write megabytes into a row.
const maxGateOutput = 64 * 1024

// declaredGate reads the project's full gate declaration (command, result format, optional report
// path).
func declaredGate(projectDir string) (verify.Declaration, error) {
	if strings.TrimSpace(projectDir) == "" {
		return verify.Declaration{}, nil
	}
	raw, err := os.ReadFile(filepath.Join(projectDir, ".orchicon", "verify"))
	if err != nil {
		return verify.Declaration{}, nil // absent is legitimate: the project declares no gate
	}
	d, perr := verify.ParseDeclaration(string(raw))
	if perr != nil {
		// A MALFORMED DECLARATION IS AN ERROR, NOT AN ABSENCE.
		return verify.Declaration{}, fmt.Errorf("%s: %w", filepath.Join(".orchicon", "verify"), perr)
	}
	return d, nil
}

// gateDeclarationDir picks the directory a gate declaration should be READ from.
func gateDeclarationDir(worktreePath, projectDir string) string {
	if d := strings.TrimSpace(worktreePath); d != "" {
		return d
	}
	return projectDir
}

// captureBaseline runs the project's gate at ref and records the per-check results.
func (r *TaskReconciler) captureBaseline(ctx context.Context, projectDir, ref, worktreePath string) *verify.Baseline {
	// THE BASELINE IS MEASURED AGAINST THE BASE COMMIT'S OWN DECLARATION, which is why the scratch
	// checkout is made FIRST and the declaration is read from inside it.
	dir, cleanup, err := r.scratchCheckout(ctx, projectDir, ref)
	if err != nil {
		r.log.Info("verify: no baseline (scratch checkout unavailable)",
			"project_dir", projectDir, "ref", ref, "error", err.Error())
		return &verify.Baseline{Commit: ref, Detail: "no isolated checkout of " + short(ref) +
			" could be created, so the base commit was not measured: " + err.Error()}
	}
	defer cleanup()

	decl, declErr := declaredGate(dir)
	if declErr != nil {
		// Named, not swallowed: the operator has to know their declaration is broken.
		return &verify.Baseline{Commit: ref, Detail: "the gate declaration could not be read at " +
			short(ref) + ": " + declErr.Error()}
	}
	if !decl.Declared() {
		// A GATE THAT DID NOT EXIST YET IS A REAL ANSWER, not a failure to measure.
		return &verify.Baseline{Commit: ref, Detail: "the base commit declares no gate, so there is nothing to " +
			"compare against"}
	}

	// THE DEPENDENCIES COME WITH IT. Without this the gate fails for a reason that has nothing to do with the base
	// commit's correctness, and the failure would be recorded AS the baseline.
	if lerr := linkIgnoredDependencies(ctx, projectDir, dir); lerr != nil {
		r.log.Info("verify: baseline dependency linking was incomplete",
			"ref", ref, "error", lerr.Error())
	}

	out, runErr := r.runGateCommand(ctx, dir, decl)

	// AN INTERRUPTED GATE IS NOT A BASELINE.
	if interrupted(ctx, runErr) {
		detail := "the gate was interrupted before it finished, so no reliable baseline exists"
		if runErr != nil {
			detail += ": " + runErr.Error()
		}
		return &verify.Baseline{Commit: ref, Detail: detail}
	}

	results, perr := r.parseGateOutput(decl, dir, out)
	if perr != nil {
		return &verify.Baseline{Commit: ref, Detail: perr.Error()}
	}
	if len(results) == 0 {
		return &verify.Baseline{Commit: ref, Detail: "the baseline gate reported no per-check results"}
	}
	// A non-zero exit WITH parseable results and NO interruption is the expected benign case: the gate
	// ran to completion and some checks failed.
	return &verify.Baseline{Commit: ref, Results: results}
}

// linkIgnoredDependencies makes the scratch checkout able to RUN the project's gate. ONLY IGNORED
// PATHS.
func linkIgnoredDependencies(ctx context.Context, projectDir, scratchDir string) error {
	out, err := runGit(ctx, projectDir, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory")
	if err != nil {
		return err
	}
	var firstErr error
	linked := 0
	for _, line := range strings.Split(out, "\n") {
		entry := strings.TrimSuffix(strings.TrimSpace(line), "/")
		if entry == "" {
			continue
		}
		src := filepath.Join(projectDir, entry)
		info, serr := os.Stat(src)
		if serr != nil || !info.IsDir() {
			continue // a single ignored FILE (a lockfile, a cache file) is not a dependency tree
		}
		dst := filepath.Join(scratchDir, entry)
		if _, derr := os.Lstat(dst); derr == nil {
			continue // the scratch tree already has this path
		}
		// The parent must exist in the scratch tree before the link can be made.
		if _, perr := os.Stat(filepath.Dir(dst)); perr != nil {
			continue
		}
		if lerr := os.Symlink(src, dst); lerr != nil {
			// Non-fatal per entry: one unlinkable cache must not cost the whole baseline. The first failure is
			// reported so a systematic problem (a filesystem without symlinks) is visible.
			if firstErr == nil {
				firstErr = fmt.Errorf("link %s: %w", entry, lerr)
			}
			continue
		}
		linked++
	}
	if linked == 0 && firstErr == nil {
		return fmt.Errorf("no ignored dependency directories were linked; a gate that needs them will fail in " +
			"the scratch checkout for a reason unrelated to the base commit")
	}
	return firstErr
}

// interrupted reports whether a gate run was cut short rather than completing (possibly with
// failures).
func interrupted(ctx context.Context, runErr error) bool {
	if ctx.Err() != nil {
		return true
	}
	if runErr == nil {
		return false
	}
	// runVerifyCommand reports its own budget expiry with this sentinel wording; match it so a timeout
	// inside the runner (which may not surface as ctx.Err here) is still caught.
	return strings.Contains(runErr.Error(), "timed out after")
}

// scratchCheckout materialises ref in a throwaway worktree under the project's own worktree dir.
func (r *TaskReconciler) scratchCheckout(ctx context.Context, projectDir, ref string) (string, func(), error) {
	if strings.TrimSpace(ref) == "" {
		return "", func() {}, fmt.Errorf("no ref to check out")
	}
	base := filepath.Join(projectDir, worktreeDirName)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", func() {}, fmt.Errorf("create worktree base: %w", err)
	}
	dir, err := os.MkdirTemp(base, scratchBaselinePrefix)
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
			r.log.Info("verify: scratch checkout cleanup failed; the orphan sweep reclaims it once stale",
				"dir", dir, "error", err.Error())
			_ = os.RemoveAll(dir)
		}
		_, _ = runGit(ctx, projectDir, "worktree", "prune")
	}
	return dir, cleanup, nil
}

// runGateCommand executes the gate with the shared bounded runner and returns combined output.
func (r *TaskReconciler) runGateCommand(ctx context.Context, dir string, decl verify.Declaration) (string, error) {
	out, err := runVerifyCommand(ctx, dir, decl.Command)
	return capGateOutput(out), err
}

// capGateOutput bounds a gate transcript WITHOUT LOSING RESULT LINES.
//
// THIS FUNCTION EXISTS BECAUSE A FRONT CUT SILENTLY BREAKS THE DIFFERENTIAL. The previous code did
// `out = out[:maxGateOutput]`, which keeps the FIRST 64KB — and a `go test ./internal/... ./cmd/...` run
// on this repository emits far more than that before it reaches the packages late in the alphabet. The
// result was measured, not theorised, on `make rebuild-dev`:
//
//	the verify package is missing from the measured baseline
//	(baseline ended at internal/runtimeimage — everything after it was cut)
//
// The consequence is not "some checks are missing" but the two failures this whole mechanism exists to
// prevent, BOTH AT ONCE:
//   - a package that was already failing but got truncated out of the BASELINE re-appears at HEAD, so a
//     pre-existing failure is blamed on the step;
//   - a package that regressed but got truncated out of HEAD is never compared, so a new failure is
//     reported as no new failures. A false NEGATIVE in a verification system is the worst outcome there
//     is, and a truncation that eats the tail produces it silently.
//
// So the budget keeps the HEAD *and* the TAIL of the transcript plus every result-shaped line from the
// middle. Result lines are short and bounded by the number of packages, so they fit comfortably; it is
// the noise between them (build chatter, failure dumps, warnings) that is expendable.
func capGateOutput(out string) string {
	if len(out) <= maxGateOutput {
		return out
	}
	// HEAD AND TAIL, and NOTHING is filtered out of the middle any more.
	const edge = maxGateOutput / 4
	head := safeCut(out, edge)
	tail := truncateTail(out, edge)
	return head + "\n... [gate output truncated; the tail follows. The report was parsed in full.] ...\n" + tail
}

// safeCut returns the first n bytes of s, backing off to a UTF-8 boundary so a multi-byte character is
// never split (the transcript carries emoji and box-drawing characters).
func safeCut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// runCompletionGate runs the project's gate on the COMPLETION path.
func (r *TaskReconciler) runCompletionGate(ctx context.Context, dir string, decl verify.Declaration) (full, capped string, err error) {
	out, err := runVerifyCommand(ctx, dir, decl.Command)
	return out, capGateOutput(out), err
}

// parseGateOutput reads a gate's per-check results using the project's DECLARED format.
func (r *TaskReconciler) parseGateOutput(decl verify.Declaration, dir, out string) (map[string]bool, error) {
	if !decl.Declared() {
		return nil, fmt.Errorf("the project declares no gate")
	}
	if !decl.Format.ParsesResults() {
		return nil, fmt.Errorf("the project declares no machine-readable result format, so its gate's per-check "+
			"results are not available (add `format = junit|tap|sarif|json` to .orchicon/verify)")
	}
	body := out
	if p := strings.TrimSpace(decl.Path); p != "" {
		raw, rerr := os.ReadFile(filepath.Join(dir, p))
		if rerr != nil {
			return nil, fmt.Errorf("the gate's declared report %q could not be read: %w", p, rerr)
		}
		body = string(raw)
	}
	results, perr := verify.ParseResults(decl.Format, body)
	if perr != nil {
		return nil, fmt.Errorf("the gate's output is not %s: %w", decl.Format, perr)
	}
	return results, nil
}

// gatherEvidence assembles the verify.Evidence for a completed step.
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
	decl, declErr := declaredGate(gateDeclarationDir(in.worktreePath, in.projectDir))
	ev.DeclaredCommand = decl.Command
	if declErr != nil {
		// A BROKEN DECLARATION IS REPORTED, and it is an abstention rather than a failure: the worker did
		// not write the declaration.
		ev.DeclaredDetail = "the gate declaration could not be read: " + declErr.Error()
		ev.HeadDetail = ev.DeclaredDetail
	}
	if ev.DeclaredCommand != "" && declErr == nil && in.stepDeclaresDeliverable {
		full, capped, gerr := r.runCompletionGate(ctx, in.worktreePath, decl)
		ev.DeclaredRan = true
		ev.DeclaredPassed = gerr == nil
		ev.DeclaredDetail = truncateTail(strings.TrimSpace(capped), 600)
		// The HEAD result set feeds the differential — parsed from the FULL transcript, before the cap, so the
		// record's size limit can never cost a measurement.
		res, perr := r.parseGateOutput(decl, in.worktreePath, full)
		if perr == nil && len(res) > 0 {
			ev.HeadResults = res
			ev.HeadMeasured = true
		} else {
			ev.HeadDetail = "the gate's per-check results were not available"
			if perr != nil {
				ev.HeadDetail = perr.Error()
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
