package orchicon

// askrefusal_test.go — a refused clarifying question reaches the MODEL as an error.
//
// The question half of consentDenialError. The consent layer refuses a malformed ask_user before any
// card exists (the tool's own validator rejects it) and must hand the adapter something back, because
// the adapter is blocked on the call. It used to hand back a bare sentence, which arrived as the
// ask_user RESULT — i.e. as the operator's answer — so the model was told the operator had said
// "ask_user could not be asked: `question` is required…", and the plane stored it as a SUCCESSFUL tool
// call (`is_error: false`). Three such rows were on the prod plane.
//
// The permission path already carried a marker for exactly this (ConsentRefusedPrefix, whose own doc
// says "the OPERATOR never saw this call, so reporting it as their refusal is a false statement about
// them"). A question now uses the SAME marker, and this pins the other half: the bridge must turn it
// into a tool ERROR, or the model still reads it as content.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// waitForQuestion plays the collector: it reads the bus until the ask_user call is raised, and returns
// its ask id.
func waitForQuestion(t *testing.T, bus scheduler.SessionBus) string {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case evt, ok := <-bus.Events():
			if !ok {
				t.Fatal("the bus closed before the question was raised")
			}
			if evt.Kind == "question" && evt.PermissionID != "" {
				return evt.PermissionID
			}
		case <-timeout:
			t.Fatal("no ask_user question reached the bus")
		}
	}
}

// askUserResult returns the tool result recorded for the ask_user call in the turn's replayed history,
// and whether one exists.
func askUserResult(req TurnRequest) (string, bool, bool) {
	for _, m := range req.Messages {
		for _, c := range m.Content {
			if c.ToolResult != nil {
				return c.ToolResult.Content, true, c.ToolResult.IsError
			}
		}
	}
	return "", false, false
}

// TestAskUserRefusalArrivesAsAToolError is the fix. The refusal the consent layer sends must land as an
// ERROR, so the model treats it as a failed attempt to ask rather than as the operator's words.
func TestAskUserRefusalArrivesAsAToolError(t *testing.T) {
	prov := &chatTestProvider{rounds: [][]Event{
		// Round 1: the model asks, with a malformed call (no question) — the shape that makes the
		// consent layer refuse to raise a card.
		{ToolCall{Index: 0, ToolCallID: "tc-ask", Name: "ask_user", ArgsJSON: `{}`}, Finish{StopReason: StopToolUse}},
		// Round 2: it answers with what it has.
		{TextDelta{Text: "Understood."}, Finish{StopReason: StopStop}},
	}}
	b := newChatBridge(t, prov)
	// The Ask tool provider must exist or the bridge never reaches the ask_user branch at all
	// (`tools == nil` short-circuits to a tool error), which is not the shape under test.
	b.SetAskTools(&fakeAskTools{})
	ctx := tenant.WithID(context.Background(), "tnt_test")
	sid, err := b.CreateConversationSession(ctx, "conv-ask-refusal", "ask-orchicon:conv-ask-refusal")
	if err != nil {
		t.Fatalf("CreateConversationSession: %v", err)
	}
	bus, err := b.Subscribe(ctx, "conv-ask-refusal")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if err := b.SendTurnMessage(ctx, "conv-ask-refusal", sid, "system",
		"orchicon/ollama-cloud/deepseek-v4-flash:0731", "hello"); err != nil {
		t.Fatalf("SendTurnMessage: %v", err)
	}

	// The consent layer refuses, and marks it — exactly what askorchicon now sends.
	askID := waitForQuestion(t, bus)
	const reason = "ask_user: `question` is required and must not be empty"
	if err := b.ReplyPermissionDecision(ctx, sid, askID, ConsentRefusedPrefix+reason); err != nil {
		t.Fatalf("ReplyPermissionDecision: %v", err)
	}

	// The NEXT round re-sends the turn's history, so the result is readable there.
	deadline := time.After(5 * time.Second)
	for {
		if _, ok, _ := askUserResult(prov.lastRequest()); ok || prov.requestCount() > 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the turn never reached a second round, so the result was never recorded")
		case <-time.After(20 * time.Millisecond):
		}
	}
	content, ok, isErr := askUserResult(prov.lastRequest())
	if !ok {
		t.Fatal("no tool result was recorded for the ask_user call")
	}
	if !isErr {
		t.Fatalf("a REFUSED question was recorded as a SUCCESSFUL tool result — the model reads it as the operator's words and every client draws it as `answered`. result: %q", content)
	}
	if !strings.Contains(content, "NOT asked") || !strings.Contains(content, reason) {
		t.Fatalf("the error must say the question was never asked AND carry the reason; got %q", content)
	}
}

// The control: a REAL answer still arrives as the answer. Without this, an error for every question
// would pass the test above.
func TestAskUserAnswerStillArrivesAsTheAnswer(t *testing.T) {
	prov := &chatTestProvider{rounds: [][]Event{
		{ToolCall{Index: 0, ToolCallID: "tc-ask", Name: "ask_user",
			ArgsJSON: `{"question":"Which branch?","options":[{"label":"develop"},{"label":"main"}]}`},
			Finish{StopReason: StopToolUse}},
		{TextDelta{Text: "Thanks."}, Finish{StopReason: StopStop}},
	}}
	b := newChatBridge(t, prov)
	// The Ask tool provider must exist or the bridge never reaches the ask_user branch at all
	// (`tools == nil` short-circuits to a tool error), which is not the shape under test.
	b.SetAskTools(&fakeAskTools{})
	ctx := tenant.WithID(context.Background(), "tnt_test")
	sid, _ := b.CreateConversationSession(ctx, "conv-ask-answer", "ask-orchicon:conv-ask-answer")
	bus, _ := b.Subscribe(ctx, "conv-ask-answer")
	if err := b.SendTurnMessage(ctx, "conv-ask-answer", sid, "system",
		"orchicon/ollama-cloud/deepseek-v4-flash:0731", "hello"); err != nil {
		t.Fatalf("SendTurnMessage: %v", err)
	}

	askID := waitForQuestion(t, bus)
	if err := b.ReplyPermissionDecision(ctx, sid, askID, "develop"); err != nil {
		t.Fatalf("ReplyPermissionDecision: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for prov.requestCount() < 2 {
		select {
		case <-deadline:
			t.Fatal("the turn never reached a second round")
		case <-time.After(20 * time.Millisecond):
		}
	}
	content, ok, isErr := askUserResult(prov.lastRequest())
	if !ok {
		t.Fatal("no tool result was recorded")
	}
	if isErr {
		t.Fatalf("the operator's ANSWER was recorded as an error: %q", content)
	}
	if content != "develop" {
		t.Fatalf("the answer must arrive verbatim as the model's result, got %q", content)
	}
}
