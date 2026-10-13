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

// TestAgentSelectionSupportedIsTheCompositeGate pins the gate the trim depends on.
//
// It is BOTH conditions, and combining them is the safety property rather than tidiness:
//
//   - the surface must have the agent route (v1 has no agent at all), and
//   - the serve must actually carry THIS execution's agent, which is only true of a
//     serve built per RUN — the container path. A HOST serve is shared and bakes none.
//
// If either were checked separately at the call site, a session on a host serve would
// select an agent that cannot exist and then TRIM the run-level prompt off the message
// — deleting it, since no agent holds it.
func TestAgentSelectionSupportedIsTheCompositeGate(t *testing.T) {
	mk := func(s apiShape, agents bool) *SessionClient {
		c := NewSessionClient("http://127.0.0.1:1", "pw", "")
		c.shape = s
		c.SetAgentsAvailable(agents)
		return c
	}
	if mk(shapeV1, true).AgentSelectionSupported() {
		t.Error("v1 has no agent route, so nothing may be selected or trimmed")
	}
	if mk(shapeV2, false).AgentSelectionSupported() {
		t.Error("a serve with no agents (the host path) must keep the full prompt on the message")
	}
	if !mk(shapeV2, true).AgentSelectionSupported() {
		t.Error("a per-run container serve with agents is exactly the case this enables")
	}
	// The default is the SAFE answer: a client nobody configured must not trim.
	if NewSessionClient("http://127.0.0.1:1", "pw", "").AgentSelectionSupported() {
		t.Error("agentsAvailable must default false — an unconfigured client keeps the full prompt")
	}
}
