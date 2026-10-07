package tui

// conv_loaded_latch_test.go — ONE FAILED RELOAD MUST NOT DISABLE THE ROLLING RAIL REFRESH (AC 16).
//
// `reloadConversations` used to set `convLoaded = false`, and only a SUCCESSFUL `onConversations` sets it back
// true — the error branch returns before reaching that line. So ONE failed reload (a blip, a 401) left the flag
// false for the rest of the session, and `refreshActiveView`'s extra list reload (gated on `convLoaded` in
// refresh.go) NEVER FIRED AGAIN — silently disabling the rolling refresh of the rail, which is exactly the
// freshness the "running" marker depends on.
//
// The candidate that might have covered it — `ask.Model.RefreshView` (screens/ask/screen.go) — reloads the
// SCREEN's screenkit list (the pane's own source), not the shell's `m.conversations`, which is what the rail
// and every turn-state read use. So it does NOT keep the rail current, and the latch is a real defect. The fix
// is the one line: `reloadConversations` no longer clears the flag ("a load is in flight" is `convLoading`'s
// job; a successful-load marker must survive a re-read).

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// A FAILED RELOAD LEAVES `convLoaded` TRUE, so the rolling refresh keeps firing.
func TestAFailedReloadDoesNotDisableTheRollingRailRefresh(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")
	// The list has loaded once (a normal session).
	m.onConversations(chat.ConversationsMsg{Convs: []chat.Conversation{{ID: "c1", Title: "a chat"}}})
	if !m.convLoaded {
		t.Fatal("fixture: a successful load did not set convLoaded")
	}

	// The operator hits the rail's retry (or any path that reloads), and it FAILS.
	m.reloadConversations()
	if !m.convLoaded {
		t.Errorf("a reload DISABLED the rolling rail refresh: reloadConversations cleared convLoaded, so a " +
			"single failed reload (a blip, a 401) stops the 5s list reload for the rest of the session (AC 16)")
	}

	// And a subsequent error landing does not clear it either — the flag means "a successful load has landed",
	// which is still true.
	m.onConversations(chat.ConversationsMsg{Err: "unauthorized"})
	if !m.convLoaded {
		t.Errorf("an errored load cleared convLoaded, disabling the rolling refresh (AC 16)")
	}
	if m.convErr == "" {
		t.Errorf("fixture: the errored load did not set convErr, so the rail's retry affordance is gone")
	}
}
