package orchicon

// Tests for mid-run nudge delivery on the native orchicon adapter
// (epic: nudges sent via SendExecutionMessage during a text-only turn).
// The injection queue must drain at EVERY turn boundary — StopStop (turn
// that ends WITHOUT tool calls), not just between tool rounds — and on
// every terminal exit path a dropped queued nudge must be visible.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// AC: a nudge queued during a TEXT-ONLY turn (StopStop, no tool calls) is
// delivered at the turn boundary: it is appended as a user message (source
// "human"), the loop CONTINUES (a further provider turn happens — the
// session answers the nudge), and the session settles on the next clean
// stop.
func TestNudgeDuringTextOnlyTurnAnswered(t *testing.T) {
	prov := &mockProvider{turns: []scriptedTurn{
		// Turn 1: a text-only final report (the previous broken path —
		// StopStop with no tool calls settled straight to the success gate).
		{events: []Event{TextDelta{Text: "final report"}}, finish: StopStop, usage: Usage{InputTokens: 200, OutputTokens: 20}},
		// Turn 2: the session answers the nudge.
		{events: []Event{TextDelta{Text: "answering your nudge"}}, finish: StopStop, usage: Usage{InputTokens: 300, OutputTokens: 25}},
	}}
	s := qaSession(t, prov, nil)
	s.queueInjected("please focus on the nudge")
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The loop must have continued past turn 1 to answer the nudge.
	if prov.requestCount() != 2 {
		t.Errorf("provider turns = %d, want 2 (loop must continue and answer the nudge)", prov.requestCount())
	}
	// The nudge must be in the final turn's history as a human user message.
	found := false
	for _, m := range prov.lastRequest().Messages {
		if m.Role == RoleUser && len(m.Content) > 0 && m.Content[0].Text != nil && strings.Contains(*m.Content[0].Text, "please focus on the nudge") {
			found = true
		}
	}
	if !found {
		t.Errorf("nudge missing from answer-turn history: %+v", prov.lastRequest().Messages)
	}
	// Session settles normally (success) on the next clean stop.
	_, _, _, _, _, _, results := cb.snapshot()
	if len(results) != 1 || !results[0].succeeded {
		t.Errorf("OnResult = %+v, want success after answering the nudge", results)
	}
}

// AC: a nudge queued during a text-only turn is recorded in the TRANSCRIPT
// with source "human" (visible in the session pane).
func TestNudgeHumanSourceInTranscript(t *testing.T) {
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "report"}}, finish: StopStop, usage: Usage{InputTokens: 200, OutputTokens: 20}},
		{events: []Event{TextDelta{Text: "answer"}}, finish: StopStop, usage: Usage{InputTokens: 300, OutputTokens: 25}},
	}}
	s := qaSession(t, prov, nil)
	s.queueInjected("please confirm")
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	evs, err := Load(s.TranscriptPath())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	found := false
	for _, e := range evs {
		if e.Type == TransUserMessage {
			var d map[string]any
			_ = json.Unmarshal(e.Data, &d)
			if d["source"] == "human" && strings.Contains(fmt.Sprint(d["text"]), "please confirm") {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("nudge not recorded as a human user_message in the transcript")
	}
}

// AC: a BURST of queued nudges is ALL delivered, in ORDER, at one turn
// boundary (not one per round).
func TestNudgeBurstDeliveredInOrder(t *testing.T) {
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "report"}}, finish: StopStop, usage: Usage{InputTokens: 200, OutputTokens: 20}},
		{events: []Event{TextDelta{Text: "answer"}}, finish: StopStop, usage: Usage{InputTokens: 300, OutputTokens: 25}},
	}}
	s := qaSession(t, prov, nil)
	s.queueInjected("nudge one")
	s.queueInjected("nudge two")
	s.queueInjected("nudge three")
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if prov.requestCount() != 2 {
		t.Fatalf("provider turns = %d, want 2", prov.requestCount())
	}
	var got []string
	for _, m := range prov.lastRequest().Messages {
		if m.Role == RoleUser && len(m.Content) > 0 && m.Content[0].Text != nil {
			for _, want := range []string{"nudge one", "nudge two", "nudge three"} {
				if *m.Content[0].Text == want {
					got = append(got, want)
				}
			}
		}
	}
	if len(got) != 3 {
		t.Fatalf("delivered %d nudges (%v), want all 3 at one boundary", len(got), got)
	}
	want := []string{"nudge one", "nudge two", "nudge three"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("burst order = %v, want %v (must preserve queue order)", got, want)
		}
	}
}

// AC: a terminal exit with a queued nudge records a VISIBLE error part
// naming the undelivered message (no silent drop). A provider stream error
// with a queued nudge → TransError names the drop.
func TestNudgeDroppedOnStreamErrorVisible(t *testing.T) {
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "partial"}}, finish: StopStop, usage: Usage{InputTokens: 1, OutputTokens: 1}, streamErr: fmt.Errorf("mid-stream failure")},
	}}
	s := qaSession(t, prov, nil)
	s.queueInjected("please don't drop me")
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Failure verdict from the stream error.
	_, _, _, _, _, _, results := cb.snapshot()
	if len(results) != 1 || results[0].succeeded || !strings.Contains(results[0].errMsg, "mid-stream failure") {
		t.Errorf("OnResult = %+v, want failure with the stream error", results)
	}
	// And a visible error part names the undelivered nudge.
	evs, err := Load(s.TranscriptPath())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	found := false
	for _, e := range evs {
		if e.Type == TransError {
			var d map[string]any
			_ = json.Unmarshal(e.Data, &d)
			msg := fmt.Sprint(d["error"])
			if strings.Contains(msg, "queued nudge") && strings.Contains(msg, "please don't drop me") {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("no error part names the undelivered queued nudge")
	}
}
