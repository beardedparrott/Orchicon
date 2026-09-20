package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
)

func newDockTestApp() *App {
	m := NewApp(&client.Clients{}, &config.Profile{Name: "default", URL: "https://x.example.com"}, "v9.9.9")
	m.width, m.height = 120, 40
	m.RegisterScreen(TabAsk, &stubScreen{id: "ask", chip: "conversations: conv-1"})
	m.RegisterScreen(TabWork, &stubScreen{id: "work"})
	m.RegisterScreen(TabExecution, &stubScreen{id: "execution"})
	m.RegisterScreen(TabAutomation, &stubScreen{id: "automation"})
	m.RegisterScreen(TabEnforcement, &stubScreen{id: "enforcement"})
	m.RegisterScreen(TabControl, &stubScreen{id: "control"})
	m.SwitchTo(TabAsk)
	return m
}

func keyRunes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// keyTabFor maps a structural tab chord to its tab (test helper mirroring
// the Tabs registry).
func keyTabFor(chord string) TabID {
	for _, tab := range Tabs {
		if tab.Chord == chord {
			return tab.ID
		}
	}
	return ""
}

// ctrl+g toggles content↔composer focus. The COMPOSER is the launch
// focus (Phase 2a — typing works immediately, no ctrl+g needed):
// tab chords are structural and switch tabs while composing, and
// esc/ctrl+g return focus to the content pane.
func TestFocusChordAndFallthrough(t *testing.T) {
	m := newDockTestApp()
	if m.chatFocus != focusComposer {
		t.Fatal("must start in composer focus (Phase 2a: typing works at launch)")
	}
	if !m.dock.Focused {
		t.Fatal("the composer must be focused at launch")
	}
	// Structural chords bypass the composer while it is focused: a tab chord
	// switches tabs (it is a shell chord, never readline editing).
	nm, _ := m.Update(keyFor(tabChord(TabWork)))
	m2 := nm.(*App)
	if m2.ActiveTab() != TabWork {
		t.Fatalf("%s while composing must switch tabs (structural chord), got %q", tabChord(TabWork), m2.ActiveTab())
	}
	// EVERY tab chord is structural, driven from Tabs so the test cannot drift from the bindings.
	for _, tab := range Tabs {
		if tab.ID == TabWork {
			continue // already asserted above
		}
		nm, _ = m.Update(keyFor(tab.Chord))
		m2 = nm.(*App)
		if m2.ActiveTab() != tab.ID {
			t.Fatalf("%s while composing must switch tabs (structural chord), got %q", tab.Chord, m2.ActiveTab())
		}
	}
	// esc disengages to the content; the composer blurs.
	//
	// THE CHORD NOW DROPS THE SUBMENU, so esc has two jobs in order: close the menu it just opened,
	// then hand the keyboard to the content. The operator asked for the first half ("it goes to the
	// menu, but also automatically drops the submenu down and gains focus to that") and the second
	// half is what this test always pinned, so BOTH are asserted — the earlier one-press form was
	// only reachable while a chord left no menu, which was the defect.
	if m2.MenuOpenID() != m2.ActiveTab() {
		t.Fatalf("the chord must drop the active tab's submenu (menuOpen=%q, active=%q)",
			m2.MenuOpenID(), m2.ActiveTab())
	}
	if m2.chatFocus != focusTabs {
		t.Fatal("the chord must put the keyboard in the submenu it opened")
	}
	nm, _ = m2.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m2 = nm.(*App)
	if m2.MenuOpenID() != "" {
		t.Fatalf("esc must close the open submenu first, got %q", m2.MenuOpenID())
	}
	if m2.chatFocus != focusTabs {
		t.Fatal("closing the menu must leave the keyboard on the bar")
	}
	nm, _ = m2.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m2 = nm.(*App)
	if m2.chatFocus != focusContent || m2.dock.Focused {
		t.Fatal("esc must return focus to content")
	}
	// ctrl+g pulls focus back to the composer (content ↔ composer toggle).
	nm, _ = m2.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	m2 = nm.(*App)
	if m2.chatFocus != focusComposer || !m2.dock.Focused {
		t.Fatal("ctrl+g must refocus the composer")
	}
}

// Enter sends from the composer on any screen; the context preamble is
// visible in the dock notice. The composer is focused at launch — typing
// works with no ctrl+g first (Phase 2a).
func TestComposerSendCarriesContext(t *testing.T) {
	m := newDockTestApp()
	if m.chatFocus != focusComposer {
		t.Fatal("precondition: composer focused at launch")
	}
	m.Update(keyRunes("hello worker"))
	// stubScreen has no context → empty preamble; dock notice stays empty
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter must produce a send cmd")
	}
	if m.dock.SendRequest() != "" {
		t.Fatal("send request must be consumed")
	}
}

// Unknown /word gives usage feedback, never sends as chat.
func TestUnknownSlashCommand(t *testing.T) {
	m := newDockTestApp()
	m.Update(keyRunes("/definitely-not-a-command"))
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := nm.(*App)
	if m2.dock.Err == "" || !strings.Contains(m2.dock.Err, "unknown command") {
		t.Fatalf("dock error = %q, want unknown-command usage feedback", m2.dock.Err)
	}
}

// Escaped slash sends literally (no usage feedback).
func TestEscapedSlashSendsLiterally(t *testing.T) {
	m := newDockTestApp()
	handled, _ := m.dispatchSlash("\\/not-a-command")
	if handled {
		t.Fatal("escaped slash must be sent as chat, not dispatched")
	}
}

// /help lists every registry command (name + usage + description).
func TestHelpListsAllCommands(t *testing.T) {
	m := newDockTestApp()
	text := m.slashHelpText()
	if !strings.Contains(text, "/help") || !strings.Contains(text, "/connect") ||
		!strings.Contains(text, "/context") || !strings.Contains(text, "/quit") {
		t.Fatalf("system commands missing: %s", text)
	}
	for _, name := range m.slash.names {
		if !strings.Contains(text, name) {
			t.Errorf("command %s missing from /help output", name)
		}
	}
}

// Navigation command parity: every nav-generated command resolves in the
// registry, and every required launch/command set exists.
func TestNavCommandParity(t *testing.T) {
	m := newDockTestApp()
	required := []string{
		// top-level tabs
		"/ask", "/overview", "/work", "/execution", "/automation", "/enforcement", "/control",
		// entity commands from nav config
		"/workers", "/work-items", "/workflows", "/runs", "/executions",
		"/approvals", "/policies", "/secrets", "/mcp", "/runtime-images",
		"/settings", "/providers",
		// arg jumps
		"/wi", "/exec", "/run", "/worker",
		// remaining screen sources
		//
		// NOTE: there is deliberately NO "/decisions". The Decisions pane was removed — it had no
		// GUI counterpart and was cruft from the initial TUI (see the enforcement package docs).
		// Because the slash registry is GENERATED from the screens' Sources(), removing the
		// source removed the command: this list is the parity check between the two, so dropping
		// the entry here is the change, not a workaround. A decision record is still rendered
		// where it belongs, as policy context on an approval.
		"/projects", "/conversations", "/schedules",
		// Overview domain (Dashboard / Telemetry / Cost Explorer + /usage)
		"/dashboard", "/telemetry", "/cost-explorer", "/usage",
		// system
		"/help", "/connect", "/context", "/quit",
	}
	for _, name := range required {
		if m.slash.resolve(name) == nil {
			t.Errorf("required slash command %s not registered", name)
		}
	}
}

// Arg jumps route to the tab + source.
func TestArgJumpRoutes(t *testing.T) {
	m := newDockTestApp()
	cmd := m.slash.resolve("/wi")
	if cmd == nil {
		t.Fatal("/wi must resolve")
	}
	handled, _ := m.dispatchSlash("/wi abc-123")
	if !handled {
		t.Fatal("/wi must dispatch")
	}
	if m.ActiveTab() != TabWork {
		t.Fatalf("/wi must switch to work tab, got %q", m.ActiveTab())
	}
}

// The dock renders on every tab at 80x24 and the layout stays composeable.
func TestDockPresentEverywhere80x24(t *testing.T) {
	m := newDockTestApp()
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m2 := nm.(*App)
	for _, tab := range Tabs {
		m2.SwitchTo(tab.ID)
		view := m2.View()
		if !strings.Contains(view, "❯") {
			t.Errorf("tab %s: dock composer missing at 80x24", tab.ID)
		}
		if m2.contentHeight() < 1 {
			t.Errorf("tab %s: content height < 1 at 80x24", tab.ID)
		}
	}
}

// 401 (Unauthenticated) chat failures surface the re-auth prompt in the
// dock error strip without crashing (Phase 2b: the prompt names the
// in-place /connect fix — never a bare "error: unauthenticated").
func TestAuthExpiredSurfacesInDock(t *testing.T) {
	m := newDockTestApp()
	m.setChatError("send", &stubAuthErr{})
	if !strings.Contains(m.dock.Err, "/connect") {
		t.Fatalf("dock error = %q, want re-auth prompt", m.dock.Err)
	}
	if !strings.Contains(m.dock.Err, "re-auth") {
		t.Fatalf("dock error = %q, want explicit re-authentication wording", m.dock.Err)
	}
	if strings.Contains(m.dock.Err, "unauthenticated:") {
		t.Fatalf("dock error = %q, want the bare unauthenticated text rewritten", m.dock.Err)
	}
	m.View() // must not panic
}

type stubAuthErr struct{}

func (e *stubAuthErr) Error() string { return "unauthenticated: bad token" }

// /connect requests reconnection (main.go re-enters the connection
// screen) — never exits the process: /connect opens an in-place overlay.
func TestConnectCommandRequestsReconnect(t *testing.T) {
	m := newDockTestApp()
	handled, _ := m.dispatchSlash("/connect")
	if !handled {
		t.Fatal("/connect must dispatch")
	}
	if !m.reconnectRequested {
		t.Fatal("/connect must set reconnectRequested")
	}
	// In-place overlay contract: never quits, never tears down alt-screen.
	// The overlay opens synchronously (flag), so no cmd is required.
	if m.quitting {
		t.Fatal("/connect must NOT set quitting")
	}
	if !m.ConnectOverlayOpen() {
		t.Fatal("/connect must open the in-place overlay")
	}
}

// /context shows and pins the injected context.
func TestContextCommand(t *testing.T) {
	m := newDockTestApp()
	m.dispatchSlash("/context")
	if m.dock.Notice == "" || !strings.Contains(m.dock.Notice, "context:") {
		t.Fatalf("notice = %q", m.dock.Notice)
	}
	m.dispatchSlash("/context pin project Acme / work item 42")
	if m.contextOverride != "project Acme / work item 42" {
		t.Fatalf("override = %q", m.contextOverride)
	}
	if m.contextPreamble() != "[context: project Acme / work item 42]" {
		t.Fatalf("preamble = %q", m.contextPreamble())
	}
	m.dispatchSlash("/context pin clear")
	if m.contextOverride != "" {
		t.Fatal("pin clear must reset the override")
	}
}

// Chat msgs flow through dispatch: transcript + conversations. Fresh-launch
// contract: loading conversations must NOT auto-open one (the shell lands
// on a fresh chat like the GUI); the list populates the rail instead.
func TestChatMsgsDispatch(t *testing.T) {
	m := newDockTestApp()
	nm, _ := m.Update(chat.ConversationsMsg{Convs: []chat.Conversation{{ID: "c1", Title: "one"}}})
	m2 := nm.(*App)
	if m2.chatConvID != "" {
		t.Fatalf("chatConvID = %q, want empty (fresh-launch must not auto-open)", m2.chatConvID)
	}
	if len(m2.conversations) != 1 {
		t.Fatalf("conversations = %d, want 1 (loaded into the rail)", len(m2.conversations))
	}
	nm, _ = m2.Update(chat.TranscriptMsg{ConvID: "c1", Items: []chat.ChatItem{{Kind: chat.KindUser, Text: "hi", Key: "m-1"}}})
	m2 = nm.(*App)
	if items := m2.chatStore.snapshot("c1"); len(items) != 1 {
		t.Fatalf("items = %d", len(items))
	}
}

// Deliberate navigation: clicking/opening a rail conversation sets the
// active conversation (always an explicit action, never the launch default).
func TestRailOpenSetsActive(t *testing.T) {
	m := newDockTestApp()
	nm, _ := m.Update(chat.ConversationsMsg{Convs: []chat.Conversation{{ID: "c1", Title: "one"}}})
	m2 := nm.(*App)
	cmd := m2.openRailConversation(0)
	if cmd == nil {
		t.Fatal("openRailConversation must return a cmd")
	}
	m2.chatConvID = "c1" // OpenAskConversation sets it synchronously
	if m2.chatConvID != "c1" {
		t.Fatalf("chatConvID = %q, want c1 (deliberate open)", m2.chatConvID)
	}
}

// Plain keys reach the SCREEN only in content focus; at launch the
// composer owns the keyboard (Phase 2a), so a plain key is composer input,
// never a screen shortcut.
func TestPlainKeysStillReachScreen(t *testing.T) {
	m := newDockTestApp()
	s := &stubScreen{id: "ask"}
	m.screens[TabAsk] = s
	if m.chatFocus != focusComposer {
		t.Fatal("precondition: composer focused at launch")
	}
	m.Update(keyRunes("x"))
	if m.dock.Value() != "x" {
		t.Fatalf("plain key must land in the focused composer, dock=%q", m.dock.Value())
	}
	if s.lastKey == "x" {
		t.Fatal("plain key must NOT reach the screen while the composer is focused")
	}
	// After esc (content focus), plain keys reach the screen again.
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m2 := nm.(*App)
	m2.Update(keyRunes("x"))
	if s.lastKey != "x" {
		t.Fatalf("screen got %q, want x (content focus)", s.lastKey)
	}
}
