package tui

// THIS IS A TUI-ONLY PREFERENCE, DELIBERATELY (criterion 10).

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

// loadProjectThemes restores the persisted project->palette bindings into memory, keeping whatever
// is already there (the same keep-and-merge rule loadCollapsedGroups uses, in prefs.go).
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

// applyScopeTheme resolves and applies the palette for the CURRENT project scope — the one place
// the rail, /project, and the launch-directory default all route through.
func (m *App) applyScopeTheme(announceScope bool, extra ...string) {
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
	notes = append(notes, extra...)
	if len(notes) == 0 {
		return
	}
	joined := notes[0]
	for _, n := range notes[1:] {
		joined += "  ·  " + n
	}
	m.dock.SetNotice(joined)
}

// This is the ONLY path that writes project_themes.
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

// CurrentProjectTheme reports the active scope's project label and whatever palette name is bound
// to it ("" when none).
func (m *App) CurrentProjectTheme() (label, bound string, hasProject bool) {
	pid := m.activeProjectID()
	if pid == "" {
		return "", "", false
	}
	return m.scopeLabel(), m.projectThemes[pid], true
}
