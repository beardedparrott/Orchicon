package askorchicon

// brainstorm_git_prompt_test.go — REQUIREMENT 1: PROJECT CREATION ASKS FOR THE GIT STRATEGY.
//
// The operator's instruction: "add in a prompt in Brainstorm mode on project creation to send an ask card asking
// what git strategy the user wants", and then, "if any git type is answered, ask 'Would you like me to initialize
// the git repository' if it doesn't already exist and then have the MCP call run the EnsureGitState".
//
// This is prose, so it is pinned as prose — but pinned, because the ASK PATH is exactly what was missing: the
// tool wrote the project row without ever offering a strategy or a repository, and the first run then had
// nothing to branch from. A future edit that quietly drops the question would restore the original defect with
// every test still green.

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

func brainstormPromptForTest() string {
	// A registry with no pool: the prompt only needs the tool LIST, which is static metadata.
	return brainstormModeSystemPrompt(db.AgentConfigRow{}, NewToolRegistry(nil, slog.New(slog.DiscardHandler), nil), "")
}

// THE STRATEGY IS ASKED, as a choice with all three options named.
func TestBrainstormPromptAsksForTheGitStrategy(t *testing.T) {
	p := brainstormPromptForTest()
	// "GIT STRATEGY" (uppercase) is how the drafting rule spells it; the lowercase spelling this test
	// used to require came from the `create_project` TOOL DESCRIPTION, which was removed as a duplicate of
	// the transport's own definition. The CONTRACT is that the strategy is asked for as a real choice, and
	// that lives in the rule text — so the assertion follows the contract, not the removed duplicate.
	for _, want := range []string{
		"GIT STRATEGY",
		"local", "pr", "none",
		"orchicon_ask_user",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("the Brainstorm prompt no longer mentions %q — project creation must ASK for the git "+
				"strategy as a real choice", want)
		}
	}
	if !strings.Contains(p, "integration branch") {
		t.Error("the prompt does not explain what `local` does (commit to a branch the platform integrates), so " +
			"the user cannot answer the question meaningfully")
	}
}

// THE REPOSITORY IS OFFERED when the answer needs one, and the FLAG is named so the agent can act on it.
func TestBrainstormPromptOffersToInitialiseTheRepository(t *testing.T) {
	p := brainstormPromptForTest()
	for _, want := range []string{
		"initialise the git repository", // the question, in the operator's own words
		"init_git",                      // the tool parameter that acts on the answer
		"EnsureGitState",                // and the statement that it is the SAME setup the console runs
	} {
		if !strings.Contains(p, want) {
			t.Errorf("the Brainstorm prompt no longer contains %q — the agent would ask for a strategy and then "+
				"have no way to set the project up", want)
		}
	}
	// AND THE CONSEQUENCE OF DECLINING IS STATED, so "no" is an informed answer rather than a shrug.
	if !strings.Contains(p, "could not branch") && !strings.Contains(p, "work in place") {
		t.Error("the prompt does not say what the user gives up by declining — without a repository a local/pr " +
			"project can only work in place")
	}
}

// IT MUST NOT TELL THE AGENT TO RUN GIT ITSELF — Brainstorm has no shell, and the platform refuses one.
func TestBrainstormPromptDoesNotHandRollGitInit(t *testing.T) {
	p := brainstormPromptForTest()
	if strings.Contains(p, "git init") && !strings.Contains(p, "Never git-init by hand") {
		t.Error("the prompt mentions `git init` without forbidding a hand-rolled one: this mode has no shell, and " +
			"the supported path is the init_git flag")
	}
}
