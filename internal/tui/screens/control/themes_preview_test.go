package control

// themes_preview_test.go — MOVING THROUGH THE THEMES APPLIES THEM.
//
// The operator: "when selecting themes, we should auto switch to the theme as the user is moving through
// them with the arrow keys or clicking on them as opposed to having to hit enter to select."
//
// Driven through the SCREEN'S REAL Update with real key messages, because the whole risk of this feature is
// whether the gesture actually reaches the hook: asserting previewTheme() directly would prove the helper
// works and prove nothing about the arrow key (the gap this codebase has been bitten by repeatedly).

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// recordingShell captures what the screen pushes at the dock, which is the only observable the operator
// has for "a theme was applied" (applyTheme's own notice) as opposed to "the palette changed".
type recordingShell struct{ notices []string }

func (s *recordingShell) DockError(string)    {}
func (s *recordingShell) DockNotice(m string) { s.notices = append(s.notices, m) }

// themesModel loads the real Themes pane and leaves the cursor on the first row (the DARK heading).
func themesModel(t *testing.T) (*Model, *recordingShell) {
	t.Helper()
	t.Cleanup(func() { theme.Use(theme.DefaultName) })
	theme.Use("dark")

	m := New(nil, nil)
	sh := &recordingShell{}
	m.SetShell(sh)
	if !m.SelectSource("themes") {
		t.Fatal("fixture: themes is not a registered source")
	}
	items, _, err := m.fetchThemes(context.Background(), "")
	if err != nil {
		t.Fatalf("fetchThemes: %v", err)
	}
	if !m.LoadItems("themes", items, "") {
		t.Fatal("fixture: could not load the themes rows")
	}
	return m, sh
}

// themeRowIDs reads the pane's rows in order, so a test can aim at a palette rather than a heading.
func themeRowIDs(t *testing.T, m *Model) []string {
	t.Helper()
	items, _, _ := m.fetchThemes(context.Background(), "")
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	if len(ids) == 0 {
		t.Fatal("fixture: the themes pane is empty")
	}
	return ids
}

// firstInactive returns the first palette row that is not the active one.
func firstInactive(t *testing.T, m *Model) string {
	t.Helper()
	for _, id := range themeRowIDs(t, m) {
		if id != "" && id != theme.Active().Name {
			return id
		}
	}
	t.Fatal("fixture: every palette row is the active one")
	return ""
}

// ARROWING DOWN ONTO A THEME APPLIES IT, with no Enter anywhere.
func TestArrowMovingThroughThemesAppliesThem(t *testing.T) {
	m, _ := themesModel(t)
	ids := themeRowIDs(t, m)
	if ids[0] != "" {
		t.Fatalf("fixture: the first row should be a section heading, got %q", ids[0])
	}
	target := firstInactive(t, m)

	// Step down one row at a time until the cursor is on the target palette — which is the operator's
	// gesture, and must have applied it without Enter.
	//
	// The cursor's row is read from ActiveItem(), NOT from DetailID: the detail is filled by a command this
	// test does not run, so DetailID would stay empty and the loop would never find its row.
	for i := 0; i < len(ids)+2; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyDown})
		if it, ok := m.Base.ActiveItem(); ok && it.ID == target {
			break
		}
	}
	it, ok := m.Base.ActiveItem()
	if !ok || it.ID != target {
		t.Fatalf("fixture: the cursor never landed on %q (it is on %+v)", target, it)
	}
	if got := theme.Active().Name; got != target {
		t.Fatalf("after arrowing onto %q the active theme is %q — moving through the list must apply the "+
			"palette, not merely select it for a later Enter", target, got)
	}
}

// STEPPING ONTO THE ALREADY-ACTIVE THEME IS NOT A CHANGE, and it must not be reported as one: the panel
// writes a notice on every real apply, so a notice here would be the operator's own flicker.
func TestSteppingOverTheActiveThemeReportsNoChange(t *testing.T) {
	m, sh := themesModel(t)
	active := theme.Active().Name

	// Seat the cursor ON the active palette.
	if !m.SelectItem("themes", active) {
		t.Fatalf("fixture: the active palette %q has no row", active)
	}
	sh.notices = nil

	// Step off it and back, and over it repeatedly — the gesture that would re-apply and re-notice.
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})

	for _, n := range sh.notices {
		if strings.Contains(n, "theme: "+active) {
			t.Errorf("stepping onto the ACTIVE theme wrote %q — a change reported where nothing changed", n)
		}
	}
	if got := theme.Active().Name; got != active {
		t.Errorf("walking over the active theme left the palette as %q, want %q", got, active)
	}
}

// A SECTION HEADING IS NOT A THEME: the cursor moves onto DARK / LIGHT and the palette must not change,
// and the active theme must never become something that is not a palette at all.
func TestMovingOntoASectionHeadingDoesNotApplyATheme(t *testing.T) {
	m, _ := themesModel(t)
	target := firstInactive(t, m)
	// Apply a known palette first, so a heading that wrongly applied something would be visible.
	if !m.SelectItem("themes", target) {
		t.Fatalf("could not select %q", target)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	applied := theme.Active().Name
	if applied == "" {
		t.Fatal("fixture: nothing applied")
	}

	// Walk the whole pane, including the LIGHT section's heading.
	for i := 0; i < len(themeRowIDs(t, m))+2; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyDown})
		if got := theme.Active().Name; theme.Lookup(got) == nil {
			t.Fatalf("step %d left the active theme as %q, which is not a palette", i, got)
		}
	}
}

// THE HOOK IS SCOPED TO THE THEMES PANE. It lives on the shared Base, so every source on this screen fires
// it — and moving the cursor over a SECRET must not repaint the whole client with a palette.
func TestMovingOverAnotherSourceDoesNotApplyATheme(t *testing.T) {
	m, _ := themesModel(t)
	before := theme.Active().Name
	if !m.SelectSource("secrets") {
		t.Skip("no secrets source on this screen")
	}
	m.Base.LoadItems("secrets", []kit2.Item{
		{ID: "s1", Title: "FIRST_SECRET"},
		{ID: "s2", Title: "SECOND_SECRET"},
	}, "")
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if got := theme.Active().Name; got != before {
		t.Errorf("moving through the secrets list changed the theme to %q — the preview must be scoped to "+
			"the themes pane", got)
	}
}

// ENTER STILL APPLIES (the commit gesture) and reconciles the pane, so the live preview did not replace it
// and the active marker can still move.
func TestEnterStillAppliesATheme(t *testing.T) {
	m, _ := themesModel(t)
	target := firstInactive(t, m)
	if !m.SelectItem("themes", target) {
		t.Fatalf("could not select %q", target)
	}
	// Selecting MOVES THE CURSOR, which now previews — so re-seat the palette to prove Enter alone does it.
	theme.Use("dark")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := theme.Active().Name; got != target {
		t.Fatalf("Enter did not apply %q (active is %q)", target, got)
	}
	if cmd == nil {
		t.Error("applying a theme returned no reconcile command, so the pane's active marker cannot move")
	}
}
