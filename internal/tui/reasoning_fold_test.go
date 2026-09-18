package tui

// reasoning_fold_test.go — THE ARROW ON THE REASONING BLOCK.
//
// The operator: "Yes I would like to get the collapsible reasoning block but I don't want it to look fully
// like the execution page. It should still have the same 'bubbles' it has now with the user and orchicon
// chat and reasoning should look similar but have a arrow on the left to expand and collapse."
//
// So: an ARROW on the left, the bubbles unchanged, and the fold state living on the shell (which owns the
// conversation) rather than in the renderer (which is a pure function of its input).

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/screens/ask"
)

func foldApp(t *testing.T) *App {
	t.Helper()
	m, _ := newAskApp(t)
	m.RegisterScreen(TabAsk, ask.New(m.clients, m.reg))
	m.SwitchTo(TabAsk)
	m.askMode = askConversations
	m.convRailOpen = true
	m.rightRailOpen = true
	m.OpenAskConversation("c1")
	m.active = TabAsk
	m.askPane = askPaneConversation
	m.chatFocus = focusContent
	return m
}

// THE ARROW IS ON THE LEFT, and it points the way the tree glyphs in this TUI already do.
func TestReasoningBlockHasAArrowOnTheLeft(t *testing.T) {
	out := chat.RenderItems([]chat.ChatItem{{Kind: chat.KindReasoning, Text: "a thought", Key: "r1"}}, 80)
	first := strings.SplitN(out, "\n", 2)[0]
	if !strings.HasPrefix(strings.TrimSpace(first), "▾") {
		t.Errorf("the reasoning header does not open with a down arrow: %q", first)
	}

	folded := chat.RenderItems(
		[]chat.ChatItem{{Kind: chat.KindReasoning, Text: "a thought", Key: "r1"}}, 80,
		func(k string) bool { return k == "r1" },
	)
	ffirst := strings.SplitN(folded, "\n", 2)[0]
	if !strings.HasPrefix(strings.TrimSpace(ffirst), "▸") {
		t.Errorf("a FOLDED block does not show a right arrow: %q", ffirst)
	}
}

// FOLDING HIDES THE BODY AND KEEPS THE HEADER. One row when folded — that is the whole value of the gesture
// on a transcript whose problem was a wall of reasoning.
func TestFoldingHidesTheBodyAndKeepsTheHeader(t *testing.T) {
	body := strings.Repeat("weighing the options. ", 40)
	open := chat.RenderItems([]chat.ChatItem{{Kind: chat.KindReasoning, Text: body, Key: "r1"}}, 80)
	folded := chat.RenderItems(
		[]chat.ChatItem{{Kind: chat.KindReasoning, Text: body, Key: "r1"}}, 80,
		func(k string) bool { return k == "r1" },
	)

	if n := len(strings.Split(strings.TrimRight(folded, "\n"), "\n")); n != 1 {
		t.Errorf("a folded block drew %d rows, want 1:\n%s", n, folded)
	}
	if !strings.Contains(folded, "reasoning") {
		t.Errorf("the folded header lost its label:\n%s", folded)
	}
	// The SIZE stays on the header, so folding does not hide how much was folded away.
	if !strings.Contains(folded, "chars") {
		t.Errorf("the folded header lost its char count:\n%s", folded)
	}
	if strings.Contains(folded, "weighing") {
		t.Errorf("a folded block still printed its body:\n%s", folded)
	}
	// And opening it again restores the body.
	if !strings.Contains(open, "weighing") {
		t.Errorf("the unfolded block never had a body:\n%s", open)
	}
}

// THE BUBBLES ARE UNCHANGED. The operator asked for the reasoning block to look SIMILAR to the existing
// chat bubbles rather than adopting the execution pane's card layout, so the user and model bands must
// still render exactly as they did.
func TestFoldingDoesNotChangeTheChatBubbles(t *testing.T) {
	items := []chat.ChatItem{
		{Kind: chat.KindUser, Text: "my question", Key: "u1"},
		{Kind: chat.KindText, Text: "the answer", Key: "t1"},
		{Kind: chat.KindReasoning, Text: "a thought", Key: "r1"},
	}
	plain := chat.RenderItems(items, 80)
	folded := chat.RenderItems(items, 80, func(k string) bool { return k == "r1" })

	for _, want := range []string{"my question", "the answer", "You"} {
		if !strings.Contains(plain, want) || !strings.Contains(folded, want) {
			t.Errorf("%q disappeared from the transcript when a reasoning block was folded", want)
		}
	}
}

// CTRL+O TOGGLES THE NEWEST REASONING BLOCK, and the toggle is what the pane then draws.
func TestToggleFoldsTheNewestReasoningBlock(t *testing.T) {
	m := foldApp(t)
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindUser, Text: "hello", Key: "u1", At: 1})
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindReasoning, Text: "first thought", Key: "r1", At: 2})
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindReasoning, Text: "newest thought", Key: "r2", At: 3})
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindText, Text: "the reply", Key: "t1", At: 4})

	if !m.toggleLastReasoningBlock() {
		t.Fatal("the toggle reported nothing changed, with a reasoning block present")
	}
	if !m.reasoningFolded["r2"] {
		t.Errorf("the toggle folded %v, want the NEWEST block r2", m.reasoningFolded)
	}
	if m.reasoningFolded["r1"] {
		t.Error("the toggle folded an older block as well")
	}
	// Pressing it again opens that block back up.
	if !m.toggleLastReasoningBlock() {
		t.Fatal("the second toggle reported nothing changed")
	}
	if m.reasoningFolded["r2"] {
		t.Error("the block did not re-open")
	}
}

// WITH NOTHING TO FOLD IT REPORTS SO, so the key does not repaint for nothing — and, more importantly, it
// never folds a block that is not there.
func TestToggleWithNoReasoningIsANoOp(t *testing.T) {
	m := foldApp(t)
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindUser, Text: "hello", Key: "u1", At: 1})
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindText, Text: "reply", Key: "t1", At: 2})
	if m.toggleLastReasoningBlock() {
		t.Error("the toggle claimed to change something with no reasoning block present")
	}
}

// THE KEY IS REACHABLE — driven through the real dispatch, because a chord bound but swallowed by the
// pane's own scroll keys is the failure this class of feature ships with.
func TestCtrlOFoldsThroughTheShell(t *testing.T) {
	m := foldApp(t)
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindUser, Text: "hello", Key: "u1", At: 1})
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindReasoning, Text: "a thought", Key: "r1", At: 2})

	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyCtrlO})
	m = nm

	if !m.reasoningFolded["r1"] {
		t.Fatalf("ctrl+o did not fold the block through dispatch — the chord is bound but unreachable. "+
			"folded=%v", m.reasoningFolded)
	}
	// And the pane draws it folded.
	if str := m.TranscriptStream("c1"); str != nil && strings.Contains(str.View(), "▸") {
		// ok — the folded header is on screen
	} else {
		t.Error("the pane does not show the folded arrow after ctrl+o")
	}
}
