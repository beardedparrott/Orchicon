package tui

// railbulk.go — bulk operations on the conversations rail.

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// The rail's item chords.
const (
	conversationBulkDeleteChord = "ctrl+x"
	// conversationSearchChord filters the rail by free text. ctrl+s rather than ctrl+f because ctrl+f is
	// already FIND IN TRANSCRIPT (router.go) — a different search, in a different pane, and overloading
	// it would leave the operator unsure which one they opened.
	conversationSearchChord = "ctrl+s"
	// conversationSelectAllChord marks every conversation CURRENTLY VISIBLE, which after a search is
	// exactly the matched set. This is what turns "delete every chat named like this" into one gesture
	// instead of a scroll-and-space exercise.
	//
	// NOT ctrl+a: that is the composer's SELECT ALL (a global route, so it would win before the rail saw
	// it) and overloading it would leave the operator unsure whether they had selected a chat or their
	// own typed text.
	conversationSelectAllChord = "ctrl+l"
)

// convSearchBox is the rail's INLINE search state: a text field plus the query it opened with, so esc
// can put it back.
type convSearchBox struct {
	input textinput.Model
	prev  string
}

const (
	convSearchPrompt = "filter > "
	// THE KEYS HINT IS TWO LINES, DELIBERATELY.
	//
	// One line was TRUNCATED in the panel ("… · typi…"), so the box silently dropped the half that said
	// what typing does — the single most important thing about it. A hint that gets cut is worse than a
	// shorter hint: the operator reads a broken sentence and assumes that is all there is. Two short lines
	// fit inside convSearchModalWidth without the terminal deciding what to keep.
	//
	// It also never says "save", because nothing is saved: the filter applies to every keystroke.
	convSearchKeysHintLine1 = "esc: cancel · enter: keep the filter"
	convSearchKeysHintLine2 = "↑/↓: move the selection · typing filters live"
	// convSearchModalWidth is WIDER than a confirmation dialog, because this panel is a text field with
	// prose under it rather than a one-sentence question.
	convSearchModalWidth = 64
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

	case conversationSearchChord:
		m.openConvSearch()
		return true, nil

	case conversationSelectAllChord:
		return true, m.selectAllVisibleConversations()

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
	if q := strings.TrimSpace(m.convSearch); q != "" {
		// A FILTER THAT IS ON MUST BE VISIBLE, or a half-empty rail reads as lost conversations.
		parts = []string{fmt.Sprintf("filter: %q", q)}
	}
	if n := m.convMarkedCount(); n > 0 {
		parts = []string{fmt.Sprintf("%d marked", n)}
	}
	// SEARCH AND SELECT-ALL ARE ALWAYS ADVERTISED, not only after something is marked: they are how an
	// operator FINDS something to mark, and a chord nobody is told about is a chord nobody uses. The
	// first attempt bound ctrl+l for select-all and never mentioned it anywhere, which is exactly why it
	// looked "not to exist".
	parts = append(parts,
		conversationSearchChord+": search",
		conversationSelectAllChord+": select all"+func() string {
			if n := len(m.scopedConversations()); n > 0 {
				return fmt.Sprintf(" (%d)", n)
			}
			return ""
		}(),
	)
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

// openConvSearch opens the rail's INLINE search box and filters as the operator types.
//
// IT IS A TEXT BOX, NOT A FORM. The first attempt used kit2.NewForm, which is a SUBMIT dialog: it
// filtered only on Enter and told the operator "hit ctrl+s to save", which is not a thing a search box
// does. A search is applied PER KEYSTROKE — that is what makes seeing the list narrow feel like
// searching rather than like filling in a form.
func (m *App) openConvSearch() {
	in := textinput.New()
	in.Prompt = convSearchPrompt
	in.PromptStyle = theme.DetailKey
	in.Cursor.Style = theme.ComposerCursor
	in.TextStyle = lipgloss.NewStyle().Foreground(theme.Text)
	in.SetValue(m.convSearch)
	in.CursorEnd()
	blink := in.Focus()
	m.convSearchBox = &convSearchBox{input: in, prev: m.convSearch}
	m.pendingConvSearchCmd = blink
	m.dock.SetError("")
	m.refreshComposerHint()
}

// convSearchKey routes a key while the rail's search box is up. handled=false lets the key continue
// through the dispatch chain so the shell's hard escapes stay reachable.
func (m *App) convSearchKey(k tea.KeyMsg) (bool, tea.Cmd) {
	if m.convSearchBox == nil {
		return false, nil
	}
	switch k.String() {
	case "esc":
		m.cancelConvSearch()
		return true, nil
	case "enter", "ctrl+j":
		// Enter KEEPS the filter and hands the keyboard back — the list stays narrowed, which is what
		// an operator expects after typing a query they can see working.
		m.commitConvSearch()
		return true, nil
	case "up", "down", "ctrl+p", "ctrl+n":
		// Move in the list WITHOUT leaving the box, so the filtered rows can be browsed and marked while
		// the query still stands. The box keeps the keyboard.
		if step := railSelectionStep(k.String()); step != 0 {
			m.selectRailConversation(step)
			m.refreshStreamStatus()
		}
		return true, nil
	case "ctrl+c":
		m.cancelConvSearch()
		return false, nil
	}
	before := m.convSearchBox.input.Value()
	next, cmd := m.convSearchBox.input.Update(k)
	m.convSearchBox.input = next
	if m.convSearchBox.input.Value() != before {
		// THE LIVE FILTER. Every keystroke re-narrows the rail, and marks are pruned to what survives so
		// a bulk action cannot act on rows the box has just hidden.
		m.convSearch = m.convSearchBox.input.Value()
		m.pruneConvMarks()
		m.refreshStreamStatus()
	}
	return true, cmd
}

// commitConvSearch keeps the query and hands the keyboard back.
func (m *App) commitConvSearch() {
	if m.convSearchBox == nil {
		return
	}
	m.convSearch = m.convSearchBox.input.Value()
	m.convSearchBox = nil
	m.refreshComposerHint()
	m.refreshStreamStatus()
}

// cancelConvSearch restores the query the box opened with, so an accidental ctrl+s loses nothing.
func (m *App) cancelConvSearch() {
	if m.convSearchBox == nil {
		return
	}
	m.convSearch = m.convSearchBox.prev
	m.convSearchBox = nil
	m.pruneConvMarks()
	m.refreshComposerHint()
	m.refreshStreamStatus()
}

// convSearchView composes the search box over the frame.
func (m *App) convSearchView(base string, w, h int) string {
	if m.convSearchBox == nil {
		return base
	}
	// INNER WIDTH IS THE PANEL WIDTH MINUS ITS BORDERS, computed here rather than from modalInnerWidth,
	// because this panel is deliberately wider than the shared modal width.
	inner := convSearchModalWidth - 2
	if inner < 8 {
		inner = 8
	}
	return m.overlayCentered(base, m.modalPanel(m.convSearchBody(inner), convSearchModalWidth))
}

// convSearchBody renders the box: the query with its caret, a LIVE count of what it matches, and the
// keys. The count is the feedback that makes typing feel connected to the list.
func (m *App) convSearchBody(inner int) string {
	// -1: bubbles' textinput renders its prompt AND a caret past the value, so a Width that fills the
	// interior exactly still overflows by the cursor cell.
	m.convSearchBox.input.Width = max(inner-lipgloss.Width(convSearchPrompt)-1, 8)
	visible := m.scopedConversations()
	total := len(m.conversations)
	var count string
	switch {
	case strings.TrimSpace(m.convSearch) == "":
		count = fmt.Sprintf("%d conversations — type to filter", total)
	case len(visible) == 0:
		count = fmt.Sprintf("no conversations match in %d", total)
	default:
		count = fmt.Sprintf("%d of %d conversations", len(visible), total)
	}
	return strings.Join([]string{
		theme.DetailKey.Render("search conversations"),
		m.convSearchBox.input.View(),
		theme.HintText.Render(truncateRight(count, inner)),
		theme.HintText.Render(truncateRight(convSearchKeysHintLine1, inner)),
		theme.HintText.Render(truncateRight(convSearchKeysHintLine2, inner)),
	}, "\n")
}

// clearConvSearch drops the filter, reporting whether there was one. Bound to esc, which clears a
// selection FIRST and keeps its established meaning when there is nothing to clear.
func (m *App) clearConvSearch() bool {
	if m.convSearch == "" {
		return false
	}
	m.convSearch = ""
	m.pruneConvMarks()
	m.refreshStreamStatus()
	return true
}

// selectAllVisibleConversations marks every conversation the rail is CURRENTLY showing.
//
// VISIBLE, NOT EVERY CONVERSATION. After a search this is precisely the matched set — the whole point —
// and it also means the project scope is respected, so "select all" can never quietly reach across
// projects the operator has filtered out. Pressing it again clears, which is the toggle every list in
// this client uses.
func (m *App) selectAllVisibleConversations() tea.Cmd {
	visible := m.scopedConversations()
	if len(visible) == 0 {
		m.dock.SetError("nothing visible to select")
		return nil
	}
	// Already fully marked -> clear (a toggle, not an accumulating trap).
	if len(m.convMarked) == len(visible) {
		allMarked := true
		for _, c := range visible {
			if !m.convMarked[c.ID] {
				allMarked = false
				break
			}
		}
		if allMarked {
			m.convMarked = nil
			m.dock.SetNotice("selection cleared")
			m.refreshStreamStatus()
			return nil
		}
	}
	if m.convMarked == nil {
		m.convMarked = map[string]bool{}
	}
	for _, c := range visible {
		m.convMarked[c.ID] = true
	}
	m.dock.SetNotice(fmt.Sprintf("%d conversations marked", len(visible)))
	m.refreshStreamStatus()
	return nil
}
