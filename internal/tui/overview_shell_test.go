package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// navStub is a Screen that records the shell's nav calls (slash + source
// focus) without touching a plane — the Overview wiring tests need no RPC.
type navStub struct {
	tab      TabID
	selected string
	ensured  int
}

func (s *navStub) Init() tea.Cmd                    { return nil }
func (s *navStub) Update(tea.Msg) (Screen, tea.Cmd) { return s, nil }
func (s *navStub) View() string                     { return "" }
func (s *navStub) Name() string                     { return string(s.tab) }
func (s *navStub) SetSize(int, int)                 {}
func (s *navStub) Close()                           {}
func (s *navStub) EnsureSubscriptions()             { s.ensured++ }
func (s *navStub) SelectSource(name string) bool    { s.selected = name; return true }

// TestOverviewIsSecondDomain pins the domain's slot: Overview is the tab
// directly after Ask Orchicon (the GUI's nav order), so its key label is F2
// and every later one shifts by one (the key-labelled tab chrome).
func TestOverviewIsSecondDomain(t *testing.T) {
	if len(Tabs) != 7 {
		t.Fatalf("tabs = %d, want 7 (Ask + 6 GUI nav groups)", len(Tabs))
	}
	if Tabs[0].ID != TabAsk {
		t.Fatalf("tab[0] = %q, want ask", Tabs[0].ID)
	}
	if Tabs[1].ID != TabOverview || Tabs[1].Title != "Overview" || Tabs[1].Chord != tabChord(TabOverview) {
		t.Fatalf("tab[1] = %+v, want the Overview domain (%s)", Tabs[1], tabChord(TabOverview))
	}
	// The printed label is the CHORD'S OWN KEY ("F2" for "f2"), derived from the chord so the key and
	// the label cannot drift. It used to be a bare number 1…7 — decoration that did not name the key
	// that switches to the tab; the label is now the key itself, and each one is underlined.
	for _, tab := range Tabs {
		if want := strings.ToUpper(tab.Chord); tab.Ordinal != want {
			t.Errorf("tab %s key label = %q, want %q (its chord is %q)", tab.ID, tab.Ordinal, want, tab.Chord)
		}
	}
}

// TestOverviewNavEntries pins the three GUI Overview sources (+ the raw
// /usage records pane) as real nav commands.
func TestOverviewNavEntries(t *testing.T) {
	m := newTestApp()
	got := map[string]bool{}
	for _, e := range m.navEntries(TabOverview) {
		got[e.Cmd] = true
	}
	for _, want := range []string{"dashboard", "telemetry", "cost-explorer", "usage"} {
		if !got[want] {
			t.Errorf("nav entry %q missing from the Overview tab: %v", want, got)
		}
	}
}

// TestOverviewTabDropdown pins the dropdown: it is built from the same nav
// registry as the slash commands (no drift) and carries all four rows.
func TestOverviewTabDropdown(t *testing.T) {
	m := newTestApp()
	m.openTabMenu(TabOverview)
	tm := m.TabMenu()
	if tm == nil {
		t.Fatal("the Overview tab's dropdown must open")
	}
	want := []string{"dashboard", "telemetry", "cost-explorer", "usage"}
	if len(tm.Entries) != len(want) {
		t.Fatalf("dropdown entries = %d, want %d (%+v)", len(tm.Entries), len(want), tm.Entries)
	}
	for i, w := range want {
		if tm.Entries[i].Cmd != w {
			t.Errorf("dropdown entry %d = %q, want %q", i, tm.Entries[i].Cmd, w)
		}
	}
}

// TestOverviewSlashNavigation pins /overview plus the four source
// commands: each switches to the Overview tab and focuses its source.
func TestOverviewSlashNavigation(t *testing.T) {
	m := newTestApp()
	stub := &navStub{tab: TabOverview}
	m.RegisterScreen(TabOverview, stub)

	if c := m.slash.resolve("/overview"); c == nil {
		t.Fatal("/overview must be registered")
	}
	handled, _ := m.dispatchSlash("/overview")
	if !handled {
		t.Fatal("/overview must dispatch")
	}
	if m.ActiveTab() != TabOverview {
		t.Fatalf("/overview: active tab = %q, want overview", m.ActiveTab())
	}
	if stub.ensured == 0 {
		t.Fatal("/overview must arm the screen's subscriptions (live telemetry)")
	}

	for cmd, src := range map[string]string{
		"/dashboard":     "dashboard",
		"/telemetry":     "telemetry",
		"/cost-explorer": "cost-explorer",
		"/usage":         "usage",
	} {
		if c := m.slash.resolve(cmd); c == nil {
			t.Fatalf("%s must be registered", cmd)
		}
		stub.selected = ""
		handled, _ := m.dispatchSlash(cmd)
		if !handled {
			t.Fatalf("%s must dispatch", cmd)
		}
		if m.ActiveTab() != TabOverview {
			t.Fatalf("%s: active tab = %q, want overview", cmd, m.ActiveTab())
		}
		if stub.selected != src {
			t.Errorf("%s: focused source = %q, want %q", cmd, stub.selected, src)
		}
	}
}

// TestOverviewChordSwitches pins the Overview tab's chord (structural: it bypasses the composer, like
// every other tab chord). The chord is READ from Tabs, so this test follows the binding.
func TestOverviewChordSwitches(t *testing.T) {
	m := newTestApp()
	stub := &navStub{tab: TabOverview}
	m.RegisterScreen(TabOverview, stub)
	nm, _ := m.Update(keyFor(tabChord(TabOverview)))
	m2 := nm.(*App)
	if m2.ActiveTab() != TabOverview {
		t.Fatalf("%s: active tab = %q, want overview", tabChord(TabOverview), m2.ActiveTab())
	}
	if stub.ensured == 0 {
		t.Fatalf("%s must arm the Overview screen's subscriptions", tabChord(TabOverview))
	}
}

// TestOverviewTabBarFitsAt80 pins the 7th-tab width maths: at 80 columns
// the bar drops the inter-tab gaps and then the ordinal prefixes so all
// seven tabs stay inside the viewport AND remain clickable.
func TestOverviewTabBarFitsAt80(t *testing.T) {
	app := newCenterTestApp(80, 24)
	bar := app.tabBarView()
	if w := lipgloss.Width(bar); w >= 80 {
		t.Fatalf("seven-tab bar width %d overflows 80 columns", w)
	}
	for _, tab := range Tabs {
		col := app.tabStartCol(tab)
		id, ok := app.TabClick(col)
		if !ok || id != tab.ID {
			t.Errorf("80-col click at %d = %q,%v want %q", col, id, ok, tab.ID)
		}
	}
}
