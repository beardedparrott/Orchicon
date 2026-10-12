package opencode

// system_delivery_test.go — THE SYSTEM PROMPT MUST REACH THE MODEL ON v2, AND EXACTLY ONCE PER SESSION.
//
// v2's prompt body has no `system` field (checked across all 118 spec paths), so the prompt travels as
// CONTENT. Two ways to get that wrong, and both are pinned here:
//
//   * NOT delivering it: the worker runs with only the minimal tool shell from the agent config and
//     never learns its task framing — and NOTHING errors, because v2 silently drops unknown keys.
//   * delivering it on EVERY turn: the context grows by a full prompt copy per message, which on a
//     long run is a large, self-inflicted cost that no single message looks wrong in isolation.
//
// The rule that avoids both: carry it on the session's FIRST message and again after a compact, and on
// no other send. Callers that know which is which say so (SendMessageCarryingSystem); the Ask path,
// where the seeded and reuse prompts are both non-empty, uses the client's own record of the sessions
// it CREATED.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// bodyOf returns the `text` a request carried, or "" — the v2 delivery channel.
func textOf(r capturedReq) string {
	t, _ := r.body["text"].(string)
	return t
}

func TestV2FirstMessageCarriesTheSystemPrompt(t *testing.T) {
	var seen []capturedReq
	srv := captureServer(t, &seen)

	c := NewSessionClient(srv.URL, "", "")
	c.setAPIShape(shapeV2)

	// Create first: this is what marks the session as awaiting the prompt.
	sid, err := c.CreateSession(context.Background(), "t")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	seen = nil

	if err := c.SendMessageCarryingSystem(context.Background(), sid, "YOU ARE A WORKER", "", "do the thing"); err != nil {
		t.Fatalf("send: %v", err)
	}
	got := textOf(promptRequest(t, seen))
	if !strings.Contains(got, "YOU ARE A WORKER") {
		t.Errorf("the first message must CARRY the system prompt on v2 — this surface has no `system` "+
			"field, so content is the only channel. got: %q", got)
	}
	if !strings.Contains(got, "do the thing") {
		t.Errorf("the message itself must still be there, got: %q", got)
	}
}

func TestV2LaterMessagesDoNotRepeatThePrompt(t *testing.T) {
	// THE GROWTH PIN. Every turn after the first must send ONLY the message.
	var seen []capturedReq
	srv := captureServer(t, &seen)

	c := NewSessionClient(srv.URL, "", "")
	c.setAPIShape(shapeV2)
	sid, _ := c.CreateSession(context.Background(), "t")
	seen = nil

	// A plain send — what redirects, retries and probes use.
	if err := c.SendMessage(context.Background(), sid, "YOU ARE A WORKER", "", "second turn"); err != nil {
		t.Fatalf("send: %v", err)
	}
	got := textOf(promptRequest(t, seen))
	if strings.Contains(got, "YOU ARE A WORKER") {
		t.Errorf("a NON-first send must not repeat the prompt — on a long run that adds a full copy "+
			"per message. got: %q", got)
	}
	if got != "second turn" {
		t.Errorf("the message must travel verbatim, got: %q", got)
	}
}

func TestV1IsUnchangedByTheDeliveryRule(t *testing.T) {
	// v1 HAS a system field and its behaviour must be byte-identical: the field is set on every send
	// (resent per turn by design), and the text is NOT prepended.
	var seen []capturedReq
	srv := captureServer(t, &seen)

	c := NewSessionClient(srv.URL, "", "")
	if got := c.APIShapeName(); got != "v1" {
		t.Fatalf("default shape = %q, want v1", got)
	}
	sid, _ := c.CreateSession(context.Background(), "t")
	seen = nil

	if err := c.SendMessageCarryingSystem(context.Background(), sid, "YOU ARE A WORKER", "", "second turn"); err != nil {
		t.Fatalf("send: %v", err)
	}
	got := promptRequest(t, seen)
	if got.body["system"] != "YOU ARE A WORKER" {
		t.Errorf("v1 must still set the `system` field, got %v", got.body["system"])
	}
	if parts, ok := got.body["parts"].([]any); !ok || len(parts) != 1 {
		t.Fatalf("v1 body must still be a parts array, got %v", got.body)
	}
	p0, _ := got.body["parts"].([]any)[0].(map[string]any)
	if p0["text"] != "second turn" {
		t.Errorf("v1 must NOT prepend the prompt into the message text — it has a field for it. "+
			"got %q", p0["text"])
	}
}

func TestTheAskPathDeliversOnceViaTheCreationMark(t *testing.T) {
	// The Ask path cannot tell a first turn from a later one by the prompt being non-empty (the seeded
	// and reuse prompts are BOTH non-empty), so it relies on the client's record of what it created.
	var seen []capturedReq
	srv := captureServer(t, &seen)

	c := NewSessionClient(srv.URL, "", "")
	c.setAPIShape(shapeV2)
	sid, _ := c.CreateSession(context.Background(), "ask")
	seen = nil

	// Turn 1 — the mark is pending, so this turn carries the prompt.
	err := c.sendMessage(context.Background(), sid, "ASK SEED PROMPT", "", "hello", nil, c.consumePromptPending(sid))
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	// Turn 2 — must NOT, even though the (reuse) prompt is non-empty.
	err = c.sendMessage(context.Background(), sid, "ASK REUSE PROMPT", "", "again", nil, c.consumePromptPending(sid))
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}

	if len(seen) < 2 {
		t.Fatalf("expected two sends, saw %d", len(seen))
	}
	if !strings.Contains(textOf(seen[0]), "ASK SEED PROMPT") {
		t.Errorf("turn 1 must carry the seed prompt, got %q", textOf(seen[0]))
	}
	if strings.Contains(textOf(seen[1]), "ASK REUSE PROMPT") {
		t.Errorf("turn 2 must NOT carry the prompt again — the mark is consumed once, got %q", textOf(seen[1]))
	}
}

func TestAFollowUpOnAnUncreatedSessionNeverCarriesThePrompt(t *testing.T) {
	// THE SAFETY CASE. A follow-up ATTACHES to a session this client did not create (a recorded URL
	// from an earlier execution), so it must never re-state the prompt — the conversation already has
	// it, and re-sending would append a second copy.
	var seen []capturedReq
	srv := captureServer(t, &seen)

	c := NewSessionClient(srv.URL, "", "")
	c.setAPIShape(shapeV2)
	// NO CreateSession: this session belongs to someone else.
	if c.consumePromptPending("ses_someone_elses") {
		t.Fatal("a session this client did not create must never be treated as awaiting the prompt")
	}
	if err := c.SendMessage(context.Background(), "ses_someone_elses", "PROMPT", "", "follow up"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if strings.Contains(textOf(promptRequest(t, seen)), "PROMPT") {
		t.Error("a follow-up must not re-state the prompt on a session that already has it")
	}
}

func TestTheAfterCompactReminderCarriesThePromptAgain(t *testing.T) {
	// A compact summarizes the conversation away, INCLUDING the first message the prompt travelled in.
	// Without re-stating it the resumed turns run with no standing instructions while appearing to
	// continue normally — which is why this send is explicit rather than mark-driven.
	var seen []capturedReq
	srv := captureServer(t, &seen)

	c := NewSessionClient(srv.URL, "", "")
	c.setAPIShape(shapeV2)
	sid, _ := c.CreateSession(context.Background(), "t")
	_ = c.consumePromptPending(sid) // a first turn has already happened
	seen = nil

	if err := c.SendMessageCarryingSystem(context.Background(), sid, "YOU ARE A WORKER", "", "post-compact"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if !strings.Contains(textOf(promptRequest(t, seen)), "YOU ARE A WORKER") {
		t.Error("the post-compact reminder must re-state the prompt, or the resumed turns lose it")
	}
}

// A guard on the wire shape: the delivery must not put the prompt somewhere v2 ignores, which is the
// failure mode that started this (an unknown key is dropped in silence).
func TestTheDeliveredPromptIsInTheTextKeyNotAFieldV2Ignores(t *testing.T) {
	var seen []capturedReq
	srv := captureServer(t, &seen)

	c := NewSessionClient(srv.URL, "", "")
	c.setAPIShape(shapeV2)
	sid, _ := c.CreateSession(context.Background(), "t")
	seen = nil
	_ = c.SendMessageCarryingSystem(context.Background(), sid, "PROMPT TEXT", "", "msg")

	got := promptRequest(t, seen)
	raw, _ := json.Marshal(got.body)
	if _, hasSystem := got.body["system"]; hasSystem {
		t.Error("v2 must not send a `system` key — it is not in the schema and is dropped in silence")
	}
	if !strings.Contains(string(raw), "PROMPT TEXT") {
		t.Errorf("the prompt must be present in the body, got %s", raw)
	}
}
