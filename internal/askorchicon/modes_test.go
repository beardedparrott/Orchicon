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
		// The failure rule, in the operator's words.
		"ON FAILURE: DIAGNOSE, REPORT, OFFER A RE-RUN",
		// THE MODEL IS ASKED AND NAMED, not assumed. This REPLACES the old
		// "THE WORKER RUNS ON YOUR MODEL" assertion, which pinned the rule the
		// operator reversed: the worker no longer silently inherits the agent's
		// model, and the agent must offer the real ref by name every dispatch.
		"ASK WHICH MODEL ON EVERY NEW DISPATCH",
		"Would you like to use the current model",
		"or would you like to choose a different one for this run?",
		"orchicon_get_current_conversation",
		// GIT IS CONFIRMED, NOT ASSUMED — strategy, and BOTH branches.
		"ASK WHICH BRANCHES ON EVERY NEW DISPATCH",
		"WHICH BRANCH TO CLONE OFF",
		"WHICH BRANCH TO MERGE INTO",
		"orchicon_list_project_branches",
		// The protocol can actually produce a RUNNABLE workflow: both halves
		// are published, which the old protocol omitted entirely — leaving a
		// draft that nothing could dispatch.
		"Publish the worker",
		"Publish the workflow",
		"publish_workflow_version",
		// Ephemeral items cannot nest.
		"TOP-LEVEL ONLY",
		// The seeded pair is an EXAMPLE, never the machinery.
		"never as the machinery",
		"Do NOT bind them for a job",
		// The platform sweep is a backstop, not a cleanup substitute.
		"BACKSTOP",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("quick work is missing %q", want)
		}
	}
	// The REVERSED rule must be GONE: a prompt still saying the worker runs on the agent's model, and that the
	// agent must not ask, would fight the instruction it now carries.
	for _, forbid := range []string{"THE WORKER RUNS ON YOUR MODEL", "do not ask the user to choose one"} {
		if strings.Contains(p, forbid) {
			t.Errorf("quick work still carries the superseded model rule %q — the worker model is now a per-dispatch "+
				"question the agent must ask, so the old prohibition cannot stay", forbid)
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

// --- The integration-completeness contract -------------------------------------------
//
// The operator's requirement, verbatim:
//
//	"all work items created or work done (in iteration mode's standpoint) should ensure that all
//	 interconnecting systems are fleshed out and called out before work is done or inside the work item for
//	 BrainStorm mode. No half written realized code/instructions. All interconnected systems should have a
//	 map and a list of what talks to it and ALL components that make it work should also be touched and
//	 worked on if needed."
//
// It is stated ONCE in a shared block plus a PER-MODE TAIL saying WHERE the map has to be written, because the
// modes differ in where the artefact lives, never in whether the map is required: a worker reads a brief and
// nothing else (Brainstorm), the user is in the loop (Iteration), and the worker never sees the conversation
// (Quick Work).

// integrationMapBlock returns the SHARED block's text so a test can assert on it in ISOLATION. The rest of a
// persona mentions Orchicon's own components legitimately — the platform primer names the adapter, the model
// question names a registered adapter kind — so a part-agnostic assertion has to be scoped to the block rather
// than to the whole prompt, or it fails on text that is right.
func integrationMapBlock(t *testing.T, prompt string) string {
	t.Helper()
	const head = "## Integration completeness — draw the map, then close it"
	i := strings.Index(prompt, head)
	if i < 0 {
		t.Fatalf("the integration-completeness block is absent from the prompt")
	}
	rest := prompt[i:]
	// Cut at the next H2 — the per-mode tail — and never at an H3 inside the block.
	if j := strings.Index(rest[len(head):], "\n## "); j >= 0 {
		rest = rest[:len(head)+j]
	}
	return rest
}

// EVERY MODE OWES THE MAP. All three produce work, so all three carry the discipline.
func TestEveryModeCarriesTheIntegrationMapContract(t *testing.T) {
	for _, mode := range everyMode {
		p := BuildSystemPrompt(mode, testAgentConfig(), testToolRegistry())
		for _, want := range []string{
			"### 1. Draw the map BEFORE you write anything",
			"### 2. Close the map — no half-realized work",
			"What it depends on",
			"What depends on it",
			"What must LEARN about it",
			"What OBSERVES it",
			"A seam nothing calls is not a feature",
			`"Verified" means the outermost consumer works`,
		} {
			if !strings.Contains(p, want) {
				t.Errorf("%s: the integration contract is missing %q — a mode without it can hand back a green, "+
					"broken feature", mode, want)
			}
		}
	}
}

// THE VOCABULARY IS PART-AGNOSTIC — systems design, architecture and UI/UX, never Orchicon's own members.
//
// The operator: "people will be using Orchicon to build out many projects, not just Orchicon itself, so the
// wording needs to be a bit more agnostic and grounded in systems design, systems architecture, and UI/UX design
// rather than calling out specific components." A rule that named the instance would teach a project with no
// adapters nothing, so the INSTANCE is asserted ABSENT and the PATTERN's vocabulary asserted present.
func TestIntegrationMapSpeaksSystemsDesignNotOrchiconComponents(t *testing.T) {
	for _, mode := range everyMode {
		block := integrationMapBlock(t, BuildSystemPrompt(mode, testAgentConfig(), testToolRegistry()))
		lower := strings.ToLower(block)

		for _, banned := range []string{"adapter", "opencode", "orchicon", "claude", "seed_workflows", "work item"} {
			if strings.Contains(lower, banned) {
				t.Errorf("%s: the shared block names Orchicon's own member %q — the rule has to read as systems design "+
					"so it applies to ANY project", mode, banned)
			}
		}
		// The systems/architecture half.
		for _, want := range []string{
			"registries and dispatch tables",
			"configuration defaults",
			"migrations",
			"the API/wire contract at each boundary",
			"the tests that assert each",
		} {
			if !strings.Contains(block, want) {
				t.Errorf("%s: the shared block is missing the systems-design vocabulary %q", mode, want)
			}
		}
		// The UI/UX half — what makes the rule apply to a design or front-end project, not only a back end.
		for _, want := range []string{
			"loading, empty, error, partial and permission-denied",
			"design-system primitives and tokens",
			"keyboard, focus, resize",
			"A user-facing element without its states is not a feature",
		} {
			if !strings.Contains(block, want) {
				t.Errorf("%s: the shared block is missing the UI/UX vocabulary %q", mode, want)
			}
		}
	}
}

// THE SHARED BLOCK IS SHARED — byte-for-byte identical in all three modes — and the TAILS DIFFER.
//
// Two halves of one design decision, so both are asserted together: a single function is what stops the rule
// drifting and stops its tokens being paid three times, and a mode-specific tail is what keeps the PLACEMENT
// rule (brief / reply / dispatch brief) from collapsing into one generic statement.
func TestTheIntegrationMapBlockIsSharedAndThePlacementTailsAreNot(t *testing.T) {
	var first string
	for _, mode := range everyMode {
		block := integrationMapBlock(t, BuildSystemPrompt(mode, testAgentConfig(), testToolRegistry()))
		if first == "" {
			first = block
			continue
		}
		if block != first {
			t.Errorf("%s's integration block differs from the first mode's — the shared block has drifted, which is "+
				"the failure having ONE function exists to prevent", mode)
		}
	}

	placements := map[string]string{
		modeBrainstorm: "## The integration map in every work item",
		modeIteration:  "## Working in the open means showing the map",
		modeQuickWork:  "## The map travels with the brief",
	}
	for _, mode := range everyMode {
		p := BuildSystemPrompt(mode, testAgentConfig(), testToolRegistry())
		want, ok := placements[mode]
		if !ok {
			t.Fatalf("%s has no expected placement tail — a mode added later must be added here too", mode)
		}
		if !strings.Contains(p, want) {
			t.Errorf("%s does not carry its own map-placement tail %q", mode, want)
		}
		// And no OTHER mode's tail leaked in: the placement rule is mode-specific or it is not a rule.
		for other, heading := range placements {
			if other != mode && strings.Contains(p, heading) {
				t.Errorf("%s carries %s's placement tail %q — the modes' map-placement rules have blurred",
					mode, other, heading)
			}
		}
	}
}

// BRAINSTORM: the map goes IN THE ITEM, because the brief is all a worker gets.
func TestBrainstormPutsTheIntegrationMapInEveryWorkItem(t *testing.T) {
	p := BuildSystemPrompt(modeBrainstorm, testAgentConfig(), testToolRegistry())
	for _, want := range []string{
		"The DESCRIPTION carries an **Integration map** section",
		"ACCEPTANCE CRITERIA must cover the EDGES, not only the centre",
		"A FEATURE OWNS ITS EDGES AND ITS INTEGRATION",
		"its LAST child is the end-to-end proof",
		"NO DEFERRED EDGES",
		"never a follow-up item",
		"requires a test that asserts the totality",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("brainstorm is missing %q — the worker cannot infer an edge the brief leaves out, so a brief "+
				"without the map is a brief that can be completed perfectly and wrongly", want)
		}
	}
}

// ITERATION: the map goes IN THE REPLY, before the first edit, and is closed in the SAME session.
func TestIterationShowsTheMapAndClosesItInTheSameSession(t *testing.T) {
	p := BuildSystemPrompt(modeIteration, testAgentConfig(), testToolRegistry())
	for _, want := range []string{
		"WRITE THE MAP IN YOUR REPLY",
		"same branch, same commit sequence",
		"the states are part of it: loading, empty, error and denied",
		"Your definition of done is the OUTERMOST consumer working",
		"Run it and show the output",
		"are checkpoints, not completion",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("iteration is missing %q — this mode must show the map, then close every edge it named", want)
		}
	}
	// The tail must not smuggle in brainstorm's authoring vocabulary: this mode authors no work items, and a
	// rule about what goes INTO one would be a rule this mode cannot act on.
	if strings.Contains(p, "The integration map in every work item") {
		t.Error("iteration carries brainstorm's map-into-the-item rule — Iteration authors no work items")
	}
}

// QUICK WORK: the map goes in the BRIEF and the WORKER'S OWN PROMPT, because the worker never sees this chat.
func TestQuickWorkPutsTheMapInTheBriefAndTheWorkerPrompt(t *testing.T) {
	p := BuildSystemPrompt(modeQuickWork, testAgentConfig(), testToolRegistry())
	for _, want := range []string{
		"Write the MAP into the ephemeral item's description",
		"Write the CLOSURE into the brief as an instruction",
		"Put the EDGES into the acceptance criteria",
		"Keep the run SINGLE-RUNNABLE",
		"BLOCKING on the first",
		"The worker's OWN prompt must carry it too",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("quick work is missing %q — the dispatched worker never sees this conversation, so the brief "+
				"and its own prompt are the only places the map can live", want)
		}
	}
}
