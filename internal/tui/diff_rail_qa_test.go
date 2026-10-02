package tui

// diff_rail_qa_test.go — INDEPENDENT QA probes for the drag-resizable diff rail.
//
// These deliberately do NOT reuse the feature's own helpers where a helper could hide the bug: each
// probe measures the DRAWN frame (or the resolver the drawing must agree with) rather than the fields
// the feature maintains.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/beardedparrott/orchicon/internal/tui/config"
)

// The pane's drawn width and the SELECTION RESOLVER must agree at every size, override or not — the AC
// says the clipboard region resolver still has to match the rendered pane columns.
func TestQADiffRailSelectionResolverAgreesWithTheDrawnPane(t *testing.T) {
	for _, w := range []int{120, 160, 240} {
		m := newDiffRailApp(t, w, 40)
		m.setDiffPaneW(w / 3)
		wantW := m.diffPaneWidth()
		if got := contentStartColumn(t, m); got != wantW {
			t.Fatalf("%d cols: drawn pane is %d wide, field says %d", w, got, wantW)
		}
		// The divider column resolves to the PANE region, ending exactly at the drawn edge.
		r, ok := m.selectionRegionAt(wantW-1, tabBarRows+2)
		if !ok {
			t.Fatalf("%d cols: the divider column %d resolves to no region — a drag there would select nothing",
				w, wantW-1)
		}
		if r.x0 != 0 || r.x1 != wantW-1 {
			t.Errorf("%d cols: the pane region is cols %d..%d, want 0..%d (the drawn pane)", w, r.x0, r.x1, wantW-1)
		}
		// One column right of the divider is the CONTENT: the two must not overlap.
		r2, ok := m.selectionRegionAt(wantW, tabBarRows+2)
		if !ok {
			t.Fatalf("%d cols: the first content column resolves to no region", w)
		}
		if r2.x0 != wantW {
			t.Errorf("%d cols: content region starts at %d, want the drawn pane edge %d — the resolver and the "+
				"draw disagree", w, r2.x0, wantW)
		}
	}
}

// WITH THE PANE CLOSED, an override cannot change anything the operator sees: contentWidth must be the
// full width and the frame must be identical with and without a stored override.
func TestQADiffRailOverrideIsInertWithThePaneClosed(t *testing.T) {
	base := newDiffRailApp(t, 160, 40)
	base.diffOpen = false
	base.setDiffPaneW(0)
	base.refreshLayout()
	wantContent := base.contentWidth()
	wantFrame := base.View()

	ov := newDiffRailApp(t, 160, 40)
	ov.diffOpen = false
	ov.setDiffPaneW(90)
	ov.refreshLayout()

	if got := ov.contentWidth(); got != wantContent {
		t.Errorf("with the pane closed, a stored override changed contentWidth: %d, want %d", got, wantContent)
	}
	if got := ov.View(); got != wantFrame {
		t.Errorf("with the pane closed, a stored override changed the rendered frame")
	}
}

// THE WHEEL IS NOT A RESIZE AND THE DRAG IS NOT A WHEEL: a wheel over the divider column still reaches the
// pane's own scroll, and a wheel DURING a drag does not silently mutate the width.
func TestQADiffRailWheelIsNotADrag(t *testing.T) {
	m := newDiffRailApp(t, 160, 40)
	m.setDiffPaneW(80)
	y := tabBarRows + 2

	nm, _ := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown, X: 79, Y: y})
	m = nm.(*App)
	if m.diffResizing {
		t.Error("a wheel over the divider started a resize")
	}
	if got := m.diffPaneW; got != 80 {
		t.Errorf("a wheel over the divider changed the override to %d", got)
	}

	// And during a live drag the wheel is inert (it must not be mistaken for motion).
	nm, _ = m.Update(mouse(tea.MouseActionPress, 79, y))
	m = nm.(*App)
	nm, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp, X: 40, Y: y})
	m = nm.(*App)
	if got := m.diffPaneWidth(); got != 80 {
		t.Errorf("a wheel during a drag moved the rail to %d, want 80", got)
	}
}

// A DRAG PAST BOTH EDGES lands exactly on the clamp, and the composed frame agrees.
func TestQADiffRailDragIsClampedAtBothEdges(t *testing.T) {
	m := newDiffRailApp(t, 240, 40)
	y := tabBarRows + 2

	// Drag far right: the applied width is the cap (half of avail) and the frame follows.
	nm, _ := m.Update(mouse(tea.MouseActionPress, m.diffPaneWidth()-1, y))
	m = nm.(*App)
	nm, _ = m.Update(mouse(tea.MouseActionMotion, 500, y))
	m = nm.(*App)
	if got, cap := m.diffPaneWidth(), 240/2; got != cap {
		t.Errorf("dragging past the right edge left the rail at %d, want the cap %d", got, cap)
	}
	if got := contentStartColumn(t, m); got != m.diffPaneWidth() {
		t.Errorf("at the cap the drawn pane is %d wide, field says %d", got, m.diffPaneWidth())
	}

	// Drag far left: the floor holds.
	nm, _ = m.Update(mouse(tea.MouseActionMotion, 1, y))
	m = nm.(*App)
	if got := m.diffPaneWidth(); got != DiffRailMinWidth {
		t.Errorf("dragging past the left edge left the rail at %d, want the floor %d", got, DiffRailMinWidth)
	}
	if got := contentStartColumn(t, m); got != DiffRailMinWidth {
		t.Errorf("at the floor the drawn pane is %d wide, want %d", got, DiffRailMinWidth)
	}
	nm, _ = m.Update(mouse(tea.MouseActionRelease, 1, y))
	m = nm.(*App)
}

// THE RELEASE PERSISTS: a drag ends by writing the settled width through the sandboxed config path, and a
// restarted model comes back at the DRAWN width.
func TestQADiffRailDragReleasePersistsTheWidth(t *testing.T) {
	path := useTempConfigDir(t)
	m := newDiffRailApp(t, 240, 40)
	y := tabBarRows + 2

	nm, _ := m.Update(mouse(tea.MouseActionPress, m.diffPaneWidth()-1, y))
	m = nm.(*App)
	nm, _ = m.Update(mouse(tea.MouseActionMotion, 99, y))
	m = nm.(*App)
	nm, _ = m.Update(mouse(tea.MouseActionRelease, 99, y))
	m = nm.(*App)
	if got := m.diffPaneWidth(); got != 100 {
		t.Fatalf("the drag left the rail at %d, want 100", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the release wrote no config file (%s): %v", path, err)
	}
	if !strings.Contains(string(data), "diff_rail_width = 100") {
		t.Fatalf("the release did not persist the width:\n%s", data)
	}
	m2 := newDiffRailApp(t, 240, 40)
	if got := contentStartColumn(t, m2); got != 100 {
		t.Errorf("the restarted model draws the pane %d wide, want the persisted 100", got)
	}
}

// THE CHORD IS REACHABLE FROM THE COMPOSER WITH TEXT ALREADY TYPED (the operator's real situation), and
// the draft survives untouched.
func TestQADiffRailChordWithADraftInTheComposer(t *testing.T) {
	m := newDiffRailApp(t, 200, 40)
	m.setFocus(focusComposer)
	m.dock.SetValue("half-typed message")
	before := m.diffPaneWidth()

	nm, _ := m.Update(ctrlArrow(t, tea.KeyCtrlRight))
	m = nm.(*App)
	if got, want := m.diffPaneWidth(), before+diffResizeStep; got != want {
		t.Fatalf("ctrl+right with a draft in the composer gave %d, want %d", got, want)
	}
	if got := m.dock.Value(); got != "half-typed message" {
		t.Errorf("the resize chord damaged the composer draft: %q", got)
	}
	if got := strings.TrimSpace(m.dock.Value()); got == "" {
		t.Error("the draft was cleared")
	}
}

// THE RAIL VISIBLE CASE: the clamp divides the width LEFT OVER after the conversations rail, and the
// override obeys it — no second width policy.
func TestQADiffRailClampRespectsTheConversationsRail(t *testing.T) {
	m := newDiffRailApp(t, 200, 40)
	m.active = TabAsk
	m.askMode = askConversations
	m.rightRailOpen = true
	m.refreshLayout()
	if !m.railVisible() {
		t.Fatal("precondition: the conversations rail should be visible")
	}
	avail := 200 - ConversationsRailWidth
	m.setDiffPaneW(199)
	if got := m.diffPaneWidth(); got > avail/2 {
		t.Errorf("with the conversations rail up the rail is %d, above half the available %d", got, avail/2)
	}
	// The frame still lines up: pane + content + rail == terminal width.
	row := strings.Split(m.View(), "\n")[tabBarRows+1]
	if got := lipgloss.Width(row); got != 200 {
		t.Errorf("a resized rail broke the frame width: %d, want 200", got)
	}
}

// A DIVIDER PRESS WITH NO MOTION MUST NOT PIN A WIDTH THE OPERATOR DID NOT DRAG TO. A stray click on the
// rail's edge is not a resize: it must not turn AUTO into a fixed override (the terminal would then stop
// scaling, and the release persists the pinned number).
func TestQADiffRailStrayDividerClickDoesNotPinAuto(t *testing.T) {
	path := useTempConfigDir(t)
	m := newDiffRailApp(t, 240, 40)
	y := tabBarRows + 2

	nm, _ := m.Update(mouse(tea.MouseActionPress, m.diffPaneWidth()-1, y))
	m = nm.(*App)
	nm, _ = m.Update(mouse(tea.MouseActionRelease, m.diffPaneWidth()-1, y))
	m = nm.(*App)

	if m.diffPaneW != 0 {
		t.Errorf("a click with no drag pinned an override of %d — AUTO no longer scales the pane", m.diffPaneW)
	}
	if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), "diff_rail_width") {
		t.Errorf("a click with no drag persisted a width:\n%s", data)
	}
}

// THE SAME COLUMN IN A DEEPER FRAME: the divider hit region must track the pane's BODY, not its tab bar.
func TestQADiffRailDividerHitRowsFollowTheBody(t *testing.T) {
	m := newDiffRailApp(t, 160, 40)
	x := m.diffPaneWidth() - 1
	if m.diffDividerHit(x, tabBarRows) {
		t.Error("the divider claims the gap row above the body")
	}
	if !m.diffDividerHit(x, tabBarRows+1) {
		t.Error("the divider does not claim the pane's first body row")
	}
	if m.diffDividerHit(x, m.height-1) {
		t.Error("the divider claims the footer row")
	}
	// One column off the divider is never a divider.
	if m.diffDividerHit(x-1, tabBarRows+2) || m.diffDividerHit(x+1, tabBarRows+2) {
		t.Error("a column beside the divider is treated as the divider")
	}
}

// THE STORED VALUE IS THE OPERATOR'S INTENT, NOT A SNAPSHOT OF A CLAMPED WIDTH: a wide width set on a big
// terminal must survive a shrink-and-grow cycle (checked through the drawn frame, not the field).
func TestQADiffRailIntentSurvivesAShrinkAndGrow(t *testing.T) {
	m := newDiffRailApp(t, 240, 40)
	m.setDiffPaneW(110)
	if got := contentStartColumn(t, m); got != 110 {
		t.Fatalf("the set width drew %d, want 110", got)
	}
	m.dispatch(tea.WindowSizeMsg{Width: 120, Height: 24})
	if got := contentStartColumn(t, m); got > 120/2 {
		t.Errorf("after the shrink the drawn pane is %d, above half of 120", got)
	}
	m.dispatch(tea.WindowSizeMsg{Width: 240, Height: 40})
	if got := contentStartColumn(t, m); got != 110 {
		t.Errorf("after growing back the drawn pane is %d, want the stored 110", got)
	}
}

// THE CONFIG FILE STILL REJECTS A TYPO IN THE NEW KEY, and accepts the key render writes — through the real
// Load/Save pair rather than the parser alone.
func TestQADiffRailConfigKeyTypoIsRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.FileName)
	cfg := &config.Config{Profiles: map[string]*config.Profile{}, DiffRailWidth: 77}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("a file render() wrote failed to load: %v", err)
	}
	if got.DiffRailWidth != 77 {
		t.Fatalf("DiffRailWidth round-tripped as %d, want 77", got.DiffRailWidth)
	}
	data, _ := os.ReadFile(path)
	bad := strings.Replace(string(data), "diff_rail_width", "diff_rail_wdith", 1)
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(path); err == nil {
		t.Error("a typo'd rail key loaded silently — the parser's unknown-key rejection is the contract")
	}
}
