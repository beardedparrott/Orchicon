package askorchicon

// quickwork_tools_test.go — THE TOOLS QUICK WORK CANNOT WORK WITHOUT, PINNED.
//
// Reversing the model rule (the worker model is now a per-dispatch QUESTION) and making the git strategy and
// branches an explicit confirmation only work if the mode can actually READ the facts it must ask about and
// actually PUBLISH what it builds. The three tools below are that capability, and each is asserted on the three
// surfaces a capability has to reach to be real: the registry (so the advertised name dispatches), the mode gate
// (so no future deny-list edit silently starves the mode), and the persona itself (so the model knows it exists).

import (
	"context"
	"strings"
	"testing"
)

// THE THREE TOOLS RESOLVE, both as the prompt advertises them and by their bare registry name.
//
// A tool the prompt names but the registry does not carry is worse than a missing tool: the model calls it, the
// call fails, and the failure looks like a platform fault rather than a typo in a prompt.
func TestQuickWorkToolsResolveInTheRegistry(t *testing.T) {
	r := NewToolRegistry(nil, nil, nil)
	for _, name := range []string{
		"publish_workflow_version", // the publish step the protocol was missing entirely
		"get_current_conversation", // how the mode reads its own model_ref instead of guessing at it
		"list_project_branches",    // how it offers real branches when confirming the git plan
	} {
		if _, ok := r.Get(name); !ok {
			t.Errorf("the full registry does not carry %q", name)
		}
		if _, ok := r.Get(normalizeAskToolName(askToolNamePrefix + name)); !ok {
			t.Errorf("the advertised `orchicon_%s` form does not resolve", name)
		}
	}
}

// AND THE MODE GATE ALLOWS THEM. Quick Work's policy denies only the ACTION phase (write/edit/batch_write/bash);
// the platform tools stay available. Asserted rather than assumed because these three are the difference between a
// mode that can dispatch a runnable workflow and one that builds a draft nothing can start.
func TestQuickWorkToolsAreAllowedInQuickWorkMode(t *testing.T) {
	for _, name := range []string{"publish_workflow_version", "get_current_conversation", "list_project_branches"} {
		ok, refusal := modeAllowsTool(modeQuickWork, name)
		if !ok {
			t.Errorf("quick work may not run %q: %s — the mode cannot dispatch a runnable run without it", name, refusal)
		}
	}
}

// THE PERSONA ADVERTISES THEM, which is the only way the model learns they exist.
func TestThePromptAdvertisesTheQuickWorkTools(t *testing.T) {
	r := NewToolRegistry(nil, nil, nil)
	p := BuildSystemPrompt(modeQuickWork, testAgentConfig(), r)
	for _, want := range []string{
		"`orchicon_publish_workflow_version`",
		"`orchicon_get_current_conversation`",
		"`orchicon_list_project_branches`",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("the prompt does not advertise %s, so the model cannot know the capability exists", want)
		}
	}
}

// THE CONVERSATION RIDES THE CONTEXT, next to the mode and by the same mechanism.
//
// This is what makes get_current_conversation possible at all: the tool is handed a context, not a parameter, so
// the session it is describing has to be ON that context. A future refactor that stamps the mode and drops the
// conversation id would break the model question silently — the tool would still resolve, and would fail loud for
// every real turn.
func TestTheConversationRidesTheContext(t *testing.T) {
	ctx := withAskConversation(context.Background(), "conv_123")
	if got := askConversationFromContext(ctx); got != "conv_123" {
		t.Errorf("round trip lost the conversation id: got %q, want %q", got, "conv_123")
	}
	// Stamping one must not disturb the other — they are read from one context at the tool boundary.
	both := withAskMode(ctx, modeQuickWork)
	if got := askConversationFromContext(both); got != "conv_123" {
		t.Errorf("stamping the mode clobbered the conversation id: got %q", got)
	}
	if got := askModeFromContext(both); got != modeQuickWork {
		t.Errorf("stamping the conversation clobbered the mode: got %q", got)
	}
	// An unstamped (and a nil) context reports empty rather than panicking: the helper is called from paths that
	// may carry neither.
	if got := askConversationFromContext(context.Background()); got != "" {
		t.Errorf("an unstamped context reported %q, want empty", got)
	}
	if got := askConversationFromContext(nil); got != "" {
		t.Errorf("a nil context reported %q, want empty", got)
	}
}

// THE MODEL LOOKUP FAILS LOUD WHEN IT CANNOT IDENTIFY THE SESSION.
//
// The tempting alternative — answer from the tenant default anyway — is the one behaviour this tool must NOT have:
// a confident model_ref for a session it could not name gets pinned into a worker, and a worker's model has no
// failover. An error the caller can see beats a wrong answer the caller cannot.
//
// The pool is nil on purpose: the conversation check runs BEFORE any database access, so this asserts the order
// as well as the message.
func TestGetCurrentConversationFailsLoudWithoutAConversation(t *testing.T) {
	_, err := toolGetCurrentConversation(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("the model lookup answered without a stamped conversation — a guess here gets pinned into a worker " +
			"that cannot fail over")
	}
	if !strings.Contains(err.Error(), "no conversation is stamped") {
		t.Errorf("the failure does not say what is wrong: %v", err)
	}
}
