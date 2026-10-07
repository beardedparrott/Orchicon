package tui

// project_theme_test.go — THE PALETTE FOLLOWS THE ACTIVE PROJECT (see projecttheme.go for the design).
//
// The operator: "It would be nice to have different colors to distinguish different sessions." These pin
// the contract the acceptance criteria state: binding is deliberate (never a preview side effect), the
// scope change is the one place that applies it (and reaches the composer, not just the frame), a project
// with no binding or no project at all falls back to the persisted default, an unknown palette degrades
// once, an explicit /theme pins the session until the next workspace change, and the new config writer
// never escapes the test sandbox.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// themePlane is a rail with two real projects and no conversations — enough to exercise the scope ->
// palette follower without the conversations machinery the other scope tests need.
func themePlane() *App {
	m := newTestApp()
	m.railProjects = []railProject{
		{ID: "p-1", Name: "Alpha", Status: "active"},
		{ID: "p-2", Name: "Beta", Status: "active"},
	}
	m.projectScope = projectScopeAll
	m.projectScopeChosen = false
	return m
}

// THE LIVE PREVIEW NEVER COMMITS ANYTHING — the second half of the operator's report.
//
// The Themes pane's OnHighlight calls the shell's PreviewTheme as the cursor moves; that path applies the
// palette and touches nothing else. Before the split, the preview went through SetTheme, so merely arrowing
// through the list PERSISTED on every cursor move — leaving the config on whatever row the cursor happened to
// stop at, which the next launch in any unbound project then inherited.
func TestCursorPreviewNeverCommitsATheme(t *testing.T) {
	t.Cleanup(func() { theme.Use(theme.DefaultName) })
	m := themePlane()
	m.profile.Theme = "forest" // the shared default, which a preview must not touch
	m.setProjectScope("p-1")

	// Simulate browsing the Themes pane: the cursor moving applies several palettes in a row, exactly what
	// previewTheme -> applyTheme -> shell.PreviewTheme does.
	for _, name := range []string{"tokyo-night", "catppuccin-latte", "dracula", "forest"} {
		if !m.PreviewTheme(name) {
			t.Fatalf("PreviewTheme(%q) failed", name)
		}
	}
	if len(m.projectThemes) != 0 {
		t.Fatalf("browsing palettes wrote a project binding: %v", m.projectThemes)
	}
	if m.profile.Theme != "forest" {
		t.Fatalf("browsing palettes rewrote the shared default (profile.Theme = %q, want forest)", m.profile.Theme)
	}
	if m.themePinned {
		t.Error("a preview pinned the session — only a commit may pin")
	}

	// THE COMMIT is what writes, and while a project is active it writes to THAT PROJECT.
	if !m.SetTheme("dracula") {
		t.Fatal("SetTheme refused a real palette")
	}
	if m.projectThemes["p-1"] != "dracula" {
		t.Fatalf("the commit did not bind the palette to the active project: %v", m.projectThemes)
	}
	if m.profile.Theme != "forest" {
		t.Fatalf("the commit rewrote the shared default that unbound projects inherit: profile.Theme = %q, "+
			"want forest", m.profile.Theme)
	}
}

// A SCOPE CHANGE APPLIES THE BOUND PALETTE, through the EXISTING theme path — including the composer
// re-pin (criterion 8: that re-pin is the documented half-working failure mode).
func TestScopeChangeAppliesTheProjectBindingAndReachesTheComposer(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() {
		lipgloss.SetColorProfile(termenv.Ascii)
		theme.Use(theme.DefaultName)
	})

	m := themePlane()
	m.setProjectScope("p-1")
	if _, ok := m.BindProjectTheme("tokyo-night"); !ok {
		t.Fatal("bind refused")
	}
	// Park on an unbound scope with a NEUTRAL theme active, so the assertion below cannot pass by
	// coincidence.
	m.setProjectScope("p-2")
	if !m.SetTheme("catppuccin-latte") {
		t.Fatal("SetTheme setup failed")
	}

	m.setProjectScope("p-1")
	if got := theme.Active().Name; got != "tokyo-night" {
		t.Fatalf("switching to the bound project's scope left the palette %q, want tokyo-night", got)
	}
	assertComposerPaintsOnlyTheActivePalette(t, m, "after a scope change applied the project's bound palette")
}

// A PROJECT WITH NO BINDING FALLS BACK TO THE PERSISTED DEFAULT (criterion 6's project half), and SCOPES
// WITH NO PROJECT AT ALL (All projects / No project) do too, with no error.
func TestNoBindingAndNoProjectFallBackToDefault(t *testing.T) {
	t.Cleanup(func() { theme.Use(theme.DefaultName) })
	m := themePlane()
	// The persisted default is "forest", and the ACTIVE palette just diverges from it (no /theme pin in
	// effect) — e.g. a project binding elsewhere left it there a moment ago.
	m.profile.Theme = "forest"

	theme.Use("dracula")
	m.setProjectScope("p-1") // a real project, never bound
	if got := theme.Active().Name; got != "forest" {
		t.Fatalf("an unbound project resolved to %q, want the persisted default forest", got)
	}

	theme.Use("dracula")
	m.setProjectScope(projectScopeAll)
	if got := theme.Active().Name; got != "forest" {
		t.Fatalf("All projects resolved to %q, want the persisted default forest", got)
	}

	theme.Use("dracula")
	m.setProjectScope(unassignedScope)
	if got := theme.Active().Name; got != "forest" {
		t.Fatalf("No project resolved to %q, want the persisted default forest", got)
	}
}

// A PROJECT BOUND TO A PALETTE THIS BUILD DOES NOT KNOW DEGRADES TO THE DEFAULT, and says so ONLY ONCE
// (criterion 7) — re-entering the same project on a later scope change must not repeat the notice.
func TestUnknownBoundPaletteDegradesOnce(t *testing.T) {
	t.Cleanup(func() { theme.Use(theme.DefaultName) })
	m := themePlane()
	m.projectThemes = map[string]string{"p-1": "not-a-real-palette-anymore"}

	m.setProjectScope("p-1")
	if got := theme.Active().Name; got != theme.DefaultName {
		t.Fatalf("an unknown bound palette rendered as %q instead of degrading to the default", got)
	}
	if !strings.Contains(m.dock.Notice, "not-a-real-palette-anymore") {
		t.Fatalf("no degrade notice on first encounter: %q", m.dock.Notice)
	}

	// Leave and come back: the palette resolves the same way, but the notice must not repeat.
	m.setProjectScope("p-2")
	m.dock.SetNotice("")
	m.setProjectScope("p-1")
	if strings.Contains(m.dock.Notice, "not-a-real-palette-anymore") {
		t.Errorf("the degrade notice repeated on a second visit: %q", m.dock.Notice)
	}
}

// A COMMIT WHILE A PROJECT IS ACTIVE IS KEPT FOR THAT PROJECT — the fixed precedence.
//
// The old rule pinned an explicit /theme for the session and let the project's ORIGINAL palette return on the
// next workspace switch, with the choice surviving only in the SHARED default — which is exactly what carried
// it into every other project. Committing now records the project's palette, so the choice outlives the scope
// switch (strictly more than the old pin promised) and touches nothing outside that project.
//
// The pin itself is unchanged: it still holds the palette for the rest of the CURRENT scope, and its release
// at a workspace change is still reported, because a silent release is indistinguishable from a pin that
// never existed.
func TestACommitInAProjectIsKeptForThatProject(t *testing.T) {
	t.Cleanup(func() { theme.Use(theme.DefaultName) })
	m := themePlane()
	m.profile.Theme = "forest" // the shared default, which a project commit must NOT touch
	m.setProjectScope("p-1")
	if _, ok := m.BindProjectTheme("tokyo-night"); !ok {
		t.Fatal("bind refused")
	}
	m.setProjectScope("p-1") // land on the bound palette
	if got := theme.Active().Name; got != "tokyo-night" {
		t.Fatalf("setup: scope did not apply its binding, got %q", got)
	}

	// The operator commits an explicit choice while working in p-1.
	if !m.SetTheme("dracula") {
		t.Fatal("SetTheme failed")
	}
	if got := theme.Active().Name; got != "dracula" {
		t.Fatalf("the commit did not apply: %q", got)
	}
	if m.projectThemes["p-1"] != "dracula" {
		t.Fatalf("the commit did not bind the palette to p-1: %v", m.projectThemes)
	}
	if m.profile.Theme != "forest" {
		t.Fatalf("the commit rewrote the SHARED default (profile.Theme = %q, want forest) — that is the leak "+
			"that carried one project's choice into every other", m.profile.Theme)
	}

	// A workspace change releases the pin (reported, never silent), and the OTHER project resolves to the
	// default the commit left alone.
	m.setProjectScope("p-2")
	if !strings.Contains(m.dock.Notice, "pin") {
		t.Errorf("the pin's release was not reported in the notice: %q", m.dock.Notice)
	}
	if got := theme.Active().Name; got != "forest" {
		t.Fatalf("an untouched project rendered %q after a commit elsewhere, want the shared default forest", got)
	}

	// And coming back, the committed project has its own palette — the promise the old pin made, now kept.
	m.setProjectScope("p-1")
	if got := theme.Active().Name; got != "dracula" {
		t.Fatalf("the committed project did not get its palette back: got %q, want dracula", got)
	}
}

// THE NEW WRITER NEVER ESCAPES THE TEST SANDBOX (criterion 9) — the documented bug was a config writer
// reaching the DEVELOPER's real ~/.orchicon/config through go test. collapsePrefsDisabled is true by
// default for this whole test binary (isolate_test.go), so BindProjectTheme must be a no-op write under
// that default, the same as every other preference writer in prefs.go.
func TestProjectThemeWriterIsSandboxed(t *testing.T) {
	if !collapsePrefsDisabled {
		t.Fatal("fixture: expected collapsePrefsDisabled by default under go test (see isolate_test.go)")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	m := themePlane()
	m.setProjectScope("p-1")
	if _, ok := m.BindProjectTheme("tokyo-night"); !ok {
		t.Fatal("bind refused")
	}
	if _, err := os.Stat(filepath.Join(home, ".orchicon", "config")); err == nil {
		t.Errorf("BindProjectTheme escaped the test sandbox and wrote under HOME (%s)", home)
	}
}

// BINDING PERSISTS ACROSS A RELAUNCH (criterion 3's wiring half; config.go's own tests pin the file
// format). A NEW App over the SAME sandboxed config dir restores the binding, and the launch-directory
// default applies it without any operator action — criterion 1's "each renders in its bound palette on a
// subsequent launch".
func TestProjectThemeBindingSurvivesARelaunch(t *testing.T) {
	path := useTempConfigDir(t)
	t.Cleanup(func() { theme.Use(theme.DefaultName) })

	m := themePlane()
	m.setProjectScope("p-1")
	if _, ok := m.BindProjectTheme("tokyo-night"); !ok {
		t.Fatal("bind refused")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("binding wrote no config file (%s): %v", path, err)
	}

	// Session two: a NEW App over the same sandboxed config dir, with the SAME launch directory default
	// a relaunch would have.
	m2 := themePlane()
	m2.launchDir = "/does/not/matter"
	m2.railProjects[0].Dir = "/does/not/matter" // p-1, by construction order in themePlane
	m2.applyLaunchDirScope()
	if m2.projectScope != "p-1" {
		t.Fatalf("fixture: the launch directory did not resolve to p-1, scope = %q", m2.projectScope)
	}
	if got := theme.Active().Name; got != "tokyo-night" {
		t.Fatalf("a relaunch into the bound project's directory rendered %q, want the persisted tokyo-night", got)
	}
}
