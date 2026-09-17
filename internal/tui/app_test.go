package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
	"github.com/beardedparrott/orchicon/internal/tui/stream"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

func newTestApp() *App {
	m := NewApp(&client.Clients{}, &config.Profile{Name: "default", URL: "https://x.example.com"}, "v9.9.9")
	m.width, m.height = 120, 40
	return m
}

// stubScreen is a minimal Screen implementation for shell tests.
type stubScreen struct {
	id      string
	closed  bool
	width   int
	height  int
	chip    string
	lastKey string
	ensure  int
}

func (s *stubScreen) Init() tea.Cmd        { return nil }
func (s *stubScreen) View() string         { return "view:" + s.id }
func (s *stubScreen) Name() string         { return s.id }
func (s *stubScreen) SetSize(w, h int)     { s.width, s.height = w, h }
func (s *stubScreen) Close()               { s.closed = true }
func (s *stubScreen) ContextChip() string  { return s.chip }
func (s *stubScreen) EnsureSubscriptions() { s.ensure++ }
func (s *stubScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		s.lastKey = k.String()
	}
	return s, nil
}

func TestTabOrderMirrorsGUINav(t *testing.T) {
	want := []string{"Ask Orchicon", "Overview", "Work", "Execution", "Automation", "Enforcement", "Control"}
	if len(Tabs) != len(want) {
		t.Fatalf("tabs = %d, want %d", len(Tabs), len(want))
	}
	for i, title := range want {
		if Tabs[i].Title != title {
			t.Fatalf("tab %d = %q, want %q", i, Tabs[i].Title, title)
		}
	}
	// THE CHORD IS THE KEY THE BAR PRINTS. Asserted from each tab's own key label rather than a literal
	// list, because that is the invariant asked for when the ctrl+letter chords were replaced — "make
	// them ctrl+number and use the numbers that we have assigned each tab menu" — and a literal list
	// here is one of the four places those old chords were spelled out, which is how they could drift
	// from the bar.
	//
	// It also checks the chord is EXPRESSIBLE as a key event, which is the part that is easy to get
	// wrong: ctrl+<digit> looks fine in a string and is undeliverable (ctrl+3 arrives as esc), and
	// alt+<digit> is deliverable but claimed by the emulator before the program sees it.
	for i, tab := range Tabs {
		want := strings.ToLower(tab.Ordinal)
		if tab.Chord != want {
			t.Fatalf("tab %d chord = %q, want %q (the key the tab bar prints)", i, tab.Chord, want)
		}
		if got := keyFor(tab.Chord).String(); got != tab.Chord {
			t.Fatalf("tab %d chord %q is not expressible as a key event (got %q)", i, tab.Chord, got)
		}
	}
}

func TestChordSwitching(t *testing.T) {
	m := newTestApp()
	for _, tab := range Tabs {
		s := &stubScreen{id: string(tab.ID)}
		m.RegisterScreen(tab.ID, s)
	}
	for _, tab := range Tabs {
		nm, _ := m.Update(keyFor(tab.Chord))
		m2 := nm.(*App)
		if m2.ActiveTab() != tab.ID {
			t.Fatalf("chord %s: active = %q, want %q", tab.Chord, m2.ActiveTab(), tab.ID)
		}
	}
}

// tabChordKeys maps each tab's function-key chord to its key event, explicitly: bubbletea's KeyType
// values do not run in source order between families (KeyF1 + 1 lands on ctrl+shift+end), so a family
// built by incrementing its first member silently produces the wrong key for six of the seven tabs.
var tabChordKeys = map[string]tea.KeyMsg{
	"f1": {Type: tea.KeyF1},
	"f2": {Type: tea.KeyF2},
	"f3": {Type: tea.KeyF3},
	"f4": {Type: tea.KeyF4},
	"f5": {Type: tea.KeyF5},
	"f6": {Type: tea.KeyF6},
	"f7": {Type: tea.KeyF7},
}

// keyFor builds the KeyMsg whose String() equals s.
func keyFor(s string) tea.KeyMsg {
	// ALT-PREFIXED keys: alt+<digit> was a former tab chord form — a KeyRunes with Alt set. Kept
	// because the chord form is the sort of thing that comes back.
	if rest, ok := strings.CutPrefix(s, "alt+"); ok {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(rest), Alt: true}
	}
	// The tab chords are function keys: f1 … f7. Listed EXPLICITLY rather than computed from KeyF1,
	// because bubbletea's KeyType values do not run in source order — KeyF1 + 1 is ctrl+shift+end, not
	// f2 (found by this very helper, when the chord assertion rejected the key it built). A computed
	// family would look like a broken feature rather than a broken helper.
	if k, ok := tabChordKeys[s]; ok {
		return k
	}
	switch s {
	case "ctrl+a":
		return tea.KeyMsg{Type: tea.KeyCtrlA}
	case "ctrl+w":
		return tea.KeyMsg{Type: tea.KeyCtrlW}
	case "ctrl+e":
		return tea.KeyMsg{Type: tea.KeyCtrlE}
	case "ctrl+f":
		return tea.KeyMsg{Type: tea.KeyCtrlF}
	case "ctrl+t":
		return tea.KeyMsg{Type: tea.KeyCtrlT}
	case "ctrl+o":
		return tea.KeyMsg{Type: tea.KeyCtrlO}
	case "ctrl+v":
		return tea.KeyMsg{Type: tea.KeyCtrlV}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// LEFT/RIGHT must NOT rotate the tab bar.
//
// They used to (a global route, which ran before the screen), which made it
// impossible to move focus between the two panes below the tab bar — the
// operator's "left+right should not be moving the tab menu at the top nor should
// it be rotating through the different screens ... it should move between the two
// panes below the menus". The tab bar keeps Tab/Shift+Tab and the ctrl chords.
func TestArrowsDoNotCycleTabs(t *testing.T) {
	m := newTestApp()
	m.RegisterScreen(TabWork, &stubScreen{id: "work"})
	m.RegisterScreen(TabExecution, &stubScreen{id: "execution"})
	m.SwitchTo(TabWork)
	if m.ActiveTab() != TabWork {
		t.Fatalf("initial tab = %q", m.ActiveTab())
	}
	for _, k := range []tea.KeyMsg{{Type: tea.KeyRight}, {Type: tea.KeyLeft}} {
		nm, _ := m.Update(k)
		if got := nm.(*App).ActiveTab(); got != TabWork {
			t.Fatalf("arrow key moved the tab bar to %q — arrows must stay on the panes", got)
		}
	}
	// Tab still walks the ring, so the tab bar is not stranded.
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if got := nm.(*App).ActiveTab(); got != TabWork {
		t.Fatalf("the first tab lands on the CURRENT tab's bar, got %q", got)
	}
	nm, _ = nm.(*App).Update(tea.KeyMsg{Type: tea.KeyTab})
	if got := nm.(*App).ActiveTab(); got != TabExecution {
		t.Fatalf("a second tab must advance to execution, got %q", got)
	}
}

// The BAR-focused case, which is the one the first fix missed.
//
// Removing only the GLOBAL left/right routes (27ad7c71) left the focusTabs branch in
// dispatch still calling tabRingPrev/tabRingNext — and the bar is exactly where Tab
// and a tab click land. So the arrows still rotated the screens whenever the bar held
// the keyboard: "left+right should not be moving the tab menu at the top nor should it
// be rotating through the different screens".
//
// They now hand the keyboard DOWN to the panes, which is the other half of the
// operator's ask ("move between the two panes below the menus so you can grab focus on
// those"). The ring stays reachable on Tab/Shift+Tab.
func TestArrowsDoNotCycleTabsEvenFromTheBar(t *testing.T) {
	m := newTestApp()
	m.RegisterScreen(TabWork, &stubScreen{id: "work"})
	m.RegisterScreen(TabExecution, &stubScreen{id: "execution"})
	m.SwitchTo(TabWork)
	m.setFocus(focusTabs)
	if m.chatFocus != focusTabs {
		t.Fatalf("precondition: focus = %v, want focusTabs", m.chatFocus)
	}
	for _, k := range []tea.KeyMsg{{Type: tea.KeyRight}, {Type: tea.KeyLeft}} {
		nm, _ := m.Update(k)
		got := nm.(*App)
		if tab := got.ActiveTab(); tab != TabWork {
			t.Fatalf("arrow key moved the tab bar to %q — arrows must not rotate the screens", tab)
		}
		if got.chatFocus != focusContent {
			t.Fatalf("arrow key left focus on %v — it must hand the keyboard down to the panes", got.chatFocus)
		}
		got.setFocus(focusTabs) // back to the bar for the next case
	}
	// The ring is still walkable from the bar. The exact stop order is the ring's own
	// business (from the bar each Tab is one advance, so the first stop is the NEXT
	// tab); the invariant this test is actually about is that Tab is not dead, since
	// arrows no longer rotate anything.
	nm := tea.Model(m)
	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		next, _ := nm.(*App).Update(tea.KeyMsg{Type: tea.KeyTab})
		nm = next
		seen[string(nm.(*App).ActiveTab())] = true
	}
	if !seen[string(TabExecution)] {
		t.Fatalf("Tab must still walk the ring to Execution — removing the arrow routes must not strand the bar; visited %v", seen)
	}
}

func TestSwitchClosesPreviousScreen(t *testing.T) {
	m := newTestApp()
	a := &stubScreen{id: "work"}
	b := &stubScreen{id: "execution"}
	m.RegisterScreen(TabWork, a)
	m.RegisterScreen(TabExecution, b)
	m.SwitchTo(TabWork)
	m.SwitchTo(TabExecution)
	if !a.closed {
		t.Fatal("previous screen must be closed on switch (unsubscribe)")
	}
	if b.closed {
		t.Fatal("new screen must not be closed")
	}
}

// The help overlay must render every route in the registry — parity
// between `?` and actual behavior is an acceptance criterion.
func TestHelpOverlayMatchesKeymap(t *testing.T) {
	m := newTestApp()
	_ = m
	routes := GlobalKeyRoutes(Tabs)
	lines := strings.Join(helpLines(routes), "\n")
	for _, r := range routes {
		if r.Keys == "" {
			continue
		}
		if !strings.Contains(lines, r.Keys) {
			t.Errorf("route %q (%s) missing from help overlay", r.Keys, r.Name)
		}
		if !strings.Contains(lines, r.Name) {
			t.Errorf("route name %q missing from help overlay", r.Name)
		}
	}
	// every tab chord appears
	for _, tab := range Tabs {
		found := false
		for _, r := range routes {
			if r.Keys == tab.Chord {
				found = true
			}
		}
		if !found {
			t.Errorf("no route registered for tab chord %s", tab.Chord)
		}
	}
}

func TestHelpOverlayToggle(t *testing.T) {
	m := newTestApp()
	m.RegisterScreen(TabWork, &stubScreen{id: "work"})
	// The help overlay opens from CONTENT focus (? is a literal character
	// while composing — typing "how?" into the composer must never open
	// an overlay). Esc also hands focus back: toggle content → composer.
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc}) // composer → content
	m2 := nm.(*App)
	if m2.chatFocus != focusContent {
		t.Fatal("esc must return focus to content (precondition)")
	}
	nm, _ = m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	m2 = nm.(*App)
	if !m2.help.open {
		t.Fatal("? must open the overlay")
	}
	view := m2.View()
	if !strings.Contains(view, "orch — keys") {
		t.Fatalf("overlay not rendered: %q", view)
	}
	nm, _ = m2.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if nm.(*App).help.open {
		t.Fatal("esc must close the overlay")
	}
}

// Inside a text input (composer concern later), chords must fall through
// to editing: the router consumes ctrl-chords first, but plain keys and
// shift+letters reach the screen. The composer owns plain keys at launch
// (Phase 2a) — after esc (content focus) they reach the screen.
func TestRouterDefersPlainKeysToScreen(t *testing.T) {
	m := newTestApp()
	s := &stubScreen{id: "ask"}
	m.RegisterScreen(TabAsk, s)
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc}) // composer → content
	m2 := nm.(*App)
	nm, _ = m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	m2 = nm.(*App)
	got := m2.screens[TabAsk].(*stubScreen)
	if got.lastKey != "x" {
		t.Fatalf("screen got %q, want x", got.lastKey)
	}
}

func TestFooterVersionDrift(t *testing.T) {
	if versionDrift("v1.2.3", "v1.2.3") {
		t.Fatal("equal versions must not drift")
	}
	if !versionDrift("v1.2.3", "v1.2.4") {
		t.Fatal("mismatched versions must drift")
	}
	if versionDrift("v1.2.3", "dev") {
		t.Fatal("dev client never warns")
	}
	if versionDrift("", "v1.2.3") {
		t.Fatal("unknown server version never warns")
	}
}

func TestFooterRendersDriftWarning(t *testing.T) {
	f := footerModel{
		URL:           "https://x",
		ServerVersion: "v1.0.0",
		ClientVersion: "v2.0.0",
		StreamStatus:  stream.StatusOpen,
	}
	v := f.View()
	if !strings.Contains(v, "⚠ drift") {
		t.Fatalf("drift warning missing: %q", v)
	}
	if !strings.Contains(v, "https://x") {
		t.Fatalf("URL missing: %q", v)
	}
}

func TestFooterDisconnectedRetrying(t *testing.T) {
	f := footerModel{URL: "https://x", StreamStatus: stream.StatusReconnecting}
	v := f.View()
	if !strings.Contains(v, "disconnected, retrying") {
		t.Fatalf("graceful degradation label missing: %q", v)
	}
}

// Tab chords must drive the active screen's live streams: the chord
// doubles as the stream re-arm, and the opening tab starts live on the
// first WindowSizeMsg (the shell's EnsureSubscriptions contract).
func TestChordEnsuresSubscriptions(t *testing.T) {
	m := newTestApp()
	s := &stubScreen{id: "work"}
	m.RegisterScreen(TabWork, s)
	m.Update(keyFor(tabChord(TabWork)))
	if s.ensure == 0 {
		t.Fatal("tab chord must ensure the screen's subscriptions")
	}
	first := s.ensure
	m.Update(keyFor(tabChord(TabWork)))
	if s.ensure != first+1 {
		t.Fatalf("chord re-press must re-arm EnsureSubscriptions: ensure=%d", s.ensure)
	}
}

func TestWorseStatusWins(t *testing.T) {
	if statusRank(stream.StatusOpen) >= statusRank(stream.StatusReconnecting) {
		t.Fatal("reconnecting must outrank open")
	}
	if statusRank(stream.StatusError) <= statusRank(stream.StatusOpen) {
		t.Fatal("error must outrank open")
	}
	if statusRank(stream.StatusOpen) != 1 {
		t.Fatal("open rank pinned (footer default)")
	}
}

// tabChord resolves a tab's chord from Tabs, so a test FOLLOWS the bindings instead of restating
// them. Restating is how the old ctrl+letter chords ended up spelled out in four places and drifted
// from the bar the operator reads.
func tabChord(id TabID) string {
	for _, t := range Tabs {
		if t.ID == id {
			return t.Chord
		}
	}
	return ""
}

// EVERY CELL IS PAINTED WITH THE THEME'S BACKGROUND.
//
// bgOpaque re-asserts the background after an inner style's SGR reset, so that a row wrapped in
// ScreenBg has no cell falling through to the TERMINAL's own colours. It used to skip the re-assert
// whenever ANY SGR followed the reset, reasoning that "its own sequence will establish state" — but
// almost every following sequence sets only a FOREGROUND, so the background stayed cleared:
//
//	...\x1b[0m\x1b[38;2;157;171;190mhttps://…\x1b[0m
//	    └ reset; the next SGR is fg-only, so the background was clear for the whole URL
//
// That is the operator's "black background box" on the footer, "the black around the adapter and
// provider selection", and the black blocks at the end of a line in an execution — a theme cannot fix
// cells the app never painted.
func TestEveryCellCarriesTheThemeBackground(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	m := newTestApp()
	m.dispatch(tea.WindowSizeMsg{Width: 120, Height: 40})
	theme.Use("light")
	t.Cleanup(func() { theme.Use("dark") })

	unpainted := unpaintedCells(m.View())
	if unpainted.total != 0 {
		t.Errorf("%d cells have no background, so the terminal's own colour shows through — the app "+
			"must paint every cell of the frame (first offender: col %d of line %d)",
			unpainted.total, unpainted.firstCol, unpainted.firstLine)
	}
}

// unpaintedCells counts the cells of a rendered frame with no background in effect. It walks the
// escape sequences rather than pattern-matching, because "is there a background here" depends on the
// whole sequence history of the row: a reset clears it, a 48;… sets it, and a 38;… leaves it alone.
type unpaintedReport struct {
	total, firstCol, firstLine int
}

func unpaintedCells(view string) unpaintedReport {
	var rep unpaintedReport
	rep.firstCol, rep.firstLine = -1, -1
	for li, line := range strings.Split(view, "\n") {
		bg := false
		col := 0
		rest := line
		for {
			i := strings.Index(rest, "\x1b[")
			text := rest
			if i >= 0 {
				text = rest[:i]
			}
			if n := lipgloss.Width(text); n > 0 && !bg {
				if rep.firstLine < 0 {
					rep.firstLine, rep.firstCol = li+1, col
				}
				rep.total += n
			}
			col += lipgloss.Width(text)
			if i < 0 {
				break
			}
			end := strings.IndexByte(rest[i:], 'm')
			if end < 0 {
				break
			}
			for _, p := range strings.Split(rest[i+2:i+end], ";") {
				switch p {
				case "", "0", "49":
					bg = false
					if p == "" {
						bg = true // an empty parameter list is 0, handled above
					}
				case "48":
					bg = true
				}
			}
			rest = rest[i+end+1:]
		}
	}
	return rep
}
