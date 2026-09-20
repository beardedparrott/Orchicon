package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// The Ask tab's dropdown is the operator's spec: two rows, "New" and
// "Conversations". New loads the launch page (what orch shows on start);
// Conversations shows the transcript view with the conversation list on the
// right, so previous sessions can be continued.
func TestAskMenuIsNewAndConversations(t *testing.T) {
	m := phase3App(120, 40)
	entries := m.navEntries(TabAsk)
	if len(entries) != 2 {
		t.Fatalf("Ask menu has %d rows, want 2 (New, Conversations): %+v", len(entries), entries)
	}
	if entries[0].Label != "New" || entries[1].Label != "Conversations" {
		t.Fatalf("Ask menu rows = %q, %q; want New, Conversations", entries[0].Label, entries[1].Label)
	}
	// Both are verb rows (Ask's list is the rail, not a source pane).
	for _, e := range entries {
		if e.Action == nil {
			t.Errorf("Ask row %q must carry an action", e.Label)
		}
	}

	// The launch page is the START state: welcome layout, no rail.
	if !m.welcomeMode() {
		t.Fatal("Ask must start on the launch page (New)")
	}
	if m.railVisible() {
		t.Fatal("the launch page must not show the conversations rail")
	}

	// Conversations: leaves the launch page and shows the rail, with no
	// session open (that is the point — browsing previous sessions).
	entries[1].Action(m)
	if m.welcomeMode() {
		t.Fatal("Conversations must leave the launch page")
	}
	if !m.railVisible() {
		t.Fatal("Conversations must show the conversations rail")
	}
	if m.chatConvID != "" {
		t.Fatalf("browsing must not open a conversation by itself (chatConvID=%q)", m.chatConvID)
	}

	// New: back to the launch page, no rail, no session.
	entries[0].Action(m)
	if !m.welcomeMode() {
		t.Fatal("New must return to the launch page")
	}
	if m.railVisible() {
		t.Fatal("New must hide the conversations rail")
	}
	if m.chatConvID != "" {
		t.Fatalf("New must clear the active conversation (chatConvID=%q)", m.chatConvID)
	}
}

// A session that is already open keeps the rail visible regardless of mode:
// continuing a conversation always shows its list.
func TestOpenSessionKeepsRailVisible(t *testing.T) {
	m := phase3App(120, 40)
	m.askMode = askNew // pretend the operator is on New
	m.chatConvID = "conv-live"
	if m.welcomeMode() {
		t.Fatal("an open session must not render the launch page")
	}
	if !m.railVisible() {
		t.Fatal("an open session must keep the conversations rail visible")
	}
}

// The dedupe: the Ask menu's rows must not leak duplicate slash commands.
func TestAskMenuSlashCommandsResolve(t *testing.T) {
	m := phase3App(120, 40)
	for _, cmd := range []string{"/new", "/conversations"} {
		if c := m.slash.resolve(cmd); c == nil {
			t.Errorf("slash command %q must resolve", cmd)
		}
	}
	_ = tea.KeyEsc
}
