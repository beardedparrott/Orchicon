package scheduler

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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
			name: "unsubstantiated DOWNGRADES to failed", succeeded: true,
			verdict: verify.VerdictUnsubstantiated, wantSucceeded: false, wantNotice: true,
			why: "the repository contradicts the claim — this is the defect case",
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

// TestUnsubstantiatedNoticeNamesTheReason pins that the failure reason an operator sees says WHAT was
// contradicted. A failure with no cause sends them to a terminal, which is the cost this feature is
// supposed to remove.
func TestUnsubstantiatedNoticeNamesTheReason(t *testing.T) {
	_, notice := applyVerdict(true, verify.Result{
		Verdict: verify.VerdictUnsubstantiated,
		Reason:  "not substantiated: work_present: HEAD did not move",
	})
	for _, want := range []string{"contradicts", "work_present"} {
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

// TestParseGateResultsGoTest pins the parser against real `go test` output shapes.
func TestParseGateResultsGoTest(t *testing.T) {
	out := `ok  	github.com/x/a	0.012s
FAIL	github.com/x/b [build failed]
--- FAIL: TestC (0.00s)
--- PASS: TestD (0.00s)
ok  	github.com/x/e	(cached)
`
	got := parseGateResults(out)
	want := map[string]bool{
		"github.com/x/a": true,
		"github.com/x/b": false,
		"TestC":          false,
		"TestD":          true,
		"github.com/x/e": true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed = %v\nwant    = %v", got, want)
	}
}

// TestParseGateResultsVitest pins a non-Go runner, because the gate must not be Go-only: the point of
// a project-declared command is that it can be anything.
func TestParseGateResultsVitest(t *testing.T) {
	out := ` FAIL  src/foo.test.ts > renders
 PASS  src/bar.test.ts > counts
`
	got := parseGateResults(out)
	if got["src/foo.test.ts"] != false {
		t.Errorf("src/foo.test.ts = %v, want false (parsed: %v)", got["src/foo.test.ts"], got)
	}
	if got["src/bar.test.ts"] != true {
		t.Errorf("src/bar.test.ts = %v, want true (parsed: %v)", got["src/bar.test.ts"], got)
	}
}

// TestParseGateResultsEmptyIsEmptyNotGreen pins that unparseable output yields an EMPTY map, never a
// synthetic pass. An empty map is turned into an incomplete baseline by the caller; a synthetic
// "everything passed" would silently certify a gate that never ran.
func TestParseGateResultsEmptyIsEmptyNotGreen(t *testing.T) {
	if got := parseGateResults("gibberish\nno results here\n"); len(got) != 0 {
		t.Fatalf("parsed = %v, want empty", got)
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
			got := appendACReportCheck(base(), tc.output)
			if got.Verdict != tc.want {
				t.Fatalf("verdict = %q, want %q — %s (reason: %s)", got.Verdict, tc.want, tc.why, got.Reason)
			}
		})
	}
}

// TestCompletionGateIsBounded pins that the completion-path gate cannot wedge a run.
//
// This is the one place the evidence gate touches the critical path: it runs SYNCHRONOUSLY inside
// OnResult, while the execution is still formally `running` and every later reconcile pass for that row
// is behind it. Reusing the integration path's 10-minute budget here would mean a slow gate holds a
// terminal status for ten minutes — a wedge, which is strictly worse than an unmeasured gate, because a
// wedge strands the run while a timeout merely abstains.
func TestCompletionGateIsBounded(t *testing.T) {
	if completionGateTimeout <= 0 {
		t.Fatal("the completion gate must have a positive bound")
	}
	if completionGateTimeout >= 10*time.Minute {
		t.Fatalf("completionGateTimeout = %s; it must be strictly shorter than the integration "+
			"path's 10m budget, or a slow gate wedges OnResult", completionGateTimeout)
	}
	// It must still be long enough for a real project's warm-cache suite, or it would abstain always
	// and the gate would be decorative.
	if completionGateTimeout < time.Minute {
		t.Fatalf("completionGateTimeout = %s; too short for a real gate to ever measure anything",
			completionGateTimeout)
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
