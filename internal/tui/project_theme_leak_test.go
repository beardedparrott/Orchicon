package tui

// project_theme_leak_test.go — A THEME CHANGE WHILE IN A PROJECT BELONGS TO THAT PROJECT.
//
// The operator, trying the shipped project-scoped-themes feature:
//
//	"I opened orch in the ai-tools directory, changed the theme, then opened orch in the Orchicon
//	 directory and the theme changed. It didn't stay the same."
//
// WHAT WAS HAPPENING. `SetTheme` (slash.go) persists the choice to the config's TOP-LEVEL `theme`, which
// cmd/orch/main.go:271 loads into the launching profile (`p.Theme = cfg.Theme`). `resolveScopeTheme`
// (projecttheme.go) falls back to that same profile value for ANY project with no binding of its own. So
// changing the theme in ai-tools rewrote the ONE value that every unbound project inherits, and opening orch
// in Orchicon — a project nobody had bound — resolved straight to it. The binding map was never consulted,
// because a plain theme change never wrote one.
//
// The project-scoped behaviour was therefore only reachable through the Themes pane's explicit bind key;
// the natural gesture ("pick a theme while I'm working in this project") did the global thing. These tests
// pin the gesture the operator actually used.
//
// THE THREE SESSIONS ARE THE CONTRACT: a change in one project must not follow into another, and must still
// be remembered when that project is opened again. A fix that merely stopped persisting would pass the
// second assertion and fail the third.

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/config"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// themeLeakPlane is two real projects with real directories, so applyLaunchDirScope can resolve either —
// the shape the operator's two sessions had.
func themeLeakPlane() *App {
	m := themePlane()
	m.railProjects = []railProject{
		{ID: "p-ai", Name: "ai-tools", Status: "active", Dir: "/home/me/ai-tools"},
		{ID: "p-orch", Name: "Orchicon", Status: "active", Dir: "/home/me/projects/Orchicon"},
	}
	m.projectScope = projectScopeAll
	m.projectScopeChosen = false
	return m
}

// launchInto models a FRESH PROCESS opened in dir: the palette package is reset to the build default (a new
// process has no in-memory palette), the profile's palette is taken from the persisted config exactly as
// cmd/orch/main.go does at launch, and the launch-directory default is applied once the project list lands.
//
// It is a helper rather than three inlined copies because the three sessions below must differ ONLY in
// which directory they start in.
func launchInto(t *testing.T, path, dir string) *App {
	t.Helper()
	theme.Use(theme.DefaultName)
	m := themeLeakPlane()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload the config a previous session wrote: %v", err)
	}
	if cfg.Theme != "" {
		m.profile.Theme = cfg.Theme // cmd/orch/main.go:271
	}
	m.launchDir = dir
	m.applyLaunchDirScope()
	return m
}

// THE REPORTED BUG, in three sessions.
func TestAThemeChangeInOneProjectDoesNotFollowIntoAnother(t *testing.T) {
	path := useTempConfigDir(t)
	t.Cleanup(func() { theme.Use(theme.DefaultName) })

	// SESSION 1 — orch in ai-tools; the operator picks a theme.
	m1 := themeLeakPlane()
	m1.launchDir = "/home/me/ai-tools"
	m1.applyLaunchDirScope()
	if m1.projectScope != "p-ai" {
		t.Fatalf("fixture: the launch directory did not resolve to p-ai (scope=%q)", m1.projectScope)
	}
	if !m1.SetTheme("dracula") {
		t.Fatal("SetTheme refused a real palette")
	}

	// THE WRITE LANDED ON THE PROJECT, NOT ON THE SHARED DEFAULT. Asserted on the FILE rather than inferred
	// from the session below, because writing BOTH is the plausible wrong fix: the shared default is the one
	// value every unbound project inherits, so touching it is the leak itself.
	cfg1, err := config.Load(path)
	if err != nil {
		t.Fatalf("read the config the commit wrote: %v", err)
	}
	if cfg1.ProjectThemes["p-ai"] != "dracula" {
		t.Errorf("the commit did not bind dracula to p-ai: %v", cfg1.ProjectThemes)
	}
	if cfg1.Theme != "" {
		t.Errorf("the commit ALSO wrote the shared default (theme = %q). That single value is what every "+
			"project without a palette of its own inherits, so writing it here IS the leak", cfg1.Theme)
	}

	// SESSION 2 — orch in Orchicon. NOTHING here was changed, so nothing may have changed.
	m2 := launchInto(t, path, "/home/me/projects/Orchicon")
	if m2.projectScope != "p-orch" {
		t.Fatalf("fixture: the launch directory did not resolve to p-orch (scope=%q)", m2.projectScope)
	}
	if got := theme.Active().Name; got != theme.DefaultName {
		t.Errorf("AI-TOOLS' THEME FOLLOWED INTO ORCHICON: opening orch in an untouched project rendered %q, "+
			"want the untouched default %q — the change belongs to ai-tools, not to every project that "+
			"has no palette of its own", got, theme.DefaultName)
	}

	// SESSION 3 — back to ai-tools. The choice must still be there: the fix for the leak is to SCOPE the
	// change, never to stop remembering it.
	m3 := launchInto(t, path, "/home/me/ai-tools")
	if m3.projectScope != "p-ai" {
		t.Fatalf("fixture: the launch directory did not resolve to p-ai on the return trip (scope=%q)", m3.projectScope)
	}
	if got := theme.Active().Name; got != "dracula" {
		t.Errorf("the theme chosen in ai-tools was not remembered when ai-tools was opened again: got %q, "+
			"want dracula — scoping a change must not lose it", got)
	}
}

// AND THE OTHER DIRECTION: a project that HAS been given its own palette keeps it, whatever a later session
// does elsewhere. This is the half that already worked through the explicit bind key, asserted here through
// the same launch path so the two gestures cannot diverge.
//
// A project rebound to a different palette must also win over the config's default — otherwise "this
// project's palette" is only ever the first one ever chosen.
func TestAProjectKeepsItsOwnPaletteAcrossSessions(t *testing.T) {
	path := useTempConfigDir(t)
	t.Cleanup(func() { theme.Use(theme.DefaultName) })

	m1 := themeLeakPlane()
	m1.launchDir = "/home/me/ai-tools"
	m1.applyLaunchDirScope()
	if !m1.SetTheme("dracula") {
		t.Fatal("SetTheme refused")
	}
	// The operator changes their mind while still in ai-tools.
	if !m1.SetTheme("gruvbox-dark") {
		t.Fatal("SetTheme refused")
	}

	m2 := launchInto(t, path, "/home/me/ai-tools")
	if m2.projectScope != "p-ai" {
		t.Fatalf("fixture: the launch directory did not resolve to p-ai (scope=%q)", m2.projectScope)
	}
	if got := theme.Active().Name; got != "gruvbox-dark" {
		t.Errorf("ai-tools rendered %q, want the LAST palette chosen there (gruvbox-dark) — a project's "+
			"palette is the one it was left with, not the first one ever set", got)
	}

	// And an unbound project still resolves to the build default, not to ai-tools' choice.
	m3 := launchInto(t, path, "/home/me/projects/Orchicon")
	if m3.projectScope != "p-orch" {
		t.Fatalf("fixture: the launch directory did not resolve to p-orch (scope=%q)", m3.projectScope)
	}
	if got := theme.Active().Name; got != theme.DefaultName {
		t.Errorf("the untouched project rendered %q, want the default %q", got, theme.DefaultName)
	}
}
