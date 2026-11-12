package claude

// queues_prompts_test.go — the EVIDENCE behind `Bridge.QueuesPrompts() == true`, pinned.

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
