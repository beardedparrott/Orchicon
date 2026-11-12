package work

// projectfilter.go — THE WORK ITEMS PANE FOLLOWS THE ACTIVE WORKSPACE.

import (
	tea "github.com/charmbracelet/bubbletea"
)

// SetActiveProject narrows the WORK ITEMS pane to projectID.
func (m *Model) SetActiveProject(projectID string) tea.Cmd {
	m.viewMu.Lock()
	changed := m.projectFilter != projectID
	m.projectFilter = projectID
	m.viewMu.Unlock()
	if !changed {
		return nil
	}
	return m.Base.Refresh(srcWorkItems)
}

// projectScopeFilter returns the current filter value, safe to read from the fetch closure (off the update loop).
func (m *Model) projectScopeFilter() string {
	m.viewMu.Lock()
	defer m.viewMu.Unlock()
	return m.projectFilter
}

// ProjectFilter reports the active filter for tests and for anything that needs to name it ("" = unfiltered).
func (m *Model) ProjectFilter() string { return m.projectScopeFilter() }
