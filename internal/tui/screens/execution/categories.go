package execution

// categories.go — the Categorize chord on the Workers and Workflows panes.
//
// The operator: "For categories, we could assign a key to create new category and a key to assign an
// item to a specific category."
//
// The screen owns the SELECTION and the shell owns the category list and the modal, so this file is
// deliberately thin: it resolves which entity is selected and hands the intent over through the same
// optional shell-hook pattern the screen already uses for its other shell-side capabilities
// (`OpenExecutionSession`, `DockError`, …). Duplicating the modal here would mean two implementations
// of the same write, and the shell's copy is the one that also serves the conversations rail.

import (
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// categorizeHost is the optional shell capability for opening the assign-or-create modal.
type categorizeHost interface {
	OpenAssignCategory(entityID, entityLabel string, target apiv1.CategoryTargetType)
}

// categorizeSelected opens the shell's assign modal for the highlighted row.
//
// It refuses LOUDLY rather than quietly doing nothing when there is no selection: a key that silently
// no-ops reads as a broken key, and this screen already has `refuse` for exactly that reason.
func (m *Model) categorizeSelected(target apiv1.CategoryTargetType) tea.Cmd {
	item, ok := m.ActiveItem()
	if !ok || item.ID == "" {
		return m.refuse("select an item first, then C to categorize it")
	}
	host, ok := m.Shell().(categorizeHost)
	if !ok || host == nil {
		return m.refuse("categorize is unavailable in this build")
	}
	m.notice = ""
	host.OpenAssignCategory(item.ID, item.Title, target)
	return nil
}
