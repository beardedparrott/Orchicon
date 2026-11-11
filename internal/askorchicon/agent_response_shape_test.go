package askorchicon

// agent_response_shape_test.go — Iteration's reply shape is part of its prompt.
//
// The operator's request: replies "shouldn't lack info, but they shouldn't be essay form... what was
// done, what was solved, and possibly what wasn't." A prompt is the only lever that shapes every
// reply, so its absence is silent: nothing fails, replies just get longer.

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// TestIterationPromptCarriesTheResponseShape asserts the guidance EXISTS and that the parts which make
// it useful survive a later edit. A source assertion is the right tool here: the builder's output IS
// the prompt, and no behavioural seam reaches a prompt sentence.
func TestIterationPromptCarriesTheResponseShape(t *testing.T) {
	prompt := BuildSystemPrompt(modeIteration, db.AgentConfigRow{}, NewToolRegistry(nil, nil, nil), "")
	if !strings.Contains(prompt, "## Response shape") {
		t.Fatal("the Iteration prompt lost its Response shape section — replies drift back to essay form")
	}
	// WHAT WAS DONE, WHAT WAS SOLVED, WHAT WASN'T — plus the one line that stops brevity becoming
	// vagueness.
	for _, want := range []string{
		"answer in a sentence or two",
		"what changed",
		"what is verified and how",
		"what is NOT solved",
		"Omit an empty part rather than padding it",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the Response shape guidance no longer says %q", want)
		}
	}
	// AND IT MUST NOT BE SHARED: Quick Work dispatches and Brainstorm plans, so neither is told to
	// report files and commits it will never touch.
	for name, other := range map[string]string{
		"quick work": BuildSystemPrompt(modeQuickWork, db.AgentConfigRow{}, NewToolRegistry(nil, nil, nil), ""),
		"brainstorm": BuildSystemPrompt("", db.AgentConfigRow{}, NewToolRegistry(nil, nil, nil), ""),
	} {
		if strings.Contains(other, "## Response shape") {
			t.Errorf("the Response shape section leaked into %s's prompt", name)
		}
	}
}

// THE RESEARCH DISCIPLINE, same reasoning as the shape above: a prompt is the only place this is stated,
// and its absence is silent — the agent just burns cycles re-deriving what a search would have settled.
func TestIterationPromptTellsTheAgentToResearchBeforeBuilding(t *testing.T) {
	prompt := BuildSystemPrompt(modeIteration, db.AgentConfigRow{}, NewToolRegistry(nil, nil, nil), "")
	if !strings.Contains(prompt, "## Research before you build") {
		t.Fatal("the Iteration prompt lost its research-before-you-build section — agents go back to " +
			"re-deriving problems a search would settle")
	}
	// EACH ROUTE THE SESSION ACTUALLY HAS. Naming a route the mode cannot reach would be advice to use
	// a tool it does not have.
	for _, want := range []string{
		"ARCHITECTURE.md",     // the project's own docs
		"module cache",        // the dependency's source
		"gh and curl",         // upstream / open source
		"fetch or search MCP", // the web, when a server is mounted
		"SEARCH FOR IT BEFORE GUESSING",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the research guidance no longer names %q", want)
		}
	}
	// AND IT MUST NOT BECOME A REASON TO HAND THE WORK BACK: this mode still does the fixing.
	if !strings.Contains(prompt, "the fix still happens HERE") {
		t.Error("the research guidance lost its boundary — it must not read as permission to delegate")
	}
	// SCOPED TO ITERATION: Quick Work dispatches and Brainstorm plans, so neither is told how to research
	// a change it will not make.
	for name, other := range map[string]string{
		"quick work": BuildSystemPrompt(modeQuickWork, db.AgentConfigRow{}, NewToolRegistry(nil, nil, nil), ""),
		"brainstorm": BuildSystemPrompt("", db.AgentConfigRow{}, NewToolRegistry(nil, nil, nil), ""),
	} {
		if strings.Contains(other, "## Research before you build") {
			t.Errorf("the research section leaked into %s's prompt", name)
		}
	}
}
