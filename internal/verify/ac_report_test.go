package verify

import (
	"strings"
	"testing"
)

// The AC report is the one place this system touches semantic judgement, so its LIMITS are tested
// as carefully as its behaviour.

func TestParseACReportWellFormed(t *testing.T) {
	out := `Did the work.

AC REPORT:
- AC1: PASS — go test ./internal/verify/ -run TestX → ok 0.01s
- AC2: PASS — frontend/src/routes/schedules.tsx:120 renders the value
- AC3: FAIL — expected "removed", observed "still present" (curl -s localhost:8080/items)
- AC4: NOT VERIFIED — could not run the app in this container
`
	rep := ParseACReport(out)
	if !rep.Present {
		t.Fatal("report block not detected")
	}
	if len(rep.Answers) != 4 {
		t.Fatalf("got %d answers, want 4: %+v", len(rep.Answers), rep.Answers)
	}
	if rep.Answers[0].Verdict != ACPass || !rep.Answers[0].Evidenced {
		t.Errorf("AC1 should be an evidenced pass: %+v", rep.Answers[0])
	}
	if rep.Answers[2].Verdict != ACFail {
		t.Errorf("AC3 should be FAIL: %+v", rep.Answers[2])
	}
	if rep.Answers[3].Verdict != ACNotVerified {
		t.Errorf("AC4 should be NOT VERIFIED: %+v", rep.Answers[3])
	}
	un := rep.Unverified()
	if len(un) != 2 {
		t.Fatalf("unverified = %d, want 2 (AC3 fail, AC4 not verified): %+v", len(un), un)
	}
}

// TestPASSWithNoEvidenceIsUnverified is the whole point of the contract: an adjective is not an answer.
func TestPASSWithNoEvidenceIsUnverified(t *testing.T) {
	for _, bad := range []string{
		"AC1: PASS — satisfied",
		"AC1: PASS — done",
		"AC1: PASS — verified",
		"AC1: PASS",
		"AC1: PASS — works correctly",
	} {
		rep := ParseACReport(bad)
		if !rep.Present {
			t.Fatalf("%q: block should be detected", bad)
		}
		if len(rep.Unverified()) != 1 {
			t.Errorf("%q: an unevidenced PASS must count as unverified, got %+v", bad, rep.Unverified())
		}
	}
}

// TestEvidenceKindsAreRecognised pins the acceptance side: real support must count, or a worker that
// follows the contract is punished and stops bothering.
func TestEvidenceKindsAreRecognised(t *testing.T) {
	for _, good := range []string{
		"AC1: PASS — go test ./pkg/ → ok",
		"AC1: PASS — internal/db/prompt.go:42",
		"AC1: PASS — make fe-lint exits 0",
		"AC1: PASS — screenshot .orchicon/ac1.png",
		"AC1: PASS — GET /items → 200 with the row absent",
		"AC1: PASS — bun x vitest reports PASS",
	} {
		rep := ParseACReport(good)
		if len(rep.Answers) != 1 || !rep.Answers[0].Evidenced {
			t.Errorf("%q: real evidence must be recognised, got %+v", good, rep.Answers)
		}
	}
}

// TestNoReportBlockIsNotPresent pins that an absent block is distinguishable from an empty one, so the
// caller can say "this step reported no criteria" rather than "0 of 0 passed".
func TestNoReportBlockIsNotPresent(t *testing.T) {
	rep := ParseACReport("Implemented the thing. All good.")
	if rep.Present {
		t.Fatal("a report with no AC lines must not be marked present")
	}
	if rep.Summary() != "" {
		t.Errorf("an absent report must render nothing, got %q", rep.Summary())
	}
}

// TestACReportToleratesFormatting pins the parser against the shapes workers actually emit, because
// a parser that only accepts the exact template reports "no criteria" for a correct report.
func TestACReportToleratesFormatting(t *testing.T) {
	variants := []string{
		"AC1: PASS — go test ./a → ok",
		"- AC1: PASS — go test ./a → ok",
		"* AC1 - PASS - go test ./a → ok",
		"ac1: pass — go test ./a → ok",
		"AC1:PASS — go test ./a → ok",
		"AC1: NOT VERIFIED — no app to run",
	}
	for _, v := range variants {
		rep := ParseACReport(v)
		if !rep.Present || len(rep.Answers) != 1 {
			t.Errorf("%q: must parse as one answer, got %+v", v, rep)
		}
	}
}

// TestSummaryCountsEvidence is the operator-facing line.
func TestSummaryCountsEvidence(t *testing.T) {
	rep := ParseACReport(`AC1: PASS — go test ./a → ok
AC2: PASS — satisfied
AC3: FAIL — expected x, observed y`)
	s := rep.Summary()
	if !strings.Contains(s, "3 criterion") {
		t.Errorf("summary should count criteria: %q", s)
	}
	if !strings.Contains(s, "2 need attention") {
		t.Errorf("summary should count the ones needing attention (AC2 no-evidence, AC3 fail): %q", s)
	}
}
