package askorchicon

// modes_test.go — THE THREE MODES, AND THE RULES EVERY ONE OF THEM OBEYS.
//
// The operator's requirements, which these encode:
//
//	"All modes including Brainstorm should be aware of these modes and which mode they are currently tied to
//	 in the system."
//	"All personalities of Orchicon should know that their name is Orchicon and that they are here to help you
//	 on your project."
//	"All modes should be aware of who they are and if they need to suggest the user to switch to a different
//	 mode for what they want to create."
//
// A persona is text, so the only way its promises survive the next edit is if they are asserted. Each test
// below is a rule the operator stated in words, turned into something that fails when it stops being true.

import (
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
)

// everyMode is the roster, so a mode added later must be added here too — and a test
// that iterates the roster cannot silently skip the new one.
var everyMode = []string{modeBrainstorm, modeIteration, modeQuickWork}

// THE SHARED IDENTITY. Every mode says its name is Orchicon and that it is here for
// the user's project. This is the operator's own wording of the requirement.
func TestEveryModeKnowsItsNameAndItsJob(t *testing.T) {
	cfg := testAgentConfig()
	reg := testToolRegistry()
	for _, mode := range everyMode {
		p := BuildSystemPrompt(mode, cfg, reg)
		if !strings.Contains(p, "You are Orchicon") {
			t.Errorf("%s: the persona does not say it is Orchicon — the operator: \"All personalities of "+
				"Orchicon should know that their name is Orchicon\"", mode)
		}
		if !strings.Contains(p, "here to help the user on THEIR project") {
			t.Errorf("%s: the persona does not say it is here for the user's project", mode)
		}
		// And it must not claim to be a different assistant. This is the line that
		// keeps a mode from drifting into a generic-chatbot voice.
		if !strings.Contains(p, "not Claude, ChatGPT, or any other AI assistant") {
			t.Errorf("%s: the identity block lost its \"not another assistant\" line", mode)
		}
	}
}

// EVERY MODE KNOWS WHICH MODE IT IS IN, and names the others.
//
// This is the operator's requirement verbatim, and it is what makes the switch
// suggestion possible: a mode that did not know the roster could not tell the user
// that another mode fits better.
func TestEveryModeKnowsWhichModeItIsInAndTheOthers(t *testing.T) {
	cfg := testAgentConfig()
	reg := testToolRegistry()
	for _, mode := range everyMode {
		p := BuildSystemPrompt(mode, cfg, reg)

		banner := "You are currently in **" + modeLabel(mode) + "** mode"
		if !strings.Contains(p, banner) {
			t.Errorf("%s: the persona does not state which mode it is in (want %q)", mode, banner)
		}
		// Every OTHER mode is named, so it can be suggested.
		for _, other := range everyMode {
			if other == mode {
				continue
			}
			if !strings.Contains(p, modeLabel(other)+" — ") {
				t.Errorf("%s: the persona does not describe %s, so it cannot suggest switching to it",
					mode, other)
			}
		}
		// And the current mode is marked AS the current one, not merely described.
		// The marker sits INSIDE the bold phrase, so it reads as one emphasised item.
		if !strings.Contains(p, "**"+modeLabel(mode)+" (you are here)**") {
			t.Errorf("%s: the roster does not mark the current mode (want %q)",
				mode, "**"+modeLabel(mode)+" (you are here)**")
		}
		// The switch rule is stated in every mode.
		if !strings.Contains(p, "### Suggesting a switch") {
			t.Errorf("%s: the persona does not carry the switch-suggestion rule", mode)
		}
		if !strings.Contains(p, "you cannot change your own mode") {
			t.Errorf("%s: the persona does not say the switch is an OFFER — a mode that thought it could "+
				"switch itself would do so instead of asking", mode)
		}
	}
}

// THE SHARED KNOWLEDGE. Every mode gets the platform primer and the tool surface:
// the modes differ in disposition, never in what they know.
func TestEveryModeSharesThePlatformAndToolKnowledge(t *testing.T) {
	cfg := testAgentConfig()
	reg := testToolRegistry()
	for _, mode := range everyMode {
		p := BuildSystemPrompt(mode, cfg, reg)
		for _, want := range []string{
			"## About Orchicon",
			"## Available Tools",
			"`orchicon_list_projects`",
			"`orchicon_create_work_item`",
			"## Session contract",
			"## Additional Instructions",
			"Tenant additional instructions.",
		} {
			if !strings.Contains(p, want) {
				t.Errorf("%s: the shared block %q is missing — the modes must share their knowledge of the "+
					"platform and their tools", mode, want)
			}
		}
	}
}

// ITERATION NEVER PROPOSES WORK ITEMS OR WORKFLOWS.
//
// The operator, verbatim: "It will never suggest work item creation nor will it ever suggest creating or
// firing off workflows or schedules." That is the mode's defining constraint, so it is asserted as an
// ABSENCE — the strongest form available for a prompt.
func TestIterationNeverProposesWorkItemsOrWorkflows(t *testing.T) {
	p := BuildSystemPrompt(modeIteration, testAgentConfig(), testToolRegistry())

	if !strings.Contains(p, "DO NOT propose creating work items in this mode") {
		t.Error("iteration does not forbid proposing work items")
	}
	// The instruction must not be softened anywhere by the brainstorm drafting rules,
	// whose first step is to ask which workflow to bind.
	if strings.Contains(p, "## Workflow & runtime prompt") {
		t.Error("iteration carries the work-item drafting rules — that contract is about CREATING items, " +
			"which this mode never does")
	}
	// And it must not tell the agent to propose a work item FIRST, which is the
	// brainstorm principle this mode exists in contrast to.
	if strings.Contains(p, "ALWAYS propose creating a work item via the orchicon_create_work_item tool FIRST") {
		t.Error("iteration carries brainstorm's work-item-first principle — the two modes' dispositions " +
			"must not blur")
	}
	// The branch-and-test discipline IS its job, so that must be present.
	for _, want := range []string{"Cut a local branch", "COMMIT EARLY AND OFTEN", "RUN THE TESTS"} {
		if !strings.Contains(p, want) {
			t.Errorf("iteration is missing its hands-on discipline %q", want)
		}
	}
}

// QUICK WORK DISPATCHES, AND CLEANS UP AFTER ITSELF.
//
// The operator: ephemeral items/workers/workflows that never appear in the console and are hard-deleted when
// the job ends; the worker uses the agent's own model_ref; a failure is reported and a re-run offered.
func TestQuickWorkDispatchesEphemerally(t *testing.T) {
	p := BuildSystemPrompt(modeQuickWork, testAgentConfig(), testToolRegistry())

	for _, want := range []string{
		// It dispatches rather than doing the work.
		"Your route to action is DISPATCH",
		// The operator's own gate on firing.
		"CONFIRM BEFORE YOU FIRE",
		// Ephemerality, stated as what it means in practice.
		"The ephemeral protocol",
		"created with the ephemeral flag set",
		"HARD-deleted when the job ends",
		"do NOT appear in any console list",
		// The model_ref rule.
		"THE WORKER RUNS ON YOUR MODEL",
		// The failure rule, in the operator's words.
		"ON FAILURE: DIAGNOSE, REPORT, OFFER A RE-RUN",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("quick work is missing %q", want)
		}
	}
	// The cleanup must be unconditional: success, failure AND abandonment.
	if !strings.Contains(p, "success, failure, and abandonment alike") {
		t.Error("quick work does not state that cleanup is unconditional — a cancelled run left behind is " +
			"exactly the invisible-record pile the operator does not want")
	}
}

// BRAINSTORM ALWAYS CLOSES THE LOOP ON WHAT TO DO WITH THE ANSWER.
//
// The operator: "Brainstorm mode should now purposefully ask you if you would like to create work items for
// the work or work directly with Orchicon after answering the user's question. This check should always be
// reinforced."
func TestBrainstormAlwaysAsksWhatToDoNext(t *testing.T) {
	p := BuildSystemPrompt(modeBrainstorm, testAgentConfig(), testToolRegistry())

	for _, want := range []string{
		"AFTER YOU HAVE ANSWERED, ALWAYS CLOSE THE LOOP ON WHAT TO DO WITH IT",
		"**Create work items**",
		"**Switch modes and I'll do it**",
		"**Hand it to a workflow**",
		`"this check should always be reinforced"`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("brainstorm is missing the reinforced next-step fork %q", want)
		}
	}
	// "Always reinforced" means it is repeated, not asked once: the instruction must
	// say so explicitly, or a model will treat a previous answer as settled.
	if !strings.Contains(p, "ASK IT AGAIN") {
		t.Error("brainstorm does not say the check repeats — the operator's \"always reinforced\" is the " +
			"whole point of it")
	}
}

// AN UNKNOWN MODE IS BRAINSTORM, and it gets a real persona rather than a broken one.
//
// The fallback matters because the mode arrives as free text from a DB column: a value
// written by an older build, or hand-edited, must not produce a prompt with no
// identity or a blank mode banner.
func TestUnknownModeFallsBackToBrainstorm(t *testing.T) {
	cfg := testAgentConfig()
	reg := testToolRegistry()
	want := BuildSystemPrompt(modeBrainstorm, cfg, reg)
	for _, mode := range []string{"", "nonsense", "ORCHICON", "brainstorm "} {
		if got := BuildSystemPrompt(mode, cfg, reg); got != want {
			t.Errorf("BuildSystemPrompt(%q) is not the brainstorm prompt — the fallback must be the safe "+
				"default", mode)
		}
	}
}

// EVERY MODE ROUND-TRIPS THROUGH THE STORE AND THE WIRE.
//
// The mode is persisted as a text value and read back as an enum, so a mode that maps
// one way and not the other would either be stored wrong or displayed blank.
func TestEveryModeRoundTrips(t *testing.T) {
	for _, mode := range everyMode {
		proto := conversationModeToProto(mode)
		if proto == apiv1.ConversationMode_CONVERSATION_MODE_UNSPECIFIED {
			t.Errorf("%s does not map to a proto enum value, so the conversation would report UNSPECIFIED", mode)
			continue
		}
		back, err := conversationModeFromProto(proto)
		if err != nil {
			t.Errorf("%s -> %v -> error: %v", mode, proto, err)
			continue
		}
		if back != mode {
			t.Errorf("%s -> %v -> %s: the round trip does not close", mode, proto, back)
		}
	}
	// And UNSPECIFIED stays the brainstorm default on the way in (an absent mode from
	// an older client must not become a new mode).
	got, err := conversationModeFromProto(apiv1.ConversationMode_CONVERSATION_MODE_UNSPECIFIED)
	if err != nil || got != modeBrainstorm {
		t.Errorf("UNSPECIFIED -> (%q, %v), want the brainstorm default", got, err)
	}
}

// THE PERSONA IS CHOSEN BY THE MODE, not by the default path.
//
// This is the regression that would make the whole feature inert: BuildSystemPrompt used
// to ignore its mode argument entirely and always return the brainstorm persona, so
// every mode behaved identically while the UI reported otherwise.
func TestPersonasDifferByMode(t *testing.T) {
	cfg := testAgentConfig()
	reg := testToolRegistry()
	seen := map[string]string{}
	for _, mode := range everyMode {
		p := BuildSystemPrompt(mode, cfg, reg)
		if prev, ok := seen[p]; ok {
			t.Errorf("%s and %s produce the SAME system prompt — the mode is being ignored", mode, prev)
		}
		seen[p] = mode
	}
	_ = db.AgentConfigRow{}
}

// NO MODE IS GRANTED THE WORK IT CANNOT DO, AND THE PERMISSIONS THAT MADE THAT UNENFORCEABLE ARE GONE.
//
// This is the test that keeps them gone. The boundary could not hold while the prompt granted the permission —
// Brainstorm was told "you may take direct action when the user explicitly asks for it", and its own next-step
// fork offered "Work directly with me". A prompt cannot be argued out of a permission it was explicitly granted,
// which is why every one of these strings had to go before the tool gate could mean anything.
func TestNoModeIsGrantedTheWorkItCannotDo(t *testing.T) {
	banned := []string{
		"you CAN take direct action",
		"You may take direct action when the user explicitly asks for it",
		"Work directly with me",
		"capability rather than preference decides",
		"only implement directly when the user explicitly declines",
		"or working through it directly",
	}
	for _, mode := range everyMode {
		p := BuildSystemPrompt(mode, testAgentConfig(), testToolRegistry())
		for _, b := range banned {
			if strings.Contains(p, b) {
				t.Errorf("%s still carries the permission %q — while that is in the prompt the tool boundary is "+
					"asking the model to decline something the prompt told it it may do", mode, b)
			}
		}
	}
}

// AND EVERY MODE STATES THE SUPERSESSION RULE, because that is what makes a mode SWITCH take effect.
//
// The model's own earlier messages are the anchor that fights a mode change: after switching it can see itself
// saying, a few messages ago, what it does or does not do. The rule is re-sent with every turn, so unlike the
// transcript it cannot go stale. Its absence in any one mode is a mode that would carry the old person forward.
func TestEveryModeSupersedesItsEarlierProse(t *testing.T) {
	for _, mode := range everyMode {
		p := BuildSystemPrompt(mode, testAgentConfig(), testToolRegistry())
		for _, want := range []string{
			"### A mode change SUPERSEDES everything said before it",
			"applied FRESH to every message",
			"is SUPERSEDED",
			// The three anchors it must explicitly refuse to carry, because each is a real one.
			"not from your own previous answers",
			"not from a refusal you gave while in another mode",
			"not from a summary of earlier conversation",
		} {
			if !strings.Contains(p, want) {
				t.Errorf("%s is missing the supersession rule %q — an earlier mode's disposition would survive a "+
					"switch and fight the new persona", mode, want)
			}
		}
	}
}
