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
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// categorizeHost is the optional shell capability for opening the assign-or-create modal.
type categorizeHost interface {
	OpenAssignCategory(entityID, entityLabel string, target apiv1.CategoryTargetType)
}

// categoryHost is the optional shell capability for READING groupings, so a pane can nest its rows
// under their category. A separate interface from categorizeHost because a screen may grow one without
// the other, and because the shell's read side is a pure lookup.
type categoryHost interface {
	CategoryOf(target apiv1.CategoryTargetType, entityID string) (id, name string)
	// CategoryGroupsFor is the ORDERED folder list, empty categories included — the GUI builds a group
	// for every category it knows and only then appends Uncategorized, so the TUI must not decide an
	// order of its own.
	CategoryGroupsFor(target apiv1.CategoryTargetType) []screenkit.GroupSpec
}

// categoryManageHost is the optional shell capability for MANAGING a grouping from the pane that shows
// it — the GUI's per-folder rename and delete, reachable from every grouped list rather than from one
// special section.
type categoryManageHost interface {
	OpenRenameCategory(categoryID string)
	OpenDeleteCategory(categoryID string)
}

// The chords that act on a CATEGORY ROW. They reuse the pane's own item keys because they apply to the
// row under the cursor: `e` edits what the cursor is on (a worker, or the grouping when the cursor is
// on a folder) and `x` deletes it. That is how the GUI reads too — the folder row carries the rename
// and delete affordances itself.
const (
	keyRenameCategory = "e"
	keyDeleteCategory = "x"
)

// grouped arranges a finished item list into collapsible category folders.
//
// It is ADDITIVE: with no shell hook, or with no categories, screenkit.GroupItemsByCategory returns
// the list UNCHANGED, so a plane with no categories renders exactly the flat list it always did.
func (m *Model) grouped(items []screenkit.Item, target apiv1.CategoryTargetType) []screenkit.Item {
	host, ok := m.Shell().(categoryHost)
	if !ok || host == nil {
		return items
	}
	groups := host.CategoryGroupsFor(target)
	return screenkit.GroupItemsByCategory(items, groups, func(id string) string {
		catID, _ := host.CategoryOf(target, id)
		return catID
	})
}

// groupRowKey handles the chords that act on a CATEGORY ROW rather than on an item, reporting whether
// it owned the key.
//
// IT RUNS BEFORE THE ITEM CHORDS, because the keys are the SAME ONES: `e` on a folder must rename the
// grouping, not open a worker form against a synthetic id. Returning false for everything else is what
// lets the item ops keep their normal path, and the ones that WOULD be wrong on a folder (publish,
// set-active, version, categorize, the flow editor) are refused with a reason by the callers.
func (m *Model) groupRowKey(kstr string) (tea.Cmd, bool) {
	if !m.isGroupRowSelected() {
		return nil, false
	}
	item, _ := m.ActiveItem()
	catID := screenkit.GroupCategoryID(item.ID)
	if catID == "" {
		return nil, false
	}
	switch kstr {
	case keyRenameCategory, keyDeleteCategory:
		host, ok := m.Shell().(categoryManageHost)
		if !ok || host == nil {
			return m.refuse("grouping management is unavailable in this build"), true
		}
		if kstr == keyRenameCategory {
			host.OpenRenameCategory(catID)
		} else {
			host.OpenDeleteCategory(catID)
		}
		m.notice = ""
		return nil, true
	}
	return nil, false
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
	// A CATEGORY ROW IS NOT AN ITEM. The grouped lists synthesize a parent row per category, and
	// categorizing one would aim a write at an id the server has never heard of.
	if screenkit.IsGroupRow(item.ID) {
		return m.refuse("that is a category row — expand it and pick an item, or press C on an item")
	}
	host, ok := m.Shell().(categorizeHost)
	if !ok || host == nil {
		return m.refuse("categorize is unavailable in this build")
	}
	m.notice = ""
	host.OpenAssignCategory(item.ID, item.Title, target)
	return nil
}

// isGroupRowSelected reports whether the cursor is on a synthesized category row, for the WRITE chords
// that would otherwise aim at a fake id (edit / delete / publish / set-active …).
func (m *Model) isGroupRowSelected() bool {
	item, ok := m.ActiveItem()
	return ok && screenkit.IsGroupRow(item.ID)
}
