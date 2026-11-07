package verify

// results_test.go — EVERY FORMAT IS PINNED BY A REAL RUNNER'S OUTPUT, AND THE TAP CASE IS THE REGRESSION.
//
// The fixtures are verbatim shapes from the runners named in each test, not invented ones: the point of a
// declared format is that a project can point at its own runner's output, so a fixture that no runner emits
// would prove nothing about whether a real project works.

import (
	"strings"
	"testing"
)

// THE REGRESSION. Raw `node --test` TAP keyed the failure on the test NUMBER and then dropped it.
//
// MEASURED BEFORE THE FIX: this exact input parsed to {"1":true, "3":true} — three tests, one failing, and the
// failure was invisible. A differential built on that map reports "no new failures" for work that broke a test,
// which is the single worst outcome a verification system can produce.
func TestTAPKeepsTheFailingTest(t *testing.T) {
	results, err := ParseResults(FormatTAP, `TAP version 13
# Subtest: arithmetic
ok 1 - arithmetic
  ---
  duration_ms: 1.2
  ...
# Subtest: division
not ok 2 - division
  ---
  duration_ms: 0.8
  error: 'expected 5 to equal 2'
  ...
# Subtest: strings
ok 3 - strings
  ---
  duration_ms: 0.1
  ...
1..3
# tests 3
# pass 2
# fail 1
`)
	if err != nil {
		t.Fatalf("ParseResults(tap): %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("parsed %d results, want 3: %#v", len(results), results)
	}
	if _, ok := results["division"]; !ok {
		t.Errorf("the failing test is not keyed by its NAME — got %#v", results)
	}
	if results["division"] {
		t.Errorf("division is recorded as passing; the TAP failure was lost")
	}
	if !results["arithmetic"] || !results["strings"] {
		t.Errorf("a passing test was not recorded as passing: %#v", results)
	}
}

// A NAME IS THE KEY, NOT THE NUMBER: renumbering a suite must not invent a regression. If the key were the
// number, a runner that filters tests would shift them all and every shifted test would read as newly failing.
func TestTAPKeysOnTheNameNotTheNumber(t *testing.T) {
	first, err := ParseResults(FormatTAP, "ok 1 - alpha\nok 2 - beta\n")
	if err != nil {
		t.Fatalf("ParseResults: %v", err)
	}
	// The same tests, one removed — beta renumbers from 2 to 1.
	second, err := ParseResults(FormatTAP, "ok 1 - beta\n")
	if err != nil {
		t.Fatalf("ParseResults: %v", err)
	}
	if !second["beta"] {
		t.Fatalf("beta is not keyed by name: %#v", second)
	}
	if NewFailures(first, second) != nil {
		t.Errorf("renumbering produced a regression: %#v", NewFailures(first, second))
	}
}

// TAP DIRECTIVES ARE NOT FAILURES. "# TODO" and "# skip" describe why a test did not run; treating them as
// failures would fail a step for a skipped test the project deliberately skips.
func TestTAPDirectivesAreNotFailures(t *testing.T) {
	results, err := ParseResults(FormatTAP, `ok 1 - alpha
ok 2 - beta # skip no database
not ok 3 - gamma # TODO not implemented
1..3
`)
	if err != nil {
		t.Fatalf("ParseResults: %v", err)
	}
	if !results["alpha"] || !results["beta"] {
		t.Errorf("a passing or skipped test was recorded as failing: %#v", results)
	}
}

// JUNIT IS THE WIDEST-SUPPORTED FORMAT, so it is the one most projects will declare.
func TestJUnitReadsFailuresByClassAndName(t *testing.T) {
	results, err := ParseResults(FormatJUnit, `<?xml version="1.0" encoding="UTF-8"?>
<testsuites>
  <testsuite name="pkg/a" tests="2" failures="1">
    <testcase classname="pkg/a" name="TestOne"/>
    <testcase classname="pkg/a" name="TestTwo">
      <failure message="expected 1, got 2">trace</failure>
    </testcase>
  </testsuite>
  <testsuite name="pkg/b" tests="1" failures="0">
    <testcase classname="pkg/b" name="TestThree"/>
  </testsuite>
</testsuites>`)
	if err != nil {
		t.Fatalf("ParseResults(junit): %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("parsed %d results, want 3: %#v", len(results), results)
	}
	if results["pkg/a.TestTwo"] {
		t.Errorf("a <failure> was recorded as passing: %#v", results)
	}
	if !results["pkg/a.TestOne"] || !results["pkg/b.TestThree"] {
		t.Errorf("a passing case was not recorded: %#v", results)
	}
}

// A JUNIT ERROR ELEMENT IS A FAILURE TOO — a test that crashed is not a test that passed.
func TestJUnitErrorIsAFailure(t *testing.T) {
	results, err := ParseResults(FormatJUnit,
		`<testsuite name="s"><testcase classname="s" name="TestBoom"><error message="panic"/></testcase></testsuite>`)
	if err != nil {
		t.Fatalf("ParseResults: %v", err)
	}
	if results["s.TestBoom"] {
		t.Error("an <error> was recorded as passing")
	}
}

// JUNIT SURVIVES BUILD CHATTER BEFORE THE DOCUMENT, which is what a real `go build && gotestsum` gate emits.
func TestJUnitIgnoresLeadingBuildOutput(t *testing.T) {
	results, err := ParseResults(FormatJUnit, `ok  	github.com/x/y	0.01s
# github.com/x/z
./z.go:3:2: undefined: thing
<testsuite name="s"><testcase classname="s" name="TestOne"/></testsuite>
some trailing line`)
	if err != nil {
		t.Fatalf("ParseResults with leading chatter: %v", err)
	}
	if !results["s.TestOne"] {
		t.Errorf("the document was not found after the chatter: %#v", results)
	}
}

// SARIF REPORTS FINDINGS, so an empty log is a CLEAN RUN — and it must say so with a passing check rather than
// an empty map, because an empty map is indistinguishable from "nothing was measured".
func TestSARIFCleanRunIsAPassingCheck(t *testing.T) {
	results, err := ParseResults(FormatSARIF, `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"vet"}},"results":[]}]}`)
	if err != nil {
		t.Fatalf("ParseResults(sarif): %v", err)
	}
	if len(results) != 1 || !results["sarif.clean"] {
		t.Errorf("a clean SARIF run must report one passing check, got %#v", results)
	}
}

func TestSARIFErrorLevelFails(t *testing.T) {
	results, err := ParseResults(FormatSARIF, `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"vet"}},
	  "results":[{"ruleId":"unused-var","level":"error","message":{"text":"unused"}},
	             {"ruleId":"style-note","level":"note","message":{"text":"consider"}}]}]}`)
	if err != nil {
		t.Fatalf("ParseResults: %v", err)
	}
	if results["unused-var"] {
		t.Error("an error-level SARIF finding was recorded as passing")
	}
	if !results["style-note"] {
		t.Error("a note-level finding was recorded as failing; notes are advisory by the spec")
	}
}

// JSON IS THE ESCAPE HATCH for a runner that emits neither of the above.
func TestJSONMap(t *testing.T) {
	results, err := ParseResults(FormatJSON, `{"pkg/a": true, "pkg/b": false}`)
	if err != nil {
		t.Fatalf("ParseResults(json): %v", err)
	}
	if results["pkg/b"] {
		t.Error("a false entry was recorded as passing")
	}
	if !results["pkg/a"] {
		t.Error("a true entry was not recorded")
	}
}

// JSON TOLERATES THE STATUS-WORD SHAPE, because an emitter writing "passed"/"failed" is common.
func TestJSONStatusWords(t *testing.T) {
	results, err := ParseResults(FormatJSON, `{"a": "passed", "b": "failed"}`)
	if err != nil {
		t.Fatalf("ParseResults: %v", err)
	}
	if results["b"] {
		t.Error("\"failed\" was recorded as passing")
	}
	if !results["a"] {
		t.Error("\"passed\" was not recorded as passing")
	}
}

// A FORMAT MISMATCH IS AN ERROR, NEVER AN EMPTY MAP. This is the load-bearing distinction in ParseResults: an
// empty map reads as "everything passed", so a project that declared junit and printed TAP must not be silently
// treated as clean.
func TestAWrongFormatIsAnErrorNotAnEmptyMap(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format ResultFormat
		out    string
	}{
		{"junit of tap output", FormatJUnit, "ok 1 - alpha\nnot ok 2 - beta\n"},
		{"tap of junit output", FormatTAP, `<?xml version="1.0"?><testsuite name="s"><testcase name="t"/></testsuite>`},
		{"sarif of nothing", FormatSARIF, "no json here"},
		{"json of nothing", FormatJSON, "go build: ok"},
		{"exit_code always", FormatExitCode, "ok 1 - alpha"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			results, err := ParseResults(tc.format, tc.out)
			if err == nil {
				t.Fatalf("ParseResults(%s) accepted output that is not that format; returned %#v, which a "+
					"caller would read as a clean run", tc.format, results)
			}
			if results != nil {
				t.Errorf("ParseResults returned a non-nil map alongside an error: %#v", results)
			}
		})
	}
}

// AN UNKNOWN FORMAT IS LOUD. A typo in the declaration must not silently disable measurement.
func TestAnUnknownFormatIsRejected(t *testing.T) {
	if _, err := ParseResultFormat("junit-xml-please"); err == nil {
		t.Fatal("an unknown result format was accepted; the project would be silently unmeasured")
	}
	// The documented spellings all resolve.
	for _, in := range []string{"junit", "JUNIT", " junit_xml ", "tap", "sarif", "json", "exit_code", ""} {
		if _, err := ParseResultFormat(in); err != nil {
			t.Errorf("ParseResultFormat(%q): %v", in, err)
		}
	}
}

// EXIT_CODE DECLARES THAT NO PER-CHECK RESULTS EXIST, which is a legitimate configuration and must say so
// rather than pretending to parse.
func TestExitCodeParsesNothing(t *testing.T) {
	if FormatExitCode.ParsesResults() {
		t.Error("exit_code claims to produce per-check results")
	}
	if !FormatJUnit.ParsesResults() || !FormatTAP.ParsesResults() {
		t.Error("junit/tap must report that they produce per-check results")
	}
}

// A SKIPPED JUNIT CASE IS NOT A REGRESSION. A build tag or a missing fixture legitimately skips a test, and
// recording it as failing would report a regression the step did not cause.
func TestJUnitSkipIsNotAFailure(t *testing.T) {
	results, err := ParseResults(FormatJUnit,
		`<testsuite name="s"><testcase classname="s" name="TestSkipped"><skipped/></testcase></testsuite>`)
	if err != nil {
		t.Fatalf("ParseResults: %v", err)
	}
	if !results["s.TestSkipped"] {
		t.Error("a <skipped/> case was recorded as failing")
	}
}

// THE SUMMARY NAMES THE FAILURES, because a count without names sends the operator back to a terminal.
func TestFormatSummaryNamesFailures(t *testing.T) {
	got := FormatSummary(map[string]bool{"a": true, "b": false, "c": false})
	if !strings.Contains(got, "2 of 3") || !strings.Contains(got, "b") || !strings.Contains(got, "c") {
		t.Errorf("FormatSummary = %q; it must give the count AND the names", got)
	}
	if s := FormatSummary(map[string]bool{"a": true}); !strings.Contains(s, "all 1") {
		t.Errorf("FormatSummary for a clean run = %q", s)
	}
}

// `go test -json` IS GO'S OWN MACHINE-READABLE OUTPUT, so it must work with no wrapper — otherwise every Go
// project would have to install gotestsum just to be measurable, which is the wrapper the operator ruled out.
func TestGoTestJSONStreamIsPerPackage(t *testing.T) {
	// Verbatim test2json shapes, including the package-level "fail" a build error produces.
	out := `{"Action":"start","Package":"example.com/a"}
{"Action":"run","Package":"example.com/a","Test":"TestOne"}
{"Action":"pass","Package":"example.com/a","Test":"TestOne","Elapsed":0.01}
{"Action":"pass","Package":"example.com/a","Elapsed":0.02}
{"Action":"run","Package":"example.com/b","Test":"TestTwo"}
{"Action":"fail","Package":"example.com/b","Test":"TestTwo","Elapsed":0.01}
{"Action":"fail","Package":"example.com/b","Elapsed":0.02}
`
	results, err := ParseResults(FormatJSON, out)
	if err != nil {
		t.Fatalf("ParseResults(json, go test -json): %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("parsed %d packages, want 2: %#v", len(results), results)
	}
	if !results["example.com/a"] {
		t.Errorf("example.com/a should pass: %#v", results)
	}
	if results["example.com/b"] {
		t.Errorf("example.com/b failed but was recorded as passing: %#v", results)
	}
}

// A PACKAGE-LEVEL fail (a build error, which has no Test field) is still a failure.
func TestGoTestJSONBuildFailureFailsThePackage(t *testing.T) {
	out := `{"Action":"build-output","Package":"example.com/broken","Output":"undefined: thing\n"}
{"Action":"fail","Package":"example.com/broken","Elapsed":0}
`
	results, err := ParseResults(FormatJSON, out)
	if err != nil {
		t.Fatalf("ParseResults: %v", err)
	}
	if results["example.com/broken"] {
		t.Errorf("a package that failed to build was recorded as passing: %#v", results)
	}
}

// AND A PACKAGE'S FINAL "pass" MUST NOT OVERWRITE AN EARLIER TEST FAILURE — the events arrive in order and the
// package summary comes last.
func TestGoTestJSONPackagePassDoesNotMaskATestFailure(t *testing.T) {
	out := `{"Action":"fail","Package":"example.com/c","Test":"TestX"}
{"Action":"pass","Package":"example.com/c","Elapsed":0.1}
`
	results, err := ParseResults(FormatJSON, out)
	if err != nil {
		t.Fatalf("ParseResults: %v", err)
	}
	if results["example.com/c"] {
		t.Error("the package summary's pass overwrote a failing test — a regression would vanish")
	}
}
