package tui

// railrows.go — the conversations rail's rows: the ACTIVE PROJECT's conversations, grouped by
// category.

import (
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// railRow is one LINE of the conversations rail.
type railRow struct {
	// folder marks a grouping row (a collapsible header). Its conv is -1.
	folder bool
	// catID is the grouping's id — NOT the synthetic row id, because rename/delete address the category itself.
	// It is also the COLLAPSE key: a folder row is always a category here, so the two cannot disagree.
	catID string
	// parent is the synthetic folder row id this row hangs under ("" at the top level).
	parent string
	depth  int
	title  string
	count  int
	// conv is the index into m.conversations for a conversation row, -1 for a folder.
	conv int
}

// hasCat reports whether a folder row is a CATEGORY folder.
func (r railRow) hasCat() bool { return r.catID != "" }

// railConvID is the conversation id a row addresses ("" for a folder row).
func (m *App) railConvID(r railRow) string {
	if r.folder || r.conv < 0 || r.conv >= len(m.conversations) {
		return ""
	}
	return m.conversations[r.conv].ID
}

// scopedConversations are the conversations the ACTIVE PROJECT shows.
func (m *App) scopedConversations() []chat.Conversation {
	return filterConversationsByScope(m.conversations, m.projectScope)
}

// railRows builds the rail's VISIBLE lines: a folder per category (in the server's order), its
// members, and a trailing Uncategorized folder — with the members of a COLLAPSED folder omitted.
func (m *App) railRows() []railRow {
	scoped := m.scopedConversations()
	if len(scoped) == 0 {
		return nil
	}
	// The index map points into the FULL conversation list, because railConvID resolves a row to a
	// conversation by that index — so the rows must carry positions in m.conversations.
	index := make(map[string]int, len(m.conversations))
	for i, c := range m.conversations {
		index[c.ID] = i
	}
	inScope := make(map[string]bool, len(scoped))
	for _, c := range scoped {
		inScope[c.ID] = true
	}
	items := make([]screenkit.Item, 0, len(scoped))
	for _, c := range scoped {
		items = append(items, screenkit.Item{ID: c.ID, Title: c.Title})
	}
	groups := m.categoryGroupsFor(apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION)
	// AND THE FOLDERS ARE SCOPED TO THE WORKSPACE, not just the rows inside them. FILTERING BY
	// MEMBERSHIP CANNOT MISFILE ANYTHING.
	if m.projectScope != projectScopeAll {
		used := make(map[string]bool, len(scoped))
		for _, c := range scoped {
			if catID, _ := m.CategoryOf(apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION, c.ID); catID != "" {
				used[catID] = true
			}
		}
		kept := make([]screenkit.GroupSpec, 0, len(groups))
		for _, g := range groups {
			if used[g.ID] {
				kept = append(kept, g)
			}
		}
		groups = kept
	}
	grouped := screenkit.GroupItemsByCategory(items, groups, func(id string) string {
		catID, _ := m.CategoryOf(apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION, id)
		return catID
	})

	// Build the full list (collapsed folders still carry their members here), then filter: the collapse
	// decision is a property of the FOLDER, so it is applied once, after every row knows its parent.
	all := make([]railRow, 0, len(grouped))
	for _, it := range grouped {
		if screenkit.IsGroupRow(it.ID) {
			all = append(all, railRow{
				folder: true, catID: screenkit.GroupCategoryID(it.ID),
				title: it.Title, count: countTrailingInt(it.Meta), conv: -1,
			})
			continue
		}
		if !inScope[it.ID] {
			// Defensive: GroupItemsByCategory returns what it was given, so this cannot fire today.
			continue
		}
		all = append(all, railRow{
			parent: it.Parent, depth: it.Depth, title: it.Title, conv: index[it.ID],
		})
	}

	rows := make([]railRow, 0, len(all))
	for _, r := range all {
		if r.parent != "" && m.convCollapsed[parentCatID(r.parent)] {
			continue // inside a collapsed folder
		}
		rows = append(rows, r)
	}
	return rows
}

// parentCatID recovers the category id from a folder row id ("" when it is not one).
func parentCatID(rowID string) string { return screenkit.GroupCategoryID(rowID) }

// countTrailingInt pulls the member count out of a folder's "N items" meta, so the rail can print it
// without re-deriving it from the (possibly collapsed) rows.
func countTrailingInt(meta string) int {
	n, seen := 0, false
	for _, r := range meta {
		if r >= '0' && r <= '9' {
			n = n*10 + int(r-'0')
			seen = true
			continue
		}
		if seen {
			break
		}
	}
	return n
}

// railConvIndexAt resolves the conversation behind a row index (-1 when the row is a folder, out of
// range, or hidden).
func (m *App) railConvIndexAt(i int) int {
	rows := m.railRows()
	if i < 0 || i >= len(rows) || rows[i].folder {
		return -1
	}
	return rows[i].conv
}

// railFolderAt resolves the folder behind a row index (nil when it is not a folder row).
func (m *App) railFolderAt(i int) *railRow {
	rows := m.railRows()
	if i < 0 || i >= len(rows) || !rows[i].folder {
		return nil
	}
	return &rows[i]
}

// toggleConvFolder collapses or expands the folder under the cursor — the arrow's gesture, and the same
// key the kit2 panes use for a tree node (enter).
func (m *App) toggleConvFolder(catID string) {
	if catID == "" {
		return
	}
	if m.convCollapsed == nil {
		m.convCollapsed = map[string]bool{}
	}
	if m.convCollapsed[catID] {
		delete(m.convCollapsed, catID)
	} else {
		m.convCollapsed[catID] = true
	}
	// The cursor can now be past the end of a shorter list.
	rows := m.railRows()
	if m.convSel >= len(rows) {
		m.convSel = max(0, len(rows)-1)
	}
	m.railFollowSelection()
	// PERSIST IT.
	m.persistCollapsedGroups()
}

// railRowsInFolder lists the conversations inside a folder, in list order — what "mark this folder"
// means.
func (m *App) railRowsInFolder(catID string) []string {
	rows := m.railRows()
	var ids []string
	// The folder's members are the rows whose parent resolves to it.
	for _, r := range rows {
		if r.folder || parentCatID(r.parent) != catID {
			continue
		}
		if id := m.railConvID(r); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}
