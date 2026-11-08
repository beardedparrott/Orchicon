package work

// projectfilter.go — THE WORK ITEMS PANE FOLLOWS THE ACTIVE WORKSPACE.
//
// The operator: "When I am viewing work items in the TUI, it should be filtered by current project selected."
//
// WHY THIS IS NOT THE PROJECTS PANE'S JOB. The Work screen lists four sources (projects, work items, runtime
// images, steps) and only the WORK ITEMS list is project-scoped data — there are 228 work items across the
// tenant and they belong to different projects, so a list spanning all of them is a list of mostly-irrelevant
// rows. The Projects pane is the opposite: it is the thing you use to see every project, so filtering IT by
// project would make the picker unable to show you anything else.
//
// THE SHELL PUSHES THE SCOPE IN (SetActiveProject). The screen does not read the shell's projectScope directly,
// because a screen must not know the shell's state model — the shell already injects its list-share preference
// and its shell reference the same way (see App.rebindScreens), and this follows that idiom. A screen that
// reached into the App would also be untestable without constructing one.

import (
	tea "github.com/charmbracelet/bubbletea"
)

// SetActiveProject narrows the WORK ITEMS pane to projectID. An empty id clears the filter.
//
// IT RELOADS WHEN THE FILTER ACTUALLY CHANGES, and reloading is the whole point: the fetch already happened
// under the old filter, so the rows on screen describe a different project. A no-op on an unchanged filter is
// not an optimisation, it is what stops every scope re-render (which happens on each theme and label update)
// from re-fetching the list.
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
