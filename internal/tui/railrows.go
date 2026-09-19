package tui

// railrows.go — the conversations rail as a list of ROWS, so a conversation can nest under its grouping.
//
// THE OPERATOR: "When an item (conversation, worker, workflow) is in a category, it should create a
// little arrow dropdown in their respective lists that can be collapsed or expanded" … "Both should
// honor the same and work in the same way."
//
// The Workers and Workflows panes got that for free by feeding kit2's tree. The rail is the shell's own
// list of chat.Conversation, so it needs the arrangement built here — and it goes through the SAME
// screenkit.GroupItemsByCategory the two panes use, which is what makes the three lists' folder order,
// empty-folder behaviour and Uncategorized rule identical BY CONSTRUCTION rather than by three
// implementations agreeing.
//
// M.convSel therefore indexes THIS list, not m.conversations: the cursor moves through what is on
// screen, and a collapsed folder's members are not on screen. Every site that wants the conversation
// behind the cursor goes through railConvIndexAt, which is what keeps "the row I am pointing at" and
// "the conversation I am acting on" from drifting apart.

import (
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// railRow is one LINE of the conversations rail.
type railRow struct {
	// folder marks a grouping row (a collapsible header). Its conv is -1.
	folder bool
	// catID is the grouping's id — NOT the synthetic row id, because rename/delete address the
	// category itself. EMPTY on a PROJECT folder, which is not a category and must not be
	// renameable or deletable through the category chords.
	catID string
	// projID is the project a row belongs to, "" when the conversation is unassigned. It is set on EVERY
	// row the project level emits, because collapsing a project has to hide all of its descendants and a
	// descendant row is the only thing that can answer "which project am I in".
	projID string
	// projRowID is the project folder's synthetic row id, so its children can point at it.
	projRowID string
	// key is the COLLAPSE key of a folder row: the category id for a category folder, the project's key for
	// a project folder. Separate from catID so the two kinds of folder can share the collapse map (and its
	// persistence) without a project ever being treated as a category.
	key string
	// projHead marks the PROJECT FOLDER ROW ITSELF (as opposed to everything inside it). It exists for one
	// reason: the collapse filter hides rows by their project, and the folder row carries its own project — so
	// without this flag collapsing a project would hide the very row you need to expand it again.
	projHead bool
	// status is a project folder's status, shown so an ARCHIVED project is visible rather than hidden — the
	// association rule is "active or otherwise", so the rail has to make the otherwise legible.
	status string
	// parent is the synthetic folder row id this row hangs under ("" at the top level).
	parent string
	depth  int
	title  string
	count  int
	// conv is the index into m.conversations for a conversation row, -1 for a folder.
	conv int
}

// hasCat reports whether a folder row is a CATEGORY folder rather than a project folder. A category folder
// always carries a catID and a project folder never does, which is what keeps the category chords from
// addressing a category that does not exist.
func (r railRow) hasCat() bool { return r.catID != "" }

// railConvID is the conversation id a row addresses ("" for a folder row).
func (m *App) railConvID(r railRow) string {
	if r.folder || r.conv < 0 || r.conv >= len(m.conversations) {
		return ""
	}
	return m.conversations[r.conv].ID
}

// projRowPrefix namespaces a project folder's synthetic row id, so it can never collide with a category's
// GroupRowID. The EMPTY project id collapses to the bare prefix, which is the unassigned level's id.
const projRowPrefix = "proj:"

// projCollapseKey is the collapse key for a project folder. It shares m.convCollapsed (and its
// persistence) with the category folders without sharing their namespace.
func projCollapseKey(projID string) string { return projRowPrefix + projID }

// unassignedProjKey is the collapse key for the level holding conversations with no project.
const unassignedProjKey = projRowPrefix

// railRows dispatches to the project-level layout when the tenant HAS projects and to the flat
// category-only layout when it does not.
//
// THE TWO LAYOUTS ARE KEPT SEPARATE RATHER THAN ONE PARAMETERISED ONE, because with no projects a single
// wrapper folder around every conversation is pure noise — it would read `▾ Conversations 3` above the very
// list the rail's own title already counts, and it would re-indent every row by a level that means nothing.
// A tenant that has never made a project gets exactly the rail it has always had; the level appears the moment
// there is something to group by.
func (m *App) railRows() []railRow {
	if len(m.railProjects) == 0 {
		return m.railRowsByCategory()
	}
	return m.railRowsByProject()
}

// railRowsByCategory is the ORIGINAL single-level layout: a folder per category (in the server's order), its
// members, and a trailing Uncategorized folder — with the members of a collapsed folder omitted. It is what
// renders when the tenant has no projects, and it is unchanged from before projects existed.
func (m *App) railRowsByCategory() []railRow {
	if len(m.conversations) == 0 {
		return nil
	}
	items := make([]screenkit.Item, 0, len(m.conversations))
	index := make(map[string]int, len(m.conversations))
	for i, c := range m.conversations {
		items = append(items, screenkit.Item{ID: c.ID, Title: c.Title})
		index[c.ID] = i
	}
	groups := m.categoryGroupsFor(apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION)
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
				folder: true, catID: screenkit.GroupCategoryID(it.ID), key: screenkit.GroupCategoryID(it.ID),
				title: it.Title, count: countTrailingInt(it.Meta), conv: -1,
			})
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

// railRowsByProject builds the TWO-level layout: a folder per PROJECT (every project, in the project list's
// order, whether or not it has conversations yet), the conversations inside each — grouped by category
// exactly as before — and a trailing Unassigned folder.
//
// THE OPERATOR: "We need to make a second higher level in organization for conversations. It should be another
// drop down where all of the conversations are associated with Projects in a parent category. For every project
// that is created (active or otherwise), there should be a list that can be dragged to and also created from."
//
// So the PROJECT becomes the parent level and the category grouping nests INSIDE it. The category layer is not
// replaced: a chat can be in a project AND carry a label, and both stay visible.
//
// EMPTY PROJECTS ARE EMITTED, which is what makes the rail a place you can move a chat TO rather than only a
// report of where chats already are — the operator's "a list that can be dragged to and also created from". A
// project with nothing in it renders as `▸ Name  0` and its fold holds nothing.
//
// A STALE PROJECT ID still gets a group. The column carries no foreign key (see the migration), so a
// conversation can outlive its project; dropping it from the rail would make the chat unreachable. It is
// grouped under the raw id instead, which is ugly and honest — and the operator can /project it somewhere.
func (m *App) railRowsByProject() []railRow {
	// Bucket the conversations by project, keeping the LIST's order within each bucket (the list is already
	// newest-first, which is the order the rail has always shown).
	buckets := map[string][]screenkit.Item{}
	index := make(map[string]int, len(m.conversations))
	for i, c := range m.conversations {
		index[c.ID] = i
		buckets[c.ProjectID] = append(buckets[c.ProjectID], screenkit.Item{ID: c.ID, Title: c.Title})
	}
	groups := m.categoryGroupsFor(apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION)
	categoryOf := func(id string) string {
		catID, _ := m.CategoryOf(apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION, id)
		return catID
	}

	var rows []railRow
	emitProject := func(projID, name, status string) {
		rowID := projRowPrefix + projID
		rows = append(rows, railRow{
			folder: true, projID: projID, projRowID: rowID, projHead: true, key: projCollapseKey(projID),
			title: name, status: status, count: len(buckets[projID]), conv: -1,
		})
		// The category grouping runs WITHIN the project, on its members only, through the same helper the kit2
		// panes use — so folder order, the empty-folder rule and the Uncategorized rule stay identical by
		// construction rather than by three implementations agreeing.
		grouped := screenkit.GroupItemsByCategory(buckets[projID], groups, categoryOf)
		for _, it := range grouped {
			if screenkit.IsGroupRow(it.ID) {
				rows = append(rows, railRow{
					folder: true, catID: screenkit.GroupCategoryID(it.ID), projID: projID,
					projRowID: rowID, key: screenkit.GroupCategoryID(it.ID), parent: rowID, depth: 1,
					title: it.Title, count: countTrailingInt(it.Meta), conv: -1,
				})
				continue
			}
			// A conversation keeps the CATEGORY ROW as its parent when it has one, because the category
			// collapse is keyed off the parent's category id — re-parenting it to the project would quietly
			// break collapsing a label. With no categories the helper returns the items UNCHANGED (Parent
			// ""), and those hang directly under the project.
			parent, depth := it.Parent, it.Depth+1
			if parent == "" {
				parent, depth = rowID, 1
			}
			rows = append(rows, railRow{
				projID: projID, projRowID: rowID, parent: parent, depth: depth,
				title: it.Title, conv: index[it.ID],
			})
		}
	}

	// EVERY project, in the order the project list gives; then any project id that is not in the list but is
	// still referenced (a stale association); then Unassigned last, and only when something is in it — a
	// permanent empty "Unassigned" would be a fixture rather than a fact.
	known := make(map[string]bool, len(m.railProjects))
	for _, p := range m.railProjects {
		known[p.ID] = true
		emitProject(p.ID, p.Name, p.Status)
	}
	for id, members := range buckets {
		if id == "" || known[id] || len(members) == 0 {
			continue
		}
		emitProject(id, "unknown project "+id, "missing")
	}
	if len(buckets[""]) > 0 {
		emitProject("", "Unassigned", "")
	}

	// COLLAPSE. A project folder hides everything beneath it; a category folder hides its own members. The
	// descendant test is by the row's own project rather than by walking parents, because every row knows it —
	// and projHead exempts the folder row itself, which would otherwise hide itself.
	out := make([]railRow, 0, len(rows))
	for _, r := range rows {
		if !r.projHead {
			if r.projID != "" && m.convCollapsed[projCollapseKey(r.projID)] {
				continue
			}
			if r.projRowID == projRowPrefix && m.convCollapsed[unassignedProjKey] {
				continue // the Unassigned fold and everything in it share one key
			}
		}
		if r.parent != "" && m.convCollapsed[parentCatID(r.parent)] {
			continue // inside a collapsed category folder
		}
		out = append(out, r)
	}
	return out
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

// toggleConvFolder collapses or expands the folder under the cursor — the arrow's gesture, and the same key
// the kit2 panes use for a tree node (enter). The key is the row's COLLAPSE key, so one command serves a
// category folder and a project folder alike.
func (m *App) toggleConvFolder(key string) {
	if key == "" {
		return
	}
	if m.convCollapsed == nil {
		m.convCollapsed = map[string]bool{}
	}
	if m.convCollapsed[key] {
		delete(m.convCollapsed, key)
	} else {
		m.convCollapsed[key] = true
	}
	// The cursor can now be past the end of a shorter list.
	rows := m.railRows()
	if m.convSel >= len(rows) {
		m.convSel = max(0, len(rows)-1)
	}
	m.railFollowSelection()
	// PERSIST IT. The operator: "Conversation categories don't stay collapsed when you leave orch and come
	// back in." The collapse was session state with a comment claiming it was deliberately local like the
	// GUI's — but the GUI's per-page collapse lives in the BROWSER's localStorage, which survives a
	// relaunch, so "local" there still means "remembered". Session state here meant a restart lost it.
	m.persistCollapsedGroups()
}

// railRowsInFolder lists the conversations inside a folder, in list order — what "mark this folder" means.
//
// IT SERVES BOTH KINDS OF FOLDER. A category folder lists its members (rows whose parent resolves to it); a
// PROJECT folder lists every conversation in that project, at any depth beneath it. A COLLAPSED folder's
// members are still listed: marking a folder means marking the folder, and hiding the contents from the
// operation would make the gesture useless exactly when the folder is large.
func (m *App) railRowsInFolder(f railRow) []string {
	rows := m.railRows()
	var ids []string
	for _, r := range rows {
		if r.folder {
			continue
		}
		if f.hasCat() {
			if parentCatID(r.parent) != f.catID {
				continue
			}
		} else if r.projRowID != f.projRowID {
			continue
		}
		if id := m.railConvID(r); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}
