package tui

// railbulk.go — bulk operations on the conversations rail.

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// The rail's item chords.
const (
	conversationBulkDeleteChord = "ctrl+x"
)

// toggleConvMark marks or unmarks the highlighted conversation and advances one row.
func (m *App) toggleConvMark() {
	// A FOLDER ROW marks its members — "space marks what is under the cursor", and for a folder that is
	// everything inside it.
	if f := m.railFolderAt(m.convSel); f != nil {
		if m.convMarked == nil {
			m.convMarked = map[string]bool{}
		}
		ids := m.railRowsInFolder(f.catID)
		all := len(ids) > 0
		for _, id := range ids {
			if !m.convMarked[id] {
				all = false
				break
			}
		}
		for _, id := range ids {
			if all {
				delete(m.convMarked, id)
			} else {
				m.convMarked[id] = true
			}
		}
		return
	}
	idx := m.railConvIndexAt(m.convSel)
	if idx < 0 {
		return
	}
	if m.convMarked == nil {
		m.convMarked = map[string]bool{}
	}
	id := m.conversations[idx].ID
	if m.convMarked[id] {
		delete(m.convMarked, id)
	} else {
		m.convMarked[id] = true
	}
	// Step to the next row, clamped at the end so the last space does not walk off the list. The step
	// walks the VISIBLE rows, so a collapsed folder is skipped rather than entered.
	if m.convSel < len(m.railRows())-1 {
		m.convSel++
		m.railFollowSelection()
	}
}

// convMarkedCount is how many conversations are marked.
func (m *App) convMarkedCount() int { return len(m.convMarked) }

// clearConvMarks drops every mark, reporting whether there was one. The bool is what lets esc clear a
// selection FIRST and keep its established meaning when there is nothing to clear.
func (m *App) clearConvMarks() bool {
	if len(m.convMarked) == 0 {
		return false
	}
	m.convMarked = nil
	return true
}

// pruneConvMarks drops marks for conversations that are no longer in the list.
func (m *App) pruneConvMarks() {
	if len(m.convMarked) == 0 {
		return
	}
	live := make(map[string]bool, len(m.conversations))
	for _, c := range m.conversations {
		live[c.ID] = true
	}
	for id := range m.convMarked {
		if !live[id] {
			delete(m.convMarked, id)
		}
	}
}

// convBulkIDs returns the marked conversation ids when they constitute a BULK SELECTION, and nil
// otherwise — the kit2 rule, including its threshold.
func (m *App) convBulkIDs() []string {
	if len(m.convMarked) < kit2.BulkThreshold {
		return nil
	}
	var ids []string
	for _, c := range m.conversations {
		if m.convMarked[c.ID] {
			ids = append(ids, c.ID)
		}
	}
	return ids
}

// convMarkedIDs is every marked id in list order (used by tests and by the hint's count).
func (m *App) convMarkedIDs() []string {
	var ids []string
	for _, c := range m.conversations {
		if m.convMarked[c.ID] {
			ids = append(ids, c.ID)
		}
	}
	return ids
}

// railSelectionStep maps a vertical key to the rail's SELECTION step — ±1, ±5.
func railSelectionStep(key string) int {
	switch key {
	case "up":
		return -1
	case "down":
		return 1
	case "pgup":
		return -5
	case "pgdown":
		return 5
	}
	return 0
}

// railOwnsVerticalKey moves the rail's selection for a vertical key, reporting whether it owned it.
func (m *App) railOwnsVerticalKey(key string) bool {
	step := railSelectionStep(key)
	if step == 0 {
		return false
	}
	// BOTH the rail being visible AND the rail being SELECTED.
	if !m.railVisible() || m.active != TabAsk || m.askPane != askPaneRail {
		return false
	}
	m.selectRailConversation(step)
	m.refreshStreamStatus()
	return true
}

// railItemKey handles the rail's item chords, reporting whether it owned the key.
func (m *App) railItemKey(k string) (bool, tea.Cmd) {
	if !m.railVisible() || m.active != TabAsk {
		return false, nil
	}
	// A FOLDER ROW MANAGES ITS GROUPING, the same placement as the GUI's FolderItem (onToggle /
	// onStartRename / onDelete) and the same contextual-key rule the Workers and Workflows panes use.
	if f := m.railFolderAt(m.convSel); f != nil {
		switch k {
		case "enter":
			m.toggleConvFolder(f.catID)
			m.refreshStreamStatus()
			return true, nil
		case categoryRenameChord:
			// A PROJECT FOLDER IS NOT A CATEGORY.
			if !f.hasCat() {
				return false, nil
			}
			m.OpenRenameCategory(f.catID)
			m.refreshStreamStatus()
			return true, nil
		case categoryDeleteChord:
			if !f.hasCat() {
				return false, nil
			}
			m.OpenDeleteCategory(f.catID)
			m.refreshStreamStatus()
			return true, nil
		}
		return false, nil
	}
	switch k {
	case conversationRenameChord:
		if idx := m.railConvIndexAt(m.convSel); idx >= 0 {
			c := m.conversations[idx]
			m.openRenameConversation(c.ID, c.Title)
			m.refreshStreamStatus()
			return true, nil
		}
		m.dock.SetError("no conversation selected in the rail")
		return true, nil

	case conversationCategorizeChord:
		// BULK when several are marked, single otherwise — the same "the selection replaces the row"
		// rule every other pane in this client follows (actionsForSelection).
		if ids := m.convBulkIDs(); len(ids) > 0 {
			m.openAssignCategories(ids, "", apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION)
			m.refreshStreamStatus()
			return true, nil
		}
		if idx := m.railConvIndexAt(m.convSel); idx >= 0 {
			c := m.conversations[idx]
			m.openAssignCategory(c.ID, c.Title, apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION)
			m.refreshStreamStatus()
			return true, nil
		}
		m.dock.SetError("no conversation selected in the rail")
		return true, nil

	case conversationBulkDeleteChord:
		if ids := m.convBulkIDs(); len(ids) > 0 {
			m.openBulkConfirm(
				fmt.Sprintf("Delete %d conversations", len(ids)),
				fmt.Sprintf("%d conversations and all their messages will be deleted.\nThis cannot be undone.", len(ids)),
				"delete",
				func() tea.Cmd { return m.bulkDeleteConversations(ids) },
			)
			m.refreshStreamStatus()
			return true, nil
		}
		// NO BULK SELECTION → delete the conversation UNDER THE CURSOR, which is what `x` does on every
		// other list in this client.
		idx := m.railConvIndexAt(m.convSel)
		if idx < 0 {
			m.dock.SetError("no conversation selected in the rail")
			return true, nil
		}
		c := m.conversations[idx]
		m.openBulkConfirm(
			"Delete conversation",
			fmt.Sprintf("“%s” and all its messages will be deleted.\nThis cannot be undone.", c.Title),
			"delete",
			func() tea.Cmd { return m.bulkDeleteConversations([]string{c.ID}) },
		)
		m.refreshStreamStatus()
		return true, nil
	}
	return false, nil
}

// bulkDeleteConversations deletes each conversation, reporting how many failed.
func (m *App) bulkDeleteConversations(ids []string) tea.Cmd {
	if m.chat == nil {
		m.dock.SetError("no chat client")
		return nil
	}
	cmds := make([]tea.Cmd, 0, len(ids))
	for _, id := range ids {
		cmds = append(cmds, m.chat.DeleteConversation(id))
	}
	m.dock.SetNotice(fmt.Sprintf("deleting %d conversations…", len(ids)))
	return tea.Batch(cmds...)
}

// openBulkConfirm raises the shell's confirm dialog for a destructive rail operation.
func (m *App) openBulkConfirm(title, body, ok string, run func() tea.Cmd) {
	d := kit2.Confirm(title, body, ok)
	d.Danger = true
	m.bulkConfirm = d
	m.bulkConfirmRun = run
	m.refreshComposerHint()
}

// bulkConfirmKey drives the confirm dialog. Like every other shell modal it OWNS each key while open,
// so nothing behind it can act on the same keystroke.
func (m *App) bulkConfirmKey(k tea.KeyMsg) (*App, tea.Cmd) {
	if m.bulkConfirm == nil {
		return m, nil
	}
	if k.String() == "ctrl+c" {
		m.quitting = true
		return m, tea.Quit
	}
	choice, closed := m.bulkConfirm.HandleKey(k)
	if !closed {
		return m, nil
	}
	run := m.bulkConfirmRun
	ok := m.bulkConfirm.Buttons[m.bulkConfirm.Sel]
	m.bulkConfirm = nil
	m.bulkConfirmRun = nil
	m.refreshComposerHint()
	if choice == "" || choice != ok || run == nil {
		return m, nil // dismissed
	}
	return m, run()
}

// bulkConfirmView composes the confirm dialog over the base view.
func (m *App) bulkConfirmView(base string, w, h int) string {
	if m.bulkConfirm == nil {
		return base
	}
	bw := m.modalWidth()
	if bw > w-2 {
		bw = w - 2
	}
	return m.overlayCentered(base, m.bulkConfirm.Box(bw, 9))
}

// railHintLine is the rail's chord list for the COMPOSER's affordance row.
func (m *App) railHintLine() string {
	if !m.railVisible() {
		return ""
	}
	parts := []string{"space: mark"}
	if n := m.convMarkedCount(); n > 0 {
		parts = []string{fmt.Sprintf("%d marked", n)}
	}
	parts = append(parts,
		conversationRenameChord+": rename",
		conversationCategorizeChord+": categorize",
	)
	// A FOLDER ROW answers its own keys, and the composer is where they are advertised (the rail is
	// where the operator looks, the composer is where the keys live).
	if f := m.railFolderAt(m.convSel); f != nil {
		if f.hasCat() {
			parts = append(parts, "enter: collapse/expand", categoryRenameChord+": rename group", categoryDeleteChord+": delete group")
		} else {
			// A PROJECT FOLDER: the arrow and the group mark, but nothing to rename — see railItemKey.
			parts = append(parts, "enter: collapse/expand", "space: mark every chat in it")
		}
	}
	if n := m.convMarkedCount(); n >= kit2.BulkThreshold {
		parts = append(parts, conversationBulkDeleteChord+": delete "+fmt.Sprintf("%d", n))
	}
	if m.convMarkedCount() > 0 {
		parts = append(parts, "esc: clear")
	}
	return strings.Join(parts, " · ")
}
