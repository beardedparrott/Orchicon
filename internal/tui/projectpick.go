package tui

// projectpick.go — /project's PICKER, and the project SCOPE the conversations rail shows.
//
// The operator, after the first attempt rendered projects as a second list beside the categories:
//
//   "I wanted a hierarchy. So a conversation would belong to a project and inside the project it would still
//    have the normal categories we had before. ... Projects are WORKSPACES essentially. ... In the TUI the
//    equivalent would be /project to set the active project. A list should pop up to make it easier to pick the
//    right one when you type /project slash command."
//
// So a project is a SCOPE the rail is filtered by, not a row in it. Choosing one narrows which conversations the
// rail shows; the category folders are unchanged and simply hold fewer items. And it is picked from a LIST,
// because a command that only accepts a name you have to already know is not an interface.
//
// WHY A BESPOKE MODAL RATHER THAN kit2.ModelPicker: that control walks the adapter/provider/model grammar in
// three tiers and commits a ref. A project is one flat choice out of a handful, so reusing it would mean
// pretending a project is a model. This is the same shape as the shell's other small modals (the rename box, the
// bulk confirm): state on the App, a centred overlay, keys routed while it is open.

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// projectScopeAll is the scope that filters nothing. It is the DEFAULT, deliberately: the project column is
// additive and defaults to empty, so a default that filtered would make every conversation that predates this
// feature look deleted on launch.
const projectScopeAll = "__all__"

// unassignedScope is the scope holding conversations with no project. It is a real scope and not the absence of
// one — those chats have to stay reachable, and this is also where a chat goes when it is moved out of a project.
const unassignedScope = ""

// projectScopeOption is one entry in the picker, and in the scope's own vocabulary.
type projectScopeOption struct {
	// Value is the scope: a project id, unassignedScope, or projectScopeAll.
	Value string
	Label string
	// Count is how many conversations the scope actually holds — a count of ITEMS, not of a project's rows.
	Count int
	// Archived marks a project that is not active, so the option can say so. The association rule is the
	// operator's "active or otherwise", so an archived project is a valid workspace and the one fact worth
	// knowing before working in it is that it is not active.
	Archived bool
}

// filterConversationsByScope returns the conversations visible in a scope.
//
// The GUI's equivalent decides the same thing for the same reason, from the same column — see
// frontend/src/lib/conversationProjects.ts. Two clients, one rule.
func filterConversationsByScope(convs []chat.Conversation, scope string) []chat.Conversation {
	if scope == projectScopeAll {
		return convs
	}
	out := make([]chat.Conversation, 0, len(convs))
	for _, c := range convs {
		if c.ProjectID == scope {
			out = append(out, c)
		}
	}
	return out
}

// projectScopeOptions builds the picker's contents: All projects, every project, any project id still referenced
// by a conversation, and No project.
//
// EVERY PROJECT IS LISTED even with no conversations, and the count then reads 0 — that is information rather
// than an omission: it is how the operator knows a workspace is empty before switching to it. A project that is
// merely REFERENCED still gets an option too, because the column carries no foreign key: a conversation can
// outlive its project, and without an option for that id the chat would be unreachable from the rail entirely.
func projectScopeOptions(projects []railProject, convs []chat.Conversation) []projectScopeOption {
	counts := map[string]int{}
	for _, c := range convs {
		counts[c.ProjectID]++
	}
	known := map[string]bool{}
	out := []projectScopeOption{
		{Value: projectScopeAll, Label: "All projects", Count: len(convs)},
	}
	for _, p := range projects {
		known[p.ID] = true
		out = append(out, projectScopeOption{
			Value:    p.ID,
			Label:    p.Name,
			Count:    counts[p.ID],
			Archived: p.Status != "" && p.Status != "active",
		})
	}
	// Referenced-but-unlisted project ids, sorted so the list does not reshuffle between opens.
	var orphans []string
	for id := range counts {
		if id != unassignedScope && !known[id] {
			orphans = append(orphans, id)
		}
	}
	sortStrings(orphans)
	for _, id := range orphans {
		out = append(out, projectScopeOption{
			Value: id, Label: "unknown project " + id, Count: counts[id], Archived: true,
		})
	}
	out = append(out, projectScopeOption{Value: unassignedScope, Label: "No project", Count: counts[unassignedScope]})
	return out
}

// projectScopeLabel names a scope for display, falling back to the raw value for a scope with no option.
func projectScopeLabel(scope string, opts []projectScopeOption) string {
	for _, o := range opts {
		if o.Value == scope {
			return o.Label
		}
	}
	if scope == "" {
		return "No project"
	}
	return scope
}

// sortStrings is a plain insertion sort — the orphan list is tiny, and importing sort for it would be noise next
// to the rest of this file's dependencies.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// projectPicker is the /project modal: a list of scopes the operator picks from.
type projectPicker struct {
	options []projectScopeOption
	sel     int
	// moveConvID is the conversation being MOVED when the picker was opened to reassign one, "" when it is
	// choosing the RAIL'S SCOPE. The two are the same question ("which project?") with different consequences,
	// so they share the control and the title says which one you are answering.
	moveConvID string
	// filter is the text the picker was opened with (""), kept so a project list arriving AFTER the overlay
	// opened can be narrowed the same way instead of silently widening (see onRailProjectsApplied).
	filter string
}

// scopePickerTitle is the overlay's heading, which is what tells the two uses apart.
func (p *projectPicker) title() string {
	if p.moveConvID != "" {
		return "MOVE CONVERSATION TO PROJECT"
	}
	return "PROJECT WORKSPACE"
}

// move attempts to move the selection. It returns false at the edges so the caller can fall through to another
// binding — the same contract the screens' pickers use, so a stale key is never silently swallowed.
func (p *projectPicker) move(delta int) bool {
	if len(p.options) == 0 {
		return false
	}
	next := p.sel + delta
	if next < 0 || next >= len(p.options) {
		return false
	}
	p.sel = next
	return true
}

// selected returns the option under the cursor.
func (p *projectPicker) selected() (projectScopeOption, bool) {
	if p.sel < 0 || p.sel >= len(p.options) {
		return projectScopeOption{}, false
	}
	return p.options[p.sel], true
}

// selectByValue parks the cursor on a value, so reopening the picker shows where you currently are.
func (p *projectPicker) selectByValue(v string) {
	for i, o := range p.options {
		if o.Value == v {
			p.sel = i
			return
		}
	}
}

// projectPickerView renders the overlay. It is a CENTRED RECTANGLE of fixed width, which is what
// overlayCentered requires — a ragged width would make the splice leave gaps.
func (m *App) projectPickerView() string {
	p := m.projectPick
	if p == nil {
		return ""
	}
	const w = 52
	inner := w - 4
	var b strings.Builder
	b.WriteString(theme.MenuTitle.Render(truncateRight(p.title(), inner)) + "\n")
	if p.moveConvID != "" {
		title := p.moveConvID
		for _, c := range m.conversations {
			if c.ID == p.moveConvID {
				title = c.Title
			}
		}
		b.WriteString(theme.HintText.Render(truncateRight("for: "+title, inner)) + "\n")
	}
	b.WriteString(theme.HintText.Render(strings.Repeat("─", inner)) + "\n")
	if p.filter != "" {
		// THE FILTER IS SHOWN, so a narrowed list cannot be mistaken for a list that IS everything — the
		// operator typed "Orch", saw one row, and needs to know why.
		b.WriteString(theme.HintText.Render(truncateRight("filter: "+p.filter, inner)) + "\n")
	}
	if len(p.options) == 0 {
		msg := "no projects yet — create one in Work"
		if p.filter != "" {
			msg = "nothing matches " + p.filter
		}
		b.WriteString(theme.HintText.Render(truncateRight(msg, inner)) + "\n")
	}
	for i, o := range p.options {
		// The archived marker and the count are dim right-hand context, so the name column stays readable and
		// the numbers line up across rows.
		meta := fmt.Sprintf("%d", o.Count)
		if o.Archived {
			meta = "archived " + meta
		}
		name := truncateRight(o.Label, max(1, inner-lipgloss.Width(meta)-3))
		pad := max(1, inner-1-lipgloss.Width(name)-lipgloss.Width(meta))
		marker := "  "
		if i == p.sel {
			marker = "▸ "
		}
		row := marker + truncateRight(name+strings.Repeat(" ", pad)+meta, inner-2)
		if i == p.sel {
			b.WriteString(theme.ListItemSelected.Render(row) + "\n")
			continue
		}
		b.WriteString(theme.ListItem.Render(row) + "\n")
	}
	// THE FOOTER NAMES BOTH ACTS. `m` moves the OPEN conversation into the highlighted workspace instead of
	// switching the rail to it — the same list answering the same question, with the write spelled out rather
	// than hidden behind a chord nobody could guess. It is offered only when there is a conversation to move.
	hint := "↑/↓ choose · enter switch workspace · esc cancel"
	if p.moveConvID != "" {
		hint = "↑/↓ choose · enter move here · esc cancel"
	} else if m.chatConvID != "" {
		hint = "↑/↓ choose · enter switch · m move the open chat · esc cancel"
	}
	b.WriteString(theme.HintText.Render(truncateRight(hint, inner)))
	return b.String()
}

// projectPickerKey routes a key to the open picker. Handled=false means the key was not ours (so Esc can close
// it, and anything else falls through) — the same shape as the shell's other modals.
func (m *App) projectPickerKey(k tea.KeyMsg) (handled bool, cmd tea.Cmd) {
	p := m.projectPick
	if p == nil {
		return false, nil
	}
	switch k.String() {
	case "esc":
		m.projectPick = nil
		return true, nil
	case "up", "ctrl+p":
		p.move(-1)
		return true, nil
	case "down", "ctrl+n":
		p.move(1)
		return true, nil
	case "enter":
		opt, ok := p.selected()
		if !ok {
			m.projectPick = nil
			return true, nil
		}
		m.projectPick = nil
		if p.moveConvID != "" {
			return true, m.moveConversationTo(p.moveConvID, opt.Value)
		}
		m.setProjectScope(opt.Value)
		return true, nil
	case "m":
		// MOVE THE OPEN CONVERSATION to the highlighted workspace — the write, where `enter` is the view. The
		// unassigned scope is a legitimate target: a chat has to be able to leave a project.
		if p.moveConvID != "" || m.chatConvID == "" {
			return false, nil // already in move mode, or nothing to move: not this key's gesture
		}
		opt, ok := p.selected()
		if !ok {
			return true, nil
		}
		m.projectPick = nil
		return true, m.moveConversationTo(m.chatConvID, opt.Value)
	}
	return false, nil
}

// projectPickerKeyCmd adapts projectPickerKey to the router's (model, cmd) shape.
//
// The picker CONSUMES every key while it is open, including ones it does not recognise — that is the modal
// discipline the shell's other overlays follow, and it is what stops a stray chord from acting on the rail
// behind the list that is about to re-scope it.
func (m *App) projectPickerKeyCmd(k tea.KeyMsg) tea.Cmd {
	_, cmd := m.projectPickerKey(k)
	return cmd
}

// setProjectScope switches the rail's workspace and says so.
//
// The label is looked up fresh so the notice names the project rather than echoing an id. The cursor is clamped
// because the visible row list just changed underneath it, and a selection past the end would leave the rail
// pointing at nothing.
func (m *App) setProjectScope(scope string) {
	m.projectScope = scope
	// A DELIBERATE CHOICE, recorded so the launch-directory default can never overrule it — including in the
	// window before the project list has landed.
	m.projectScopeChosen = true
	opts := projectScopeOptions(m.railProjects, m.conversations)
	label := projectScopeLabel(scope, opts)
	m.dock.SetNotice("project: " + label)
	if m.convSel >= len(m.railRows()) {
		m.convSel = max(0, len(m.railRows())-1)
	}
	m.convScroll = 0
}

// openProjectPicker opens the picker for a purpose. It loads the project list first when the rail has none —
// /project is how a TUI-only operator discovers projects, so it must not require having looked at the rail
// first. (/projects is a different command — it opens the Work area's Projects PANE; see slash.go.)
func (m *App) openProjectPicker(moveConvID string) tea.Cmd {
	return m.openProjectPickerFiltered(moveConvID, "")
}

// openProjectPickerFiltered opens the picker for a PURPOSE, with the option list narrowed to a filter string.
//
// The operator: "If I type '/project Orch', it would show me the project Orchicon." The filter matches the
// project NAME or its id, case-insensitively, as a SUBSTRING — so a partial word still finds it, and the
// operator is never left staring at an empty list wondering whether the project exists.
//
// THE OPTION LIST DEPENDS ON THE PURPOSE (see pickerOptions): a move target list has no "All projects" row.
// The filter rules themselves live in filterScopeOptions.
func (m *App) openProjectPickerFiltered(moveConvID, filter string) tea.Cmd {
	p := &projectPicker{options: m.pickerOptions(moveConvID, filter), moveConvID: moveConvID, filter: filter}
	if filter == "" {
		if moveConvID == "" {
			p.selectByValue(m.projectScope)
		} else {
			// For a move, park the cursor on the conversation's CURRENT project so the list opens where it is.
			for _, c := range m.conversations {
				if c.ID == moveConvID {
					p.selectByValue(c.ProjectID)
					break
				}
			}
		}
	}
	m.projectPick = p
	if len(m.railProjects) == 0 && m.clients != nil {
		return m.loadRailProjects()
	}
	return nil
}

// projectOptionsFiltered is projectScopeOptions narrowed by a filter string (see openProjectPickerFiltered).
func (m *App) projectOptionsFiltered(filter string) []projectScopeOption {
	return filterScopeOptions(projectScopeOptions(m.railProjects, m.conversations), filter)
}

// projectMoveOptions is the target list for MOVING a conversation.
//
// "All projects" is NOT a place to put a conversation. It is a way to LOOK at the list, and __all__ is not a
// project id — the server rejects it, so a move onto it fails with a 404 that reads like the app is broken. The
// GUI's own move control filters it out for exactly this reason; the TUI offered it, ONE ROW UP from the first
// project, in a picker whose cursor opens on the conversation's current project.
//
// "No project" STAYS. A conversation has to be able to leave a project, and that is the only target that says so.
func (m *App) projectMoveOptions() []projectScopeOption {
	opts := projectScopeOptions(m.railProjects, m.conversations)
	out := make([]projectScopeOption, 0, len(opts))
	for _, o := range opts {
		if o.Value == projectScopeAll {
			continue
		}
		out = append(out, o)
	}
	return out
}

// filterScopeOptions narrows a scope/move option list to a filter string.
//
// The filter matches the project NAME or its id, case-insensitively, as a SUBSTRING — so a partial word still
// finds it, and the operator is never left staring at an empty list wondering whether the project exists. The
// two non-project scopes are dropped while a filter is active: neither is a project, and matching them on the
// literal text would be a false hit ("No" finding "No project").
func filterScopeOptions(opts []projectScopeOption, filter string) []projectScopeOption {
	if filter == "" {
		return opts
	}
	needle := strings.ToLower(filter)
	kept := make([]projectScopeOption, 0, len(opts))
	for _, o := range opts {
		if o.Value == projectScopeAll || o.Value == unassignedScope {
			continue
		}
		if strings.Contains(strings.ToLower(o.Label), needle) || strings.Contains(strings.ToLower(o.Value), needle) {
			kept = append(kept, o)
		}
	}
	return kept
}

// pickerOptions builds a picker's option list for its PURPOSE. One entry point, because the two purposes differ
// in exactly one row and a second list would be a second place to get it wrong.
func (m *App) pickerOptions(moveConvID, filter string) []projectScopeOption {
	if moveConvID != "" {
		return filterScopeOptions(m.projectMoveOptions(), filter)
	}
	return m.projectOptionsFiltered(filter)
}

// moveConversationTo is the WRITE behind every move gesture, guarded in one place.
//
// TWO GUARDS, both of them fixes for what the operator hit:
//
//   - ALL PROJECTS IS REFUSED as a target. The server rejects __all__ as a project id, so this would be a doomed
//     write whose only outcome is "project move failed: ..." — which reads as a broken app rather than as a bad
//     target. projectMoveOptions no longer OFFERS it; this refuses it if it arrives by any other route (the `m`
//     key, whose picker is the SCOPE list and legitimately contains it).
//   - A TARGET THAT CHANGES NOTHING SAYS SO. The move picker's cursor OPENS ON the conversation's current
//     project, so Enter without moving the cursor is the most natural thing to press — and it used to write, tell
//     the operator "moved to <the project it was already in>", and reload to a list where the chat was still
//     sitting exactly where it was. That is the operator's "it is still just sitting there in the same list".
//     Nothing was broken; nothing was REPORTED either. Now it says "already in X" and writes nothing.
func (m *App) moveConversationTo(convID, target string) tea.Cmd {
	if target == projectScopeAll {
		m.dock.SetError("All projects is a view, not a destination — pick a project, or No project")
		return nil
	}
	for _, c := range m.conversations {
		if c.ID != convID {
			continue
		}
		if c.ProjectID == target {
			label := m.projectLabelFor(target)
			if target == "" {
				label = "No project"
			}
			m.dock.SetNotice("already in " + label + " — nothing to move")
			return nil
		}
		break
	}
	return m.setConversationProject(convID, target)
}

// onRailProjectsApplied refreshes an OPEN picker after a project load, so the list is never stale behind the
// overlay: /project on a cold rail would otherwise show only the conversations' own ids and no project names.
func (m *App) onRailProjectsApplied() {
	if m.projectPick == nil {
		return
	}
	keep := m.projectPick.sel
	// REBUILD THROUGH THE SAME PURPOSE AND FILTER the picker was opened with, so a list that lands after the
	// overlay opened can neither silently widen it past what the operator asked for NOR re-offer "All projects"
	// as a move target (which is why this goes through pickerOptions rather than rebuilds the list itself).
	m.projectPick.options = m.pickerOptions(m.projectPick.moveConvID, m.projectPick.filter)
	if keep < len(m.projectPick.options) {
		m.projectPick.sel = keep
	} else if len(m.projectPick.options) > 0 {
		m.projectPick.sel = len(m.projectPick.options) - 1
	} else {
		m.projectPick.sel = 0
	}
}
