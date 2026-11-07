package askorchicon

// context_budget_test.go — THE OPENING CONTEXT IS BOUNDED, because a local model has to fit in it.
//
// Measured before these changes, on this repository: the opening context of an Ask session was ~25,400
// tokens — 85 tool definitions (~13,200), a markdown copy of those same 85 names+descriptions (~6,100),
// and the static persona (~6,300) — BEFORE any conversation, project context, or skill manifest. On a
// 32k local model that leaves almost nothing for the actual work, and the failure is not an error: the
// session simply degrades as the window fills.
//
// The tests here pin the three cuts that do not cost information. They are bounds, not exact figures,
// because the exact figure depends on the registry and drifting it on every tool addition would make the
// test noise rather than signal.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// TestToolListDoesNotDuplicateDescriptions pins the biggest single cut.
//
// The prompt used to render `- orchicon_<name>: <description> (<mutability>)` for all 85 tools — 24,438
// bytes ≈ 6,100 tokens — a second, lossier copy of what the transport already sends as
// `orchicon.ToolDef{Name, Description, ParamsJSON}`. The NAMES must stay (a model does not discover a
// capability it has never heard of — several tests exist because of that lesson), but the per-tool
// DESCRIPTION must not, because keeping it pays twice for less.
func TestToolListDoesNotDuplicateDescriptions(t *testing.T) {
	reg := NewToolRegistry(nil, nil, nil)
	p := BuildSystemPrompt(modeIteration, db.AgentConfigRow{}, reg, "")

	// Names are present — discoverability is preserved.
	for _, want := range []string{"`orchicon_list_projects`", "`orchicon_create_work_item`"} {
		if !strings.Contains(p, want) {
			t.Errorf("the tool name %s must still appear: a model cannot ask for a capability it has not heard of", want)
		}
	}
	// Descriptions are NOT — this is the duplication being removed.
	for _, td := range reg.List() {
		if len(td.Description) < 120 {
			continue // short descriptions are unlikely to collide with other prose
		}
		if strings.Contains(p, td.Description) {
			t.Errorf("the prompt still carries the full description of %q — that is a duplicate of the tool "+
				"definition the transport sends, and it costs thousands of tokens", td.Name)
		}
	}
}

// TestOpeningContextIsBounded guards the whole budget so a future addition cannot quietly undo these cuts.
func TestOpeningContextIsBounded(t *testing.T) {
	const maxPromptBytes = 40 * 1024 // ~10k tokens of prose; was ~27-37 KB before the cuts
	for _, mode := range []string{modeBrainstorm, modeIteration, modeQuickWork} {
		p := BuildSystemPrompt(mode, db.AgentConfigRow{}, NewToolRegistry(nil, nil, nil), "")
		if len(p) > maxPromptBytes {
			t.Errorf("%s system prompt is %d bytes (~%d tokens), over the %d-byte budget — the opening "+
				"context must stay small enough for a local model to work in",
				mode, len(p), len(p)/4, maxPromptBytes)
		}
	}
}

// TestHistoryIsBoundedByBytesNotJustCount pins the second cut.
//
// History was capped at 10 MESSAGES with no size bound at all. Ten messages is one thing when they are
// ten sentences and quite another when an assistant turn embeds a large file read: 10 × 20 KB is ~50k
// tokens of replay before the operator has typed anything on the current turn.
func TestHistoryIsBoundedByBytesNotJustCount(t *testing.T) {
	// A history of ten messages, each carrying a big blob — the pathological case.
	var history []db.MessageRow
	blob := strings.Repeat("x", 20*1024)
	for i := 0; i < 10; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		history = append(history, db.MessageRow{Role: role, Content: fmt.Sprintf("msg %d %s", i, blob)})
	}

	out := buildSystemPrompt(modeIteration, db.AgentConfigRow{}, NewToolRegistry(nil, nil, nil),
		history, true, nil, "", "", "")

	// The replay must be bounded. The SECTION is measured from its heading to the next one — measuring
	// from the heading to the end of the prompt conflated the history with everything after it, which is
	// how the first version of this assertion "failed" on a history that was already bounded.
	start := strings.Index(out, "## Conversation history")
	if start < 0 {
		t.Fatal("no conversation-history section was rendered")
	}
	end := strings.Index(out[start:], "## This conversation's project")
	if end < 0 {
		end = len(out) - start
	}
	replay := out[start : start+end]
	// Slack covers the section's own heading, trailing note, and the elision notice.
	if len(replay) > historyByteBudget+2*1024 {
		t.Fatalf("history replay is %d bytes (~%d tokens) against a %d-byte budget — ten large messages "+
			"must not be replayed in full", len(replay), len(replay)/4, historyByteBudget)
	}
	// And it must SAY it elided, so the model knows to re-read rather than assuming it saw everything.
	if !strings.Contains(replay, "shortened to fit the context budget") {
		t.Error("the elision must be announced — a model that does not know it lost content will answer as if it had it")
	}
}

// TestSmallHistoryIsUntouched is the control: the budget must not touch an ordinary conversation.
func TestSmallHistoryIsUntouched(t *testing.T) {
	history := []db.MessageRow{
		{Role: "user", Content: "How do I run the tests?"},
		{Role: "assistant", Content: "Use make ci-go."},
	}
	out := buildSystemPrompt(modeIteration, db.AgentConfigRow{}, NewToolRegistry(nil, nil, nil),
		history, true, nil, "", "", "")
	if !strings.Contains(out, "How do I run the tests?") || !strings.Contains(out, "Use make ci-go.") {
		t.Fatal("an ordinary conversation must be replayed verbatim")
	}
	if strings.Contains(out, "shortened to fit") {
		t.Error("nothing should be elided when the history already fits")
	}
}

// TestElisionKeepsHeadAndTail pins WHAT is dropped: the middle, which is re-fetchable, never the question.
func TestElisionKeepsHeadAndTail(t *testing.T) {
	content := "THE QUESTION: why does the build fail? " + strings.Repeat("y", 5000) + " THE ANSWER: a missing import."
	got := elideHistoryMessage(content, 500)
	if len(got) > 500 {
		t.Fatalf("elided message is %d bytes, over the 500 budget", len(got))
	}
	if !strings.Contains(got, "why does the build fail") {
		t.Error("the head carries the question and must survive")
	}
	if !strings.Contains(got, "a missing import") {
		t.Error("the tail carries the conclusion and must survive")
	}
	if !strings.Contains(got, "elided to fit") {
		t.Error("the elision must be marked in place")
	}
}
