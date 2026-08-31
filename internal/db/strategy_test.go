package db

import "testing"

// TestEffectiveGitStrategyPrecedenceBitIdentical proves the centralized
// EffectiveGitStrategy decision (effectiveGitStrategyValue) is bit-identical
// to the two pre-existing inline resolution sites it replaced:
//
//   - reconciler.go skipPRMarkerStamp (default "" — workflow wins, else project)
//   - workflow_reconciler.go step-success PR gate (default "local" — workflow
//     wins, else project)
//
// The only legitimate divergence is the default: site A's empty "unset" and
// site B's "local" behave identically at the PR gate (`!= "pr"`), and the
// task's canonical default is "local". A `none`-strategy run must resolve to
// "none" at every layer, and nothing may change whether a `pr`/`local` run
// is considered for the PR gate.
func TestEffectiveGitStrategyPrecedenceBitIdentical(t *testing.T) {
	valid := []string{"", "local", "pr", "none"}

	// siteA — reconciler.go skipPRMarkerStamp's original inline resolution.
	siteA := func(wfStr, projStr string) string {
		s := ""
		if wfStr != "" {
			s = wfStr
		}
		if s == "" && projStr != "" {
			s = projStr
		}
		return s
	}
	// siteB — workflow_reconciler.go step-success PR gate's original inline
	// resolution (the task's canonical default is "local").
	siteB := func(wfStr, projStr string) string {
		if wfStr != "" {
			return wfStr
		}
		if projStr != "" {
			return projStr
		}
		return "local"
	}

	for _, wfStr := range valid {
		for _, projStr := range valid {
			got := effectiveGitStrategyValue(wfStr, projStr)

			// The centralized value must equal the canonical (site B) value
			// for every input — no precedence drift.
			if got != siteB(wfStr, projStr) {
				t.Fatalf("effectiveGitStrategyValue(%q,%q) = %q, want siteB %q",
					wfStr, projStr, got, siteB(wfStr, projStr))
			}

			// The PR-gate decision (require PR iff == "pr") must be
			// identical across the centralized value AND both inline sites.
			if (got == "pr") != (siteA(wfStr, projStr) == "pr") {
				t.Fatalf("PR-gate mismatch at (%q,%q): centralized %q (pr=%v) vs siteA %q (pr=%v)",
					wfStr, projStr, got, got == "pr", siteA(wfStr, projStr), siteA(wfStr, projStr) == "pr")
			}
			if (got == "pr") != (siteB(wfStr, projStr) == "pr") {
				t.Fatalf("PR-gate mismatch at (%q,%q): centralized vs siteB", wfStr, projStr)
			}

			// The centralized value carries a real strategy — never an empty
			// "unset" that a later caller could misread as "no strategy".
			if got == "" {
				t.Fatalf("effectiveGitStrategyValue(%q,%q) returned empty — a run must always resolve to a concrete strategy", wfStr, projStr)
			}
		}
	}
}

// TestEffectiveGitStrategyNoneWinsEverywhere asserts that a `none` workflow
// strategy overrides even an explicitly-specified project strategy — the
// enforcement guarantee that `git_strategy=none` is honoured end-to-end.
func TestEffectiveGitStrategyNoneWinsEverywhere(t *testing.T) {
	cases := []struct {
		name             string
		workflowStrategy string
		projectStrategy  string
		want             string
	}{
		{"none wins over project local", "none", "local", "none"},
		{"none wins over project pr", "none", "pr", "none"},
		{"project none when workflow empty", "", "none", "none"},
		{"workflow local wins over project none", "local", "none", "local"},
		{"workflow pr wins over project none", "pr", "none", "pr"},
		{"default local when neither set", "", "", "local"},
		{"workflow empty falls to project pr", "", "pr", "pr"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveGitStrategyValue(tc.workflowStrategy, tc.projectStrategy); got != tc.want {
				t.Fatalf("effectiveGitStrategyValue(%q,%q) = %q, want %q",
					tc.workflowStrategy, tc.projectStrategy, got, tc.want)
			}
		})
	}
}
