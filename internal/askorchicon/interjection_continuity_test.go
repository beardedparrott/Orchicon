package askorchicon

// interjection_continuity_test.go — what a superseded turn's successor is told.
//
// REPORTED: "whenever I interject on an ask orchicon live session, the model doing the work seems to lose
// track of what it was already working on and seems to think that the current work may have been done by
// someone else. This is a real issue with interjecting. It should just be adding additional context to the
// current running session and not break things."
//
// The mechanism: interjecting SUPERSEDES, which aborts the provider session mid-generation. What survives in
// the session is the EVIDENCE of the cut-off turn — tool results, file changes, transcript — without the
// assistant turn that produced it. A model handed that state sees finished work it has no record of doing.
//
// These pin the corrective statement, and the conditions under which it appears.

import (
	"strings"
	"testing"
)

func TestContinuitySectionAnswersBothReportedSymptoms(t *testing.T) {
	got := interjectionContinuitySection()

	// SYMPTOM 1: "thinks the current work may have been done by someone else".
	for _, want := range []string{"YOURS", "YOUR OWN", "produced by YOU"} {
		if !strings.Contains(got, want) {
			t.Errorf("the section does not assert %q, so a model reading orphaned evidence can still "+
				"conclude another party did the work:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "another person") || !strings.Contains(got, "another agent") {
		t.Error("the section does not name the mis-attributions it is correcting")
	}

	// SYMPTOM 2: "loses track of what it was already working on".
	if !strings.Contains(got, "Resume") {
		t.Errorf("the section does not tell the model to RESUME the task it was on:\n%s", got)
	}
	if !strings.Contains(got, "ADDITIONAL CONTEXT") {
		t.Error("the section does not say the operator's message is additional context — which is the " +
			"behaviour the operator asked for in their own words")
	}
	// AND THE THING NOT TO DO, which is what "loses track" looks like in practice.
	if !strings.Contains(got, "do not redo it") {
		t.Error("the section does not forbid redoing work it already did, so a model that cannot see its " +
			"own authorship re-does it")
	}
}

func TestContinuitySectionNamesConcreteEvidenceNotAbstractions(t *testing.T) {
	// A model mis-attributes what it is LOOKING AT. Naming the artefacts (tool results, file changes,
	// transcript) is what makes the assertion checkable against its own context; "previous context" would
	// not be.
	got := interjectionContinuitySection()
	for _, want := range []string{"tool result", "file change", "transcript"} {
		if !strings.Contains(got, want) {
			t.Errorf("the section does not name %q among the evidence that is the model's own", want)
		}
	}
}

func TestContinuitySectionInventsNoSpecifics(t *testing.T) {
	// It must not name a project, a file or a task: this function knows none of them, and a guess presented
	// as fact is worse than silence.
	got := interjectionContinuitySection()
	for _, forbidden := range []string{"Orchicon/", "internal/", "/home/", ".go", "the project you"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("the section invents a specific (%q) it cannot know:\n%s", forbidden, got)
		}
	}
}

// TestSupersedeIsWhatAddsContinuity pins the CONDITION, which is the half most likely to be got wrong: adding
// this to every turn would tell an ordinary message it was interrupted, and omitting it on the wedged-session
// path would leave exactly the case the operator hit without it.
func TestSupersedeIsWhatAddsContinuity(t *testing.T) {
	src := readChatSource(t)

	if !strings.Contains(src, "interruptedTurn = prevOk") {
		t.Fatal("nothing records that a running turn was actually interrupted. `opts.supersede` alone is " +
			"not the condition — an interjection with NO running turn behaves like ChatStream and was never " +
			"interrupted, so telling it otherwise is false")
	}
	if !strings.Contains(src, "if interruptedTurn {") {
		t.Fatal("the continuity section is not gated on an actual interruption")
	}
	// BOTH VARIANTS. `seed` is used when the session is recreated (a wedged interjection recycles it) and
	// `reuse` in the steady state; adding it to only one leaves a live path without it.
	if !strings.Contains(src, "seedSystem += continuity") || !strings.Contains(src, "reuseSystem += continuity") {
		t.Error("the continuity section is added to only one system-prompt variant; both are reachable on an " +
			"interjection (seed after a wedged-session recycle, reuse in the steady state)")
	}
}

// TestContinuityRidesTheSystemPromptNotTheUserMessage is the other half: the operator must not find a paragraph
// they did not type sitting in their own message in the transcript.
func TestContinuityRidesTheSystemPromptNotTheUserMessage(t *testing.T) {
	src := readChatSource(t)
	if strings.Contains(src, "msg += continuity") || strings.Contains(src, "msg = msg + continuity") {
		t.Fatal("the continuity text is appended to the USER message, so it is persisted and rendered in the " +
			"operator's transcript as if they had typed it — the system prompt is model-only")
	}
}
