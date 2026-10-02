package diffs

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// wheelUpAt / wheelDownAt build the wheel events handleMouse switches on.
func wheelUpAt(x, y int) tea.MouseMsg {
	return tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress}
}

func wheelDownAt(x, y int) tea.MouseMsg {
	return tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress}
}

// longWrapRow is a single row long enough to wrap across many physical lines —
// the fixture for the "scroll state outliving its content" cases below.
func longWrapRow() Row {
	long := strings.Repeat("abcdefghij", 60)
	return Row{
		LineNoOld: 1, LineNoNew: 1, HasOld: true, HasNew: true,
		Sign: " ", OldText: long, NewText: long, Kind: KindCtx,
	}
}

// wrapFixtureGroups returns enough distinct files that the tree/timeline lists
// overflow a small viewport.
func wrapFixtureGroups(t *testing.T) []FileGroup {
	t.Helper()
	vecs := vecByName(t)
	names := []string{
		"create-new-file", "modify-two-lines", "delete-file", "append-lines",
		"no-trailing-newline", "multi-hunk-three-context",
	}
	edits := make([]*apiv1.FileEdit, 0, len(names))
	for i, n := range names {
		edits = append(edits, makeEdit(vecs[n], i+1))
	}
	return GroupByFile(edits)
}

// bodyIsBlank reports whether the ACTIVE tab's body renders no content at all —
// the failure mode a stale scroll produces once the list/index tabs share the
// Diff tab's viewport.
func bodyIsBlank(m *Model) bool {
	var body string
	switch m.Tab {
	case TabDiff:
		body = m.diffBody()
	case TabTree:
		body = m.treeBody()
	case TabTimeline:
		body = m.timelineBody()
	}
	return strings.TrimSpace(ansi.Strip(body)) == ""
}

// TestSetTabClampsStaleScroll pins the regression the shared viewport introduces:
// scrolling a long Diff and then switching to Tree/Timeline must NOT leave the
// (much shorter) list scrolled past its end and render a blank body. The shell's
// restoreDiffPaneState calls SetTab, so this is the path a pane REOPEN takes.
func TestSetTabClampsStaleScroll(t *testing.T) {
	m := NewModel(nil, nil)
	m.Width, m.Height = 48, 6
	m.groups = wrapFixtureGroups(t)
	m.rows = []Row{longWrapRow()}
	m.SelectedPath = m.groups[0].Path
	m.Tab = TabDiff

	m.scroll = m.maxScroll()
	if m.scroll == 0 {
		t.Fatalf("the diff fixture did not overflow (maxScroll 0)")
	}
	for _, tab := range []Tab{TabTree, TabTimeline} {
		m.Tab = TabDiff
		m.scroll = m.maxScroll()
		m.SetTab(tab)
		// The list's extent is much smaller than the diff's, so the clamp is
		// what guarantees a visible page — assert the INVARIANT (within the
		// active tab's extent), not a particular clamp target.
		if m.scroll > m.maxScroll() {
			t.Errorf("SetTab(%s) left scroll=%d past maxScroll=%d", tab, m.scroll, m.maxScroll())
		}
		if bodyIsBlank(m) {
			t.Errorf("SetTab(%s) rendered a BLANK body — the diff scroll leaked into the list", tab)
		}
	}
}

// TestClickTabClampsStaleScroll is the same guarantee for the MOUSE path: a click
// on a tab label must clamp too (clickTab used to assign m.Tab directly, a
// separate path from the `h`/`l` keys which did clamp).
func TestClickTabClampsStaleScroll(t *testing.T) {
	m := NewModel(nil, nil)
	m.Width, m.Height = 48, 6
	m.groups = wrapFixtureGroups(t)
	m.rows = []Row{longWrapRow()}
	m.SelectedPath = m.groups[0].Path
	m.Tab = TabDiff
	m.scroll = m.maxScroll()

	// terminal X 10 → contentX 9 → the "tree" tab label's hit region.
	m.click(10, paneTopRow)
	if m.Tab != TabTree {
		t.Fatalf("tab click did not switch to tree (got %s)", m.Tab)
	}
	if m.scroll > m.maxScroll() {
		t.Errorf("mouse tab switch left scroll=%d past the list's maxScroll=%d", m.scroll, m.maxScroll())
	}
	if bodyIsBlank(m) {
		t.Errorf("mouse tab switch rendered a BLANK tree body (scroll=%d, viewH=%d)", m.scroll, m.viewHeight())
	}
	// And the body row under the cursor must resolve to the file the viewport is
	// showing there — group[scroll+bodyRow], the viewport-aware mapping. (Capture
	// the offset BEFORE the click: a row click selects and re-focuses the Diff
	// tab, which resets the scroll.)
	shown := m.scroll
	m.click(5, paneBodyRow)
	if want := m.groups[shown].Path; m.SelectedPath != want {
		t.Errorf("click on body row 0 selected %q, want %q (the row the viewport shows)", m.SelectedPath, want)
	}
}

// TestResizeClampsStaleScroll pins that a widening resize (which reduces the
// wrapped line count) re-clamps: otherwise the pane keeps its old scroll and
// renders a blank body at the new width.
func TestResizeClampsStaleScroll(t *testing.T) {
	m := NewModel(nil, nil)
	m.Width, m.Height = 48, 8
	m.rows = []Row{longWrapRow()}
	m.Tab = TabDiff
	m.scroll = m.maxScroll()
	if m.scroll == 0 {
		t.Fatalf("the narrow pane did not overflow")
	}
	m.SetSize(240, 8)
	if m.scroll > m.maxScroll() {
		t.Errorf("after widening, scroll=%d exceeds maxScroll=%d", m.scroll, m.maxScroll())
	}
	if bodyIsBlank(m) {
		t.Errorf("widening resize rendered a BLANK body (scroll=%d, maxScroll=%d)", m.scroll, m.maxScroll())
	}
}

// TestFetchDoneClampsStaleScroll pins that a fetch which replaces the rows with a
// SHORTER diff re-clamps the scroll, so a new owner (or a file whose latest edit
// shrank) cannot leave the viewport sliced past its end.
func TestFetchDoneClampsStaleScroll(t *testing.T) {
	vecs := vecByName(t)
	m := NewModel(nil, nil)
	m.Width, m.Height = 48, 8
	m.rows = []Row{longWrapRow()}
	m.Tab = TabDiff
	m.SelectedPath = vecs["modify-two-lines"].Path
	m.scroll = m.maxScroll()
	if m.scroll == 0 {
		t.Fatalf("the long diff did not overflow")
	}

	m.Update(FetchDoneMsg{Snapshot: &Snapshot{Edits: []*apiv1.FileEdit{makeEdit(vecs["modify-two-lines"], 1)}}})
	if len(m.rows) == 0 {
		t.Fatalf("fetch produced no rows")
	}
	if m.scroll > m.maxScroll() {
		t.Errorf("after the fetch, scroll=%d exceeds maxScroll=%d", m.scroll, m.maxScroll())
	}
	if bodyIsBlank(m) {
		t.Errorf("a shrinking fetch rendered a BLANK body (scroll=%d, maxScroll=%d)", m.scroll, m.maxScroll())
	}
}

// TestEveryViewportPathClampsScroll is the guard against the NEXT stale-scroll
// path: it drives each state change that can alter the rendered line count and
// asserts the scroll stays within the ACTIVE tab's extent, so a new caller cannot
// reintroduce the blank-body regression unnoticed.
func TestEveryViewportPathClampsScroll(t *testing.T) {
	m := NewModel(nil, nil)
	m.Width, m.Height = 48, 6
	m.groups = wrapFixtureGroups(t)
	m.rows = []Row{longWrapRow()}
	m.SelectedPath = m.groups[0].Path
	m.Tab = TabDiff

	steps := []struct {
		name string
		do   func()
	}{
		{"SetTab(tree)", func() { m.SetTab(TabTree) }},
		{"SetTab(timeline)", func() { m.SetTab(TabTimeline) }},
		{"SetTab(diff)", func() { m.SetTab(TabDiff) }},
		{"clickTab(tree)", func() { m.click(10, paneTopRow) }},
		{"clickTab(timeline)", func() { m.click(18, paneTopRow) }},
		{"key l", func() { m.SetTab(TabDiff); m.handleKey(keyMsg("l")) }},
		{"key h", func() { m.SetTab(TabDiff); m.handleKey(keyMsg("h")) }},
		{"SetSize(narrow)", func() { m.SetTab(TabDiff); m.SetSize(48, 6) }},
		{"SetSize(wide)", func() { m.SetSize(200, 40) }},
		{"wheel up", func() { m.handleMouse(wheelUpAt(5, paneBodyRow)) }},
		{"wheel down", func() { m.handleMouse(wheelDownAt(5, paneBodyRow)) }},
	}
	for _, s := range steps {
		m.Tab = TabDiff
		m.scroll = m.maxScroll() // a full-scrolled Diff, the risky starting state
		s.do()
		if m.scroll > m.maxScroll() {
			t.Errorf("%s: scroll=%d exceeds maxScroll=%d", s.name, m.scroll, m.maxScroll())
		}
		if m.scroll < 0 {
			t.Errorf("%s: scroll=%d is negative", s.name, m.scroll)
		}
		if bodyIsBlank(m) {
			t.Errorf("%s: rendered a BLANK body (tab=%s scroll=%d maxScroll=%d)",
				s.name, m.Tab, m.scroll, m.maxScroll())
		}
	}
}

// TestSelectPathClampsStaleScroll pins the row-click path: selecting a file whose
// diff is SHORTER than the current scroll must reach a visible body (SelectPath
// resets the scroll, and this asserts the reset is what the viewport reads).
func TestSelectPathClampsStaleScroll(t *testing.T) {
	vecs := vecByName(t)
	m := NewModel(nil, nil)
	m.Width, m.Height = 48, 8
	m.groups = GroupByFile([]*apiv1.FileEdit{
		makeEdit(vecs["modify-two-lines"], 1),
		makeEdit(vecs["create-new-file"], 2),
	})
	m.SelectedPath = m.groups[0].Path
	m.rows = []Row{longWrapRow()}
	m.Tab = TabDiff
	m.scroll = m.maxScroll()

	m.SelectPath(m.groups[1].Path)
	if m.scroll != 0 {
		t.Errorf("SelectPath left scroll=%d, want 0", m.scroll)
	}
	if bodyIsBlank(m) {
		t.Errorf("SelectPath rendered a BLANK body")
	}
}
