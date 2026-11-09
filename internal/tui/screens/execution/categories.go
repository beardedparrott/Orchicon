package execution

// categories.go — the Categorize chord on the Workers and Workflows panes.

import (
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// categorizeHost is the optional shell capability for opening the assign-or-create modal.
type categorizeHost interface {
	OpenAssignCategory(entityID, entityLabel string, target apiv1.CategoryTargetType)
}

// categoryManageHost is the optional shell capability for MANAGING a grouping from the pane that
// shows it — the GUI's per-folder rename and delete.
type categoryManageHost interface {
	OpenRenameCategory(categoryID string)
	OpenDeleteCategory(categoryID string)
}

// The chords that act on a CATEGORY ROW.
const (
	keyRenameCategory = "e"
	// keyDeleteCategory is the SHARED delete chord, not a copy of its value: a grouping is deleted by
	// the same key that deletes the items inside it.
	keyDeleteCategory = keyDelete
)

// grouped arranges a finished item list into collapsible category folders, using THE RESPONSE'S OWN
// CATEGORIES.
//
// WHY THE RESPONSE AND NOT THE SHELL'S CACHE — two reasons, and the first is the operator's report:
//
//	"None of the GUI categories are coming up for Workers or Workflows in the TUI."
//
// The shell's cache is loaded ONCE at startup, so a grouping created in the other client while the TUI
// is running never appeared. Both list responses have carried their own categories and assignments all
// along (worker/service.go and workflow/service.go enrich them, which is how the GUI's screens get
// them) — and this pane was throwing them away to consult a cache that could be minutes stale.
//
// The second reason is correctness rather than freshness: this runs inside the fetch COMMAND, on a
// goroutine, so reading the shell's cache from here raced the main loop that writes it. A response's own
// data has no such problem.
//
// It is ADDITIVE: with no categories, screenkit.GroupItemsByCategory returns the list UNCHANGED, so a
// plane with no groupings renders exactly the flat list it always did.
func (m *Model) grouped(items []screenkit.Item, cats []*apiv1.Category, assigns []*apiv1.CategoryAssignment) []screenkit.Item {
	if len(cats) == 0 {
		return items
	}
	groups := make([]screenkit.GroupSpec, 0, len(cats))
	for _, c := range cats {
		groups = append(groups, screenkit.GroupSpec{
			ID: c.GetId(), Name: c.GetName(), SortOrder: int(c.GetSortOrder()),
		})
	}
	byEntity := make(map[string]string, len(assigns))
	for _, a := range assigns {
		byEntity[a.GetEntityId()] = a.GetCategoryId()
	}
	return screenkit.GroupItemsByCategory(items, groups, func(id string) string { return byEntity[id] })
}

// groupRowKey handles the chords that act on a CATEGORY ROW rather than on an item, reporting
// whether it owned the key.
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
