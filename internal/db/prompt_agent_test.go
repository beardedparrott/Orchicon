package db

import (
	"fmt"
	"strings"
	"testing"
)

// TestAgentSystemPromptIsTheExactCompositePrefix is the BYTE-CONTRACT test for
// the prompt split.
//
// AgentSystemPrompt (stable prefix + worker identity) must be a byte-identical
// PREFIX of the composite prompt, because the opencode adapter bakes exactly
// these bytes into a worker's agent on 2.x and then trims them back off the
// message it sends. The expected value below is written out LONGHAND — the same
// literal headings and spacing the composite builder originally emitted inline —
// so this fails if either side's format drifts.
//
// A DRIFT IS NOT CATASTROPHIC, and the direction matters: TrimPrefix simply does
// not match, so the full composite travels on the message (duplicated against
// the agent) rather than being silently truncated. Losing prompt text is the
// failure this guards against; duplicating it is the safe degradation.
func TestAgentSystemPromptIsTheExactCompositePrefix(t *testing.T) {
	v := WorkerVersionRow{
		Role:     "the role",
		Skills:   "the skills",
		Behavior: "the behavior",
		AgentsMD: "the agents md",
	}
	got := AgentSystemPrompt(v, "img:tag", "runtime")

	wantIdentity := "# Role\n\nthe role\n\n" +
		"## Skills\n\nthe skills\n\n" +
		"## Behavior\n\nthe behavior\n\n" +
		"## AGENTS.md\n\nthe agents md\n\n"
	want := StablePromptPrefix("img:tag", "runtime") + wantIdentity
	if got != want {
		t.Errorf("AgentSystemPrompt is not the exact composite prefix.\n got: %q\nwant: %q", got, want)
	}
	if !strings.HasPrefix(got, StablePromptPrefix("img:tag", "runtime")) {
		t.Error("AgentSystemPrompt must BEGIN with the shared stable prefix — that is the cache-warm part")
	}
}

// TestAgentSystemPromptOmitsAbsentIdentitySections pins that an empty field
// contributes NOTHING — not a heading with no body. A worker with no AGENTS.md
// must not gain an empty "## AGENTS.md" section, because the adapter trims these
// bytes off the message: an emitted-but-empty heading would have to match on
// both sides forever.
func TestAgentSystemPromptOmitsAbsentIdentitySections(t *testing.T) {
	got := AgentSystemPrompt(WorkerVersionRow{Role: "only a role"}, "img", "")
	want := StablePromptPrefix("img", "") + "# Role\n\nonly a role\n\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	for _, absent := range []string{"Skills", "Behavior", "AGENTS.md"} {
		if strings.Contains(got, absent) {
			t.Errorf("an absent %s section must not be emitted: %q", absent, got)
		}
	}
}

// TestWorkerIdentityBlockTrimsWhitespace pins the TrimSpace the composite builder
// applied to each field before emitting it: a worker whose role has leading
// newlines must produce the same bytes as one whose role does not, or the agent
// and the composite would disagree about those bytes.
func TestWorkerIdentityBlockTrimsWhitespace(t *testing.T) {
	a := WorkerIdentityBlock(WorkerVersionRow{Role: "  the role  \n"})
	b := WorkerIdentityBlock(WorkerVersionRow{Role: "the role"})
	if a != b {
		t.Errorf("identity block must trim each field: %q vs %q", a, b)
	}
	if a != fmt.Sprintf("# Role\n\n%s\n\n", "the role") {
		t.Errorf("unexpected identity block: %q", a)
	}
}
