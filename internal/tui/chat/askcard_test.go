package chat

import (
	"strings"
	"testing"
	"time"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestConversationItemsEmitsAskCard proves the TUI renders a recorded ask_user
// call as a clarifying-question card: conversationItems emits a KindAsk item
// carrying the question and options parsed out of the message's tool_calls, with
// a stable per-message key.
func TestConversationItemsEmitsAskCard(t *testing.T) {
	msgs := []*apiv1.ChatMessage{{
		Id:   "m1",
		Role: "assistant",
		ToolCalls: []*apiv1.ToolCall{{
			Id:           "tc-1",
			Type:         "function",
			FunctionName: "orchicon_ask_user",
			Arguments:    `{"question":"Which branch should the run clone off?","options":[{"label":"develop","description":"integration"},{"label":"main"}]}`,
		}},
		CreatedAt: timestamppb.New(time.Unix(1, 0)),
	}}

	items := conversationItems(msgs)
	var ask *ChatItem
	for i := range items {
		if items[i].Kind == KindAsk {
			ask = &items[i]
		}
	}
	if ask == nil {
		t.Fatalf("conversationItems did not emit a KindAsk item: %+v", items)
	}
	if ask.Ask == nil {
		t.Fatal("KindAsk item carries no ParsedAsk")
	}
	if ask.Ask.Question != "Which branch should the run clone off?" {
		t.Errorf("question = %q, want it intact", ask.Ask.Question)
	}
	if len(ask.Ask.Options) != 2 || ask.Ask.Options[0].Label != "develop" || ask.Ask.Options[1].Label != "main" {
		t.Errorf("options = %+v, want develop + main intact", ask.Ask.Options)
	}
	if ask.Ask.Options[0].Description != "integration" {
		t.Errorf("description = %q, want integration", ask.Ask.Options[0].Description)
	}
	if ask.Key != "m-m1-ask" {
		t.Errorf("key = %q, want m-m1-ask (stable across reloads)", ask.Key)
	}

	// The card renders the question and the numbered options.
	out := RenderItems(items, 80)
	if !strings.Contains(out, "Which branch should the run clone off?") {
		t.Errorf("rendered transcript does not carry the question:\n%s", out)
	}
	if !strings.Contains(out, "develop") || !strings.Contains(out, "main") {
		t.Errorf("rendered transcript does not carry the options:\n%s", out)
	}
}

// TestParseAskUserCallToleratesMalformedArguments: an unreadable payload must never panic, and
// what it SHOULD produce depends on whether the call has resolved.
//
// THIS TEST USED TO ASSERT THE BUG. It required a card for malformed arguments with NO result —
// which is the PLACEHOLDER state, not a corrupt one: the transcript records a tool call when it is
// ISSUED with `{}` for its arguments, and `ask_user` does not complete while the question is open
// (it BLOCKS; that is the pause). So the placeholder was drawn as a "could not be read" card beside
// the live question — TWO cards for ONE question, for as long as the operator was deciding. The
// operator hit exactly that in the GUI, in those words.
func TestParseAskUserCallToleratesMalformedArguments(t *testing.T) {
	call := &apiv1.ToolCall{Id: "tc-1", FunctionName: "ask_user", Arguments: "{not json"}

	// OPEN (no result): the placeholder. A live card is already drawing the question, so this must
	// draw NOTHING rather than an error about arguments that have not been written yet.
	if got := parseAskUserCall([]*apiv1.ToolCall{call}, nil); got != nil {
		t.Fatalf("an OPEN unreadable call produced a card (%q) — that is the placeholder, and it "+
			"draws alongside the live question as a duplicate", got.Question)
	}

	// RESOLVED and still unreadable: genuinely corrupt. The notice IS worth showing here — it is the
	// only remaining evidence that a question was asked.
	got := parseAskUserCall([]*apiv1.ToolCall{call}, []*apiv1.ToolResult{{
		ToolCallId: "tc-1", Output: "yes",
	}})
	if got == nil {
		t.Fatal("a RESOLVED ask_user call with corrupt arguments must still produce a card")
	}
	if !strings.Contains(got.Question, "could not be read") {
		t.Errorf("question = %q, want the malformed-arguments notice", got.Question)
	}
	if !got.Answered {
		t.Error("the resolved call must report itself answered")
	}
	// A non-ask tool call is ignored entirely.
	if parseAskUserCall([]*apiv1.ToolCall{{FunctionName: "list_projects"}}, nil) != nil {
		t.Error("a non-ask tool call must not produce a card")
	}
}
