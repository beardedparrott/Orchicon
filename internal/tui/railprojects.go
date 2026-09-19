package tui

// railprojects.go — the harness that puts every PROJECT in the conversations rail.
//
// The operator: "For every project that is created (active or otherwise), there should be a list that can be
// dragged to and also created from."
//
// THE RAIL CANNOT DERIVE THE PROJECT LIST FROM THE CONVERSATIONS. It has to know about projects with ZERO
// conversations — those are precisely the empty folders the operator wants to be able to move a chat INTO — and
// a list derived from the conversations can only ever contain projects that already have one. So the shell
// holds the project list alongside the conversation list and refreshes them together, which is also what keeps
// a project created in the other client from being invisible here until a relaunch.

import (
	"context"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// projectStatusWord renders the proto enum as the domain word the server uses for a project's status
// (internal/project's statusFromProto), so the rail's "archived" marker and the conversation-project block the
// agent receives say the same thing about the same project.
//
// UNSPECIFIED maps to "" — deliberately: a status the client cannot name is better rendered as nothing than as
// a word that might be wrong, and the rail treats "" as "do not mark it".
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

// projLabelForConv names the project a conversation sits in, for a rail row. It returns "" when the
// conversation is unassigned — an unassigned chat is not "in" anything, and inventing a label for it would
// put a word on a row where the absence is the fact.
//
// An ARCHIVED project still names itself, because the association rule is "active or otherwise" and a chat
// parked in an archived workspace is exactly the case where the operator needs telling.
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

// resolveProjectRef was removed with the argument-as-selection form of the command: /projects takes a FILTER
// now, and moving a conversation goes through the picker's own action (see projectPickerKey), so nothing needs
// to turn a project name into an id from a slash argument any more.

// setConversationProject moves the OPEN conversation into a project (or unassigns it) — the write behind
// /project, and the same rpc the GUI's folder drop target calls.
//
// It refuses with a notice rather than a silent no-op when there is nothing open, because /project with no
// conversation would otherwise look like it worked.
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
}

// railProjectsMsg carries the project list back to the shell.
type railProjectsMsg struct {
	Projects []railProject
	Err      string
}

// loadRailProjects fetches the tenant's projects for the rail's grouping.
//
// ARCHIVED PROJECTS ARE INCLUDED, which is the operator's "active or otherwise": a chat parked in an archived
// project must still be findable in its folder, and the folder has to exist for the association to render at
// all. The status travels with each project so the rail can mark it rather than silently presenting an
// archived project as a working one.
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
			// NOT A RAIL FAILURE. The conversations loaded; only the folder names are missing, and the rail
			// still renders every conversation under its project id. Reporting this as the rail's error state
			// would blank a working list over a cosmetic miss, so it degrades quietly.
			return railProjectsMsg{Err: err.Error()}
		}
		out := make([]railProject, 0, len(resp.Msg.GetProjects()))
		for _, p := range resp.Msg.GetProjects() {
			out = append(out, railProject{
				ID: p.GetId(), Name: p.GetName(), Status: projectStatusWord(p.GetStatus()),
				Dir: strings.TrimSpace(p.GetProjectDir()),
			})
		}
		return railProjectsMsg{Projects: out}
	}
}

// onRailProjects applies a fetched project list.
func (m *App) onRailProjects(msg railProjectsMsg) tea.Cmd {
	if msg.Err != "" {
		// LEFT UNLOADED ON FAILURE, deliberately: railProjectsLoaded stays false so the next conversations load
		// retries. Marking it loaded on a failure would leave the operator with an empty picker for the rest of
		// the session over one transient error.
		return nil
	}
	m.railProjects = msg.Projects
	m.railProjectsLoaded = true
	// THE WORKSPACE FROM THE LAUNCH DIRECTORY, when the operator has not chosen one.
	//
	// The operator: "The default project in the TUI should be the one based on the directory you launched it
	// from." It is applied HERE rather than at startup because the project list is what answers it — there is
	// nothing to match a directory against until it arrives — and only while the scope is UNCHOSEN, so it can
	// never override a deliberate selection (including one made in the moment before the list landed).
	m.applyLaunchDirScope()
	// A picker opened on a cold rail showed only the conversations' own ids; now that the names have arrived,
	// refresh the list behind the overlay so /project never presents raw ids to someone who just asked for it.
	m.onRailProjectsApplied()
	// The cursor can be past the end of a shorter or longer list now.
	if m.convSel >= len(m.railRows()) {
		m.convSel = max(0, len(m.railRows())-1)
	}
	return nil
}

// applyLaunchDirScope points the rail at the workspace the operator launched from, once, and only while they
// have not chosen for themselves.
//
// A directory that matches NOTHING leaves the scope at All projects: an unmatched launch directory is not a
// reason to hide the conversation list, and inventing a default workspace would be worse than none.
func (m *App) applyLaunchDirScope() {
	if m.projectScopeChosen || m.launchDir == "" || m.projectScope != projectScopeAll {
		return
	}
	p, ok := m.projectForDir(m.launchDir)
	if !ok {
		return
	}
	m.projectScope = p.ID
}

// projectForDir resolves the project a directory belongs to, using the SAME boundary rule the launch prompt
// uses (dirInsideProject) — one rule, so the prompt cannot call a directory attached while the rail cannot name
// its project.
//
// The DEEPEST match wins. A directory can sit inside two projects (a nested repo, or a project whose dir is a
// parent of another's), and the more specific one is the workspace the operator is actually in.
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

// activeProjectID is the project a NEW conversation belongs to: the selected workspace, when it names a real
// project.
//
// Empty for All projects and for the No-project scope, which is what those scopes mean. A scope naming a project
// the rail does not know (a stale selection) is also empty rather than passed through: the server REJECTS an
// unknown project id on create, so forwarding one would fail the send outright instead of filing the chat as
// unassigned.
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
