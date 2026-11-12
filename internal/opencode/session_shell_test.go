package opencode

import (
	"strings"
	"testing"
)

// sessionToolShell is the serve-baked agent prompt, inherited by EVERY session on the serve —
// including Ask Orchicon conversations, which select no agent and so take the serve default.
func TestSessionToolShellIsNeutral(t *testing.T) {
	banned := []string{
		"autonomous coding agent",
		"you are an", // any identity claim
		"budget",     // budget framing of any kind
		"re-sends the whole conversation",
		"prefer the fewest tool calls",
		"disabled in favor of", // FALSE for Ask, whose file suite grants read/grep/write/edit
		"micro calls",
		"finish faster",
	}
	lower := strings.ToLower(sessionToolShell)
	for _, b := range banned {
		if strings.Contains(lower, strings.ToLower(b)) {
			t.Errorf("the shared session shell must not contain %q — it is inherited by Ask conversations and reads as a budget", b)
		}
	}

	// It must still do its actual job: tell the model which tool fits which job,
	// standing in for opencode's large built-in `build` prompt.
	for _, want := range []string{"batch_read", "batch_grep", "batch_write", "bash", "orchicon_*"} {
		if !strings.Contains(sessionToolShell, want) {
			t.Errorf("the shell must still advertise %q (it replaces opencode's built-in agent prompt)", want)
		}
	}
}

// The shell is what the serve registers as the default agent prompt, so the
// neutrality must hold through the config build (and survive JSON round-tripping).
func TestServeConfigCarriesTheNeutralShell(t *testing.T) {
	out := BuildConfigContent(ConfigOptions{
		AgentName:    workerAgent,
		AgentPrompt:  sessionToolShell,
		DefaultAgent: workerAgent,
	})
	if !strings.Contains(out, "batch_read") {
		t.Fatal("the built serve config must carry the tool inventory")
	}
	for _, b := range []string{"autonomous coding agent", "budget"} {
		if strings.Contains(strings.ToLower(out), b) {
			t.Errorf("the built serve config must not carry %q", b)
		}
	}
}
