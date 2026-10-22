package askorchicon

// stall_defaults_test.go — the stall windows must be generous enough for a REASONING model.
//
// The operator: "Specifically on ASK conversations, we need to really relax that setting some. 2 minutes
// is far too aggressive as a default."
//
// They were hitting the NO-TOOL window, which was 120s. That value was carried over from before tools
// raised the other window, on the theory that "nothing is open, so silence means nothing is happening" —
// which is WRONG about the one case it most needs to be right about: a reasoning model emits NOTHING
// while it thinks, and DeepSeek/GLM-class models routinely reason for several minutes on a hard question.
//
// These assertions are on the DEFAULTS specifically (the live tenant had the column blank, so the code
// default is what the operator actually experienced).

import (
	"testing"
	"time"
)

// THE NO-TOOL DEFAULT MUST BE COMFORTABLY ABOVE "TWO MINUTES".
func TestAskStallNoToolDefaultIsNotTwoMinutes(t *testing.T) {
	t.Setenv("ORCHICON_ASK_STALL_NO_PROGRESS_WINDOW", "")
	if defaultAskStallNoToolNoProgressWindow <= 2*time.Minute {
		t.Fatalf("the no-tool stall default is %s. 120s aborts a healthy reasoning model mid-thought — "+
			"the operator's '2 minutes is far too aggressive as a default'.",
			defaultAskStallNoToolNoProgressWindow)
	}
	// A reasoning model needs real room; under 5 minutes is still a trap.
	if defaultAskStallNoToolNoProgressWindow < 5*time.Minute {
		t.Errorf("the no-tool stall default is only %s — a reasoning model can exceed that on a hard "+
			"question, and every second is a healthy turn that gets aborted",
			defaultAskStallNoToolNoProgressWindow)
	}
}

// THE ORDERING MUST SURVIVE THE RELAXATION: an open tool may be silent LONGER than nothing-open,
// because a tool may legitimately run (a build, a test) while nothing-open has no such excuse — but both
// must stay well inside the 30-minute reply window.
func TestAskStallWindowOrderingIsPreserved(t *testing.T) {
	if defaultAskMCPToolWedgeWindow >= defaultAskStallNoToolNoProgressWindow {
		t.Errorf("the MCP wedge window (%s) must be SHORTER than the no-tool window (%s): a wedged tool "+
			"is healed first, and the coarser signal covers the silence a wedge cannot describe",
			defaultAskMCPToolWedgeWindow, defaultAskStallNoToolNoProgressWindow)
	}
	if defaultAskStallNoToolNoProgressWindow >= defaultAskStallNoProgressWindow {
		t.Errorf("the no-tool window (%s) must be SHORTER than the with-tool window (%s): nothing-open "+
			"has no legitimate reason to be silent for as long as a running tool does",
			defaultAskStallNoToolNoProgressWindow, defaultAskStallNoProgressWindow)
	}
	if defaultAskStallNoProgressWindow >= defaultReplyWindow {
		t.Errorf("the with-tool window (%s) must stay inside the reply window (%s), or the stall monitor "+
			"can never be the thing that ends a turn", defaultAskStallNoProgressWindow, defaultReplyWindow)
	}
}
