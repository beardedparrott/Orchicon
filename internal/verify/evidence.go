package verify

// evidence.go — the COLLECTOR half.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Runner executes a command in a directory and returns combined output.
type Runner func(ctx context.Context, dir, command string) (output string, err error)

// Evidence is everything the collector could learn about one completed step. A field that could not
// be read stays at its zero value and is reported as an abstention — never as a pass.
type Evidence struct {
	// GitReadable is false when the work tree could not be read at all (no repo, git missing, the
	// directory vanished). Every git-based check abstains when it is false.
	GitReadable bool
	// GitError carries the read failure, so the abstention says WHY.
	GitError string

	// BaseRef is the commit the step's work was cut from (the run branch's base, or the parent
	// branch). Empty when unknown.
	BaseRef string
	// HeadRef is what the work tree is on now.
	HeadRef string

	// CommitsAhead is how many commits HEAD is ahead of BaseRef. -1 means "not measured".
	CommitsAhead int

	// ChangedFiles is the set of paths git reports as changed between BaseRef and HEAD. nil means
	// "not measured" (distinct from empty, which means "measured and nothing changed").
	ChangedFiles map[string]struct{}

	// ClaimedFiles is the set of paths the worker said it touched, extracted from its own report.
	ClaimedFiles map[string]struct{}

	// DeclaredCommand is the project's `.orchicon/verify` command, "" when it declares none.
	DeclaredCommand string
	// DeclaredResult is the outcome of running DeclaredCommand at HEAD.
	DeclaredRan    bool
	DeclaredPassed bool
	DeclaredDetail string

	// Baseline records, per check name, whether that check passed at the BASE commit. nil means no
	// baseline was captured, which makes the differential check abstain rather than guess.
	Baseline *Baseline
	// HeadResults is the same set of results measured at the step's HEAD.
	HeadResults map[string]bool
	// HeadMeasured is false when HeadResults could not be produced.
	HeadMeasured bool
	// HeadDetail carries the failure reason when HeadResults could not be produced.
	HeadDetail string

	// TreeClean reports whether the work tree has no uncommitted changes. nil means "not read".
	TreeClean *bool
	// DirtySummary names what is dirty, for the failure detail.
	DirtySummary string

	// StepDeclaresDeliverable reports whether this step is expected to produce changes at all.
	StepDeclaresDeliverable bool
	// SkipReason explains WHY the deliverable checks are skipped, for the record.
	SkipReason string
}

// Baseline is the recorded outcome of the project's gate at a specific commit.
type Baseline struct {
	// Commit is the ref the baseline was measured at.
	Commit string
	// Results maps a stable check key to whether it passed. For Go projects the key is
	// "ok <package>" / "FAIL <package>"; for the declared command it is "declared".
	Results map[string]bool
	// Detail carries anything the measurement could not express as pass/fail (a build failure that
	// prevented the run).
	Detail string
}

// Incomplete reports whether the baseline is unusable for a differential comparison.
func (b *Baseline) Incomplete() bool { return b == nil || b.Detail != "" || len(b.Results) == 0 }

// Collect assembles the evidence and decides.
func Collect(ctx context.Context, ev Evidence) Result {
	var r Result

	// --- Layer 2: universal git invariants ------------------------------------------------------
	//
	// These need no project cooperation and cannot be talked around: they read the repository, not
	// the report. They are first because they are the cheapest and because a contradiction here makes
	// the expensive checks pointless.
	collectGitInvariants(&r, ev)

	// --- Layer 1: the project's gate, base vs HEAD, and the declared command's exit code ---------
	//
	// THE REGRESSION SET IS COMPUTED FIRST because BOTH checks depend on it: the declared command may only
	// call a red gate a problem when the differential cannot explain it. Computing it once also guarantees
	// the two checks cannot disagree about what regressed.
	regressed := regressionsAgainstBaseline(ev)
	collectDeclaredCommand(ctx, &r, ev, regressed)
	collectDifferential(&r, ev, regressed)

	return r.Conclude()
}

// collectGitInvariants appends the checks that work on any repository.
func collectGitInvariants(r *Result, ev Evidence) {
	if !ev.GitReadable {
		detail := strings.TrimSpace(ev.GitError)
		if detail == "" {
			detail = "the work tree could not be read"
		}
		// NOT a pass and NOT a fail. The work may be perfect; nothing demonstrated it. Three
		// abstentions so the record names each thing that went unmeasured.
		r.Add("work_present", CheckAbstain, detail)
		r.Add("claimed_files_recorded", CheckAbstain, detail)
		r.Add("tree_clean", CheckAbstain, detail)
		return
	}

	// 1.
	switch {
	case !ev.StepDeclaresDeliverable:
		reason := strings.TrimSpace(ev.SkipReason)
		if reason == "" {
			reason = "this step does not declare a code deliverable"
		}
		r.Add("work_present", CheckSkip, reason)
		r.Add("claimed_files_recorded", CheckSkip, reason)
	case ev.CommitsAhead < 0:
		r.Add("work_present", CheckAbstain, "commit count could not be measured")
	case ev.CommitsAhead == 0:
		r.Add("work_present", CheckFail,
			fmt.Sprintf("HEAD did not move off the base (%s) — no commits were added by this step", shortRef(ev.BaseRef)))
	case ev.BaseRef == "":
		// HEAD moved but there is no base to compare against: unmeasurable, not wrong.
		r.Add("work_present", CheckAbstain, "the base commit is unknown, so progress could not be measured")
	default:
		r.Add("work_present", CheckPass, fmt.Sprintf("%d commit(s) on top of %s", ev.CommitsAhead, shortRef(ev.BaseRef)))
	}

	// 2. ONLY A CONTRADICTION FAILS.
	switch {
	case !ev.StepDeclaresDeliverable:
		// already skipped above
	case ev.ChangedFiles == nil:
		r.Add("claimed_files_recorded", CheckAbstain, "changed files could not be measured")
	case len(ev.ClaimedFiles) == 0:
		// Nothing claimed: no contradiction is possible. Informational, never a failure — a worker
		// that omits file attribution has made an unverifiable claim, not a false one.
		r.Add("claimed_files_recorded", CheckSkip, "the worker claimed no specific files")
	default:
		missing := missingPaths(ev.ClaimedFiles, ev.ChangedFiles)
		if len(missing) > 0 {
			r.Add("claimed_files_recorded", CheckFail,
				fmt.Sprintf("%d claimed file(s) show no change: %s", len(missing), joinCapped(missing, 3)))
		} else {
			r.Add("claimed_files_recorded", CheckPass,
				fmt.Sprintf("all %d claimed file(s) appear in the diff", len(ev.ClaimedFiles)))
		}
	}

	// 3.
	switch {
	case ev.StepDeclaresDeliverable:
		if ev.TreeClean == nil {
			r.Add("tree_clean", CheckAbstain, "working-tree state could not be read")
		} else if *ev.TreeClean {
			r.Add("tree_clean", CheckPass, "no uncommitted changes")
		} else {
			r.Add("tree_clean", CheckFail, "uncommitted changes remain: "+ev.DirtySummary)
		}
	default:
		r.Add("tree_clean", CheckSkip, "this step does not declare a code deliverable")
	}
}

// collectDeclaredCommand appends the result of the project's own `.orchicon/verify` command.
//
// THE EXIT CODE ALONE IS NEVER A FAILURE, and this is the correction of a real defect that failed every
// step of a live run six times.
//
// `project_gate` and `no_new_failures` answer DIFFERENT QUESTIONS, and this check used to answer the second
// one badly:
//
//	project_gate     — did the declared command exit 0?                          (a blunt, whole-repo signal)
//	no_new_failures  — did THIS STEP make anything worse?                        (the question that matters)
//
// On a repository with ANY pre-existing breakage the first is not merely uninformative, it is actively
// wrong: it fails every step for something that step did not do. Measured on this platform's own project,
// where the gate is red at baseline (2 pre-existing failures): a PLAN-ONLY architect step — which by
// contract writes no code at all — was failed with `project_gate: the project's declared gate FAILED`, then
// re-dispatched through all six recovery attempts, each one failing identically. Nothing the step did could
// have changed the outcome, and the differential it was supposed to defer to would have cleared it.
//
// So the exit code is attributed, not trusted:
//
//	exit 0                                              → PASS
//	exit != 0, and the differential explains it           → PASS, with the pre-existing failures named
//	exit != 0, and the differential found a regression    → PASS here; `no_new_failures` carries the failure
//	exit != 0, and there is NO baseline to attribute it   → ABSTAIN (unverified), never FAIL
//
// The third case is deliberate: a regression is reported ONCE, by the check that can attribute it. Failing
// two checks for one cause reads as two problems and buries the one that says what and whose it is.
func collectDeclaredCommand(_ context.Context, r *Result, ev Evidence, regressed []string) {
	switch {
	case !ev.StepDeclaresDeliverable:
		// A step that owes no code cannot move the gate, so the check does not apply — SKIP, not abstain.
		reason := strings.TrimSpace(ev.SkipReason)
		if reason == "" {
			reason = "this step does not declare a code deliverable"
		}
		r.Add("project_gate", CheckSkip, reason)
		r.Add("no_new_failures", CheckSkip, reason)
		return
	case strings.TrimSpace(ev.DeclaredCommand) == "":
		// The project declares no gate.
		r.Add("project_gate", CheckAbstain,
			"the project declares no gate (no .orchicon/verify), so nothing can be confirmed passing")
	case !ev.DeclaredRan:
		r.Add("project_gate", CheckAbstain, "the declared gate could not be executed")
	case ev.DeclaredPassed:
		detail := "the project's declared gate passed"
		if d := strings.TrimSpace(ev.DeclaredDetail); d != "" {
			detail += ": " + firstLine(d)
		}
		r.Add("project_gate", CheckPass, detail)
	case !ev.Baseline.Incomplete() && ev.HeadMeasured && len(regressed) > 0:
		// Red, and something regressed. The differential reports it — not this check.
		r.Add("project_gate", CheckPass,
			"the gate is red, and the regression it contains is reported by no_new_failures")
	case !ev.Baseline.Incomplete() && ev.HeadMeasured:
		// Red, but identically red before the step began. THIS is the case that was failing good work.
		existing := countBaselineFailures(ev.Baseline)
		r.Add("project_gate", CheckPass, fmt.Sprintf(
			"the gate is not fully green, but it is identically not-green at the baseline (%d pre-existing "+
				"failure(s)) — nothing here is attributable to this step", existing))
	default:
		// Red and unattributable: no baseline, or HEAD could not be re-measured.
		detail := "the gate is red and there is no baseline to attribute it against, so it is not assigned " +
			"to this step"
		if d := strings.TrimSpace(ev.DeclaredDetail); d != "" {
			if failure := firstFailureLine(d); failure != "" {
				detail += ": " + failure
			} else {
				detail += " (the captured output did not include the failing line)"
			}
		}
		r.Add("project_gate", CheckAbstain, detail)
	}
}

// countBaselineFailures reports how many checks were already failing when the baseline was taken.
func countBaselineFailures(b *Baseline) int {
	if b == nil {
		return 0
	}
	n := 0
	for _, passing := range b.Results {
		if !passing {
			n++
		}
	}
	return n
}

// collectDifferential appends the base-vs-HEAD comparison — the check that settles "pre-existing".
func collectDifferential(r *Result, ev Evidence, regressed []string) {
	// Already recorded as a skip by collectDeclaredCommand when the step owes no code; a second entry would
	// be a duplicate. Nothing to add.
	if !ev.StepDeclaresDeliverable {
		return
	}
	// ONE CAUSE, ONE LINE.
	if strings.TrimSpace(ev.DeclaredCommand) == "" {
		return
	}
	// NOTHING FAILED, SO NOTHING REGRESSED.
	if ev.HeadMeasured && len(ev.HeadResults) > 0 && !anyFailing(ev.HeadResults) {
		r.Add("no_new_failures", CheckPass, fmt.Sprintf(
			"the gate is fully green at this step's HEAD (%d check(s)), so nothing regressed", len(ev.HeadResults)))
		return
	}
	if ev.Baseline.Incomplete() {
		detail := "no baseline was captured for this step, so new failures cannot be distinguished from existing ones"
		if ev.Baseline != nil && strings.TrimSpace(ev.Baseline.Detail) != "" {
			detail = "the baseline is incomplete: " + firstLine(ev.Baseline.Detail)
		}
		r.Add("no_new_failures", CheckAbstain, detail)
		return
	}
	if !ev.HeadMeasured {
		detail := strings.TrimSpace(ev.HeadDetail)
		if detail == "" {
			detail = "the gate could not be re-run at this step's HEAD"
		}
		r.Add("no_new_failures", CheckAbstain, "HEAD could not be measured: "+firstLine(detail))
		return
	}
	if len(regressed) > 0 {
		r.Add("no_new_failures", CheckFail,
			fmt.Sprintf("%d check(s) newly failing: %s", len(regressed), joinCapped(regressed, 3)))
		return
	}
	fixed := NewPasses(ev.Baseline.Results, ev.HeadResults)
	detail := fmt.Sprintf("no new failures against the baseline at %s", shortRef(ev.Baseline.Commit))
	if len(fixed) > 0 {
		detail += fmt.Sprintf(" (%d previously-failing check(s) now pass)", len(fixed))
	}
	r.Add("no_new_failures", CheckPass, detail)
}

// resultPrefixes are the leading tokens a raw test-runner line carries BEFORE the identity it is
// reporting on.
var resultPrefixes = []string{"ok", "FAIL", "--- FAIL:", "--- PASS:", "?"}

// normalizeResultKey reduces a result line to the IDENTITY it reports on, so the same subject
// compares equal across a change of outcome.
func normalizeResultKey(name string) string {
	// Fields collapses every run of whitespace, so "FAIL\tpkg [build failed]" and
	// "FAIL  pkg  0.1s" both become ["FAIL", "pkg", ...] regardless of which separator was used.
	fields := strings.Fields(strings.TrimSpace(name))
	for len(fields) > 0 && isResultPrefix(fields[0]) {
		fields = fields[1:]
	}
	if len(fields) == 0 {
		return ""
	}
	// The first surviving field is the subject identity. Package lines may continue with a duration
	// or a "[build failed]" note, and sub-test names contain "/" but no whitespace, so they survive.
	return fields[0]
}

// isResultPrefix reports whether a leading field is an outcome marker rather than the subject.
func isResultPrefix(s string) bool {
	switch s {
	case "ok", "FAIL", "PASS", "?", "---":
		return true
	}
	// "--- FAIL:" / "--- PASS:" arrive as one field after Fields collapses "--- FAIL:" → "---",
	// "FAIL:" only if spaced; handle the joined form too.
	return s == "FAIL:" || s == "PASS:"
}

// normalizeResults rewrites a raw results map to identity-keyed form.
func normalizeResults(in map[string]bool) map[string]bool {
	if in == nil {
		return nil
	}
	out := make(map[string]bool, len(in))
	for name, passing := range in {
		key := normalizeResultKey(name)
		if key == "" {
			continue
		}
		if prev, ok := out[key]; ok {
			out[key] = prev && passing
			continue
		}
		out[key] = passing
	}
	return out
}

// regressionsAgainstBaseline is NewFailures, guarded so an unmeasured baseline or HEAD yields nil rather
// than a misleading empty-but-present set.
func regressionsAgainstBaseline(ev Evidence) []string {
	if ev.Baseline.Incomplete() || !ev.HeadMeasured {
		return nil
	}
	return NewFailures(ev.Baseline.Results, ev.HeadResults)
}

// NewFailures returns the checks that pass in the baseline and fail at HEAD, sorted.
func NewFailures(baseline, head map[string]bool) []string {
	if baseline == nil || head == nil {
		return nil
	}
	base, now := normalizeResults(baseline), normalizeResults(head)
	var out []string
	for name, wasPassing := range base {
		if !wasPassing {
			continue // it was already failing; not this step's doing
		}
		if nowPassing, measured := now[name]; measured && !nowPassing {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// NewPasses returns the checks that failed in the baseline and pass at HEAD, sorted. Informational:
// it is how the record says "this step also fixed something", and it is deliberately not a criterion.
func NewPasses(baseline, head map[string]bool) []string {
	if baseline == nil || head == nil {
		return nil
	}
	base, now := normalizeResults(baseline), normalizeResults(head)
	var out []string
	for name, wasPassing := range base {
		if wasPassing {
			continue
		}
		if nowPassing, measured := now[name]; measured && nowPassing {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// missingPaths returns the claimed paths that are absent from changed, sorted.
func missingPaths(claimed, changed map[string]struct{}) []string {
	norm := make(map[string]struct{}, len(changed))
	for p := range changed {
		norm[normalisePath(p)] = struct{}{}
	}
	var out []string
	for p := range claimed {
		if !hasSuffixPath(normalisePath(p), norm) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// normalisePath reduces a path to its most specific repository-relative form, so the same file
// claimed in one spelling and recorded in another compares equal.
func normalisePath(p string) string {
	p = filepath.ToSlash(strings.TrimSpace(p))
	p = strings.TrimPrefix(p, "./")
	if !strings.HasPrefix(p, "/") {
		return p
	}
	// Absolute: drop the leading root and every segment before the first plausible source root.
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, s := range segs {
		if sourceRootSegment(s) {
			return strings.Join(segs[i:], "/")
		}
	}
	// No recognisable root: fall back to the last two segments, which is the most a suffix match can
	// honestly claim.
	if len(segs) > 2 {
		return strings.Join(segs[len(segs)-2:], "/")
	}
	return strings.Join(segs, "/")
}

// sourceRootSegment reports whether a path segment marks the start of a project tree.
func sourceRootSegment(s string) bool {
	switch s {
	case "internal", "src", "cmd", "pkg", "test", "tests", "docs", "frontend", "backend",
		"proto", "scripts", "db", "deploy", "site", "tools", "api", "skills":
		return true
	}
	return false
}

// hasSuffixPath reports whether claimed p is recorded in changed either exactly (after
// normalisation) or as a unique path-suffix of a changed entry.
func hasSuffixPath(p string, changed map[string]struct{}) bool {
	if _, ok := changed[p]; ok {
		return true
	}
	suffix := "/" + p
	for c := range changed {
		if strings.HasSuffix(c, suffix) {
			return true
		}
	}
	return false
}

// shortRef abbreviates a commit or ref for a one-line record.
func shortRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "(unknown)"
	}
	if i := strings.LastIndex(ref, "/"); i >= 0 && len(ref)-i <= 8 {
		// a branch name: keep it whole, it is more informative than a hash
		return ref
	}
	if len(ref) > 12 {
		return ref[:12]
	}
	return ref
}

// firstLine returns the first non-empty line of s, trimmed and capped — for an error excerpt.
// firstFailureLine returns the first line of a gate transcript that actually reports a FAILURE, or "" when
// the transcript names none.
//
// THE OBSERVED DEFECT: the failure reason read `project_gate: the project's declared gate FAILED: …
// internal/version [no test files]`. `[no test files]` is a PASSING line. `firstLine` had returned the first
// line of a TAIL-TRUNCATED excerpt, which is by construction the oldest surviving line and therefore the
// least likely to be the failure — so the one thing an operator needs (WHICH check broke, and why) was
// absent, and the message actively pointed at the wrong one.
//
// This scans for the failure instead of taking the first line, and returns "" rather than a misleading
// excerpt when the excerpt genuinely contains none — the caller then says the cause was not in the captured
// output, which is true and actionable, instead of naming a green line.
func firstFailureLine(s string) string {
	for _, raw := range strings.Split(s, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		failed := strings.HasPrefix(line, "FAIL") ||
			strings.Contains(line, "--- FAIL:") ||
			strings.Contains(lower, " build failed") ||
			strings.HasSuffix(line, " FAILED") ||
			strings.Contains(line, ": FAILED") ||
			strings.Contains(line, "cannot find package") ||
			strings.Contains(line, "undefined:") ||
			strings.Contains(line, "panic:") ||
			(strings.Contains(line, ".go:") && strings.Contains(lower, "error"))
		if failed {
			if len(line) > 200 {
				return line[:200] + "…"
			}
			return line
		}
	}
	return ""
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > 200 {
			return line[:200] + "…"
		}
		return line
	}
	return ""
}

// joinCapped joins up to max items and summarises the remainder, so a record stays one line even
// when a step breaks forty tests.
func joinCapped(items []string, max int) string {
	if len(items) <= max {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:max], ", ") + fmt.Sprintf(" (+%d more)", len(items)-max)
}

// ErrNoRepo is returned by a collector when the directory is not a git work tree.
var ErrNoRepo = errors.New("not a git work tree")

// ReadFileTrimmed reads a small file and trims it, treating absence as empty. Used for the project's
// declared gate file.
func ReadFileTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// anyFailing reports whether any check in a result set failed.
func anyFailing(results map[string]bool) bool {
	for _, passing := range results {
		if !passing {
			return true
		}
	}
	return false
}
