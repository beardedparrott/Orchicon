package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/verify"
)

// The scheduler-side tests cover two things the pure package cannot: the POLICY (which verdict changes
// which status) and the PARSING of a project's real gate output. Both are places where a quiet edit
// would re-open the defect without breaking a compile.

// TestApplyVerdictPolicy pins the entire policy in one table, because it is the whole behavioural
// contract of this feature and every row was a deliberate decision.
func TestApplyVerdictPolicy(t *testing.T) {
	cases := []struct {
		name          string
		succeeded     bool
		verdict       verify.Verdict
		wantSucceeded bool
		wantNotice    bool
		why           string
	}{
		{
			name: "worker failed stays failed", succeeded: false,
			verdict: verify.VerdictVerified, wantSucceeded: false, wantNotice: false,
			why: "a failed worker is never upgraded by evidence — there is nothing to substantiate",
		},
		{
			name: "worker failed and unverified stays failed", succeeded: false,
			verdict: verify.VerdictUnverified, wantSucceeded: false, wantNotice: false,
			why: "no notice: the step is already failing, so 'unverified' adds nothing for the operator",
		},
		{
			name: "verified stays succeeded", succeeded: true,
			verdict: verify.VerdictVerified, wantSucceeded: true, wantNotice: false,
			why: "no behavioural change for good work",
		},
		{
			name: "unsubstantiated stays SUCCEEDED with a notice", succeeded: true,
			verdict: verify.VerdictUnsubstantiated, wantSucceeded: true, wantNotice: true,
			why: "the verdict is a record now: a contradiction is said out loud and never acts on the step. " +
				"This is the operator's decision — the mechanism was holding people back",
		},
		{
			name: "unverified keeps succeeded but is never silent", succeeded: true,
			verdict: verify.VerdictUnverified, wantSucceeded: true, wantNotice: true,
			why: "failing it would fail every project with no gate and the mechanism would be disabled",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotSucceeded, notice := applyVerdict(tc.succeeded, verify.Result{Verdict: tc.verdict})
			if gotSucceeded != tc.wantSucceeded {
				t.Errorf("succeeded = %v, want %v (%s)", gotSucceeded, tc.wantSucceeded, tc.why)
			}
			if (notice != "") != tc.wantNotice {
				t.Errorf("notice = %q, want present=%v (%s)", notice, tc.wantNotice, tc.why)
			}
		})
	}
}

// TestUnsubstantiatedNoticeNamesTheReason pins that the notice an operator sees says WHAT was contradicted. A
// verdict with no cause sends them to a terminal, which is the cost this feature exists to remove.
func TestUnsubstantiatedNoticeNamesTheReason(t *testing.T) {
	_, notice := applyVerdict(true, verify.Result{
		Verdict: verify.VerdictUnsubstantiated,
		Reason:  "not substantiated: work_present: HEAD did not move",
	})
	for _, want := range []string{"does not support", "work_present"} {
		if !contains(notice, want) {
			t.Errorf("notice %q missing %q", notice, want)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

// THE GATE IS READ IN THE FORMAT THE PROJECT DECLARES, not guessed from its output.
//
// These replace the three tests that pinned the old line parser. The parser is gone: pattern-matching log lines
// was wrong in principle (a log line is a rendering, not a contract) and broken in practice (raw `node --test`
// TAP keyed its failure on the test number and dropped it). The format layer's own tests live in
// internal/verify/results_test.go; what is pinned HERE is the scheduler's wiring — that the declaration's format
// and path are what actually get used.
func TestTheGateIsReadInTheDeclaredFormat(t *testing.T) {
	dir := t.TempDir()
	decl := verify.Declaration{Command: "make test", Format: verify.FormatJSON}
	r := &TaskReconciler{log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	got, err := r.parseGateOutput(decl, dir, `{"pkg/a": true, "pkg/b": false}`)
	if err != nil {
		t.Fatalf("parseGateOutput: %v", err)
	}
	if len(got) != 2 || got["pkg/b"] || !got["pkg/a"] {
		t.Fatalf("parsed = %v, want pkg/a true and pkg/b false", got)
	}
}

// A DECLARED REPORT PATH IS READ INSTEAD OF THE OUTPUT, which is how a runner that cannot write to stdout
// (surefire, pytest) is supported without a wrapper.
func TestADeclaredReportPathIsReadFromDisk(t *testing.T) {
	dir := t.TempDir()
	report := `{"pkg/a": false}`
	if err := os.WriteFile(filepath.Join(dir, "report.json"), []byte(report), 0o644); err != nil {
		t.Fatal(err)
	}
	decl := verify.Declaration{Command: "make test", Format: verify.FormatJSON, Path: "report.json"}
	r := &TaskReconciler{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	// The command's own output says nothing; only the file carries the results.
	got, err := r.parseGateOutput(decl, dir, "BUILD SUCCESSFUL\n")
	if err != nil {
		t.Fatalf("parseGateOutput: %v", err)
	}
	if got["pkg/a"] {
		t.Fatalf("parsed = %v, want pkg/a false (from the report file)", got)
	}
}

// NO DECLARED FORMAT IS AN ERROR, NEVER AN EMPTY MAP. An empty map reads as "everything passed", which is how a
// verification system starts certifying work it never measured.
func TestNoDeclaredFormatIsAnErrorNotAnEmptyMap(t *testing.T) {
	r := &TaskReconciler{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	got, err := r.parseGateOutput(verify.Declaration{Command: "make test"}, t.TempDir(),
		"ok  pkg/a\nFAIL  pkg/b\n")
	if err == nil {
		t.Fatalf("a declaration with no result format was accepted; returned %v", got)
	}
	if got != nil {
		t.Fatalf("a non-nil result map came back alongside the error: %v", got)
	}
	if !strings.Contains(err.Error(), "format") {
		t.Errorf("the error must name the missing format, got %q", err)
	}
}

// TestBaselineCacheRoundTrip pins the cache, including that a corrupt or incomplete entry reads as a
// MISS rather than a clean baseline.
func TestBaselineCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	commit := "abc123def456"

	if b := loadCachedBaseline(dir, commit); b != nil {
		t.Fatalf("expected no baseline before capture, got %+v", b)
	}

	want := &verify.Baseline{Commit: commit, Results: map[string]bool{"pkg/a": true, "pkg/b": false}}
	if err := storeCachedBaseline(dir, want); err != nil {
		t.Fatalf("store: %v", err)
	}
	got := loadCachedBaseline(dir, commit)
	if got == nil {
		t.Fatal("baseline did not round-trip")
	}
	if !reflect.DeepEqual(got.Results, want.Results) {
		t.Fatalf("results = %v, want %v", got.Results, want.Results)
	}

	// An INCOMPLETE baseline is not cached at all — storing one would let a failed measurement
	// masquerade as a clean one.
	incomplete := &verify.Baseline{Commit: "other", Detail: "gate could not run"}
	if err := storeCachedBaseline(dir, incomplete); err != nil {
		t.Fatalf("store incomplete: %v", err)
	}
	if b := loadCachedBaseline(dir, "other"); b != nil {
		t.Fatalf("an incomplete baseline must not be cached, got %+v", b)
	}

	// A corrupt file reads as absent.
	if err := os.WriteFile(filepath.Join(dir, ".orchicon", baselineCacheDir, "corrupt.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}
	if b := loadCachedBaseline(dir, "corrupt"); b != nil {
		t.Fatalf("corrupt cache must read as absent, got %+v", b)
	}
}

// TestSummariseDirty pins the failure detail: it must name files, not just say "dirty".
func TestSummariseDirty(t *testing.T) {
	out := " M internal/foo.go\n?? internal/bar.go\nA  internal/baz.go\n M internal/qux.go\n M internal/extra.go\n"
	got := summariseDirty(out)
	for _, want := range []string{"internal/foo.go", "internal/bar.go", "internal/baz.go", "internal/qux.go"} {
		if !contains(got, want) {
			t.Errorf("summary %q missing %q", got, want)
		}
	}
	// Capped at four entries with an ellipsis, so a huge diff cannot bloat an error_message row.
	if !contains(got, "…") {
		t.Errorf("summary %q should be elided when more entries exist", got)
	}
}

// TestPathSet pins diff-name parsing including blank lines.
func TestPathSet(t *testing.T) {
	got := pathSet("a.go\n\nb.go\n  c.go  \n")
	want := map[string]struct{}{"a.go": {}, "b.go": {}, "c.go": {}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if empty := pathSet("\n\n"); len(empty) != 0 {
		t.Fatalf("empty diff should yield an empty set, got %v", empty)
	}
}

// TestStepDeclaresDeliverableExemptions pins WHICH steps are exempt from the commit check.
//
// The default is true on purpose: wrongly expecting a deliverable produces a loud, fixable "no
// commits", whereas wrongly exempting a code step silently removes the only universal check there is.
// So the exemption list must stay short and each entry justified.
func TestStepDeclaresDeliverableExemptions(t *testing.T) {
	r := &TaskReconciler{}
	cases := []struct {
		name        string
		stepID      string
		workerName  string
		wantDeclare bool
	}{
		{"senior engineer owes code", "step-sse", "Senior Software Engineer", true},
		{"quick worker owes code", "step-quick", "Quick Software Engineer", true},
		{"QA owes fixes", "step-qa", "QA Engineer", true},
		{"reviewer owes fixes", "step-pr-reviewer", "PR Reviewer", true},
		{"devops owes a PR", "step-devops-pr", "DevOps Engineer", true},
		{"architect writes an ignored plan", "step-q4xlbg6v", "Principal Software Architect", false},
		{"design approver is a gate", "step-i64wso0x", "Design Approver", false},
		{"code approver is a gate", "step-9gfybwnv", "Code Approver", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			declares, reason := r.stepDeclaresDeliverable(nil, execFor(tc.stepID, tc.workerName))
			if declares != tc.wantDeclare {
				t.Fatalf("declares = %v, want %v (reason: %q)", declares, tc.wantDeclare, reason)
			}
			if !declares && reason == "" {
				t.Error("an exemption must say WHY, for the record")
			}
		})
	}
}

// TestStandaloneDispatchOwesDeliverable pins the no-workflow case.
func TestStandaloneDispatchOwesDeliverable(t *testing.T) {
	r := &TaskReconciler{}
	declares, _ := r.stepDeclaresDeliverable(nil, execFor("", "Senior Software Engineer"))
	if !declares {
		t.Fatal("a standalone dispatch was assigned a work item, so it owes a deliverable")
	}
}

// execFor builds the minimal execution row the deliverable check reads.
func execFor(stepID, workerName string) db.ExecutionRow {
	return db.ExecutionRow{WorkflowStepID: stepID, WorkerName: workerName}
}

// TestFormatVerificationSummary pins the operator-facing evidence block. The failure mode this guards
// is a review that reports a clean run while nothing was checked.
func TestFormatVerificationSummary(t *testing.T) {
	mk := func(name, verdict, reason string) db.WorkflowStepRunRow {
		meta := map[string]any{"_verification": map[string]any{"verdict": verdict, "reason": reason}}
		raw, _ := json.Marshal(meta)
		return db.WorkflowStepRunRow{StepName: name, Result: raw, Status: "succeeded"}
	}

	t.Run("unsubstantiated sorts first and is named", func(t *testing.T) {
		got := formatVerificationSummary([]db.WorkflowStepRunRow{
			mk("Senior Software Engineer", "verified", ""),
			mk("QA Engineer", "unsubstantiated", "work_present: HEAD did not move"),
		})
		if !contains(got, "QA Engineer") || !contains(got, "✗") {
			t.Fatalf("missing the contradicted step: %s", got)
		}
		// The contradicted line must come before the verified one.
		if indexOf(got, "QA Engineer") > indexOf(got, "Senior Software Engineer") {
			t.Errorf("contradicted step should sort before verified steps:\n%s", got)
		}
	})

	t.Run("no evidence at all says so explicitly", func(t *testing.T) {
		got := formatVerificationSummary(nil)
		if !contains(got, "UNCONFIRMED") {
			t.Fatalf("an unverified run must say so, got: %s", got)
		}
	})

	t.Run("unverified is counted separately from verified", func(t *testing.T) {
		got := formatVerificationSummary([]db.WorkflowStepRunRow{
			mk("Senior Software Engineer", "verified", ""),
			mk("QA Engineer", "unverified", "project_gate: no .orchicon/verify"),
		})
		if !contains(got, "1 of 2 step(s) verified") {
			t.Fatalf("must count verified vs total, got: %s", got)
		}
		if !contains(got, "not a failure") {
			t.Errorf("must explain that unverified is not a failure, got: %s", got)
		}
	})

	t.Run("all verified reads cleanly", func(t *testing.T) {
		got := formatVerificationSummary([]db.WorkflowStepRunRow{mk("Senior Software Engineer", "verified", "")})
		if !contains(got, "All 1 step(s) verified") {
			t.Fatalf("got: %s", got)
		}
	})

	t.Run("superseded steps are excluded", func(t *testing.T) {
		row := mk("SSE", "unsubstantiated", "x")
		row.SupersededBy = "later"
		got := formatVerificationSummary([]db.WorkflowStepRunRow{row})
		if contains(got, "SSE") {
			t.Fatalf("a superseded step is not final work and must not appear: %s", got)
		}
	})
}

// TestAppendACReportCheck pins how the worker's own acceptance-criteria report folds into the verdict.
//
// This is the one semantic-adjacent check in the system, so its posture is deliberate: it surfaces what
// the worker admitted rather than judging whether the work is good.
func TestAppendACReportCheck(t *testing.T) {
	base := func() verify.Result {
		r := verify.Result{}
		r.Add("work_present", verify.CheckPass, "1 commit")
		return r
	}
	cases := []struct {
		name, output string
		want         verify.Verdict
		why          string
	}{
		{
			name: "no report block abstains", output: "just prose",
			want: verify.VerdictUnverified,
			why:  "many items carry no acceptance criteria, so skipping the section is not evidence of a problem",
		},
		{
			name: "all criteria pass with evidence", output: "AC1: PASS — go test ./a → ok\nAC2: PASS — internal/x.go:12",
			want: verify.VerdictVerified,
			why:  "a fully evidenced report is the contract being met",
		},
		{
			name: "pass without evidence abstains", output: "AC1: PASS — satisfied",
			want: verify.VerdictUnverified,
			why:  "an adjective is not an answer, but the evidence heuristic must not fail an honest step",
		},
		{
			name: "an explicit FAIL contradicts a success claim", output: "AC1: FAIL — expected x observed y",
			want: verify.VerdictUnsubstantiated,
			why:  "the worker's own report says a criterion did not pass while its summary says success",
		},
		{
			name: "NOT VERIFIED contradicts a success claim", output: "AC1: NOT VERIFIED — no app available",
			want: verify.VerdictUnsubstantiated,
			why:  "a worker that could not establish a criterion has told the operator the step is not done",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := appendACReportCheck(base(), tc.output, "")
			if got.Verdict != tc.want {
				t.Fatalf("verdict = %q, want %q — %s (reason: %s)", got.Verdict, tc.want, tc.why, got.Reason)
			}
		})
	}
}

// THE COMPLETION GATE CARRIES NO INVENTED TIME LIMIT — the operator's rule, and the one thing this file used to
// pin the OPPOSITE of.
//
// The gate runs SYNCHRONOUSLY inside OnResult, and the old code answered that by inventing a 3-minute ceiling
// (then, before that, borrowing the integration path's 10-minute one). Both are gone. A ceiling nobody configured
// is a hidden policy that fails good work on a slow machine, and it is not the mechanism for a runaway: stall
// detection, tool-hang cancellation and the execution reaper are, and each of them acts on evidence of being stuck
// rather than on elapsed time.
//
// WHAT REPLACES IT IS CANCELLABILITY, and that is a real property worth pinning: the run's own context reaches the
// command, so an operator's cancel, a container going away, or a budget the OPERATOR set all stop the gate. That
// bound is somebody's decision; a number in this file is not.
func TestTheCompletionGateHasNoInventedTimeout(t *testing.T) {
	src, err := os.ReadFile("verify_evidence.go")
	if err != nil {
		t.Fatalf("read verify_evidence.go: %v", err)
	}
	// A SOURCE ASSERTION, and legitimately so: the property is the ABSENCE of a local deadline, and there is no
	// runtime seam that can observe a timeout that never fires. This proves the wiring is gone; the cancellation
	// half is proved by TestTheCompletionGateHonoursCancellation below.
	for _, banned := range []string{"context.WithTimeout", "context.WithDeadline", "time.AfterFunc"} {
		if strings.Contains(string(src), banned) {
			t.Errorf("verify_evidence.go still arms a local bound (%s); the run's own context is the only "+
				"limit this file may apply", banned)
		}
	}
}

// AND THE BOUND THAT DOES EXIST IS THE CALLER'S: a cancelled run context stops the gate.
//
// This is the property the removed timeouts were trying to buy, obtained the right way — from the context that
// already knows the execution was cancelled.
func TestTheCompletionGateHonoursCancellation(t *testing.T) {
	r := &TaskReconciler{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	decl := verify.Declaration{Command: "sleep 30", Format: verify.FormatExitCode}
	start := time.Now()
	_, _, err := r.runCompletionGate(ctx, t.TempDir(), decl)
	if err == nil {
		t.Fatal("a cancelled context did not stop the gate")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the gate ran for %s after cancellation; it must stop promptly", elapsed)
	}
}

// TestCompletionGateTimeoutAbstainsNotFails pins the posture on timeout: unverified, never failed.
func TestCompletionGateTimeoutAbstainsNotFails(t *testing.T) {
	r := &TaskReconciler{}
	// A command that outlives the budget is not something we can wait for in a test; instead assert the
	// contract at the decision layer: a declared gate that never ran yields an ABSTENTION.
	ev := verify.Evidence{
		GitReadable:             true,
		BaseRef:                 "develop",
		CommitsAhead:            1,
		ChangedFiles:            map[string]struct{}{"a.go": {}},
		DeclaredCommand:         "make test",
		DeclaredRan:             false, // never completed
		StepDeclaresDeliverable: true,
		TreeClean:               boolPtr(true),
		Baseline:                nil,
		HeadMeasured:            false,
	}
	got := verify.Collect(context.Background(), ev)
	if got.Verdict == verify.VerdictUnsubstantiated {
		t.Fatalf("a gate that could not be measured must never be reported as a failure: %s", got.Reason)
	}
	if got.Verdict != verify.VerdictUnverified {
		t.Fatalf("verdict = %q, want unverified", got.Verdict)
	}
	_ = r
}

// TestBaselineCaptureAgainstARealRepo is the outermost proof for the baseline half, and it is a REAL
// integration test rather than a fixture on purpose: the mechanism's whole claim is that it can measure
// an arbitrary project's gate, and a synthetic Baseline map would prove only that the map round-trips.
//
// It is skipped unless a declared gate is present above the test directory, so it is inert in a checkout
// that has no `.orchicon/verify` — which is most of them.
//
// THE GATE IS NARROWED VIA THE PROJECT DIRECTORY, and getting that wrong is what this test got wrong
// first. captureBaseline reads its command from projectDir/.orchicon/verify, so pointing projectDir back
// at the repository root made it run the project's FULL gate — 100+ packages, and enough output to trip
// the truncation cap. The test therefore passes a SCRATCH project dir carrying a one-package gate. This is
// also why the truncation bug it exposed was a real find and not a test artifact: production hits the same
// cap with the full gate.
func TestBaselineCaptureAgainstARealRepo(t *testing.T) {
	// OPT-IN, and it is opt-in for a concrete reason that cost a real rebuild: this test creates a git
	// worktree and runs a real Go test invocation inside it. Even narrowed to two packages that is tens of
	// seconds, and its first version was pointed at the repository root — so it ran the project's FULL
	// gate and blocked `make rebuild-dev` for five minutes before failing. A verification test that makes
	// the build worse is not worth having on by default.
	//
	// Run it deliberately, after changing the baseline mechanism:
	//
	//	ORCHICON_VERIFY_E2E=1 go test ./internal/scheduler/ -run TestBaselineCaptureAgainstARealRepo -v
	//
	// Same posture as the standing `make tui-pty-gate`: a real-surface proof that is real enough to be
	// expensive, so it is asked for rather than assumed.
	if os.Getenv("ORCHICON_VERIFY_E2E") != "1" {
		t.Skip("opt-in: set ORCHICON_VERIFY_E2E=1 to run the real-gate baseline proof")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	repoOut, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skip("not in a git work tree")
	}
	repo := strings.TrimSpace(string(repoOut))
	if _, err := os.Stat(filepath.Join(repo, ".orchicon", "verify")); err != nil {
		t.Skip("this repository declares no .orchicon/verify gate")
	}

	refOut, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	ref := strings.TrimSpace(string(refOut))

	// A scratch project dir whose ONLY job is to carry a narrowed gate. captureBaseline resolves its
	// command from here and its scratch checkout under here, so the test measures the MECHANISM against a
	// real repository without running this repository's whole suite.
	scratch := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scratch, ".orchicon"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The narrowed gate must run in the CHECKOUT (which is a worktree of the real repo), not in the
	// scratch dir — so it is expressed as a real command the checkout can satisfy.
	// THE DECLARATION NOW CARRIES A FORMAT, because the platform no longer guesses one out of the output. `go test
	// -json` is Go's own machine-readable form, so this measures the real path a Go project would use with no
	// wrapper installed.
	declBody := "command = go test -json ./internal/verify/ ./internal/workitem/\nformat = json\n"
	if err := os.WriteFile(filepath.Join(scratch, ".orchicon", "verify"), []byte(declBody), 0o644); err != nil {
		t.Fatal(err)
	}

	r := &TaskReconciler{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// The scratch checkout is created from the REPO, so pass the repo as the git project and the scratch
	// dir as the gate declaration. captureBaseline takes one dir for both, so the helper below performs
	// the same two steps explicitly — the point is to exercise the real scratchCheckout + runGateCommand +
	// parseGateResults path, not to re-test the wiring.
	dir, cleanup, cerr := r.scratchCheckout(ctx, repo, ref)
	if cerr != nil {
		t.Fatalf("scratchCheckout: %v", cerr)
	}
	defer cleanup()

	decl, derr := declaredGate(scratch)
	if derr != nil {
		t.Fatalf("declaredGate: %v", derr)
	}
	out, runErr := r.runGateCommand(ctx, dir, decl)
	if runErr != nil {
		t.Logf("gate exited non-zero (fine — the baseline records per-check results): %v", runErr)
	}
	results, perr := r.parseGateOutput(decl, dir, out)
	if perr != nil {
		t.Fatalf("parseGateOutput: %v (tail: %s)", perr, truncateTail(out, 800))
	}
	if len(results) == 0 {
		t.Fatalf("the gate produced no per-check results; tail: %s", truncateTail(out, 800))
	}
	if _, ok := results["github.com/beardedparrott/orchicon/internal/verify"]; !ok {
		t.Fatalf("the verify package is missing from the measured results: %v", results)
	}

	b := &verify.Baseline{Commit: ref, Results: results}
	// The cache is keyed by commit, which is what makes sibling runs share one measurement.
	if err := storeCachedBaseline(scratch, b); err != nil {
		t.Fatalf("store: %v", err)
	}
	got := loadCachedBaseline(scratch, ref)
	if got == nil {
		t.Fatal("a cached baseline did not load back — every sibling run would re-measure")
	}
	if len(got.Results) != len(b.Results) {
		t.Fatalf("round-trip changed the result set: %d vs %d", len(got.Results), len(b.Results))
	}
}

// THE OUTPUT CAP CANNOT COST A MEASUREMENT ANY MORE, and that is a stronger guarantee than the old fix.
//
// THE BUG THIS REPLACES: the cap used to be a FRONT cut, so a transcript larger than 64KB silently lost every
// result after the cut. On `make rebuild-dev` the baseline ended at internal/runtimeimage and everything after it
// vanished from BOTH sides of the comparison — so a pre-existing failure got blamed on the step, and a genuine
// regression read as "no new failures". The first fix kept "result-shaped lines" from the elided middle, which
// worked but meant the cap had to know the result FORMAT — the pattern-matching defect this change removes.
//
// THE STRUCTURAL FIX: parsing happens on the FULL transcript and the cap is applied only to what the RECORD
// keeps. So a transcript of any size yields every result, and the cap becomes a record-size decision that cannot
// change a verdict.
func TestAHugeTranscriptStillYieldsEveryResult(t *testing.T) {
	// A NDJSON stream (go test -json) whose bulk dwarfs the cap, with packages spread first to last.
	var b strings.Builder
	const n = 400
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "{\"Action\":\"pass\",\"Package\":\"example.com/pkg%03d\"}\n", i)
		b.WriteString(strings.Repeat("noise line that is not an event\n", 40))
	}
	b.WriteString("{\"Action\":\"fail\",\"Package\":\"example.com/zzz-last\"}\n")

	full := b.String()
	if len(full) <= maxGateOutput {
		t.Fatalf("test transcript is only %d bytes; it must exceed the %d cap to be meaningful", len(full), maxGateOutput)
	}
	if len(capGateOutput(full)) > maxGateOutput {
		t.Fatalf("capped output exceeds the %d cap", maxGateOutput)
	}

	// PARSED FROM THE FULL TRANSCRIPT — which is what the collector does.
	results, err := verify.ParseResults(verify.FormatJSON, full)
	if err != nil {
		t.Fatalf("ParseResults: %v", err)
	}
	if len(results) != n+1 {
		t.Fatalf("parsed %d packages, want %d — the transcript's size lost measurements", len(results), n+1)
	}
	if results["example.com/zzz-last"] {
		t.Error("the LAST package is recorded as passing; it failed")
	}
	if !results["example.com/pkg000"] {
		t.Error("the FIRST package is missing from the results")
	}
}

// TestCapGateOutputIsIdentityWhenSmall pins that the fast path does not mangle a short transcript.
func TestCapGateOutputIsIdentityWhenSmall(t *testing.T) {
	in := "ok  \tgithub.com/x/a\t0.1s\nFAIL\tgithub.com/x/b\t0.2s\n"
	if got := capGateOutput(in); got != in {
		t.Fatalf("a small transcript must pass through unchanged, got %q", got)
	}
}

// TestInterruptedGateProducesNoBaseline is the regression test for the bug `make rebuild-dev` exposed.
//
// THE BUG: captureBaseline accepted any non-zero exit WITH parseable output as a valid baseline. But a
// gate that was KILLED (timeout) also exits non-zero and also prints the packages that finished before
// the kill. So a partial run was recorded as a complete baseline, cached per commit, and every package
// that never ran became invisible to the differential — because NewFailures skips keys absent from the
// baseline. A regression in an unmeasured package could then NEVER be reported. That is a false negative
// in a verification system, the worst failure available, and it is silent.
//
// Observed exactly: the test's own context expired at 300s mid-run; ~50 packages were parsed; the stored
// baseline ended at internal/runtimeimage with internal/verify missing.
func TestInterruptedGateProducesNoBaseline(t *testing.T) {
	// A cancelled context is the interruption signal that matters most.
	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if !interrupted(ctx, nil) {
			t.Fatal("a cancelled context must count as interrupted even when the command reported no error")
		}
	})
	// The runner's own budget expiry surfaces as wording, not always as ctx.Err — because it uses its
	// own WithTimeout internally and returns the chain's error.
	t.Run("runner timeout wording", func(t *testing.T) {
		err := errors.New("timed out after 10m0s")
		if !interrupted(context.Background(), err) {
			t.Fatal("the runner's timeout error must count as interrupted")
		}
	})
	// The benign, expected case: the gate finished and some checks failed.
	t.Run("completed with failures is not interrupted", func(t *testing.T) {
		if interrupted(context.Background(), errors.New("exit status 1")) {
			t.Fatal("a gate that ran to completion and failed is a VALID baseline — it tells us what was already broken")
		}
	})
	t.Run("clean run is not interrupted", func(t *testing.T) {
		if interrupted(context.Background(), nil) {
			t.Fatal("a clean run is not interrupted")
		}
	})
}

// TestNotApplicableIsRenderedDistinctly pins that the operator can tell the three states apart in the
// acceptance review. Counting a not-applicable step as "verified" would inflate the confirmed total, and
// counting it as "unverified" would train the operator to ignore that word where it matters.
func TestNotApplicableIsRenderedDistinctly(t *testing.T) {
	mk := func(name, verdict string) db.WorkflowStepRunRow {
		raw, _ := json.Marshal(map[string]any{
			"_verification": map[string]any{"verdict": verdict, "reason": ""},
		})
		return db.WorkflowStepRunRow{StepName: name, Result: raw, Status: "succeeded"}
	}

	got := formatVerificationSummary([]db.WorkflowStepRunRow{
		mk("Principal Software Architect", "not_applicable"),
		mk("Senior Software Engineer", "verified"),
	})
	if !strings.Contains(got, "·") {
		t.Errorf("a not-applicable step must have its own mark:\n%s", got)
	}
	if !strings.Contains(got, "1 of 2 step(s) verified") {
		t.Errorf("verified must count the confirmed steps only, not every step:\n%s", got)
	}
	if !strings.Contains(got, "not mechanically checked") {
		t.Errorf("the summary must say what the not-applicable row means:\n%s", got)
	}

	// A run made only of planning/approval steps must read honestly, not as a clean sweep.
	only := formatVerificationSummary([]db.WorkflowStepRunRow{mk("Architect", "not_applicable")})
	if strings.Contains(only, "All 1 step(s) verified") {
		t.Errorf("a run with nothing checkable must not read as fully verified:\n%s", only)
	}
	if !strings.Contains(only, "nothing here was mechanically confirmed") {
		t.Errorf("it must say plainly that nothing was confirmed:\n%s", only)
	}
}

// TestNotApplicableDoesNotFailOrNotice pins the status consequence: a plan-only step succeeds quietly.
func TestNotApplicableDoesNotFailOrNotice(t *testing.T) {
	ok, notice := applyVerdict(true, verify.Result{Verdict: verify.VerdictNotApplicable})
	if !ok {
		t.Fatal("a not-applicable step must not be failed — it did its job")
	}
	if notice != "" {
		t.Fatalf("a not-applicable step must not raise an advisory notice (%q): flagging every plan and "+
			"approval step is how the notice stops meaning anything", notice)
	}
}

// TestPreExistingRedGateStillFailsARegression is the guard on the fix: relaxing the binary gate check must
// not blind the differential. A real regression on a repo that is ALREADY red must still fail the step.
func TestPreExistingRedGateStillFailsARegression(t *testing.T) {
	got := verify.Collect(context.Background(), verify.Evidence{
		GitReadable: true, BaseRef: "develop", CommitsAhead: 1,
		ChangedFiles:            map[string]struct{}{"internal/a/a.go": {}},
		StepDeclaresDeliverable: true,
		TreeClean:               boolPtr(true),
		DeclaredCommand:         "make test", DeclaredRan: true, DeclaredPassed: false,
		DeclaredDetail: "FAIL\tpkg/old\t0.1s\nFAIL\tpkg/new\t0.2s\n",
		// Already red at baseline (pkg/old), and this step broke pkg/new as well.
		Baseline:     &verify.Baseline{Commit: "b", Results: map[string]bool{"pkg/old": false, "pkg/new": true}},
		HeadResults:  map[string]bool{"pkg/old": false, "pkg/new": false},
		HeadMeasured: true,
	})
	if got.Verdict != verify.VerdictUnsubstantiated {
		t.Fatalf("verdict = %q, want unsubstantiated — a repo that is already red must NOT hide a NEW "+
			"failure (reason: %s)", got.Verdict, got.Reason)
	}
	if !strings.Contains(got.Reason, "pkg/new") {
		t.Errorf("the reason must name the REGRESSED check, not the pre-existing one: %q", got.Reason)
	}
	if strings.Contains(got.Reason, "pkg/old") {
		t.Errorf("the pre-existing failure must not be blamed on this step: %q", got.Reason)
	}
}

// NOTHING THE VERIFIER FINDS CAN FAIL A STEP — the operator's rule, asserted over EVERY verdict.
//
// This is the test that would have caught the "too strict" product defect: whatever the evidence says, the step's
// status is the worker's and the process's to decide. A regression in applyVerdict that reintroduced a downgrade
// would fail here, for every verdict at once, rather than only for the one case someone remembered.
func TestNoVerdictCanFailAStep(t *testing.T) {
	for _, v := range []verify.Verdict{
		verify.VerdictVerified, verify.VerdictUnverified,
		verify.VerdictUnsubstantiated, verify.VerdictNotApplicable,
	} {
		t.Run(string(v), func(t *testing.T) {
			// A worker that succeeded keeps succeeding, whatever the evidence concluded.
			if ok, _ := applyVerdict(true, verify.Result{Verdict: v}); !ok {
				t.Errorf("verdict %q failed a step that the worker reported as succeeding — the verifier must "+
					"never change a status", v)
			}
			// A worker that failed keeps failing, and is never upgraded either.
			if ok, _ := applyVerdict(false, verify.Result{Verdict: v}); ok {
				t.Errorf("verdict %q upgraded a FAILED worker — evidence never outranks a real failure", v)
			}
		})
	}
}

// AND THE ONE CASE THAT USED TO FAIL NOW LANDS GREEN WITH A NOTICE ATTACHED — stated explicitly, because it is the
// cost of the rule above and it should not be discovered later by surprise.
//
// A worker reports success having changed nothing. The evidence says so (work_present failed, verdict
// unsubstantiated). The step is SUCCEEDED, and the only trace is the notice.
func TestAnEmptyClaimLandsGreenWithANotice(t *testing.T) {
	res := verify.Collect(context.Background(), verify.Evidence{
		GitReadable: true, BaseRef: "develop", CommitsAhead: 0,
		ChangedFiles:            map[string]struct{}{},
		StepDeclaresDeliverable: true,
		TreeClean:               boolPtr(true),
		DeclaredCommand:         "make test", DeclaredRan: true, DeclaredPassed: true,
		Baseline:     &verify.Baseline{Commit: "b", Results: map[string]bool{"pkg/a": true}},
		HeadResults:  map[string]bool{"pkg/a": true},
		HeadMeasured: true,
	})
	if res.Verdict != verify.VerdictUnsubstantiated {
		t.Fatalf("fixture: verdict = %q, want unsubstantiated (no commits is a contradiction)", res.Verdict)
	}
	ok, notice := applyVerdict(true, res)
	if !ok {
		t.Error("a step that changed nothing failed — the operator's rule is that the verifier never fails a step")
	}
	if notice == "" {
		t.Error("the step landed green SILENTLY; the evidence must still say what it found")
	}
	if !strings.Contains(notice, "does not support") {
		t.Errorf("the notice must say the repository does not support the claim, got %q", notice)
	}
}

// THE SCRATCH CHECKOUT MUST BE ABLE TO RUN THE PROJECT'S GATE — the operator's "stop assuming the gate can run
// with nothing installed in a scratch worktree".
//
// A git worktree carries TRACKED files only, so node_modules, .dev, vendor and every build cache are absent and a
// gate that needs any of them dies instantly. That failure would be recorded AS the baseline, which is worse than
// no baseline: it looks like a measurement.
//
// MEASURED BEFORE THE FIX: `npx vitest --version` in a fresh scratch checkout of this repository exited 1 with
// "npx canceled due to missing packages", because nothing had linked frontend/node_modules in.
//
// SKIPPED when the repository has no ignored dependency tree to link — it is a real integration test that needs a
// real checkout, and most test runs have neither.
func TestTheScratchCheckoutCanRunTheProjectsGate(t *testing.T) {
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Skip("no repository root")
	}
	if _, serr := os.Stat(filepath.Join(repo, ".git")); serr != nil {
		t.Skip("not a checkout")
	}
	r := &TaskReconciler{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	dir, cleanup, cerr := r.scratchCheckout(ctx, repo, "HEAD")
	if cerr != nil {
		t.Skipf("scratch checkout unavailable: %v", cerr)
	}
	defer cleanup()

	// The dependency this repository's FRONTEND gate needs, as the concrete instance of the class.
	deps := filepath.Join(dir, "frontend", "node_modules")
	if _, serr := os.Stat(filepath.Join(repo, "frontend", "node_modules")); serr != nil {
		t.Skip("this checkout has no frontend node_modules to link")
	}
	if _, serr := os.Stat(deps); serr == nil {
		t.Fatalf("fixture: the scratch checkout already has frontend/node_modules — the test cannot prove the link")
	}
	if lerr := linkIgnoredDependencies(ctx, repo, dir); lerr != nil {
		t.Fatalf("linkIgnoredDependencies: %v", lerr)
	}
	fi, serr := os.Lstat(deps)
	if serr != nil {
		t.Fatalf("frontend/node_modules is still absent from the scratch checkout (%v) — a gate that needs it "+
			"cannot run, so the baseline would measure a tree nothing can build", serr)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("frontend/node_modules is not a symlink; installing a copy would measure a DIFFERENT dependency " +
			"set from the one the project actually runs")
	}
}
