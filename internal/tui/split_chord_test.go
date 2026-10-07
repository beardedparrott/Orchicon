package tui

// split_chord_test.go — THE WIDTH CHORD RESIZES WHICHEVER SPLIT IS IN FRONT OF YOU.
//
// The operator: "I am talking about every pane in the TUI where there is a tree and detail view. I would like
// to be able to expand the tree view to see the full work item names, execution names, etc."
//
// Every such screen renders through kit2.Base.SinglePane, so ONE preference on the Base reaches them all —
// and ctrl+left/right already resize the diff rail while explicitly doing nothing when the rail is closed
// (diffRailWidthStep returns false). That made the chord free to serve the screen split, in precedence order:
// the rail when it is open (it is drawn over everything, so it is what the operator is looking at), otherwise
// the active screen's tree/detail split.

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// splitScreenStub embeds kit2.Base exactly as every real screen does, so it carries the split the shell
// adjusts — the same embedding kit2BaseProbe and pasteScreenStub rely on in their own suites.
type splitScreenStub struct{ kit2.Base }

func (s *splitScreenStub) Init() tea.Cmd                              { return nil }
func (s *splitScreenStub) Update(tea.Msg) (screenkit.Screen, tea.Cmd) { return s, nil }
func (s *splitScreenStub) View() string                               { return "" }
func (s *splitScreenStub) Name() string                               { return "split-stub" }
func (s *splitScreenStub) Close()                                     {}

func keyCtrl(k tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: k} }

// splitApp builds a shell with a Base-embedding screen active and the diff rail CLOSED. The screen is given a
// width so the split arithmetic has a frame to work in.
func splitApp(t *testing.T) (*App, *splitScreenStub) {
	t.Helper()
	m := newTestApp()
	st := &splitScreenStub{}
	m.RegisterScreen(TabWork, st)
	m.dispatch(tea.WindowSizeMsg{Width: 140, Height: 40})
	m.SwitchTo(TabWork)
	st.SetSize(120, 30)
	if m.diffOpen {
		t.Fatal("the diff rail is open — these tests are about the chord when it is closed")
	}
	return m, st
}

func TestTheChordWidensTheScreensTreeWhenTheRailIsClosed(t *testing.T) {
	m, st := splitApp(t)
	before := st.ListSharePct()

	nm, _ := m.Update(keyCtrl(tea.KeyCtrlRight))
	m = nm.(*App)
	if got := st.ListSharePct(); got <= before {
		t.Fatalf("ctrl+right left the tree pane at %d (was %d) — the chord did not reach the screen's split",
			got, before)
	}
	widened := st.ListSharePct()

	nm, _ = m.Update(keyCtrl(tea.KeyCtrlLeft))
	m = nm.(*App)
	if got := st.ListSharePct(); got >= widened {
		t.Errorf("ctrl+left did not narrow the split: %d -> %d", widened, got)
	}
	if st.ListSharePct() != before {
		t.Errorf("right then left landed on %d, want the original %d", st.ListSharePct(), before)
	}

	// ctrl+down returns it to an even share, which is the "I have made a mess of this" gesture.
	nm, _ = m.Update(keyCtrl(tea.KeyCtrlRight))
	m = nm.(*App)
	nm, _ = m.Update(keyCtrl(tea.KeyCtrlDown))
	m = nm.(*App)
	if got := st.ListSharePct(); got != 50 {
		t.Errorf("ctrl+down left the split at %d, want the default 50", got)
	}
}

// THE APP IS THE SOURCE OF TRUTH, because screens are built LAZILY: a split stored only on the screen the
// operator was looking at would be missing on every tab they had not visited yet.
func TestTheSplitIsPushedToALaterBuiltScreen(t *testing.T) {
	m, st := splitApp(t)
	nm, _ := m.Update(keyCtrl(tea.KeyCtrlRight))
	m = nm.(*App)
	chosen := st.ListSharePct()
	if chosen == 50 {
		t.Fatal("the chord did not move the split, so this test would prove nothing")
	}

	// A screen that did not exist when the operator chose the width must still come up with it.
	fresh := &splitScreenStub{}
	m.RegisterScreen(TabExecution, fresh)
	m.SwitchTo(TabExecution)
	// A SIZE message runs the rebind pass without touching the split (a chord here would nudge the very screen
	// under test and the assertion would be about the nudge, not about the preference).
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	if got := fresh.ListSharePct(); got != chosen {
		t.Errorf("a screen built after the choice came up at %d, want the chosen %d — the preference must "+
			"live on the APP, not only on the screens alive at the time", got, chosen)
	}
}

// THE RAIL WINS WHEN IT IS OPEN: the chord is one gesture, and what it resizes is what the operator is
// looking at. Opening the rail is what puts the rail in front.
func TestTheRailKeepsTheChordWhenItIsOpen(t *testing.T) {
	m, st := splitApp(t)
	shareBefore := st.ListSharePct()

	ex := &diffStubOwner{detailID: "exec-1"}
	m.RegisterScreen(TabExecution, ex)
	m.setFocus(focusContent)
	m.SwitchTo(TabExecution)
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	m = nm.(*App)
	if !m.diffOpen {
		t.Fatal("ctrl+d did not open the diff rail")
	}
	m.width, m.height = 140, 40
	railBefore := m.diffPaneWidth()

	nm, _ = m.Update(keyCtrl(tea.KeyCtrlRight))
	m = nm.(*App)
	if m.diffPaneWidth() <= railBefore {
		t.Errorf("ctrl+right did not widen the OPEN diff rail: %d -> %d", railBefore, m.diffPaneWidth())
	}
	// The screen's own split is untouched — one chord, one target.
	if st.ListSharePct() != shareBefore {
		t.Errorf("the chord moved the SCREEN's split (%d -> %d) while the rail was open", shareBefore,
			st.ListSharePct())
	}
}
