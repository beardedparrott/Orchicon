package claude

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// A `thinking_delta` carries its text in `delta.thinking`, NOT `delta.text` —
// the same asymmetry the native anthropic path handles. Reading `text` here
// yields EMPTY reasoning on every delta, silently, so the field is pinned.
func TestParseLineDecodesThinkingDelta(t *testing.T) {
	ev, err := ParseLine([]byte(`{"type":"stream_event","event":{"type":"content_block_delta",` +
		`"index":0,"delta":{"type":"thinking_delta","thinking":"Let me consider: 1.10 - 1.00 = 0.10"}}}`))
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if !ev.IsReasoning {
		t.Fatal("IsReasoning = false for a thinking_delta — the reasoning part would be dropped")
	}
	if ev.Text != "Let me consider: 1.10 - 1.00 = 0.10" {
		t.Fatalf("Text = %q, want the delta's `thinking` content (NOT its `text`, which is absent)", ev.Text)
	}
	if ev.Type != "thinking_delta" {
		t.Errorf("Type = %q, want thinking_delta", ev.Type)
	}
}

// A text delta must stay text: a thinking delta is a DIFFERENT kind, and folding
// them together would put the model's private reasoning into the worker's answer.
func TestParseLineKeepsTextAndThinkingApart(t *testing.T) {
	ev, _ := ParseLine([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"The ball is $0.05"}}}`))
	if ev.IsReasoning {
		t.Error("a text_delta was marked as reasoning")
	}
	if ev.Text != "The ball is $0.05" {
		t.Errorf("Text = %q", ev.Text)
	}
}

// A signature_delta carries the block's cryptographic signature and no
// displayable content — it must not become a reasoning event with junk text.
func TestParseLineIgnoresSignatureDelta(t *testing.T) {
	ev, _ := ParseLine([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"signature_delta","signature":"ErUBCkYIBRgCIk..."}}}`))
	if ev.IsReasoning || ev.Text != "" {
		t.Fatalf("a signature_delta produced reasoning: %+v", ev)
	}
}

// A whole `thinking` block on an assistant message (no partial-delta stream)
// must also be recognised — and `redacted_thinking`, which carries no text by
// design, must contribute nothing rather than appearing as an empty bubble.
func TestParseLineDecodesWholeThinkingBlocks(t *testing.T) {
	ev, _ := ParseLine([]byte(`{"type":"assistant","message":{"content":[` +
		`{"type":"thinking","thinking":"step one. "},` +
		`{"type":"redacted_thinking","data":"opaque"},` +
		`{"type":"thinking","thinking":"step two."},` +
		`{"type":"text","text":"answer"}]}}`))
	if !ev.IsReasoning {
		t.Fatal("IsReasoning = false for an assistant message with thinking blocks")
	}
	if ev.Reasoning != "step one. step two." {
		t.Fatalf("Reasoning = %q, want both thinking blocks concatenated (redacted contributes nothing)", ev.Reasoning)
	}
	// The prose stays prose — the two must not merge.
	if ev.Text != "answer" {
		t.Fatalf("Text = %q, want only the text block", ev.Text)
	}
}

// THE MAPPED CONSEQUENCE: reasoning becomes its OWN durable part
// (db.SessionPartReasoning), and — the part that matters — it never reaches
// OnText, which is the worker's prose and feeds its ORCHICON WORKER SUMMARY.
func TestReasoningBecomesItsOwnPartAndNeverTheWorkersOutput(t *testing.T) {
	rec := &recordingCallbacks{}
	ctx := t.Context()

	type part struct {
		kind    string
		payload string
	}
	var parts []part
	m := NewMapper("exec-reason", rec, MapperDeps{
		TenantID: "t1",
		Log:      quietLogger(),
		Record: func(_ context.Context, kind string, payload map[string]any) {
			b, _ := json.Marshal(payload)
			parts = append(parts, part{kind: kind, payload: string(b)})
		},
	})

	m.Handle(ctx, mustParse(t, `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"SECRET-REASONING-ONE"}}}`))
	m.Handle(ctx, mustParse(t, `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"the answer is 0.05"}}}`))
	m.Handle(ctx, mustParse(t, `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"SECRET-REASONING-TWO"}}}`))
	m.Handle(ctx, mustParse(t, `{"type":"result","subtype":"success","result":"the answer is 0.05"}`))

	// (a) The transcript carries a reasoning part with BOTH spans coalesced.
	var sawReasoning bool
	for _, p := range parts {
		if p.kind != "reasoning" {
			continue
		}
		sawReasoning = true
		if !strings.Contains(p.payload, "SECRET-REASONING-ONE") || !strings.Contains(p.payload, "SECRET-REASONING-TWO") {
			t.Errorf("both thinking spans must coalesce into ONE reasoning part per turn: %s", p.payload)
		}
	}
	if !sawReasoning {
		t.Fatalf("no reasoning part was recorded (parts: %+v) — the execution view would show no reasoning", parts)
	}

	// (b) NONE of the reasoning reached OnText.
	for _, e := range rec.snapshot() {
		if strings.Contains(e, "SECRET-REASONING") {
			t.Fatalf("reasoning leaked into OnText (%q) — it would appear inside the worker's answer and its summary", e)
		}
	}
	// (c) ...nor the accumulated output, which becomes the worker summary.
	if out := m.Output(); strings.Contains(out, "SECRET-REASONING") {
		t.Fatalf("reasoning leaked into the accumulated output: %q", out)
	}
	if !strings.Contains(m.Output(), "the answer is 0.05") {
		t.Errorf("the prose was lost: %q", m.Output())
	}
}

func mustParse(t *testing.T, line string) StreamEvent {
	t.Helper()
	ev, err := ParseLine([]byte(line))
	if err != nil {
		t.Fatalf("ParseLine(%s): %v", line, err)
	}
	return ev
}

// THE ASK PATH, same contract. opencode emits reasoning as
// `Type:"reasoning"` + `IsReasoning:true` (chatsession.go), and the askorchicon
// drain opens a THINK segment on exactly that shape — so claude must match it or
// an Ask conversation shows the model's thinking as ordinary prose, or not at all.
func TestAskMapsReasoningOntoTheBus(t *testing.T) {
	h, slot := askHarness(t)
	ctx := t.Context()
	sid, err := h.b.CreateConversationSession(ctx, "conv-think", "t")
	if err != nil {
		t.Fatalf("CreateConversationSession: %v", err)
	}
	bus, err := h.b.Subscribe(ctx, "conv-think")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer bus.Close()
	if err := h.b.SendTurnMessage(ctx, "conv-think", sid, "", "claude/anthropic/claude-sonnet-5", "hi"); err != nil {
		t.Fatalf("SendTurnMessage: %v", err)
	}
	fp := *slot

	fp.push(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"THOUGHT-ONE"}}}`)
	ev := nextEvent(t, bus)
	if ev.Kind != "delta" || ev.Type != "reasoning" || !ev.IsReasoning {
		t.Fatalf("thinking delta mapped to %+v, want Kind=delta Type=reasoning IsReasoning=true (the shape opencode emits and the drain's think-segment opens on)", ev)
	}
	if ev.Text != "THOUGHT-ONE" {
		t.Errorf("reasoning text = %q, want the delta's `thinking` content", ev.Text)
	}

	// And the prose stays prose.
	fp.push(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"the answer"}}}`)
	ev2 := nextEvent(t, bus)
	if ev2.Type != "text" || ev2.IsReasoning {
		t.Fatalf("text delta mapped to %+v, want Kind=delta Type=text and NOT reasoning", ev2)
	}
}
