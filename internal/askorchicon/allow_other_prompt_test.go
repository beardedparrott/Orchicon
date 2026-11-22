package askorchicon

// allow_other_prompt_test.go — EVERY mode must put a free-text row on its option cards.
//
// THE COMPLAINT. Ask cards arrived with a closed list of options and no way to answer in your own
// words, so an operator whose real answer was not on the list had two choices: pick something that
// was not true, or abandon the card. The agent was technically correct — `allow_other` was optional
// and it simply did not set it.
//
// A tool's schema is a suggestion; a rule in the PROMPT is what an agent actually follows. So the
// requirement lives in writeToolList, which every mode calls, and the tool description was made to
// agree with it rather than leaving two different stories in the same context.
//
// These tests assert the prompt, per mode, because writeToolList is shared and a future refactor
// that gave one mode its own tool section would silently drop the rule from that mode alone.

import (
	"strings"
	"testing"
)

func TestEveryModeTellsTheAgentToAllowOther(t *testing.T) {
	for _, tc := range []struct{ mode, label string }{
		{modeBrainstorm, "Brainstorm"},
		{modeIteration, "Iteration"},
		{modeQuickWork, "Quick Work"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			p := BuildSystemPrompt(tc.mode, testAgentConfig(), NewToolRegistry(nil, nil, nil), "")
			if !strings.Contains(p, "ALWAYS SET `allow_other: true`") {
				t.Fatalf("%s prompt does not command allow_other on option cards", tc.label)
			}
			// The rule must say WHY, because an agent that understands the reason applies it to the
			// card shape the sentence did not anticipate. A bare "set this" is the kind of
			// instruction that gets dropped under context pressure.
			if !strings.Contains(p, "shortcut rather than a cage") {
				t.Errorf("%s prompt states the rule without its reason", tc.label)
			}
			// And it must be unconditional: the exceptions an agent invents for itself are exactly
			// the cards that came back without a free-text row.
			if !strings.Contains(p, "there is no exception") {
				t.Errorf("%s prompt leaves room for the agent to skip allow_other", tc.label)
			}
		})
	}
}

// TestAskUserToolDescriptionAgreesWithThePrompt guards against the two going out of step. The agent
// reads BOTH the prompt and the tool schema in the same turn; if the schema still says allow_other is
// a sometimes-thing while the prompt says always, the weaker wording wins often enough to matter.
func TestAskUserToolDescriptionAgreesWithThePrompt(t *testing.T) {
	reg := NewToolRegistry(nil, nil, nil)
	tool, ok := reg.Get("ask_user")
	if !ok {
		t.Fatal("ask_user is not registered")
	}
	if !strings.Contains(tool.Description, "ALWAYS") {
		t.Errorf("ask_user description does not say allow_other is always required:\n%s", tool.Description)
	}
	prop, ok := tool.Properties["allow_other"]
	if !ok {
		t.Fatal("allow_other is no longer a property of ask_user")
	}
	if !strings.Contains(prop.Description, "EVERY OPTION CARD") {
		t.Errorf("allow_other property description is not emphatic:\n%s", prop.Description)
	}
}

// TestFreeTextOnlyQuestionStillWorks makes sure the rule did not make options mandatory: a question
// with no options is answered entirely in free text, and the instruction says so.
func TestFreeTextOnlyQuestionStillWorks(t *testing.T) {
	p := BuildSystemPrompt(modeIteration, testAgentConfig(), NewToolRegistry(nil, nil, nil), "")
	if !strings.Contains(p, "Omit it only for a free-text-only question") {
		t.Error("the rule does not carve out the free-text-only case, so it would forbid a valid card")
	}
}
