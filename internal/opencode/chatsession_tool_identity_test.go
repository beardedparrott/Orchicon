package opencode

// chatsession_tool_identity_test.go — THE ASK PATH'S TOOL IDENTITY.
//
// 2.x splits a call across THREE events, verified against its own schema:
//
//	session.tool.input.started → the NAME   {id, name}
//	session.tool.called        → the INPUT  {id, input}
//	session.tool.success/.failed → ONLY the id (ToolBase is
//	                              {sessionID, assistantMessageID, id})
//
// The EXECUTION path correlates them by id (session_run.rememberV2Tool /
// fillV2Tool). The Ask path had no equivalent — and its index additionally
// gated on `message.part.updated`, a v1-only envelope, so on 2.x it recorded
// nothing at all. Every resolved tool part therefore reached the consumers
// carrying neither the tool's name nor its args.
//
// THAT IS WHAT KILLED A LIVE TURN. The Ask stall monitor builds a repetition
// signature as `tool + "|" + json(args)`; with both missing it is the literal
// `|null`, so every distinct call in a turn collapsed into ONE signature and
// the monitor reported `8 repeats of |null` on a turn that was working. The
// monitor now refuses to count an identity-less part (internal/askorchicon),
// and this restores the identity so genuine loops are still detected.

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// TestV2ToolIdentityIsRestoredAcrossEvents is the fix: the resolved part carries
// the name and the args that arrived on the two earlier events.
func TestV2ToolIdentityIsRestoredAcrossEvents(t *testing.T) {
	calls := newToolCallIndex()

	// The stream in the order a 2.x serve emits it.
	classifyBusEventIndexed(v2Event(t, v2ToolInputStarted), calls)
	classifyBusEventIndexed(v2Event(t, v2ToolCalled), calls)
	se := classifyBusEventIndexed(v2Event(t, v2ToolSuccess), calls)

	if se == nil || se.Part == nil {
		t.Fatal("the resolved tool event produced no part")
	}
	tool, _ := se.Part["tool"].(string)
	if tool != "shell" {
		t.Errorf("resolved part tool = %q, want %q — without the name every call collapses into one signature, "+
			"which is the `|null` false kill", tool, "shell")
	}
	state, _ := se.Part["state"].(map[string]any)
	in, _ := state["input"].(map[string]any)
	if in == nil || in["command"] != "go test ./..." {
		t.Errorf("resolved part state.input = %#v, want the input announced by session.tool.called", state["input"])
	}
}

// TestV2ToolIdentityIsAbsentWithoutTheIndex is the CONTRAST that makes the test
// above mean something: it shows the pre-fix state, so a future change that
// drops the correlation fails here with a message naming the consequence rather
// than only in the stall monitor's behaviour.
func TestV2ToolIdentityIsAbsentWithoutTheIndex(t *testing.T) {
	// No index: exactly what the Ask path did before this fix.
	se := classifyBusEvent(v2Event(t, v2ToolSuccess))
	if se == nil || se.Part == nil {
		t.Fatal("the resolved tool event produced no part")
	}
	if tool, _ := se.Part["tool"].(string); tool != "" {
		t.Fatalf("tool = %q without the index — this test's premise (the resolution carries no name) no longer "+
			"holds, so it no longer demonstrates what the index is for", tool)
	}
}

// TestV2ToolIdentitySurvivesASecondResolution pins WHY neither entry is
// consumed, and it is a case the first draft of this fix got WRONG: consuming
// the name at stamping left a SECOND resolution of the same call with no name —
// the very identity-less state that produced the `|null` false kill.
//
// Both entries are retained, which is also consistent with the args entry: the
// consent path reads it by call id AFTER a call resolves (argsForPermission), so
// it can never be consumed either.
func TestV2ToolIdentitySurvivesASecondResolution(t *testing.T) {
	calls := newToolCallIndex()
	classifyBusEventIndexed(v2Event(t, v2ToolInputStarted), calls)
	classifyBusEventIndexed(v2Event(t, v2ToolCalled), calls)

	first := classifyBusEventIndexed(v2Event(t, v2ToolSuccess), calls)
	second := classifyBusEventIndexed(v2Event(t, v2ToolSuccess), calls)

	for name, se := range map[string]*scheduler.SessionEvent{"first": first, "second": second} {
		if se == nil || se.Part == nil {
			t.Fatalf("%s resolution produced no part", name)
		}
		if tool, _ := se.Part["tool"].(string); tool != "shell" {
			t.Errorf("%s resolution tool = %q, want %q — identity must survive a re-resolution", name, tool, "shell")
		}
	}

	// And the args are still readable by call id, which the consent path relies on.
	if got := calls.get("call_1"); got == nil || got["command"] != "go test ./..." {
		t.Errorf("args for call_1 = %#v, want them retained for the consent path", got)
	}
}
