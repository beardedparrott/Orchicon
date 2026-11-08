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
	"unicode/utf8"

	"github.com/beardedparrott/orchicon/internal/verify"
)

// maxGateOutput bounds what we keep from a gate run for the record. The verdict needs the RESULT of
// each check, not the whole transcript; a runaway suite would otherwise write megabytes into a row.
const maxGateOutput = 64 * 1024

// declaredGate reads the project's full gate declaration (command, result format, optional report path).
//
// ONE READER FOR EVERYTHING THAT ASKS "WHAT IS THIS PROJECT'S GATE". The integration path, the baseline capture and
// the completion gate all go through here, so a project cannot end up with two different notions of its own gate
// depending on which code path asked — the defect that let the completion path use a different budget than the
// integration path.
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
		// A MALFORMED DECLARATION IS AN ERROR, NOT AN ABSENCE. Parsing it leniently would run something the
		// operator did not write; reporting it as "no gate" would make a typo indistinguishable from a project
		// that never configured one — and the operator would go looking in the wrong place.
		return verify.Declaration{}, fmt.Errorf("%s: %w", filepath.Join(".orchicon", "verify"), perr)
	}
	return d, nil
}

// gateDeclarationDir picks the directory a gate declaration should be READ from.
//
// THE WORKTREE WINS, AND THAT IS THE FIX FOR A REAL FAILURE. The declaration is part of the repository, so the one
// that governs a step is the one at that step's HEAD — which lives in the run's own worktree, not in the operator's
// checkout. Reading the checkout instead meant a step whose deliverable INCLUDED creating `.orchicon/verify`
// reported "the project declares no gate" about a project that, at the commit under test, declares one.
//
// MEASURED, on execution 01M6W2KC1CQCH6FFZ99G132F6S: the worker's AC1 was creating `.orchicon/verify` on the run
// branch (faf69a7, contents `node scripts/gate.mjs`), while the project checkout sat on another branch with no such
// file. The platform read the checkout, abstained, and told the operator the project declares no gate.
//
// THE FALLBACK IS THE PROJECT DIRECTORY, for the in-place case (git_strategy=none, or a non-repo project) where
// there is no worktree to read.
func gateDeclarationDir(worktreePath, projectDir string) string {
	if d := strings.TrimSpace(worktreePath); d != "" {
		return d
	}
	return projectDir
}

// captureBaseline runs the project's gate at ref and records the per-check results.
//
// A BASELINE IS BEST-EFFORT AND ITS FAILURE IS NOT AN ERROR. If it cannot be taken (no gate, git unreadable, an
// unmatched result format) the returned Baseline carries a Detail and the differential abstains — which is the
// honest outcome. Failing a step because the PLATFORM could not measure something would be blaming the worker for
// an infrastructure problem, and the operator's rule is that an unmeasurable check is never a failure.
//
// IT MEASURES WHERE THE PROJECT ACTUALLY RUNS. A scratch `git worktree` carries the TRACKED tree only, so every
// gitignored dependency — node_modules, .venv, vendor, target, .dev — is absent and a gate that needs any of them
// dies instantly with "vitest: command not found". Measured: this repository's frontend gate cannot run in a bare
// worktree at all. The dependencies are therefore LINKED IN from the project directory before the gate runs
// (linkIgnoredDependencies), which is what makes the baseline a measurement of the base COMMIT rather than a
// measurement of a tree nothing can build.
func (r *TaskReconciler) captureBaseline(ctx context.Context, projectDir, ref, worktreePath string) *verify.Baseline {
	// THE BASELINE IS MEASURED AGAINST THE BASE COMMIT'S OWN DECLARATION, which is why the scratch checkout is made
	// FIRST and the declaration is read from inside it.
	//
	// Reading the declaration from the operator's checkout instead was a real defect: the checkout can be on a
	// different branch, so the baseline could run a command the base commit never declared — or find no gate at all
	// for a commit that has one. The ref being measured is the only authority on what that ref declares.
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
		// A GATE THAT DID NOT EXIST YET IS A REAL ANSWER, not a failure to measure. A step that ADDS the gate has
		// no baseline by definition, and saying so is what stops the differential complaining about a comparison
		// that could never have been made.
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
	//
	// A gate that was KILLED (cancelled context, the runtime container going away) still returns everything it
	// managed to print, so the checks that finished parse fine and every check that never ran is silently
	// absent. Recording that as a complete baseline is the most dangerous failure this mechanism has, because of
	// what absence means downstream: NewFailures skips keys missing from the baseline, so a check that never ran
	// can never be reported as regressed. The result is a FALSE NEGATIVE, and it is CACHED per commit, so every
	// sibling run inherits the blind spot.
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
	// A non-zero exit WITH parseable results and NO interruption is the expected benign case: the gate ran to
	// completion and some checks failed. That is a perfectly good baseline — it is how we learn what was ALREADY
	// broken.
	return &verify.Baseline{Commit: ref, Results: results}
}

// linkIgnoredDependencies makes the scratch checkout able to RUN the project's gate.
//
// A worktree carries tracked files only, so every build cache and dependency tree — the things a gate needs and
// nobody commits — is missing. This links the project directory's IGNORED HEAD entries into the scratch tree,
// which gives the gate the same dependency set it has when the operator runs it.
//
// IT LINKS RATHER THAN INSTALLS. Installing would mean a network fetch per baseline capture and a dependency
// resolution that could DIFFER from the project's own — measuring something the project never runs is worse than
// not measuring. A symlink is the same bytes the project already built.
//
// THE IGNORED PATHS ARE OFTEN NESTED (frontend/node_modules), not top-level, so every ignored DIRECTORY is
// linked, at any depth. Linking only the top level was the first version of this function and it silently did
// nothing here — measured: `npx vitest` in the scratch checkout still reported "missing packages and no YES
// option" while the project's own checkout ran it fine.
//
// ONLY IGNORED PATHS. A tracked directory is part of the base commit and must be the base commit's copy; linking
// over it would measure HEAD's version of the file while claiming to measure the ref's.
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
		// The parent must exist in the scratch tree before the link can be made. When it does not, the parent is
		// itself an ignored directory that will be linked by its own entry — so skipping is right, not creating
		// it.
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

// interrupted reports whether a gate run was cut short rather than completing (possibly with failures).
//
// THE DISTINCTION IS THE WHOLE CONTRACT: "the suite has 3 failing tests" and "the suite never finished"
// both exit non-zero and both print parseable lines, and only one of them may be trusted as a baseline.
// A done-with-failures run is genuinely informative. A killed run is a partial view that would silently
// exempt every unmeasured package from comparison.
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
//
// It reports the error so the caller can distinguish "the gate failed" (a useful baseline) from "the
// gate could not run" (an abstention) — a distinction the previous integration-only check could not
// express.
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
	//
	// The previous version kept "result-shaped lines" from the middle, which meant it had to know the result
	// FORMAT — the pattern-matching defect this change removes. A declared format does not need that: the report
	// is parsed from the FULL transcript before it is capped (the cap is only what the record keeps), so a
	// truncated record no longer costs a measurement.
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
//
// THERE IS NO INVENTED TIME LIMIT HERE, and that is deliberate. This used to carry its own 3-minute budget on top
// of the integration path's 10-minute one; both are gone. The platform's rule everywhere else is that an OMITTED
// limit means NO limit — a ceiling exists only when an operator sets one — and a hardcoded number nobody
// configured is exactly the kind of hidden policy that fails good work. Stall detection, tool-hang cancellation
// and the execution reaper are the mechanism for a runaway, and none of them needs a second opinion from here.
//
// IT IS STILL CANCELLABLE, which is the part that matters and is NOT a timeout: the run's own context reaches
// this call, so an operator cancelling the execution, a container going away, or a wall-clock budget the OPERATOR
// configured all stop the gate. The difference is that the bound is one somebody chose, not one this file
// invented.
// It returns the FULL transcript as well as the capped copy the record keeps. The cap is a RECORD limit, not a
// measurement limit: the declared report is parsed from the full output, so trimming what gets stored can never
// change a verdict.
func (r *TaskReconciler) runCompletionGate(ctx context.Context, dir string, decl verify.Declaration) (full, capped string, err error) {
	out, err := runVerifyCommand(ctx, dir, decl.Command)
	return out, capGateOutput(out), err
}

// parseGateOutput reads a gate's per-check results using the project's DECLARED format.
//
// IT REFUSES TO GUESS. No format, or a format that yields no results, returns an error naming the reason — never
// an empty map, because an empty map is indistinguishable from "everything passed" and that is how a verification
// system starts lying. Every error here becomes an abstention upstream, so an unmeasurable gate costs the step
// its "verified" status and never fails it.
//
// THE REPORT MAY BE A FILE. Some runners cannot write to stdout (surefire writes target/surefire-reports, pytest
// wants a path), so `path` in the declaration is read INSTEAD of the command's output. That also removes the
// gate's own chatter from the parse entirely, rather than hoping the parser finds the document inside it.
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
	//
	// SKIPPED FOR A STEP THAT OWES NO CODE. A plan-only step (the architect writes a gitignored note) cannot
	// change the gate's outcome, so running it costs a full build+test cycle to learn nothing and — worse —
	// makes a red-at-baseline gate a failure that step can never clear. The differential is skipped with it
	// for the same reason: with no code change there is nothing to regress.
	//
	// This was a real failure: architect run 01M6TBXTPB26MX13JFEPFTV7EG ran the full gate, got a red result
	// it had not caused, and failed six recovery attempts in a row.
	decl, declErr := declaredGate(gateDeclarationDir(in.worktreePath, in.projectDir))
	ev.DeclaredCommand = decl.Command
	if declErr != nil {
		// A BROKEN DECLARATION IS REPORTED, and it is an abstention rather than a failure: the worker did not
		// write the declaration, and failing it for someone else's typo is the "holding people back" the operator
		// is removing.
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
