package claude

// queues_prompts_test.go — the EVIDENCE behind `Bridge.QueuesPrompts() == true`, pinned.
//
// The declaration says a mid-turn write is queued by the CLI and answered at the next boundary. That is only
// true while two things hold in session.go, and both are load-bearing for interjections:
//
//  1. SendTurn increments `queued` when it writes, so the run loop KNOWS a turn is outstanding;
//  2. onTerminal DECREMENTS it and returns early while it is still positive — that early return is what stops
//     the session finishing at the first `result` and abandoning the queued turn.
//
// If either is removed, the declaration becomes a lie: an interjection would be written to the child and then
// the session would close at the current turn's boundary, leaving the operator's message unanswered with no
// error anywhere — the same class of silent loss the interjection work exists to remove.
//
// A SOURCE ASSERTION, deliberately: driving the real behaviour needs the claude CLI binary, which CI does not
// have, so a behavioural test would be skipped everywhere and guard nothing.

import (
	"os"
	"strings"
	"testing"
)

func TestTheQueueEvidenceBehindQueuesPromptsIsIntact(t *testing.T) {
	b, err := os.ReadFile("session.go")
	if err != nil {
		t.Fatalf("read session.go: %v", err)
	}
	src := string(b)

	// 1. SendTurn accounts for the turn it writes.
	sendTurn := funcBody(src, "func (s *session) SendTurn(")
	if !strings.Contains(sendTurn, "s.queued++") {
		t.Error("session.SendTurn no longer increments `queued`. Bridge.QueuesPrompts() claims a mid-turn " +
			"write is queued to the next boundary; without the counter the run loop cannot know a turn is " +
			"outstanding, so the session would finish at the current `result` and never answer the " +
			"interjection")
	}

	// 2. onTerminal keeps the session alive while one is outstanding.
	onTerminal := funcBody(src, "func (s *session) onTerminal(")
	if !strings.Contains(onTerminal, "s.queued--") {
		t.Error("session.onTerminal no longer decrements `queued`")
	}
	if !strings.Contains(onTerminal, "if queued > 0 {") || !strings.Contains(onTerminal, "return") {
		t.Error("session.onTerminal no longer returns early while a queued turn is outstanding, so the " +
			"session finishes at the first `result` and the interjection is lost silently")
	}
}

// funcBody returns a top-level func's source text, from its signature to the next top-level declaration.
func funcBody(src, signature string) string {
	i := strings.Index(src, signature)
	if i < 0 {
		return ""
	}
	rest := src[i+len(signature):]
	if j := strings.Index(rest, "\nfunc "); j > 0 {
		rest = rest[:j]
	}
	return rest
}
