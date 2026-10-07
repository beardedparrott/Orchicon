package claude

// permissions_ask_test.go — THE ASK PROFILE'S PROMPT COMES FROM THE PERMISSION SYSTEM.
//
// This is the other half of the fix for "claude adapter ask sessions seem blocked by everything".
// The hook no longer emits an `ask` verdict (the CLI turns that into a tool ERROR, not a host
// prompt — see TestRunHookAbstainsSoThePermissionSystemCanAsk), so the tools that need an operator
// decision must be named HERE, in `permissions.ask`. Without this list the hook abstains and nothing
// raises a prompt: writes and commands would simply run with no card, which is the opposite failure
// and a worse one.
//
// The two halves are only correct TOGETHER, so both are asserted.

import (
	"testing"
)

// launchArgs is the REAL argv for a profile, so the assertion is over the document the CLI is actually
// handed rather than over a bare BuildSettings call. It reuses the package's own settingsDoc (which
// digs the --settings value out of the argv — see permissions_test.go).
func launchArgs(t *testing.T, profile string) []string {
	t.Helper()
	args, err := PermissionArgs(PermissionOptions{Profile: profile, ProjectDir: "/tmp/p"})
	if err != nil {
		t.Fatalf("PermissionArgs(%s): %v", profile, err)
	}
	return args
}

// askList reads permissions.ask out of a profile's launch argv. A nil result means the key is ABSENT,
// which is a different fact from an empty list and is asserted as such by the callers.
func askList(t *testing.T, profile string) []string {
	t.Helper()
	perms, ok := settingsDoc(t, launchArgs(t, profile))["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("the %s profile's settings carry no permissions block", profile)
	}
	raw, ok := perms["ask"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		out = append(out, v.(string))
	}
	return out
}

// THE ASK PROFILE NAMES ITS PROMPT-TOOLS.
func TestAskProfileNamesTheToolsThatMustPrompt(t *testing.T) {
	args := launchArgs(t, ProfileAskEnvValue)
	got := askList(t, ProfileAskEnvValue)
	if len(got) == 0 {
		t.Fatalf("the Ask profile's settings carry NO permissions.ask list, so nothing raises a host prompt.\n"+
			"The hook abstains on an ask verdict (a hook 'ask' becomes a tool ERROR, not a card), so without "+
			"this list a write or a command would run with NO consent at all.\nargv: %v", args)
	}
	// Every tool the Ask rule table wants a decision on must be named. A missing entry is a silent
	// hole: that tool falls to claude's default flow with nothing forcing a prompt.
	// `mcp__*` is DELIBERATELY ABSENT now, and its absence is what this test is guarding: an entry
	// here makes the permission system PROMPT, so an MCP entry would put the card back that the
	// operator asked to remove. MCP is allowed by the HOOK instead (an explicit allow, which bypasses
	// the permission system) — see TestDecideToolForAskAllowsEveryMCPTool.
	for _, want := range []string{"Write", "Edit", "MultiEdit", "NotebookEdit", "Bash"} {
		found := false
		for _, g := range got {
			if g == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("permissions.ask is missing %q (got %v) — that call would not raise a card", want, got)
		}
	}
}

// THE WORKER PROFILE GETS NO ASK LIST — it is non-interactive, so a prompt there is an unanswerable
// refusal, which is exactly what the worker sandbox must not have.
func TestWorkerProfileHasNoAskList(t *testing.T) {
	if got := askList(t, ProfileWorkerEnvValue); len(got) != 0 {
		t.Errorf("the WORKER profile carries permissions.ask %v — a headless worker has nobody to answer, so "+
			"the prompt would resolve as a refusal", got)
	}
	// AND THE ZERO VALUE IS THE WORKER: an existing caller that never sets a profile must not acquire
	// prompts.
	if got := askList(t, ""); len(got) != 0 {
		t.Errorf("a caller that set no profile got permissions.ask %v — the zero value must stay the worker "+
			"sandbox", got)
	}
}

// THE HOOK IS STILL REGISTERED IN BOTH PROFILES, and the ask list is ADDITIVE: the deny list, the
// todo allow list and the hook command are all untouched by this change.
func TestAddingTheAskListChangedNothingElse(t *testing.T) {
	for _, profile := range []string{ProfileWorkerEnvValue, ProfileAskEnvValue, ""} {
		doc := settingsDoc(t, launchArgs(t, profile))
		perms, ok := doc["permissions"].(map[string]any)
		if !ok {
			t.Fatalf("profile %q: the settings carry no permissions block", profile)
		}
		if _, ok := perms["deny"]; !ok {
			t.Errorf("profile %q: the deny list vanished", profile)
		}
		if _, ok := perms["allow"]; !ok {
			t.Errorf("profile %q: the todo allow list vanished", profile)
		}
		if perms["disableBypassPermissionsMode"] != "disable" {
			t.Errorf("profile %q: the bypass-pin vanished", profile)
		}
		hooks, ok := doc["hooks"].(map[string]any)
		if !ok {
			t.Fatalf("profile %q: the hooks block vanished", profile)
		}
		if _, ok := hooks["PreToolUse"]; !ok {
			t.Errorf("profile %q: the PreToolUse hook registration vanished", profile)
		}
	}
}
