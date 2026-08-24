package execution

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// TestComposeFollowUpPromptCarriesSharedPrefix verifies that a follow-up
// session's per-message system prompt opens with the same stable prompt
// prefix as the composite prompt: the stored AGENTS.md no longer carries the
// safety rules (seedAgentsMD strips them at persistence), so the follow-up
// prompt must supply them via db.StablePromptPrefix or every follow-up turn
// would silently lose the HARD-limit safety block.
func TestComposeFollowUpPromptCarriesSharedPrefix(t *testing.T) {
	v := db.WorkerVersionRow{
		Role:     "You are a senior full-stack engineer.",
		Skills:   "Go • React",
		Behavior: "Write tests alongside implementation.",
		AgentsMD: "## Git workflow\ncommit early and often.\n",
	}
	out := composeFollowUpPrompt(v, "orchicon-dev:latest")
	for _, want := range []string{
		"autonomous worker running inside the Orchicon orchestration platform",
		"## Safety rules (HARD limits)",
		"## Efficiency — minimize tool output and tool calls",
		"Batch your tool calls — split calls are FORBIDDEN.",
		"## Runtime environment",
		"orchicon-dev:latest",
		"# Role",
		"senior full-stack engineer",
		"## Git workflow",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("follow-up prompt missing %q; got:\n%s", want, out)
		}
	}
	// The stable prefix must be byte-identical to the shared composite head.
	prefix := db.StablePromptPrefix("orchicon-dev:latest")
	if !strings.HasPrefix(out, prefix) {
		t.Errorf("follow-up prompt must open with the shared stable prefix")
	}
}

// TestComposeFollowUpPromptEmptyWorkerKeepsCustomPrompt verifies the custom
// SystemPrompt fallback is preserved (and still preceded by the prefix).
func TestComposeFollowUpPromptEmptyWorkerKeepsCustomPrompt(t *testing.T) {
	v := db.WorkerVersionRow{SystemPrompt: "custom authored system prompt"}
	out := composeFollowUpPrompt(v, "")
	if !strings.HasPrefix(out, db.StablePromptPrefix("")) {
		t.Errorf("custom-prompt follow-up must still open with the stable prefix")
	}
	if !strings.Contains(out, "custom authored system prompt") {
		t.Errorf("custom system prompt dropped; got:\n%s", out)
	}
}
