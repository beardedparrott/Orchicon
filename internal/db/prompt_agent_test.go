package db

import (
	"fmt"
	"strings"
	"testing"
)

// TestAgentSystemPromptIsTheExactCompositePrefix is the BYTE-CONTRACT test for the prompt split.
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

// TestAgentSystemPromptOmitsAbsentIdentitySections pins that an empty field contributes NOTHING —
// not a heading with no body.
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

// TestWorkerIdentityBlockTrimsWhitespace pins the TrimSpace the composite builder applied to each
// field before emitting it.
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
