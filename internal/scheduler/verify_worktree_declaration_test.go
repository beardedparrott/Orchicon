package scheduler

// verify_worktree_declaration_test.go — THE DECLARATION IS READ FROM THE COMMIT UNDER TEST.
//
// THE DEFECT, measured on execution 01M6W2KC1CQCH6FFZ99G132F6S:
//
//	the worker's AC1 was CREATING `.orchicon/verify` on its run branch (faf69a7, contents `node scripts/gate.mjs`)
//	while the project checkout sat on another branch with no such file.
//
// The platform read the CHECKOUT, found nothing, and told the operator "the project declares no gate (no
// .orchicon/verify)" — about a project that, at the commit under test, declares one. The baseline could not be
// taken either (same read, same miss), so the notice carried a second line about a baseline that was never going
// to exist. Two abstentions, one cause.
//
// A gate declaration is PART OF THE REPOSITORY, so the one that governs a step is the one at that step's HEAD.

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/beardedparrott/orchicon/internal/verify"
)

// THE WORKTREE WINS when there is one.
func TestTheDeclarationIsReadFromTheWorktree(t *testing.T) {
	project := t.TempDir()
	worktree := t.TempDir()
	// The checkout declares a DIFFERENT gate from the worktree — so a test that passes cannot be reading the
	// checkout by accident.
	writeGateFile(t, project, "command = echo checkout\nformat = exit_code\n")
	writeGateFile(t, worktree, "command = echo worktree\nformat = json\n")

	got := gateDeclarationDir(worktree, project)
	if got != worktree {
		t.Fatalf("gateDeclarationDir = %q, want the worktree %q", got, worktree)
	}
	d, err := declaredGate(got)
	if err != nil {
		t.Fatalf("declaredGate: %v", err)
	}
	if d.Command != "echo worktree" {
		t.Errorf("read the command %q; the declaration must come from the commit under test", d.Command)
	}
	if d.Format != "json" {
		t.Errorf("read the format %q, want json", d.Format)
	}
}

// AND THE PROJECT DIRECTORY IS THE FALLBACK for the in-place case, where there is no worktree at all.
func TestTheDeclarationFallsBackToTheProjectWithoutAWorktree(t *testing.T) {
	project := t.TempDir()
	writeGateFile(t, project, "command = echo inplace\nformat = exit_code\n")
	if got := gateDeclarationDir("", project); got != project {
		t.Fatalf("gateDeclarationDir = %q, want the project %q", got, project)
	}
}

// writeGateFile writes a `.orchicon/verify` under dir.
func writeGateFile(t *testing.T, dir, body string) {
	t.Helper()
	sub := filepath.Join(dir, ".orchicon")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "verify"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// NOW THE WHOLE EXECUTION, REPLAYED. Execution 01M6W2KC1CQCH6FFZ99G132F6S in one test:
//
//	the base commit declares no gate  (the step is the one that adds it)
//	HEAD declares one, and it passes  (18 checks, 0 failing)
//	the work is committed and the tree is clean
//	the acceptance criteria were all reported PASS with evidence
//
// It used to land `unverified` with a two-line notice. It must now land VERIFIED: the gate was found in the
// worktree, it ran, it passed, nothing regressed because nothing failed, and the AC report confirmed the rest.
func TestTheReplayedExecutionIsVerified(t *testing.T) {
	res := verify.Collect(context.Background(), verify.Evidence{
		GitReadable: true, BaseRef: "4efb3ea", CommitsAhead: 1,
		ChangedFiles: map[string]struct{}{
			".orchicon/verify": {}, "server/index.mjs": {}, "scripts/gate.mjs": {},
		},
		ClaimedFiles:            map[string]struct{}{".orchicon/verify": {}, "scripts/gate.mjs": {}},
		StepDeclaresDeliverable: true,
		TreeClean:               boolPtr(true),
		// THE GATE WAS FOUND (this is the fix) and it is GREEN at HEAD.
		DeclaredCommand: "node scripts/gate.mjs", DeclaredRan: true, DeclaredPassed: true,
		HeadResults: map[string]bool{
			"static/missing_file_404_json": true,
			"static/traversal_dotdot":      true,
			"zzz/gate_self_test":           true,
		},
		HeadMeasured: true,
		// The base commit declares no gate, which is honest: the step ADDS it.
		Baseline: &verify.Baseline{Commit: "4efb3ea",
			Detail: "the base commit declares no gate, so there is nothing to compare against"},
	})
	// The AC report the worker actually produced: every criterion PASS, each with evidence.
	res = appendACReportCheck(res, `ORCHICON WORKER SUMMARY: success
AC REPORT:
- **AC1: PASS** — cat .orchicon/verify -> node scripts/gate.mjs (tracked=yes, not ignored)
- **AC2: PASS** — node scripts/gate.mjs -> 18 passed, 0 failed, exit=0
- **AC3: PASS** — curl -si http://127.0.0.1:9090/ -> HTTP/1.1 200 OK
- **AC4: PASS** — second server -> exit=1, "port 9090 is already in use"
- **AC5: PASS** — /healthz -> {"status":"ok"}; /api/nope -> 404 not_found
- **AC6: PASS** — /../../etc/passwd -> 404, no root:
- **AC7: PASS** — grep -c data-testid web/index.html -> 15
- **AC8: PASS** — dead-proxy run -> 18 passed, exit=0, real 0m0.429s
`, "")

	if res.Verdict != verify.VerdictVerified {
		t.Fatalf("verdict = %q, want verified — reason: %s\nchecks: %s", res.Verdict, res.Reason,
			res.FormatRecords())
	}
	if _, notice := applyVerdict(true, res); notice != "" {
		t.Fatalf("a verified step still produced a notice: %q", notice)
	}
}

// AND THE GREEN-AT-HEAD CASE DOES NOT ABSTAIN, which is the second half of the reported noise: a differential
// exists to place a FAILURE, so when nothing failed there is nothing to place.
func TestAFullyGreenGateNeedsNoBaseline(t *testing.T) {
	res := verify.Collect(context.Background(), verify.Evidence{
		GitReadable: true, BaseRef: "abc1234", CommitsAhead: 1,
		ChangedFiles:            map[string]struct{}{"a.go": {}},
		ClaimedFiles:            map[string]struct{}{"a.go": {}},
		StepDeclaresDeliverable: true,
		TreeClean:               boolPtr(true),
		DeclaredCommand:         "make test", DeclaredRan: true, DeclaredPassed: true,
		HeadResults:  map[string]bool{"pkg/a": true, "pkg/b": true},
		HeadMeasured: true,
		Baseline:     nil, // no baseline at all
	})
	var seen bool
	for _, c := range res.Checks {
		if c.Name != "no_new_failures" {
			continue
		}
		seen = true
		if c.Status != verify.CheckPass {
			t.Errorf("no_new_failures = %q (%s); a fully green gate cannot have regressed", c.Status, c.Detail)
		}
		if !contains(c.Detail, "fully green") {
			t.Errorf("the detail must say why: %q", c.Detail)
		}
	}
	if !seen {
		t.Fatal("no_new_failures was not reported at all")
	}
	if res.Verdict != verify.VerdictVerified {
		t.Errorf("verdict = %q, want verified — reason: %s", res.Verdict, res.Reason)
	}
}

// ONE CAUSE, ONE LINE: when the project declares no gate, only `project_gate` says so. The differential must not
// add a second complaint about a baseline it was never going to get.
func TestNoGateIsReportedOnceNotTwice(t *testing.T) {
	res := verify.Collect(context.Background(), verify.Evidence{
		GitReadable: true, BaseRef: "abc1234", CommitsAhead: 1,
		ChangedFiles:            map[string]struct{}{"a.go": {}},
		ClaimedFiles:            map[string]struct{}{"a.go": {}},
		StepDeclaresDeliverable: true,
		TreeClean:               boolPtr(true),
		DeclaredCommand:         "", // no gate at all
	})
	var gate, diff int
	for _, c := range res.Checks {
		switch c.Name {
		case "project_gate":
			gate++
		case "no_new_failures":
			diff++
		}
	}
	if gate != 1 {
		t.Errorf("project_gate reported %d time(s), want 1", gate)
	}
	if diff != 0 {
		t.Errorf("no_new_failures reported %d time(s); with no declared gate there is no differential to run, "+
			"and a second line describes the same absence twice — which is what the operator saw", diff)
	}
}

// THE WIRING, NOT THE HELPER.
func TestGatherEvidenceFindsAGateThatExistsOnlyInTheWorktree(t *testing.T) {
	worktree := initGitRepoWithGate(t, "command = true\nformat = exit_code\n")
	project := t.TempDir() // the operator's checkout: NO .orchicon/verify

	r := &TaskReconciler{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ev := r.gatherEvidence(context.Background(), evidenceInput{
		projectDir:              project,
		worktreePath:            worktree,
		baseRef:                 "HEAD",
		stepDeclaresDeliverable: true,
	})
	if ev.DeclaredCommand == "" {
		t.Fatal("gatherEvidence found no gate even though the WORKTREE declares one — it is reading the operator's " +
			"checkout, which is exactly the defect that reported 'the project declares no gate' for a step whose " +
			"deliverable was creating the gate")
	}
	if ev.DeclaredCommand != "true" {
		t.Errorf("DeclaredCommand = %q, want the worktree's gate", ev.DeclaredCommand)
	}
}

// initGitRepoWithGate builds a committed repo whose `.orchicon/verify` carries the given body, and returns its path.
func initGitRepoWithGate(t *testing.T, gateBody string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, ".orchicon")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "verify"), []byte(gateBody), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "gate")
	return dir
}

// THE NOTICE MUST NOT CLAIM NOTHING WAS CONFIRMED WHEN SOMETHING WAS.
func TestTheNoticeCannotSayNothingWasConfirmedWhenACheckPassed(t *testing.T) {
	res := verify.Result{Verdict: verify.VerdictUnverified}
	res.Add("acceptance_criteria", verify.CheckPass, "8 criterion/criteria reported: 8 pass (8 with evidence), 0 need attention")
	res.Add("project_gate", verify.CheckAbstain, "the project declares no gate")
	res = res.Conclude()

	_, notice := applyVerdict(true, res)
	if notice == "" {
		t.Fatal("fixture: no notice was produced")
	}
	if contains(notice, "nothing confirmed it") {
		t.Errorf("the notice still claims nothing confirmed the step, while a check PASSED: %q", notice)
	}
	// It must name what WAS established, so the reader can see the AC check confirmed things.
	if !contains(notice, "1 of 2 check(s) confirmed") {
		t.Errorf("the notice must report the count of confirmed checks, got %q", notice)
	}
	// And it must still carry the reason, or the operator cannot tell what went unmeasured.
	if !contains(notice, "project_gate") {
		t.Errorf("the notice dropped the reason: %q", notice)
	}
}

// A NOTICE IS NOT AN ERROR — the marker the presentation layer uses to tell the two apart.
func TestANoticeIsDistinguishableFromAFailure(t *testing.T) {
	res := verify.Result{Verdict: verify.VerdictUnverified}
	res.Add("project_gate", verify.CheckAbstain, "the project declares no gate")
	_, notice := applyVerdict(true, res.Conclude())

	if !verify.IsNotice(notice) {
		t.Errorf("a verification notice is not recognised as one: %q", notice)
	}
	// A real failure must NOT be mistaken for a notice, or an error would be rendered as "additional info".
	if verify.IsNotice("execution lost: control plane restarted") {
		t.Error("a real failure was classified as an advisory notice")
	}
}
