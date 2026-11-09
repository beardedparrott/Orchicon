package tui

// projectselect.go — SET THE WORKSPACE WITHOUT OPENING THE PICKER.

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// selectProjectByName switches the workspace to the project whose NAME or id matches want, and
// reports whether it found one. EXACT-ON-CASE-INSENSITIVE FIRST, SUBSTRING ONLY AS A FALLBACK.
func (m *App) selectProjectByName(want string) bool {
	name := strings.TrimSpace(want)
	if name == "" {
		return false
	}
	// Exact, name first then id. Both compared case-insensitively: the create form lets a person type a name in
	// whatever case they like, and typing "orchicon" for a project called "Orchicon" is not an error.
	for _, p := range m.railProjects {
		if strings.EqualFold(strings.TrimSpace(p.Name), name) || strings.EqualFold(p.ID, name) {
			m.setProjectScope(p.ID)
			return true
		}
	}
	// Substring, but ONLY when it is unambiguous. Two matches is not a decision.
	var found string
	for _, p := range m.railProjects {
		if strings.Contains(strings.ToLower(p.Name), strings.ToLower(name)) {
			if found != "" {
				return false // ambiguous — leave the scope alone rather than guess
			}
			found = p.ID
		}
	}
	if found == "" {
		return false
	}
	m.setProjectScope(found)
	return true
}

// applyCreatedProjectScope makes a just-created project the workspace, and refreshes the rail's
// project list.
func (m *App) applyCreatedProjectScope(msg launchCreatedMsg) tea.Cmd {
	name := strings.TrimSpace(msg.name)
	if name == "" {
		// No name recorded (a form submitted without one). The ID is still selectable, and if the list has
		// not caught up yet the fallback is simply that the scope stays where it was.
		if msg.projectID == "" {
			return nil
		}
		name = msg.projectID
	}
	m.pendingProjectName = name
	// If the list already holds it (a fast reload from a previous action), apply now so the scope is correct
	// before the reload returns — and clear the park, since it has been consumed.
	if m.selectProjectByName(name) {
		m.pendingProjectName = ""
	}
	return m.loadRailProjects()
}
