package tui

// projecttheme.go — the TUI PALETTE FOLLOWS THE ACTIVE PROJECT, so two `orch` windows sitting in different
// projects look different (the operator: "It would be nice to have different colors to distinguish different
// sessions").
//
// THE TRAP THIS FILE GUARDS. App.SetTheme (slash.go) is the COMMIT: it persists the operator's choice —
// BOUND to the active project when there is one, or written as the top-level default when no project is in
// scope. Calling it on every scope change would overwrite a project's palette with whatever the current scope
// happens to resolve to, every time the operator switched workspace. So this file never calls SetTheme:
// applyScopeTheme calls applyThemeAndRefresh directly (apply only), and the project binding is persisted
// through its OWN read-modify-write (persistProjectThemes), completely separate from the default palette's.
//
// SetTheme ROUTES ITS WRITE THROUGH BindProjectTheme for exactly that reason: a palette chosen while working
// in a project is that project's, and the shared default — which every UNBOUND project inherits — is only
// written from a scope with no workspace of its own.
//
// THE PRECEDENCE DECISION (criterion 5). An explicit /theme PINS the palette for the rest of THIS scope: it
// stays active across anything that does not change the workspace. The next scope change — the rail, /project,
// or the launch-directory default — releases the pin and the new scope's own palette (bound, or the persisted
// default when it has none) takes over. This reads as "a workspace switch is itself the signal that the
// operator is done with the one-off override", which matches what they asked for: palette-per-session, where
// a session IS a project. The alternative — an explicit choice surviving every later workspace switch for the
// rest of the process — was considered and rejected, because it would make exactly ONE early /theme defeat the
// entire feature for the rest of the run. Either direction must never be SILENT, so every place that flips
// themePinned also writes a notice saying so (see applyScopeTheme).
//
// THIS IS A TUI-ONLY PREFERENCE, DELIBERATELY (criterion 10). See config.Config.ProjectThemes's doc comment:
// the palettes here are terminal-validated, not the GUI's CSS tokens, so there is no GUI counterpart and none
// is planned.

import (
	"github.com/beardedparrott/orchicon/internal/tui/config"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// scopeThemeReason explains WHY resolveScopeTheme picked the palette it did, for the notice in
// applyScopeTheme.
type scopeThemeReason int

const (
	scopeThemeNone     scopeThemeReason = iota // All projects / No project: nothing to bind to
	scopeThemeDefault                          // a real project, but no palette bound to it
	scopeThemeBound                            // a real project, bound to a palette this build knows
	scopeThemeDegraded                         // bound to a palette this build does NOT know
)

// loadProjectThemes restores the persisted project->palette bindings into memory, keeping whatever is
// already there (the same keep-and-merge rule loadCollapsedGroups uses, in prefs.go).
//
// It goes through collapsedPrefsPath, the SAME redirect that keeps `go test ./...` from reading or writing
// the developer's own ~/.orchicon/config (see isolate_test.go) — criterion 9.
func (m *App) loadProjectThemes() {
	path := collapsedPrefsPath()
	if path == "" {
		return // no config location (or a test): nothing persisted to restore
	}
	cfg, err := config.Load(path)
	if err != nil {
		return
	}
	if len(cfg.ProjectThemes) == 0 {
		return
	}
	if m.projectThemes == nil {
		m.projectThemes = map[string]string{}
	}
	for id, name := range cfg.ProjectThemes {
		if _, exists := m.projectThemes[id]; !exists {
			m.projectThemes[id] = name
		}
	}
}

// persistProjectThemes writes the in-memory bindings back, read-modify-write through the SAME
// collapsedPrefsPath redirect loadProjectThemes uses — criterion 9 again, for the writer.
//
// It re-derives the WHOLE map from m.projectThemes rather than patching one entry, the same rule
// persistCollapsedGroups follows: the map in memory is the truth for this process, and an unbind has to be
// able to remove a key, not just add one.
func (m *App) persistProjectThemes() {
	path := collapsedPrefsPath()
	if path == "" {
		return // no config location (or a test): nothing to write, and nothing to report
	}
	cfg, err := config.Load(path)
	if err != nil {
		m.dock.SetNotice("project theme (this run only — cannot read " + path + ")")
		return
	}
	cfg.ProjectThemes = map[string]string{}
	for id, name := range m.projectThemes {
		cfg.ProjectThemes[id] = name
	}
	if err := config.Save(path, cfg); err != nil {
		m.dock.SetNotice("project theme (this run only — cannot write " + path + ": " + err.Error() + ")")
	}
}

// scopeLabel names the active project scope for a notice, the same way the rail's own notices do.
func (m *App) scopeLabel() string {
	return projectScopeLabel(m.projectScope, projectScopeOptions(m.railProjects, m.conversations))
}

// resolveScopeTheme picks the palette the active scope resolves to, and says why. unknownName is only set
// for scopeThemeDegraded, naming the palette the project is bound to that this build cannot find.
func (m *App) resolveScopeTheme() (name string, reason scopeThemeReason, unknownName string) {
	def := theme.DefaultName
	if m.profile != nil && m.profile.Theme != "" {
		def = m.profile.Theme
	}
	pid := m.activeProjectID()
	if pid == "" {
		return def, scopeThemeNone, ""
	}
	bound, ok := m.projectThemes[pid]
	if !ok || bound == "" {
		return def, scopeThemeDefault, ""
	}
	if theme.Lookup(bound) == nil {
		return def, scopeThemeDegraded, bound
	}
	return bound, scopeThemeBound, ""
}

// applyScopeTheme resolves and applies the palette for the CURRENT project scope — the one place the rail,
// /project, and the launch-directory default all route through, so none of them can disagree with the
// others about when the palette follows the workspace.
//
// It applies through applyThemeAndRefresh directly (apply, never persist) — see this file's "trap" note —
// so the project's own palette can never clobber the operator's saved default.
//
// announceScope controls whether the scope's OWN label is worth a notice: the rail and /project are a
// deliberate operator action and always say which project they landed on, but the launch-directory default
// is automatic and already silent about the scope itself (applyLaunchDirScope), so it only speaks up here
// when something a notice actually needs to cover happens — the pin releasing, or a degraded palette.
func (m *App) applyScopeTheme(announceScope bool) {
	wasPinned := m.themePinned
	m.themePinned = false
	name, reason, unknownName := m.resolveScopeTheme()
	changed := theme.Active().Name != name
	if changed {
		m.applyThemeAndRefresh(name)
	}

	var notes []string
	if announceScope {
		notes = append(notes, "project: "+m.scopeLabel())
	}
	switch reason {
	case scopeThemeBound:
		if changed || wasPinned {
			note := "theme: " + name + " (bound to this project)"
			if wasPinned {
				note += " — your /theme pin ended at this workspace change"
			}
			notes = append(notes, note)
		}
	case scopeThemeDegraded:
		pid := m.activeProjectID()
		key := pid + "\x00" + unknownName
		if !m.degradedPaletteWarned[key] {
			if m.degradedPaletteWarned == nil {
				m.degradedPaletteWarned = map[string]bool{}
			}
			m.degradedPaletteWarned[key] = true
			notes = append(notes, "theme: "+name+" (this project is bound to \""+unknownName+
				"\", which this build does not know — using the default palette instead)")
		} else if changed || wasPinned {
			notes = append(notes, "theme: "+name+" (default — the project's bound palette is still unknown)")
		}
	case scopeThemeDefault, scopeThemeNone:
		if wasPinned {
			notes = append(notes, "theme: "+name+" (default — your /theme pin ended at this workspace change)")
		} else if changed {
			notes = append(notes, "theme: "+name+" (default palette)")
		}
	}
	if len(notes) == 0 {
		return
	}
	joined := notes[0]
	for _, n := range notes[1:] {
		joined += "  ·  " + n
	}
	m.dock.SetNotice(joined)
}

// BindProjectTheme binds palette `name` to the scope's active project, persisting it to the top-level
// project_themes map (see config.Config.ProjectThemes) WITHOUT touching the persisted DEFAULT palette —
// that split is the whole point of this file's trap note. ok is false when the scope names no project
// (All projects / No project), where there is nothing to bind.
//
// This is the ONLY path that writes project_themes. The Control→Themes pane's cursor PREVIEW (screen.go)
// calls SetTheme, never this — browsing must never silently rebind a project (criterion 2).
func (m *App) BindProjectTheme(name string) (label string, ok bool) {
	pid := m.activeProjectID()
	if pid == "" {
		return "", false
	}
	if m.projectThemes == nil {
		m.projectThemes = map[string]string{}
	}
	m.projectThemes[pid] = name
	m.persistProjectThemes()
	return m.scopeLabel(), true
}

// UnbindProjectTheme removes the scope's active project's binding, if it has one. ok is false when the
// scope names no project, or the project had no binding to remove.
func (m *App) UnbindProjectTheme() (label string, ok bool) {
	pid := m.activeProjectID()
	if pid == "" {
		return "", false
	}
	if _, bound := m.projectThemes[pid]; !bound {
		return m.scopeLabel(), false
	}
	delete(m.projectThemes, pid)
	m.persistProjectThemes()
	return m.scopeLabel(), true
}

// CurrentProjectTheme reports the active scope's project label and whatever palette name is bound to it
// ("" when none). hasProject is false for All projects / No project, where binding is meaningless — the
// Control→Themes pane uses this to decide whether to offer bind/unbind at all.
func (m *App) CurrentProjectTheme() (label, bound string, hasProject bool) {
	pid := m.activeProjectID()
	if pid == "" {
		return "", "", false
	}
	return m.scopeLabel(), m.projectThemes[pid], true
}
