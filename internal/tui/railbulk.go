package tui

// railbulk.go — bulk operations on the conversations rail.
//
// The operator: "We need to add bulk operations to conversations like we do with the other items in
// orch. Spacebar selects, then we should be able to bulk delete or bulk assign to a category".
//
// WHAT "LIKE THE OTHER ITEMS" MEANS, CONCRETELY. The rest of this client gets its multi-select from
// kit2 (Base/Table): SPACE marks the cursor row and advances one, ESC clears the marks, and an
// operation applies to the whole selection once MORE THAN ONE row is marked — one marked row is not a
// selection, it is the row the cursor is on, and offering "delete 1 conversation" beside "delete"
// would be a slower way to say the same thing (kit2.BulkThreshold). The rail is not a kit2 table (it
// is the shell's own list of chat.Conversation), so the rule is re-implemented here rather than
// borrowed — but it is the SAME rule, including the threshold, which is read from kit2 so the two
// cannot drift.
//
// THE RAIL'S KEYS ARE DRIVEN FROM THE COMPOSER, which is the one constraint that makes this surface
// different. With an empty box the arrows move the rail and a bare letter would be untypeable as the
// first character of a message, so the item actions are MODIFIER chords: ctrl+n renames, ctrl+t
// categorizes, and ctrl+x is the bulk delete. (Space is not a letter, so it keeps the letter-free
// mark gesture the rest of the app uses.)
//
// SPACE NO LONGER OPENS. It did — "space or enter selects", which was the operator's earlier wording —
// but "selects" now means MARK, which is what it means on every other list here, and the operator
// asked for exactly that ("Spacebar selects, then we should be able to bulk delete or bulk assign").
// ENTER remains the open gesture, so nothing became unreachable.

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// The rail's item chords. Named here so the rail's hint, the help overlay and the tests cannot drift
// from the binding — the same discipline the tab chords got (they were once written down in four
// places, which is how they drifted).
const (
	conversationBulkDeleteChord = "ctrl+x"
)

// toggleConvMark marks or unmarks the highlighted conversation and advances one row.
//
// ADVANCING IS THE POINT: kit2 marks and steps together (Table.ToggleMark + Move(1)) so a run of
// spaces builds a selection the way every multi-select list does, without forcing mark-then-down
// every time.
func (m *App) toggleConvMark() {
	// A FOLDER ROW marks its members — "space marks what is under the cursor", and for a folder that is
	// everything inside it. It is a TOGGLE on the group: if all of them are already marked, space clears
	// the group, so the same key reads the same way as it does on a single row.
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
//
// A reload is the honest reconciliation for a bulk delete, and without this a deleted conversation
// would stay marked: the count would overstate the selection and the next bulk action would try to
// write to a row that no longer exists (the table equivalent is Table.PruneMarks, called from
// Base.LoadItems for the same reason).
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
//
// The ids come back in LIST ORDER, not map order, so a bulk write is deterministic: an operator
// watching the rail sees the rows acted on in the order they are displayed, and a partial failure
// reports a stable set. (A map would reshuffle them run to run.)
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
//
// IT IS DELIBERATELY NOT scrollKeyDelta. That returns a LINE delta for scrolling a detail pane (±3
// and ±12), and reusing it here moved the rail THREE rows per press: the first press of down landed on
// row 3. The two are different units for a reason — three lines is a comfortable scroll, one row is a
// selection — so they are separate functions rather than one with a caller that has to remember.
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
//
// ONE implementation for the composer AND the content, because they disagreed: the rail's arrows were
// only wired into the composer branch, so with the keyboard in the CONTENT — where the operator is
// when reading the transcript, and what the footer's "ctrl+g composer" tells them — the arrows fell
// through to the Ask screen. The Ask screen has a HIDDEN "conversations" source (HideSources), so its
// cursor moved, the pane re-requested that conversation's detail, and the transcript changed while the
// rail's highlight never moved: exactly the report — "it moves through the conversations but the
// currently selected conversation selector is not moving".
func (m *App) railOwnsVerticalKey(key string) bool {
	step := railSelectionStep(key)
	if step == 0 {
		return false
	}
	// BOTH the rail being visible AND the rail being SELECTED. Visibility alone was the
	// old test, and it is true whenever a conversation is open — so the rail claimed the
	// vertical keys even with the transcript selected, which is the operator's "hitting
	// enter on a conversation ... up/down" gap: there was no key left to scroll with.
	if !m.railVisible() || m.active != TabAsk || m.askPane != askPaneRail {
		return false
	}
	m.selectRailConversation(step)
	m.refreshStreamStatus()
	return true
}

// railItemKey handles the rail's item chords, reporting whether it owned the key.
//
// Called for BOTH focus levels: the chords act on the rail's own selection, so they must work wherever
// the keyboard is. The caller is responsible for the empty-composer rule (see router.go).
func (m *App) railItemKey(k string) (bool, tea.Cmd) {
	if !m.railVisible() || m.active != TabAsk {
		return false, nil
	}
	// A FOLDER ROW MANAGES ITS GROUPING, the same placement as the GUI's FolderItem (onToggle /
	// onStartRename / onDelete) and the same contextual-key rule the Workers and Workflows panes use:
	// enter toggles the arrow, `e` renames, `x` deletes.
	if f := m.railFolderAt(m.convSel); f != nil {
		switch k {
		case "enter":
			m.toggleConvFolder(f.catID)
			m.refreshStreamStatus()
			return true, nil
		case categoryRenameChord:
			// A PROJECT FOLDER IS NOT A CATEGORY. hasCat is what keeps a project's name from being
			// renamed (or deleted) through the category chords — the chords stay available on the row
			// and simply do not apply, rather than the rail growing a second contextual key set.
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
		//
		// The operator: "In conversations there is no ctrl+x to just delete 1 item. You have to select
		// multiple first." Requiring two marks meant there was NO WAY to delete a single conversation —
		// the one thing the key is most obviously for. A single delete still confirms, because it is
		// irreversible and takes the messages with it.
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
//
// The deletes are BATCHED as independent commands rather than chained: each DeleteConversation is its
// own RPC and returns its own ConversationMutatedMsg, so the list reconciles per row and one failure
// does not hide the rest. Ordering is preserved by the ids slice (convBulkIDs returns list order).
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
//
// The shell hosts it for the same reason it hosts the rename and assign modals: the surface it names
// belongs to the shell. `run` is what happens on the affirmative choice — captured here rather than
// re-derived at confirm time, so a list change behind the dialog (the rolling refresh re-seats the
// cursor, a stream pokes a row in) cannot retarget the write.
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
//
// It used to be rendered INSIDE the rail, and that was wrong twice over: the rail's inner width is 28
// cells against a 34-cell chord list, so it was TRUNCATED mid-word ("ctrl+n: rename · ctrl+t: ca…" —
// the operator's screenshot), and the rail is not where the keys live. The composer is: the rail's
// keys are driven from there, and the operator asked for the move — "put the shortcuts in the composer
// on conversations instead of in that pane".
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
	// A FOLDER ROW answers its own keys, and the composer is where they are advertised (the rail is where
	// the operator looks, the composer is where the keys live). Naming them only for folder rows keeps the
	// hint about what the cursor can actually do.
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
