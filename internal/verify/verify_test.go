package verify

import (
	"context"
	"strings"
	"testing"
)

// The tests below are grouped by the question they answer, because the thing most likely to regress
// here is not a computation but a POLICY: which verdict a given combination of evidence produces.
// A future edit that quietly makes "unverified" count as success would pass a test suite that only
// checked set arithmetic, so the precedence itself is pinned.

func boolPtr(b bool) *bool { return &b }

// TestNoCommitsIsUnsubstantiated pins the check that closes the reported defect.
//
// The real case: work item 01M5X62Z1X8N08KD7KAY0G6FSV, run 01M5XSQZG3VV0X0WGVTMHNE8VS. The worker
// reported success having changed nothing ("confirmed the fix is already merged... no duplicate PR
// was created") and the platform recorded the work item as succeeded with a green acceptance review.
// Here the repository contradicts the claim, so the verdict must be a FAILURE — not unverified,
// because "nothing happened" is a positive fact, not an absence of evidence.
func TestNoCommitsIsUnsubstantiated(t *testing.T) {
	got := Collect(context.Background(), Evidence{
		GitReadable:             true,
		BaseRef:                 "develop",
		CommitsAhead:            0,
		ChangedFiles:            map[string]struct{}{},
		StepDeclaresDeliverable: true,
		TreeClean:               boolPtr(true),
	})
	if got.Verdict != VerdictUnsubstantiated {
		t.Fatalf("verdict = %q, want %q (reason: %s)", got.Verdict, VerdictUnsubstantiated, got.Reason)
	}
	if !strings.Contains(got.Reason, "work_present") {
		t.Errorf("reason %q does not name the failing check", got.Reason)
	}
	if got.Verdict.IsSuccess() {
		t.Error("unsubstantiated must never count as success")
	}
}

// TestNoDeclaredGateIsUnverified pins the central design rule: a project that declares no gate cannot
// have a step CERTIFIED, so it lands in the third state rather than being rounded up to success.
//
// This is the state this platform's own repository is in today (`.orchicon/verify` does not exist at
// the Orchicon project root), which is exactly why a run can look green and still be wrong.
func TestNoDeclaredGateIsUnverified(t *testing.T) {
	got := Collect(context.Background(), Evidence{
		GitReadable:             true,
		BaseRef:                 "develop",
		CommitsAhead:            2,
		ChangedFiles:            map[string]struct{}{"internal/foo.go": {}},
		ClaimedFiles:            map[string]struct{}{"internal/foo.go": {}},
		StepDeclaresDeliverable: true,
		TreeClean:               boolPtr(true),
		// DeclaredCommand deliberately empty; no baseline.
	})
	if got.Verdict != VerdictUnverified {
		t.Fatalf("verdict = %q, want %q — an undeclared gate must not certify a step", got.Verdict, VerdictUnverified)
	}
	if got.Verdict.IsSuccess() {
		t.Error("unverified must not count as success")
	}
	if !strings.Contains(got.Reason, "project_gate") {
		t.Errorf("reason %q should name the undeclared gate", got.Reason)
	}
	// The git checks themselves passed — the abstention is what held the verdict back.
	for _, c := range got.Checks {
		if (c.Name == "work_present" || c.Name == "tree_clean") && c.Status != CheckPass {
			t.Errorf("%s = %q, want pass — the git evidence here is good", c.Name, c.Status)
		}
	}
}

// TestEverythingPassingIsVerified is the positive control: when the evidence is complete, the step is
// certified. Without this, a rule that abstained unconditionally would pass every other test.
func TestEverythingPassingIsVerified(t *testing.T) {
	got := Collect(context.Background(), Evidence{
		GitReadable:             true,
		BaseRef:                 "develop",
		CommitsAhead:            1,
		ChangedFiles:            map[string]struct{}{"a.go": {}, "a_test.go": {}},
		ClaimedFiles:            map[string]struct{}{"a.go": {}},
		DeclaredCommand:         "make test",
		DeclaredRan:             true,
		DeclaredPassed:          true,
		Baseline:                &Baseline{Commit: "abc123", Results: map[string]bool{"ok pkg/a": true}},
		HeadResults:             map[string]bool{"ok pkg/a": true},
		HeadMeasured:            true,
		StepDeclaresDeliverable: true,
		TreeClean:               boolPtr(true),
	})
	if got.Verdict != VerdictVerified {
		t.Fatalf("verdict = %q, want verified (reason: %s)", got.Verdict, got.Reason)
	}
	if !got.Verdict.IsSuccess() {
		t.Error("verified must count as success")
	}
}

// TestNewFailureFailsEvenWhenDeclaredGatePasses pins that the differential is not redundant with the
// declared command.
//
// A project whose gate passes overall can still have regressed a specific check if the gate is
// partial (a targeted test run, a subset). The differential is the backstop: it compares the SET, so
// a regression the project's own command did not cover still fails the step.
func TestNewFailureFailsEvenWhenDeclaredGatePasses(t *testing.T) {
	got := Collect(context.Background(), Evidence{
		GitReadable:     true,
		BaseRef:         "develop",
		CommitsAhead:    1,
		ChangedFiles:    map[string]struct{}{"pkg/b/b.go": {}},
		DeclaredCommand: "go test ./internal/verify/", DeclaredRan: true, DeclaredPassed: true,
		Baseline: &Baseline{Commit: "base1", Results: map[string]bool{
			"ok pkg/a":      true,
			"ok pkg/b":      true,
			"FAIL pkg/c":    false, // already broken; not this step's doing
			"ok pkg/unseen": true,
		}},
		HeadResults: map[string]bool{
			"ok pkg/a":      true,
			"FAIL pkg/b":    false, // REGRESSED
			"FAIL pkg/c":    false, // still broken, unchanged
			"ok pkg/unseen": true,
		},
		HeadMeasured:            true,
		StepDeclaresDeliverable: true,
		TreeClean:               boolPtr(true),
	})
	if got.Verdict != VerdictUnsubstantiated {
		t.Fatalf("verdict = %q, want unsubstantiated", got.Verdict)
	}
	if !strings.Contains(got.Reason, "pkg/b") {
		t.Errorf("reason %q must name the regressed check", got.Reason)
	}
	if strings.Contains(got.Reason, "pkg/c") {
		t.Errorf("reason %q must NOT blame a pre-existing failure", got.Reason)
	}
}

// TestPreExistingFailureIsNotBlamed is the "pre-existing" case that generates most of the argument in
// this platform's FACTS LEARNED ledger. A check failing at BOTH ends is not this step's failure, and
// the platform must not require a worker to guess about it.
func TestPreExistingFailureIsNotBlamed(t *testing.T) {
	got := Collect(context.Background(), Evidence{
		GitReadable:     true,
		BaseRef:         "develop",
		CommitsAhead:    1,
		ChangedFiles:    map[string]struct{}{"pkg/a/a.go": {}},
		DeclaredCommand: "make test", DeclaredRan: true, DeclaredPassed: true,
		Baseline:                &Baseline{Commit: "base", Results: map[string]bool{"ok pkg/a": true, "FAIL pkg/old": false}},
		HeadResults:             map[string]bool{"ok pkg/a": true, "FAIL pkg/old": false},
		HeadMeasured:            true,
		StepDeclaresDeliverable: true,
		TreeClean:               boolPtr(true),
	})
	if got.Verdict != VerdictVerified {
		t.Fatalf("verdict = %q, want verified — an unchanged pre-existing failure is not a regression (reason: %s)",
			got.Verdict, got.Reason)
	}
}

// TestClaimedFileWithNoChangeFails pins the fabrication cross-check.
func TestClaimedFileWithNoChangeFails(t *testing.T) {
	got := Collect(context.Background(), Evidence{
		GitReadable:     true,
		BaseRef:         "develop",
		CommitsAhead:    1,
		ChangedFiles:    map[string]struct{}{"internal/real.go": {}},
		ClaimedFiles:    map[string]struct{}{"internal/real.go": {}, "internal/imaginary.go": {}},
		DeclaredCommand: "true", DeclaredRan: true, DeclaredPassed: true,
		Baseline:                &Baseline{Commit: "b", Results: map[string]bool{"x": true}},
		HeadResults:             map[string]bool{"x": true},
		HeadMeasured:            true,
		StepDeclaresDeliverable: true,
		TreeClean:               boolPtr(true),
	})
	if got.Verdict != VerdictUnsubstantiated {
		t.Fatalf("verdict = %q, want unsubstantiated", got.Verdict)
	}
	if !strings.Contains(got.Reason, "imaginary.go") {
		t.Errorf("reason %q must name the unrecorded file", got.Reason)
	}
}

// TestPathNormalisationDoesNotPunishHonesty pins that a leading "./" or an absolute path in the
// worker's claim does not fail it. A verification system that fails correct work is worse than none.
func TestPathNormalisationDoesNotPunishHonesty(t *testing.T) {
	got := Collect(context.Background(), Evidence{
		GitReadable:     true,
		BaseRef:         "develop",
		CommitsAhead:    1,
		ChangedFiles:    map[string]struct{}{"internal/foo.go": {}},
		ClaimedFiles:    map[string]struct{}{"./internal/foo.go": {}, "/abs/internal/foo.go": {}},
		DeclaredCommand: "true", DeclaredRan: true, DeclaredPassed: true,
		Baseline:                &Baseline{Commit: "b", Results: map[string]bool{"x": true}},
		HeadResults:             map[string]bool{"x": true},
		HeadMeasured:            true,
		StepDeclaresDeliverable: true,
		TreeClean:               boolPtr(true),
	})
	if got.Verdict != VerdictVerified {
		t.Fatalf("verdict = %q, want verified — path forms must be normalised (reason: %s)", got.Verdict, got.Reason)
	}
}

// TestDirtyTreeFails pins that uncommitted work is a delivery failure rather than a suggestion. Each
// step runs in a pristine sibling worktree, so uncommitted work is invisible downstream.
func TestDirtyTreeFails(t *testing.T) {
	got := Collect(context.Background(), Evidence{
		GitReadable:     true,
		BaseRef:         "develop",
		CommitsAhead:    1,
		ChangedFiles:    map[string]struct{}{"a.go": {}},
		DeclaredCommand: "true", DeclaredRan: true, DeclaredPassed: true,
		Baseline:                &Baseline{Commit: "b", Results: map[string]bool{"x": true}},
		HeadResults:             map[string]bool{"x": true},
		HeadMeasured:            true,
		StepDeclaresDeliverable: true,
		TreeClean:               boolPtr(false),
		DirtySummary:            "M internal/foo.go",
	})
	if got.Verdict != VerdictUnsubstantiated {
		t.Fatalf("verdict = %q, want unsubstantiated", got.Verdict)
	}
	if !strings.Contains(got.Reason, "uncommitted") {
		t.Errorf("reason %q must say the tree is dirty", got.Reason)
	}
}

// TestPlanOnlyStepSkipsDeliverableChecks pins that a legitimately non-code step is not punished.
//
// A plan-only step (the Principal Software Architect writes a gitignored note) has no commit to make.
// Under a naive "no commits = failure" rule it would fail every run, which is the fastest possible
// way to get a verification system switched off. Inapplicable checks SKIP; they do not abstain,
// because they carry no information about the claim.
//
// NOTE the absence of a declared gate here. WHEN there is one, the step's own collector does not run it
// at all (it cannot change the outcome), so the gate check is skipped by the producer rather than by this
// function — see TestPlanOnlyStepSkipsTheGateEntirely.
func TestPlanOnlyStepSkipsDeliverableChecks(t *testing.T) {
	got := Collect(context.Background(), Evidence{
		GitReadable:             true,
		BaseRef:                 "develop",
		CommitsAhead:            0,
		ChangedFiles:            map[string]struct{}{},
		StepDeclaresDeliverable: false,
		SkipReason:              "this is a plan-only step",
	})
	// Every check is inapplicable, so nothing is confirmed: `not_applicable`, never a failure and never a
	// claim of verification.
	if got.Verdict != VerdictNotApplicable {
		t.Fatalf("verdict = %q, want %q — a plan-only step is neither confirmed nor at fault (reason: %s)",
			got.Verdict, VerdictNotApplicable, got.Reason)
	}
	if got.Verdict == VerdictUnsubstantiated {
		t.Fatal("a plan-only step must not be failed for producing no code")
	}
}

// TestUnreadableGitIsUnverifiedNotFailed pins that a broken read never manufactures a failure.
func TestUnreadableGitIsUnverifiedNotFailed(t *testing.T) {
	got := Collect(context.Background(), Evidence{
		GitReadable:             false,
		GitError:                "chdir: no such file or directory",
		StepDeclaresDeliverable: true,
	})
	if got.Verdict != VerdictUnverified {
		t.Fatalf("verdict = %q, want unverified (reason: %s)", got.Verdict, got.Reason)
	}
	if got.Verdict == VerdictUnsubstantiated {
		t.Error("an unreadable repository must not be reported as a contradiction")
	}
}

// TestContradictionOutranksAbstention pins the precedence: a real fail beats any number of unknowns.
func TestContradictionOutranksAbstention(t *testing.T) {
	got := Collect(context.Background(), Evidence{
		GitReadable:             true,
		BaseRef:                 "develop",
		CommitsAhead:            0, // FAIL
		ChangedFiles:            map[string]struct{}{},
		StepDeclaresDeliverable: true,
		// and nothing else is measurable (abstentions everywhere)
	})
	if got.Verdict != VerdictUnsubstantiated {
		t.Fatalf("verdict = %q, want unsubstantiated — a contradiction outranks absences", got.Verdict)
	}
}

// TestNewFailuresSetArithmetic covers the comparison directly, including its nil-safety.
func TestNewFailures(t *testing.T) {
	cases := []struct {
		name     string
		baseline map[string]bool
		head     map[string]bool
		want     []string
	}{
		{"nil baseline yields nothing", nil, map[string]bool{"a": false}, nil},
		{"nil head yields nothing", map[string]bool{"a": true}, nil, nil},
		{"pass to fail is a regression", map[string]bool{"a": true}, map[string]bool{"a": false}, []string{"a"}},
		{"fail to fail is not", map[string]bool{"a": false}, map[string]bool{"a": false}, nil},
		{"new check absent from baseline is ignored", map[string]bool{}, map[string]bool{"a": false}, nil},
		{"disappeared check is not a regression", map[string]bool{"a": true}, map[string]bool{}, nil},
		{"stable pass is not", map[string]bool{"a": true}, map[string]bool{"a": true}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NewFailures(tc.baseline, tc.head)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestFormatRecordsLeadsWithTheVerdict pins the record's shape, because the acceptance review is what
// the operator reads and an evidence block that buries the verdict is not usable.
func TestFormatRecordsLeadsWithTheVerdict(t *testing.T) {
	r := Result{}
	r.Add("work_present", CheckFail, "HEAD did not move")
	r.Add("project_gate", CheckAbstain, "no .orchicon/verify")
	r.Add("tree_clean", CheckPass, "clean")
	got := r.Conclude().FormatRecords()

	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) < 2 {
		t.Fatalf("record too short: %q", got)
	}
	if !strings.Contains(lines[0], "unsubstantiated") {
		t.Errorf("first line %q must carry the verdict", lines[0])
	}
	// Failures render before abstentions and passes, so the reason is at the top.
	if !strings.Contains(lines[1], "work_present") {
		t.Errorf("second line %q must be the failing check", lines[1])
	}
}

// TestVerdictIsSuccess covers the whole vocabulary, because a single wrong entry here silently
// re-opens the defect the package exists to close.
func TestVerdictIsSuccess(t *testing.T) {
	if !VerdictVerified.IsSuccess() {
		t.Error("verified must be success")
	}
	if VerdictUnverified.IsSuccess() {
		t.Error("unverified MUST NOT be success")
	}
	if VerdictUnsubstantiated.IsSuccess() {
		t.Error("unsubstantiated must not be success")
	}
}

// TestResultKeyNormalisation pins the bug that made the differential report "no new failures" for a
// genuine regression.
//
// `go test` prints the OUTCOME in the line itself — `ok pkg/b` on success, `FAIL pkg/b` on failure —
// so a map keyed on the raw line sees a pass "disappear" and an unrelated fail "appear", never the
// same subject changing state. The differential then finds nothing and passes a step that broke the
// build. This is the single most dangerous failure mode in the whole package (a false NEGATIVE: the
// mechanism reports green on broken work), so it is pinned directly at the key level as well as
// through Collect.
func TestResultKeyNormalisation(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ok  \tgithub.com/x/pkg", "github.com/x/pkg"},
		{"FAIL\tgithub.com/x/pkg", "github.com/x/pkg"},
		{"ok  github.com/x/pkg 0.123s", "github.com/x/pkg"},
		{"FAIL github.com/x/pkg [build failed]", "github.com/x/pkg"},
		{"--- FAIL: TestThing", "TestThing"},
		{"--- PASS: TestThing/sub_case", "TestThing/sub_case"},
		{"github.com/x/pkg", "github.com/x/pkg"},
	}
	for _, c := range cases {
		if got := normalizeResultKey(c.in); got != c.want {
			t.Errorf("normalizeResultKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestGoTestShapeRegressionIsDetected is the end-to-end version: raw `go test` lines at both ends,
// where the same package flips from ok to FAIL.
func TestGoTestShapeRegressionIsDetected(t *testing.T) {
	got := Collect(context.Background(), Evidence{
		GitReadable:             true,
		BaseRef:                 "develop",
		CommitsAhead:            1,
		ChangedFiles:            map[string]struct{}{"internal/b/b.go": {}},
		StepDeclaresDeliverable: true,
		TreeClean:               boolPtr(true),
		DeclaredCommand:         "go test ./...", DeclaredRan: true, DeclaredPassed: true,
		Baseline: &Baseline{Commit: "base", Results: map[string]bool{
			"ok  \tgithub.com/x/a": true,
			"ok  \tgithub.com/x/b": true,
		}},
		HeadResults: map[string]bool{
			"ok  \tgithub.com/x/a": true,
			"FAIL\tgithub.com/x/b": false,
		},
		HeadMeasured: true,
	})
	if got.Verdict != VerdictUnsubstantiated {
		t.Fatalf("verdict = %q, want unsubstantiated — a package that went ok→FAIL is a regression (reason: %s)",
			got.Verdict, got.Reason)
	}
	if !strings.Contains(got.Reason, "github.com/x/b") {
		t.Errorf("reason %q must name the regressed package", got.Reason)
	}
}

// TestPreExistingRedGateDoesNotFailTheStep is the regression test for the defect that failed a live run six
// times, and it is written to the exact shape of that run.
//
// THE OBSERVED FAILURE. A Principal Software Architect step — a PLAN-ONLY step, contractually barred from
// writing code — reported `success` having produced a 438-line plan. The evidence gate overrode it with
// `not substantiated: project_gate: the project's declared gate FAILED`, and every one of the six recovery
// attempts failed identically. Nothing the step could have done would have changed the outcome.
//
// THE CAUSE. `project_gate` was a BINARY check on the declared command's exit code. On a repository with any
// pre-existing breakage that is not uninformative, it is actively wrong: it blames every step for a failure
// that step did not cause. The differential exists precisely to attribute a red gate — and it would have
// cleared this step, because nothing regressed.
//
// The exit code must therefore be ATTRIBUTED, not trusted.
func TestPreExistingRedGateDoesNotFailTheStep(t *testing.T) {
	got := Collect(context.Background(), Evidence{
		GitReadable:             true,
		BaseRef:                 "develop",
		CommitsAhead:            1,
		ChangedFiles:            map[string]struct{}{"architecture-notes/plan.md": {}},
		StepDeclaresDeliverable: true,
		TreeClean:               boolPtr(true),

		DeclaredCommand: "go build ./... && go vet ./... && go test ./internal/... ./cmd/...",
		DeclaredRan:     true,
		DeclaredPassed:  false, // RED — as it was on the real run
		DeclaredDetail:  "FAIL\tgithub.com/x/runtime\t0.5s\nok  \tgithub.com/x/version\t[no test files]\n",

		// Red at BOTH ends, identically: the same check was failing before the step started.
		Baseline:     &Baseline{Commit: "base", Results: map[string]bool{"github.com/x/runtime": false, "github.com/x/version": true}},
		HeadResults:  map[string]bool{"github.com/x/runtime": false, "github.com/x/version": true},
		HeadMeasured: true,
	})

	if got.Verdict == VerdictUnsubstantiated {
		t.Fatalf("a step must not be FAILED for a gate that was identically red before it started — this is "+
			"the defect that failed run 01M6TBXTPB26MX13JFEPFTV7EG six times (reason: %s)", got.Reason)
	}
	if got.Verdict != VerdictVerified {
		t.Fatalf("verdict = %q, want verified: nothing regressed, so the step is substantiated (reason: %s)",
			got.Verdict, got.Reason)
	}
	// And the record must SAY the gate is red, so the operator is not told everything is green.
	for _, c := range got.Checks {
		if c.Name == "project_gate" && !strings.Contains(c.Detail, "pre-existing") {
			t.Errorf("the project_gate record must name the pre-existing breakage, got %q", c.Detail)
		}
	}
}

// TestRegressionIsReportedOnceNotTwice pins attribution: one cause, one failure line.
func TestRegressionIsReportedOnceNotTwice(t *testing.T) {
	got := Collect(context.Background(), Evidence{
		GitReadable: true, BaseRef: "develop", CommitsAhead: 1,
		ChangedFiles:            map[string]struct{}{"a.go": {}},
		StepDeclaresDeliverable: true,
		TreeClean:               boolPtr(true),
		DeclaredCommand:         "make test", DeclaredRan: true, DeclaredPassed: false,
		DeclaredDetail: "FAIL\tpkg/a\t0.1s\n",
		Baseline:       &Baseline{Commit: "b", Results: map[string]bool{"pkg/a": true}},
		HeadResults:    map[string]bool{"pkg/a": false},
		HeadMeasured:   true,
	})
	if got.Verdict != VerdictUnsubstantiated {
		t.Fatalf("a genuine regression must fail the step (got %q, reason: %s)", got.Verdict, got.Reason)
	}
	if n := len(got.Failed()); n != 1 {
		t.Fatalf("%d checks failed for ONE regression (%v) — the operator reads that as two problems, and "+
			"the one naming what broke is buried", n, got.Failed())
	}
	if !strings.Contains(got.Reason, "pkg/a") {
		t.Errorf("the reason must name the regressed check, got %q", got.Reason)
	}
}

// TestRedGateWithoutBaselineAbstains pins that an unattributable failure is not blamed on the worker.
func TestRedGateWithoutBaselineAbstains(t *testing.T) {
	got := Collect(context.Background(), Evidence{
		GitReadable: true, BaseRef: "develop", CommitsAhead: 1,
		ChangedFiles:            map[string]struct{}{"a.go": {}},
		StepDeclaresDeliverable: true,
		TreeClean:               boolPtr(true),
		DeclaredCommand:         "make test", DeclaredRan: true, DeclaredPassed: false,
		DeclaredDetail: "FAIL\tpkg/a\t0.1s\n",
		// No baseline at all — the platform cannot place the failure.
	})
	if got.Verdict == VerdictUnsubstantiated {
		t.Fatalf("a red gate the platform cannot attribute must not be charged to the step (reason: %s)", got.Reason)
	}
}

// TestFailureReasonNamesAFailureNotAPassingLine pins the second defect from the live run.
//
// The recorded reason was: `project_gate: the project's declared gate FAILED: … internal/version [no test
// files]` — and `[no test files]` is a PASSING line. A tail-truncated excerpt starts at the oldest surviving
// line, so taking the FIRST line reliably names something that is not the failure. The operator's one
// question — which check broke — went unanswered, and the message pointed at a green line.
func TestFailureReasonNamesAFailureNotAPassingLine(t *testing.T) {
	excerpt := "ok  \tgithub.com/x/version\t[no test files]\nFAIL\tgithub.com/x/runtime\t0.512s\n" +
		"--- FAIL: TestProviderAPIKeyEnvMatchesOpenCodeRegistry (0.00s)\n"
	got := firstFailureLine(excerpt)
	if got == "" {
		t.Fatal("a transcript containing a FAIL must yield a failing line")
	}
	if strings.Contains(got, "[no test files]") {
		t.Fatalf("the reason names a PASSING line (%q) — the operator cannot diagnose that", got)
	}
	if !strings.Contains(got, "FAIL") {
		t.Fatalf("the reason %q does not name a failure", got)
	}

	// Nothing failing in the excerpt: say so rather than naming a green line.
	if got := firstFailureLine("ok  \tpkg/a\t0.1s\nok  \tpkg/b\t[no test files]\n"); got != "" {
		t.Fatalf("an excerpt with no failure must yield \"\", got %q", got)
	}
}

// TestPlanOnlyStepSkipsTheGateEntirely pins the third defect from the live run.
//
// The architect step was made to run the full declared gate — `go build ./... && go vet ./... && go test
// ./internal/... ./cmd/...` — even though a plan-only step cannot change the gate's outcome. On a repository
// that is red at baseline, that turned a correctly-executed step into an unclearable failure.
//
// The check must SKIP (carry no information about the claim), not abstain (carry an absence), or every
// architect step would sit at `unverified` forever for a question never asked of it.
func TestPlanOnlyStepSkipsTheGateEntirely(t *testing.T) {
	got := Collect(context.Background(), Evidence{
		GitReadable:             true,
		BaseRef:                 "develop",
		CommitsAhead:            0,
		ChangedFiles:            map[string]struct{}{},
		StepDeclaresDeliverable: false,
		SkipReason:              "architect steps deliver a plan note, which this project gitignores",
		// Deliberately NO declared command and NO baseline: the plan-only step was never measured.
	})
	// NOT `verified`, and not a failure either: with every check skipped, nothing was confirmed. Claiming
	// `verified` here would be the silent-success defect this package exists to remove, wearing a
	// different hat — a confirmation asserted where none happened.
	if got.Verdict != VerdictNotApplicable {
		t.Fatalf("a step with every check skipped must be %q, got %q (reason: %s)",
			VerdictNotApplicable, got.Verdict, got.Reason)
	}
	if got.Verdict == VerdictUnsubstantiated {
		t.Fatal("a plan-only step must never be failed for producing no repository evidence")
	}
	var sawGate, sawDiff bool
	for _, c := range got.Checks {
		switch c.Name {
		case "project_gate":
			sawGate = true
			if c.Status != CheckSkip {
				t.Errorf("project_gate = %q, want skip — a plan-only step was never asked this question",
					c.Status)
			}
		case "no_new_failures":
			sawDiff = true
			if c.Status != CheckSkip {
				t.Errorf("no_new_failures = %q, want skip", c.Status)
			}
		}
	}
	if !sawGate || !sawDiff {
		t.Error("the record must still NAME the skipped checks, so the operator sees what was not applied")
	}
}

// TestAllChecksSkippedIsNotVerified pins the verdict vocabulary: a step with NOTHING checked has not been
// confirmed, and saying so is the whole point of this package.
//
// The temptation — and the first version of this code did it — is to let `verified` be the default when
// nothing failed and nothing abstained, which means every plan-only and approval step reports a
// confirmation that never happened. That is the silent success this package exists to remove, wearing a
// different hat.
func TestAllChecksSkippedIsNotVerified(t *testing.T) {
	got := Collect(context.Background(), Evidence{
		StepDeclaresDeliverable: false,
		SkipReason:              "graph plumbing carries no deliverable",
		GitReadable:             true, // readable, but every check is skipped by the deliverable rule
	})
	if got.Verdict == VerdictVerified {
		t.Fatal("a step with no applicable check must not report `verified` — nothing was confirmed")
	}
	if got.Verdict != VerdictNotApplicable {
		t.Fatalf("verdict = %q, want %q (reason: %s)", got.Verdict, VerdictNotApplicable, got.Reason)
	}
	// not_applicable is not a confirmation, and must not claim to be one.
	if got.Verdict.IsSuccess() {
		t.Error("not_applicable must not report as a confirmed success")
	}
}
