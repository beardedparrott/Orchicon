package tui

// rail_sync_test.go — THE RAIL'S HIGHLIGHT MUST AGREE WITH THE CONVERSATION ON SCREEN.
//
// REPORTED: "the highlighted bar on the conversation is not always in sync with the current
// conversation on the screen and it's very confusing."
//
// The cause was structural, not a stray assignment: convSel is an INDEX into railRows() and
// the highlight renders purely as `i == convSel`. Nothing anywhere derived it from chatConvID
// — the conversation the transcript actually shows. So every route that opened a
// conversation WITHOUT going through the rail left the two disagreeing.
//
// These tests pin the sync at both entry points, and pin the two cases it must NOT "fix"
// (an explicit cursor move, and an open conversation the rail cannot show).

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// railIndexOf returns the rail row index of the conversation with the given id (-1 if absent).
func railIndexOf(m *App, id string) int {
	for i, r := range m.railRows() {
		if r.folder || r.conv < 0 || r.conv >= len(m.conversations) {
			continue
		}
		if m.conversations[r.conv].ID == id {
			return i
		}
	}
	return -1
}

// TestHighlightFollowsAConversationOpenedNotFromTheRail is the direct regression: opening a
// conversation by any route moves the highlight to it.
func TestHighlightFollowsAConversationOpenedNotFromTheRail(t *testing.T) {
	m := railApp(t, 4)
	// The cursor sits on the first row while the shell shows conv-01.
	if m.convSel != 0 {
		t.Fatalf("fixture: convSel = %d, want 0", m.convSel)
	}

	// Open conv-03 the way a non-rail route does (a search hit, a mention, a /switch): it sets
	// the shown conversation and never touches the rail.
	m.OpenAskConversation("conv-03")

	want := railIndexOf(m, "conv-03")
	if want < 0 {
		t.Fatal("fixture: conv-03 is not in the rail")
	}
	if m.convSel != want {
		t.Fatalf("after opening conv-03 the highlight is on row %d, but conv-03 is row %d — this "+
			"is the reported bug: the bar marks one conversation while the screen shows another "+
			"(the highlight was the rail's cursor, never the open conversation)", m.convSel, want)
	}
}

// TestHighlightFollowsANewlyCreatedConversation: a send that lazily creates a conversation
// opens it via chatConvCreatedMsg, which sets chatConvID directly. That is the most visible
// form of the bug — the FIRST message in a brand-new conversation left the bar behind.
func TestHighlightFollowsANewlyCreatedConversation(t *testing.T) {
	m := railApp(t, 3)
	if m.convSel != 0 {
		t.Fatalf("fixture: convSel = %d, want 0", m.convSel)
	}

	// The conversation the create produced, already in the list (the rail reloaded).
	m.conversations = append(m.conversations, chat.Conversation{ID: "conv-new", Title: "brand new"})
	m.convLoaded = true

	nm, _ := m.Update(chatConvCreatedMsg{convID: "conv-new", text: "hello"})
	m = nm.(*App)

	want := railIndexOf(m, "conv-new")
	if want < 0 {
		t.Fatal("fixture: conv-new is not in the rail")
	}
	if m.convSel != want {
		t.Fatalf("after a send created and opened conv-new, the highlight is on row %d, but the "+
			"new conversation is row %d — the first message in a new conversation left the bar on "+
			"the previous one", m.convSel, want)
	}
}

// TestSyncDoesNotUndoAnExplicitCursorMove: when the rail already agrees, the sync is inert —
// so an operator moving the highlight by hand is never overruled.
func TestSyncDoesNotUndoAnExplicitCursorMove(t *testing.T) {
	m := railApp(t, 4)
	// Put the cursor on conv-01 (already the shown one), then move it deliberately.
	m.railSyncSelectionToOpenConversation()
	before := m.convSel
	m.railSyncSelectionToOpenConversation()
	if m.convSel != before {
		t.Fatalf("a repeat sync moved the cursor from %d to %d; it must be inert when it agrees",
			before, m.convSel)
	}
}

// TestSyncLeavesTheCursorWhenTheConversationIsHidden: an open conversation the rail cannot
// show (filtered out, or inside a collapsed folder) must leave the cursor alone. Relocating
// it somewhere arbitrary would be a second confusion rather than a fix.
func TestSyncLeavesTheCursorWhenTheConversationIsHidden(t *testing.T) {
	m := railApp(t, 3)
	m.convSel = 1
	// Shown conversation is NOT in the rail at all.
	m.chatConvID = "conv-not-in-rail"
	m.railSyncSelectionToOpenConversation()
	if m.convSel != 1 {
		t.Fatalf("the cursor moved from 1 to %d for a conversation the rail cannot show; it must "+
			"stay put rather than be relocated arbitrarily", m.convSel)
	}
}
