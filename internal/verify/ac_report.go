package verify

// ac_report.go — PARSING the AC REPORT block.

import (
	"fmt"
	"regexp"
	"strings"
)

// ACAck is the verdict word a worker puts on a criterion.
type ACVerdict string

const (
	ACPass        ACVerdict = "pass"
	ACFail        ACVerdict = "fail"
	ACNotVerified ACVerdict = "not_verified"
	ACMalformed   ACVerdict = "malformed"
)

// ACAnswer is one criterion's reported outcome.
type ACAnswer struct {
	Number int
	// Verdict is what the worker claimed.
	Verdict ACVerdict
	// Evidence is the text after the verdict, which is supposed to name what demonstrates it.
	Evidence string
	// Evidenced reports whether Evidence looks like something checkable. See looksLikeEvidence.
	Evidenced bool
}

// ACReport is the parsed block.
type ACReport struct {
	Answers []ACAnswer
	// Present reports whether an AC REPORT block was found at all.
	Present bool
}

// Unverified returns the answers that are FAIL, NOT VERIFIED, malformed, or PASS-without-evidence —
// i.e. every criterion the operator should look at before trusting the step.
func (r ACReport) Unverified() []ACAnswer {
	var out []ACAnswer
	for _, a := range r.Answers {
		switch {
		case a.Verdict == ACFail, a.Verdict == ACNotVerified, a.Verdict == ACMalformed:
			out = append(out, a)
		case a.Verdict == ACPass && !a.Evidenced:
			out = append(out, a)
		}
	}
	return out
}

// Summary renders the block for an acceptance review or a notice.
func (r ACReport) Summary() string {
	if !r.Present {
		return ""
	}
	pass, evidenced := 0, 0
	for _, a := range r.Answers {
		if a.Verdict == ACPass {
			pass++
			if a.Evidenced {
				evidenced++
			}
		}
	}
	return fmt.Sprintf("%d criterion/criteria reported: %d pass (%d with evidence), %d need attention",
		len(r.Answers), pass, evidenced, len(r.Unverified()))
}

// acLineRe matches "AC1: PASS — evidence" with either hyphen style and optional leading bullet.
var acLineRe = regexp.MustCompile(`(?i)^[\s\-*•]*(AC\d+)\s*[:.\-–—]\s*(PASS|FAIL|NOT\s+VERIFIED|UNVERIFIED)\b[\s—:\-–—]*(.*)$`)

// evidenceHintRe recognises the KINDS of support a reviewer can actually re-check: a path, a
// command, a file:line reference, or an explicit observed value.
var evidenceHintRe = regexp.MustCompile(
	`(?i)(` +
		`[\w./-]+\.(go|ts|tsx|js|jsx|py|sql|md|json|yaml|yml|png|jpg|sh)` + // a file path
		`|:\d+` + // file:line
		`|\b(go test|make|npm|bun|pnpm|yarn|pytest|cargo|dotnet|mvn|gradle|gh|git|curl)\b` + // a command
		`|→|->` + // an observed transition
		`|\bPASS\b|\bFAIL\b` + // a runner result
		`)`)

// looksLikeEvidence reports whether a criterion's support names something checkable.
func looksLikeEvidence(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 4 {
		return false
	}
	// A bare adjective is not evidence, however emphatically phrased.
	switch strings.ToLower(strings.Trim(s, ". ")) {
	case "satisfied", "done", "verified", "yes", "works", "correct", "implemented", "complete", "completed", "ok":
		return false
	}
	return evidenceHintRe.MatchString(s)
}

// ParseACReport extracts the AC REPORT block from a worker's output.
func ParseACReport(output string) ACReport {
	var rep ACReport
	for _, line := range strings.Split(output, "\n") {
		m := acLineRe.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		rep.Present = true
		num := 0
		_, _ = fmt.Sscanf(strings.ToUpper(m[1]), "AC%d", &num)
		evidence := strings.TrimSpace(m[3])
		verdict := ACPass
		switch strings.ToUpper(strings.Join(strings.Fields(m[2]), " ")) {
		case "PASS":
			verdict = ACPass
		case "FAIL":
			verdict = ACFail
		case "NOT VERIFIED", "UNVERIFIED":
			verdict = ACNotVerified
		default:
			verdict = ACMalformed
		}
		rep.Answers = append(rep.Answers, ACAnswer{
			Number:    num,
			Verdict:   verdict,
			Evidence:  evidence,
			Evidenced: looksLikeEvidence(evidence),
		})
	}
	return rep
}
