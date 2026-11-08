package verify

// results.go — WHAT THE PROJECT SAYS ITS RESULTS ARE, INSTEAD OF GUESSING FROM ITS OUTPUT.
//
// THE DEFECT THIS REPLACES. The platform used to infer pass/fail by pattern-matching log lines. That is wrong in
// principle — a log line is a rendering, not a contract, and every runner changes it — and it was broken in
// practice, measurably:
//
//	raw `node --test` TAP:  "not ok 2 - division"
//	  → the parser tried "not ok" as a status prefix, failed, fell through to the runner-prefixed rule which
//	    wants the status FIRST, and produced key "2" ... which `parseRunnerPrefixed` then rejected because the
//	    subject "2" came from the test NUMBER, not the test name.
//	  → measured: input with 3 tests and 1 failure parsed to {"1":true, "3":true}. THE FAILURE VANISHED.
//
// A verification system that silently drops failures is worse than no verification, because it reports "no new
// failures" over work that broke a test it never measured.
//
// THE FIX IS TO STOP GUESSING. The project declares the FORMAT it emits, and the platform parses that format —
// JUnit XML, TAP, SARIF, or a JSON map — or does not parse at all and takes the exit code. Every language has a
// runner that can emit one of these; none of them needs a wrapper written for Orchicon.
//
// WHY A DECLARATION RATHER THAN A DETECTOR. A detector is the same pattern-matching defect with a longer list,
// and it fails the same way: silently, on the runner nobody thought of. A declaration is one line the operator
// writes and can verify, and when it is absent the platform says "not measured" out loud.

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ResultFormat is how a project's gate reports per-check results.
//
// THE ZERO VALUE IS DELIBERATE. `exit_code` (which parses nothing) is the format every project has even if it
// declares nothing, so the zero value must be the SAFE one — not a format that would start parsing output
// nobody promised was in that shape.
type ResultFormat string

const (
	// FormatExitCode parses no results at all. The gate's exit status is the whole answer, which means the
	// differential cannot run and every step records "not measured". This is NOT a failure to be fixed by
	// guessing; it is a project that has not declared a machine-readable format, and the platform says so.
	FormatExitCode ResultFormat = "exit_code"
	// FormatJUnit is JUnit XML — the widest-supported machine-readable test format there is. Go (gotestsum),
	// Python (pytest --junit-xml), Java (surefire), JS (jest-junit), Rust, .NET and Ruby all emit it.
	FormatJUnit ResultFormat = "junit"
	// FormatTAP is the Test Anything Protocol. Handled properly here, including the numbered form that the
	// line parser silently dropped.
	FormatTAP ResultFormat = "tap"
	// FormatSARIF is the static-analysis interchange format (SARIF 2.1.0). Rules become the results.
	FormatSARIF ResultFormat = "sarif"
	// FormatJSON is a plain JSON map of check name to boolean, for a project whose runner emits neither of the
	// above: {"pkg/a": true, "pkg/b": false}.
	FormatJSON ResultFormat = "json"
)

// ParseResultFormat resolves a declared format name, accepting the usual spellings.
//
// AN UNKNOWN FORMAT IS AN ERROR, not a silent fallback to exit_code. A project that declared `junit` with a typo
// must be told, because the alternative — quietly not measuring — is the failure mode this whole file exists to
// remove.
func ParseResultFormat(s string) (ResultFormat, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "exit", "exit_code", "exitcode", "none":
		return FormatExitCode, nil
	case "junit", "junit_xml", "junitxml", "xunit":
		return FormatJUnit, nil
	case "tap":
		return FormatTAP, nil
	case "sarif":
		return FormatSARIF, nil
	case "json":
		return FormatJSON, nil
	}
	return "", fmt.Errorf("unknown result format %q (want one of: junit, tap, sarif, json, exit_code)", s)
}

// ParsesResults reports whether this format yields per-check results at all.
func (f ResultFormat) ParsesResults() bool { return f != "" && f != FormatExitCode }

// ParseResults turns a gate transcript into a per-check result map for the declared format.
//
// IT RETURNS AN ERROR RATHER THAN AN EMPTY MAP when the transcript is not the declared format. The distinction
// is load-bearing: an empty map means "every check passed" to a naive caller, and a format mismatch would then
// read as a clean run. The caller records the error as "not measured" instead.
func ParseResults(format ResultFormat, out string) (map[string]bool, error) {
	switch format {
	case FormatJUnit:
		return parseJUnit(out)
	case FormatTAP:
		return parseTAP(out)
	case FormatSARIF:
		return parseSARIF(out)
	case FormatJSON:
		return parseJSONResults(out)
	case FormatExitCode, "":
		return nil, fmt.Errorf("the declared format (%s) carries no per-check results", FormatExitCode)
	}
	return nil, fmt.Errorf("unknown result format %q", format)
}

// xmlSuite is the JUnit shape. Only the fields that carry results are modelled; everything else in the document
// is ignored, so a real surefire/gotestsum/pytest file parses without a schema fight.
type xmlSuite struct {
	XMLName  xml.Name   `xml:"testsuite"`
	Name     string     `xml:"name,attr"`
	Suites   []xmlSuite `xml:"testsuite"`
	Cases    []xmlCase  `xml:"testcase"`
	Failures int        `xml:"failures,attr"`
	Errors   int        `xml:"errors,attr"`
}

type xmlCase struct {
	Name    string     `xml:"name,attr"`
	Class   string     `xml:"classname,attr"`
	Failure *xmlNested `xml:"failure"`
	Error   *xmlNested `xml:"error"`
	Skipped *xmlNested `xml:"skipped"`
}

type xmlNested struct {
	Message string `xml:"message,attr"`
}

// parseJUnit reads JUnit XML into a per-test result map.
//
// THE KEY IS classname.name, WHICH IS THE UNIQUE IDENTITY. Using the bare test name collides across packages —
// every Go package has a TestMain, every Java class has its own setUp — and a collision makes one file's pass
// silently overwrite another's failure.
func parseJUnit(out string) (map[string]bool, error) {
	body, err := xmlWindow(out)
	if err != nil {
		return nil, err
	}
	var doc xmlSuite
	if derr := xml.Unmarshal([]byte(body), &doc); derr != nil {
		// A <testsuites> wrapper is the other common root; try it before giving up.
		var wrapper struct {
			XMLName xml.Name   `xml:"testsuites"`
			Suites  []xmlSuite `xml:"testsuite"`
		}
		if werr := xml.Unmarshal([]byte(body), &wrapper); werr != nil || len(wrapper.Suites) == 0 {
			return nil, fmt.Errorf("the output is not JUnit XML: %v", derr)
		}
		doc.Suites = wrapper.Suites
	}
	results := map[string]bool{}
	collectJUnit(&doc, results)
	if len(results) == 0 {
		return nil, fmt.Errorf("the JUnit document declares no test cases")
	}
	return results, nil
}

// collectJUnit walks a suite tree, flattening nested suites.
func collectJUnit(s *xmlSuite, into map[string]bool) {
	for _, c := range s.Cases {
		key := strings.TrimSpace(c.Class)
		if key == "" {
			key = strings.TrimSpace(s.Name)
		}
		name := strings.TrimSpace(c.Name)
		if key != "" && name != "" {
			key += "."
		}
		key += name
		if key == "" {
			continue
		}
		// A SKIP IS NOT A FAILURE and not a pass either — it carries no information, so it is recorded as
		// passing rather than as a regression. Treating a skip as a failure would fail a step for a build tag.
		passing := c.Failure == nil && c.Error == nil
		if prev, seen := into[key]; seen {
			into[key] = prev && passing
			continue
		}
		into[key] = passing
	}
	for i := range s.Suites {
		collectJUnit(&s.Suites[i], into)
	}
}

// parseTAP reads TAP, including the numbered form the line parser dropped.
//
// THE POINT OF GETTING THIS RIGHT: "not ok 2 - division" carries the test NUMBER and the test NAME, and the old
// parser keyed on whichever field it happened to land on. This key is the NAME, and the number is discarded —
// it is positional and a runner that reorders or filters tests would renumber it.
//
// A test without a name falls back to its number, prefixed so it cannot collide with a real name.
func parseTAP(out string) (map[string]bool, error) {
	results := map[string]bool{}
	sawTAP := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "TAP version") || line == "1..0" || strings.HasPrefix(line, "1..") {
			sawTAP = true
			continue
		}
		lower := strings.ToLower(line)
		var passing bool
		var rest string
		switch {
		case strings.HasPrefix(lower, "not ok"):
			passing, rest = false, line[len("not ok"):]
		case strings.HasPrefix(lower, "ok"):
			passing, rest = true, line[len("ok"):]
		default:
			// TODO / # skip / Bail out / diagnostics carry no per-test result.
			continue
		}
		sawTAP = true
		if key := tapName(rest); key != "" {
			if prev, seen := results[key]; seen {
				results[key] = prev && passing
			} else {
				results[key] = passing
			}
		}
	}
	if !sawTAP {
		return nil, fmt.Errorf("the output carries no TAP result lines")
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("the TAP output carries no identifiable tests")
	}
	return results, nil
}

// tapName extracts the test NAME from the text after the status, discarding the leading number.
//
//	" 2 - division  # time=0.8ms"  → "division"
//	" 2"                           → "tap#2"
func tapName(rest string) string {
	s := strings.TrimSpace(rest)
	// Drop the trailing directive comment ("# skip", "# TODO", "# time=…").
	if i := strings.Index(s, " #"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	// Strip the leading test number, if any.
	num := ""
	if sp := strings.IndexAny(s, " \t"); sp >= 0 {
		if _, err := strconv.Atoi(strings.TrimSuffix(s[:sp], "-")); err == nil {
			num = strings.TrimSpace(s[:sp])
			s = strings.TrimSpace(s[sp:])
		}
	} else if _, err := strconv.Atoi(s); err == nil {
		num, s = s, ""
	}
	// Strip the description separator.
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "-"))
	s = strings.TrimSpace(s)
	if s != "" {
		return s
	}
	if num != "" {
		return "tap#" + num
	}
	return ""
}

// sarifLog is the SARIF 2.1.0 shape, reduced to what carries a verdict.
type sarifLog struct {
	Runs []struct {
		Tool struct {
			Driver struct {
				Name string `json:"name"`
			} `json:"driver"`
		} `json:"tool"`
		Results []struct {
			RuleID  string `json:"ruleId"`
			Level   string `json:"level"`
			Message struct {
				Text string `json:"text"`
			} `json:"message"`
		} `json:"results"`
	} `json:"runs"`
}

// parseSARIF reads a SARIF log: each finding's rule becomes a check, failing when its level says so.
//
// A SARIF LOG LISTS PROBLEMS, so the absence of a rule is a pass and the presence of one at error/warning level
// is a failure. "note" is not a failure — it is advisory by definition in the spec.
func parseSARIF(out string) (map[string]bool, error) {
	body, err := jsonWindow(out)
	if err != nil {
		return nil, err
	}
	var log sarifLog
	if jerr := json.Unmarshal([]byte(body), &log); jerr != nil {
		return nil, fmt.Errorf("the output is not SARIF JSON: %v", jerr)
	}
	if len(log.Runs) == 0 {
		return nil, fmt.Errorf("the SARIF log has no runs")
	}
	results := map[string]bool{}
	sawFinding := false
	for _, run := range log.Runs {
		for _, r := range run.Results {
			sawFinding = true
			key := strings.TrimSpace(r.RuleID)
			if key == "" {
				key = strings.TrimSpace(r.Message.Text)
			}
			if key == "" {
				key = "sarif.finding"
			}
			failing := strings.EqualFold(r.Level, "error") || strings.EqualFold(r.Level, "warning")
			if prev, seen := results[key]; seen {
				results[key] = prev && !failing
			} else {
				results[key] = !failing
			}
		}
	}
	// A clean run has NO findings, which is a legitimate and important answer: everything passed. It is
	// reported as ONE passing check, because an empty map would be indistinguishable from "not measured".
	if !sawFinding {
		results["sarif.clean"] = true
	}
	return results, nil
}

// parseJSONResults reads a plain {"check": true} map, OR the NDJSON event stream that `go test -json` emits.
//
// THE EVENT STREAM IS WHY GO WORKS WITHOUT A WRAPPER. `go test -json` is Go's own machine-readable output and it
// is a stream of events, not a map — so a JSON format that only understood maps would force every Go project to
// install gotestsum just to be measurable, which is exactly the wrapper the operator said should not be needed.
//
// Each package's events collapse to ONE result: failing if any of its tests failed, or if the package's own
// build failed. That is the granularity the differential needs — "did this package get worse" — and it is the
// same unit `go test` itself reports per package.
func parseJSONResults(out string) (map[string]bool, error) {
	if results, ok := parseGoTestJSON(out); ok {
		return results, nil
	}
	body, err := jsonWindow(out)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if jerr := json.Unmarshal([]byte(body), &raw); jerr != nil {
		return nil, fmt.Errorf("the output is not a JSON object: %v", jerr)
	}
	results := map[string]bool{}
	for k, v := range raw {
		switch t := v.(type) {
		case bool:
			results[k] = t
		case string:
			// Tolerate the status-word shape, since a JSON emitter that writes "passed"/"failed" is common.
			switch strings.ToLower(t) {
			case "pass", "passed", "ok", "success", "succeeded":
				results[k] = true
			case "fail", "failed", "error", "failure":
				results[k] = false
			}
		}
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("the JSON object carries no boolean results")
	}
	return results, nil
}

// xmlWindow finds the XML document inside a transcript that may carry build chatter around it.
//
// GATES PRINT OTHER THINGS. `go build && go test` emits build output before the runner's document, and a runner
// writing to stdout is not obliged to be the only writer. So the outermost element is located rather than
// assuming the transcript starts with '<'.
func xmlWindow(out string) (string, error) {
	start := strings.IndexByte(out, '<')
	if start < 0 {
		return "", fmt.Errorf("the output contains no XML")
	}
	// Back off any earlier closing tag by taking the first '<' — a document that begins with a declaration is
	// handled by the XML decoder itself.
	body := out[start:]
	if end := strings.LastIndexByte(body, '>'); end >= 0 {
		body = body[:end+1]
	}
	return body, nil
}

// jsonWindow finds the JSON document inside a transcript that may carry other output.
//
// IT TRIES EACH LINE THAT STARTS A TOP-LEVEL OBJECT and accepts the first that decodes as a whole, because a
// gate that prints "go build: ok" before a JSON report must not defeat the parse. Scanning backwards from the
// last '{' is the cheap version and is wrong when the report contains nested objects (the last '{' is inside
// it), so this tries every candidate in order.
func jsonWindow(out string) (string, error) {
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		return strings.TrimSpace(out), nil
	}
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "{") {
			continue
		}
		if json.Valid([]byte(t)) {
			return t, nil
		}
	}
	// A multi-line pretty-printed report: from the first '{' to the last '}'.
	start := strings.IndexByte(out, '{')
	end := strings.LastIndexByte(out, '}')
	if start >= 0 && end > start {
		candidate := out[start : end+1]
		if json.Valid([]byte(candidate)) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("the output contains no JSON object")
}

// FormatSummary renders a result map for a record: the failures, then a count.
//
// Failures first and named, because "3 of 120 checks failed" without saying which sends the operator back to a
// terminal — the whole reason the record exists is to answer that question where they already are.
func FormatSummary(results map[string]bool) string {
	if len(results) == 0 {
		return "no per-check results"
	}
	var failed []string
	for k, passing := range results {
		if !passing {
			failed = append(failed, k)
		}
	}
	if len(failed) == 0 {
		return fmt.Sprintf("all %d check(s) passed", len(results))
	}
	sort.Strings(failed)
	shown := failed
	suffix := ""
	if len(shown) > 3 {
		shown = shown[:3]
		suffix = fmt.Sprintf(" (+%d more)", len(failed)-3)
	}
	return fmt.Sprintf("%d of %d check(s) failing: %s%s", len(failed), len(results),
		strings.Join(shown, ", "), suffix)
}

// goTestEvent is one line of `go test -json` output (the test2json shape). Only the fields that carry a verdict
// are modelled.
type goTestEvent struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
}

// parseGoTestJSON aggregates a `go test -json` stream into per-package results. ok is false when the output is not
// that stream, so the caller can fall through to the plain-map form.
//
// PACKAGE-LEVEL, deliberately: the differential compares base against HEAD, and a test that is RENAMED would read
// as one test disappearing and another appearing — never "this used to pass and now fails". Package granularity
// survives renames, and it is the unit a build failure can be attributed to at all.
func parseGoTestJSON(out string) (map[string]bool, bool) {
	results := map[string]bool{}
	events := 0
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "{") {
			continue
		}
		var ev goTestEvent
		if err := json.Unmarshal([]byte(t), &ev); err != nil {
			continue // a non-event line inside the stream is not fatal
		}
		if ev.Action == "" || ev.Package == "" {
			continue
		}
		events++
		switch ev.Action {
		case "fail":
			// A package-level fail (build failure) or a test-level fail both mark the package failing.
			results[ev.Package] = false
		case "pass", "skip":
			// Only record a pass if nothing has failed it already: events arrive in order, and a package's own
			// final "pass" must not overwrite a test's earlier "fail".
			if _, seen := results[ev.Package]; !seen {
				results[ev.Package] = true
			}
		}
	}
	if events == 0 || len(results) == 0 {
		return nil, false
	}
	return results, true
}
