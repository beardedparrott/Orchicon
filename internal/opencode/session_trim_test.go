package opencode

import (
	"log/slog"
	"testing"
)

// TestTrimCarriedAgentPrompt pins the trim that removes the duplicated prefix on
// 2.x — and, more importantly, every case where it must REFUSE to trim.
//
// The two failures are not equivalent. A trim that does not happen costs tokens
// (the prefix is sent twice); a trim that lands in the wrong place DELETES the
// beginning of the worker's task. So every uncertain case must send the full
// prompt, and that is what this asserts.
func TestTrimCarriedAgentPrompt(t *testing.T) {
	const prefix = "PLATFORM PROMPT AND WORKER IDENTITY\n\n"
	const composite = prefix + "# Task\n\nFix the thing.\n\n# Instructions\n\nGo."
	log := slog.Default()

	t.Run("an exact length trims to the task", func(t *testing.T) {
		got := trimCarriedAgentPrompt(log, "e1", composite, len(prefix))
		if got != "# Task\n\nFix the thing.\n\n# Instructions\n\nGo." {
			t.Errorf("trimmed = %q", got)
		}
	})

	t.Run("no length means no trim", func(t *testing.T) {
		// 0 is what a v1 manifest carries, and what an older plane's manifest
		// carries — "do not trim" is the whole meaning of the zero value.
		if got := trimCarriedAgentPrompt(log, "e1", composite, 0); got != composite {
			t.Errorf("n=0 must send the full prompt, got %q", got)
		}
	})

	t.Run("a length at or past the end sends the full prompt", func(t *testing.T) {
		for _, n := range []int{len(composite), len(composite) + 10} {
			if got := trimCarriedAgentPrompt(log, "e1", composite, n); got != composite {
				t.Errorf("n=%d must send the full prompt (a trim there would leave nothing)", n)
			}
		}
	})

	t.Run("a length that lands in the WRONG place sends the full prompt", func(t *testing.T) {
		// The drift case: the recorded length no longer matches the composite's
		// opening, so the remainder does not begin with the task section. Trimming
		// here would cut into the worker's instructions.
		for _, n := range []int{3, len(prefix) - 5, len(prefix) + 4} {
			if got := trimCarriedAgentPrompt(log, "e1", composite, n); got != composite {
				t.Errorf("n=%d landed mid-text: the full prompt must be sent, got %q", n, got)
			}
		}
	})
}

// TestAgentSelectionSupportedIsSurfaceSpecific pins the gate the trim depends on:
// a surface that delivers the prompt through an agent reports true, and v1 — where
// the message is the ONLY carrier — must report false, or the trim would delete a
// prompt nothing else sends.
func TestAgentSelectionSupportedIsSurfaceSpecific(t *testing.T) {
	v1 := NewSessionClient("http://127.0.0.1:1", "pw", "")
	v1.shape = shapeV1
	if v1.AgentSelectionSupported() {
		t.Error("v1 has no agent: the message is the only carrier and must not be trimmed")
	}
	v2 := NewSessionClient("http://127.0.0.1:1", "pw", "")
	v2.shape = shapeV2
	if !v2.AgentSelectionSupported() {
		t.Error("v2 delivers the prompt through the agent, so the trim applies there")
	}
}
