package tui

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/screens/ask"
)

// TestSpaceNeverSelectsThroughTheTabDropdown is the regression pin for the operator's report:
// "In conversations in the TUI, spacebar is selecting a conversation when it should be marking it for
// bulk operations instead."
//
// THE MECHANISM, measured rather than assumed. On the Ask tab the dropdown's rows are conversation
// VERBS — "New" clears the open conversation, "Conversations" re-scopes the rail — so a space that
// activated the highlighted row literally selected something. Two shell sites did that:
//
//	shell.go menuHandleKey   — "enter", " ", "space" all ran MenuSelect. With the menu open, space
//	                           activated the row under the cursor.
//	shell.go menuActivationKey — from a composer with an UNTRIMMED empty buffer, space OPENED the menu
//	                           (so the operator's next space landed in it), and it ignored the rail
//	                           entirely, which owns space whenever it is on screen.
//
// What must now be true, all four:
//  1. With the menu open, space does NOT activate its row — the open conversation survives. Enter
//     still activates it (so nothing became unreachable).
//  2. Space CLOSES the menu rather than falling through to the composer behind it.
//  3. On the rail, space MARKS — it neither opens nor activates a menu.
//  4. The launch-page affordance survives: with NO rail, space still opens the dropdown.
func TestSpaceNeverSelectsThroughTheTabDropdown(t *testing.T) {
	// 3. On the rail, space marks and never opens a menu.
	t.Run("space marks on the rail, never opens the menu", func(t *testing.T) {
		m := railAppWithConversations(t)
		m.setFocus(focusComposer)
		m.dock.SetValue("")

		nm, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace})
		got := nm.(*App)

		if got.MenuOpenID() != "" {
			t.Fatalf("space opened the %q dropdown over the conversations rail — the menu is drawn over "+
				"the very list being marked", got.MenuOpenID())
		}
		if ids := got.convMarkedIDs(); len(ids) != 1 || ids[0] != "conv-01" {
			t.Fatalf("space on the rail must MARK the highlighted conversation, got %v", ids)
		}
	})

	// 1 + 2. With the menu already open (the operator opened it with Enter, or a chord dropped it
	// down), space must not select the highlighted row and must not leak into the composer.
	t.Run("open menu: space does not activate its row", func(t *testing.T) {
		m := railAppWithConversations(t)
		m.openTabMenu(TabAsk)
		if m.TabMenu() == nil || m.TabMenu().Sel != 0 {
			t.Fatal("fixture: the Ask menu should be open on its first row")
		}
		openBefore := m.chatConvID

		nm, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace})
		got := nm.(*App)

		if got.chatConvID != openBefore {
			t.Errorf("space ACTIVATED the menu row and changed the open conversation (%q → %q) — this is "+
				"the operator's 'spacebar is selecting a conversation'", openBefore, got.chatConvID)
		}
		if got.MenuOpenID() != "" {
			t.Errorf("space must close the dropdown, not leave it up (menuOpen=%q)", got.MenuOpenID())
		}
		if v := got.dock.Value(); v != "" {
			t.Errorf("space leaked into the composer behind the menu: %q", v)
		}
	})

	// 1 (the other half). ENTER still activates the row — the select gesture is moved, not removed.
	t.Run("enter still activates the menu row", func(t *testing.T) {
		m := railAppWithConversations(t)
		m.openTabMenu(TabAsk)
		openBefore := m.chatConvID
		nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		got := nm.(*App)
		if got.chatConvID == openBefore {
			t.Errorf("enter no longer activates the dropdown's row (convID stayed %q) — the select gesture "+
				"must remain reachable", openBefore)
		}
	})

	// 4. The launch page keeps the affordance: no rail → space still opens the dropdown.
	t.Run("launch page: space still opens the menu", func(t *testing.T) {
		m := newTestApp()
		m.RegisterScreen(TabAsk, ask.New(nil, m.reg))
		m.dispatch(tea.WindowSizeMsg{Width: 140, Height: 40})
		m.SwitchTo(TabAsk)
		m.askMode = askNew // the launch page: no rail, nothing to mark
		m.setFocus(focusComposer)
		m.dock.SetValue("")
		if m.railVisible() {
			t.Fatal("fixture: the launch page must not show the rail")
		}
		nm, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace})
		if got := nm.(*App); got.MenuOpenID() == "" {
			t.Error("space on the launch page must still open the tab dropdown — that affordance is not " +
				"what the operator reported, and removing it would make the menu unreachable there")
		}
	})
}

// railAppWithConversations is the real-Ask-screen rail fixture (real screen, not a stub: its hidden
// "conversations" source is a space participant the stub would not model).
func railAppWithConversations(t *testing.T) *App {
	t.Helper()
	m := newTestApp()
	m.RegisterScreen(TabAsk, ask.New(nil, m.reg))
	m.dispatch(tea.WindowSizeMsg{Width: 140, Height: 40})
	m.SwitchTo(TabAsk)
	m.askMode = askConversations
	m.convRailOpen = true
	m.rightRailOpen = true
	m.convLoaded = true
	m.convErr = ""
	m.chatConvID = "conv-01"
	for i := 1; i <= 5; i++ {
		m.conversations = append(m.conversations, chat.Conversation{
			ID:    fmt.Sprintf("conv-%02d", i),
			Title: fmt.Sprintf("conversation %02d", i), MessageN: 2,
		})
	}
	m.convSel, m.convScroll = 0, 0
	m.dock.SetValue("")
	m.refreshLayout()
	return m
}
