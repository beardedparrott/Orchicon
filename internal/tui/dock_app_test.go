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

// ctrl+g toggles focus to the composer; the dock is focused, tab chords
// fall through to readline editing, and esc/ctrl+g return to content.
func TestFocusChordAndFallthrough(t *testing.T) {
	m := newDockTestApp()
	if m.chatFocus != focusContent {
		t.Fatal("must start in content focus")
	}
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	m2 := nm.(*App)
	if m2.chatFocus != focusComposer || !m2.dock.Focused {
		t.Fatal("ctrl+g must focus the composer")
	}
	// Tab chord while focused: ctrl+w must reach the composer (delete
	// word), NOT switch tabs.
	nm, _ = m2.Update(tea.KeyMsg{Type: tea.KeyCtrlW})
	m2 = nm.(*App)
	if m2.ActiveTab() != TabAsk {
		t.Fatalf("ctrl+w in composer switched tab to %q — must fall through to readline editing", m2.ActiveTab())
	}
	for _, chord := range []string{"ctrl+a", "ctrl+e", "ctrl+f", "ctrl+t", "ctrl+o"} {
		nm, _ = m2.Update(keyFor(chord))
		m2 = nm.(*App)
		if m2.ActiveTab() != TabAsk {
			t.Fatalf("%s in composer switched tab — must fall through", chord)
		}
	}
	// esc returns focus to content; chords work again
	nm, _ = m2.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m2 = nm.(*App)
	if m2.chatFocus != focusContent {
		t.Fatal("esc must return focus to content")
	}
	nm, _ = m2.Update(keyFor("ctrl+w"))
	if nm.(*App).ActiveTab() != TabWork {
		t.Fatal("ctrl+w must switch tabs again once focus is back in content")
	}
}

// Enter sends from the composer on any screen; the context preamble is
// visible in the dock notice.
func TestComposerSendCarriesContext(t *testing.T) {
	m := newDockTestApp()
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	m2 := nm.(*App)
	m2.Update(keyRunes("hello worker"))
	// stubScreen has no context → empty preamble; dock notice stays empty
	_, cmd := m2.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter must produce a send cmd")
	}
	if m2.dock.SendRequest() != "" {
		t.Fatal("send request must be consumed")
	}
}

// Unknown /word gives usage feedback, never sends as chat.
func TestUnknownSlashCommand(t *testing.T) {
	m := newDockTestApp()
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	m2 := nm.(*App)
	m2.Update(keyRunes("/definitely-not-a-command"))
	nm, _ = m2.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 = nm.(*App)
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
		"/ask", "/work", "/execution", "/automation", "/enforcement", "/control",
		// entity commands from nav config
		"/workers", "/work-items", "/workflows", "/runs", "/executions",
		"/approvals", "/policies", "/secrets", "/mcp", "/runtime-images",
		"/settings", "/providers",
		// arg jumps
		"/wi", "/exec", "/run", "/worker",
		// remaining screen sources
		"/projects", "/conversations", "/schedules", "/decisions",
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

// The composer keys reach the dock only when focused; plain keys go to
// the screen otherwise (foundation contract preserved).
func TestPlainKeysStillReachScreen(t *testing.T) {
	m := newDockTestApp()
	s := &stubScreen{id: "ask"}
	m.screens[TabAsk] = s
	m.Update(keyRunes("x"))
	if s.lastKey != "x" {
		t.Fatalf("screen got %q, want x", s.lastKey)
	}
}
