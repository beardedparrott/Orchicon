package tui

// The operator: "For every project that is created (active or otherwise), there should be a list
// that can be dragged to and also created from." THE RAIL CANNOT DERIVE THE PROJECT LIST.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// projectStatusWord renders the proto enum as the domain word the server uses for a project's
// status (internal/project's statusFromProto).
func projectStatusWord(st apiv1.ProjectStatus) string {
	switch st {
	case apiv1.ProjectStatus_PROJECT_STATUS_DRAFTING:
		return "drafting"
	case apiv1.ProjectStatus_PROJECT_STATUS_ACTIVE:
		return "active"
	case apiv1.ProjectStatus_PROJECT_STATUS_PAUSED:
		return "paused"
	case apiv1.ProjectStatus_PROJECT_STATUS_ARCHIVED:
		return "archived"
	case apiv1.ProjectStatus_PROJECT_STATUS_DELETED:
		return "deleted"
	default:
		return ""
	}
}

// projLabelForConv names the project a conversation sits in, for a rail row.
func (m *App) projLabelForConv(c chat.Conversation) string {
	if c.ProjectID == "" {
		return ""
	}
	for _, p := range m.railProjects {
		if p.ID == c.ProjectID {
			return p.Name
		}
	}
	// Not in the project list: the column carries no foreign key, so this is a stale association. The raw id is
	// ugly and honest, and it is better than silently rendering the chat as unassigned when it is not.
	return c.ProjectID
}

// projectLabelFor renders a project id for a notice: its name, the raw id when the project is unknown (a stale
// association), or "Unassigned" for the empty id.
func (m *App) projectLabelFor(projectID string) string {
	if projectID == "" {
		return "Unassigned"
	}
	for _, p := range m.railProjects {
		if p.ID == projectID {
			return p.Name
		}
	}
	return projectID
}

// resolveProjectRef was removed with the argument-as-selection form of the command: /project takes
// a FILTER now, and moving a conversation goes through the picker's own action (see.

// setConversationProject moves the OPEN conversation into a project (or unassigns it) — the write
// behind /project, and the same rpc the GUI's folder drop target calls.
func (m *App) setConversationProject(convID, projectID string) tea.Cmd {
	if convID == "" {
		m.dock.SetError("no conversation is open — /project applies to the chat you are in")
		return nil
	}
	if m.chat == nil {
		return nil
	}
	return m.chat.SetConversationProject(convID, projectID)
}

// railProject is the minimum the rail needs to draw a project folder — plus its DIRECTORY, which is what ties a
// launch directory to a workspace.
type railProject struct {
	ID     string
	Name   string
	Status string
	// Dir is the project's project_dir. It is here so the rail can answer "which workspace was I launched in?"
	// with the SAME boundary rule the launch prompt uses (dirInsideProject) rather than a second one.
	Dir string
	// SkillFiles is the project's skill_files path list, for the SHOWING half of the conversation scope
	// modal: a conversation's skills are the UNION of its own and its project's (the same union.
	SkillFiles []string
}

// railProjectsMsg carries the project list back to the shell.
type railProjectsMsg struct {
	Projects []railProject
	Err      string
}

// loadRailProjects fetches the tenant's projects for the rail's grouping.
func (m *App) loadRailProjects() tea.Cmd {
	if m.clients == nil {
		return nil
	}
	cl := m.clients
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		resp, err := cl.Projects.ListProjects(ctx, connect.NewRequest(&apiv1.ListProjectsRequest{
			PageSize: 200,
		}))
		if err != nil {
			// NOT A RAIL FAILURE.
			return railProjectsMsg{Err: err.Error()}
		}
		out := make([]railProject, 0, len(resp.Msg.GetProjects()))
		for _, p := range resp.Msg.GetProjects() {
			out = append(out, railProject{
				ID: p.GetId(), Name: p.GetName(), Status: projectStatusWord(p.GetStatus()),
				Dir:        strings.TrimSpace(p.GetProjectDir()),
				SkillFiles: p.GetSkillFiles(),
			})
		}
		return railProjectsMsg{Projects: out}
	}
}

// onRailProjects applies a fetched project list.
func (m *App) onRailProjects(msg railProjectsMsg) tea.Cmd {
	if msg.Err != "" {
		// LEFT UNLOADED ON FAILURE, deliberately: railProjectsLoaded stays false so the next conversations
		// load retries.
		return nil
	}
	m.railProjects = msg.Projects
	m.railProjectsLoaded = true
	// A SELECTION DECIDED BEFORE THE LIST ARRIVED (projectselect.go).
	if name := m.pendingProjectName; name != "" {
		m.pendingProjectName = ""
		m.selectProjectByName(name)
	}
	// THE WORKSPACE FROM THE LAUNCH DIRECTORY, when the operator has not chosen one.
	m.applyLaunchDirScope()
	// A picker opened on a cold rail showed only the conversations' own ids; now that the names have arrived,
	// refresh the list behind the overlay so /project never presents raw ids to someone who just asked for it.
	m.onRailProjectsApplied()
	// The cursor can be past the end of a shorter or longer list now.
	if m.convSel >= len(m.railRows()) {
		m.convSel = max(0, len(m.railRows())-1)
	}
	// AND THE WORKSPACE REACHES THE SCREENS IN THIS SAME CYCLE.
	return m.pushProjectScope()
}

// applyLaunchDirScope points the rail at the workspace the operator launched from, once, and only
// while they have not chosen for themselves.
func (m *App) applyLaunchDirScope() {
	if m.projectScopeChosen || m.launchDir == "" || m.projectScope != projectScopeAll {
		return
	}
	p, ok := m.projectForDir(m.launchDir)
	if !ok {
		return
	}
	m.projectScope = p.ID

	// AND IF THAT HID ANYTHING, SAY SO.
	var extra []string
	hidden := m.scopeHiddenCount()
	// Record what THIS notice already covers, so the list-reload path (noteScopeHidesNewRows) only speaks
	// up when a row arrives that this line did not account for — one voice for the same fact.
	m.scopeHiddenNoticed = hidden
	if hidden > 0 {
		extra = append(extra, fmt.Sprintf("scope: %s — %d conversation(s) not shown; /project to widen",
			projectScopeLabel(p.ID, projectScopeOptions(m.railProjects, m.conversations)), hidden))
	}
	// THE PALETTE FOLLOWS, same as any other scope change — see projecttheme.go.
	m.applyScopeTheme(false, extra...)
}

// noteScopeHidesNewRows re-announces an ACTIVE scope's narrowing when a re-read of the conversation
// list shows it is hiding a row the operator has not been told about.
func (m *App) noteScopeHidesNewRows() {
	hidden := m.scopeHiddenCount()
	if hidden == m.scopeHiddenNoticed {
		return // nothing new is being hidden: no notice, no churn on the status line
	}
	m.scopeHiddenNoticed = hidden
	if hidden == 0 {
		return // the scope no longer hides anything — there is nothing to warn about
	}
	m.dock.SetNotice(fmt.Sprintf("scope: %s — %d conversation(s) not shown; /project to widen",
		projectScopeLabel(m.projectScope, projectScopeOptions(m.railProjects, m.conversations)), hidden))
}

// scopeHiddenCount is how many of the LOADED conversations the active scope hides.
func (m *App) scopeHiddenCount() int {
	if m.projectScope == projectScopeAll {
		return 0
	}
	if hidden := len(m.conversations) - len(m.scopedConversations()); hidden > 0 {
		return hidden
	}
	return 0
}

// projectForDir resolves the project a directory belongs to, using the SAME boundary rule the
// launch prompt uses (dirInsideProject) — one rule.
func (m *App) projectForDir(dir string) (railProject, bool) {
	var best railProject
	bestLen := -1
	for _, p := range m.railProjects {
		if !dirInsideProject(p.Dir, dir) {
			continue
		}
		if n := len(strings.TrimSpace(p.Dir)); n > bestLen {
			best, bestLen = p, n
		}
	}
	return best, bestLen >= 0
}

// activeProjectID is the project a NEW conversation belongs to: the selected workspace, when it
// names a real project.
func (m *App) activeProjectID() string {
	if m.projectScope == projectScopeAll || m.projectScope == unassignedScope {
		return ""
	}
	for _, p := range m.railProjects {
		if p.ID == m.projectScope {
			return p.ID
		}
	}
	return ""
}
