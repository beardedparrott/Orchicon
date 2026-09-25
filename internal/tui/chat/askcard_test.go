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

// TestParseAskUserCallToleratesMalformedArguments: a malformed recorded payload
// must not drop the card or panic — it renders a visible error row instead.
func TestParseAskUserCallToleratesMalformedArguments(t *testing.T) {
	got := parseAskUserCall([]*apiv1.ToolCall{{
		FunctionName: "ask_user",
		Arguments:    "{not json",
	}})
	if got == nil {
		t.Fatal("a recorded ask_user call must still produce a card")
	}
	if !strings.Contains(got.Question, "could not be read") {
		t.Errorf("question = %q, want the malformed-arguments notice", got.Question)
	}
	// A non-ask tool call is ignored entirely.
	if parseAskUserCall([]*apiv1.ToolCall{{FunctionName: "list_projects"}}) != nil {
		t.Error("a non-ask tool call must not produce a card")
	}
}
