// Package work implements the Work screen: Projects, Work Items (full CRUD
// with Tree / Board / Archive views) and Runtime Images (CRUD + live build
// logs), backed by ProjectService, WorkItemService and RuntimeImageService.
// Every write goes through the kit2 mutation executor (optimistic apply,
// rollback on failure, source reconciliation).
//
// Collection semantics honoured here (the platform contract):
//   - the hierarchy is max 4 levels (epic → feature → task → subtask); a
//     kind switch re-resolves it and epic/feature are non-schedulable —
//     both enforced server-side and surfaced as-is,
//   - archiving is blocked while an item has children (server-enforced),
//   - ONLY ReorderWorkItems mutates sequence order: the Tree and Board are
//     display groupings and never renumber anything.
package work

import (
	"context"
	"strconv"
	"strings"
	"sync"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/mutate"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/stream"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// Source names (also the slug half of the shell's nav commands).
const (
	srcProjects  = "projects"
	srcWorkItems = "workitems"
	srcImages    = "images"
)

// projectOpt / workflowOpt are the create form's select options.
type projectOpt struct{ ID, Name string }
type workflowOpt struct{ ID, Name string }

// Model is the Work screen.
type Model struct {
	kit2.Base
	cl          *client.Clients
	reg         *subs.Registry
	tenantID    string // "" lets the plane resolve it from the credential
	sub         *stream.Sub[*apiv1.StreamProjectEventsResponse]
	reconnected bool

	w, h int

	// viewMu guards the work-items display grouping (it is read from the
	// fetch closure, which runs off the update loop).
	viewMu sync.Mutex
	view   viewMode

	// create-form option lists (loaded before the form opens).
	projects  []projectOpt
	workflows []workflowOpt

	// form is the open typed form (nil = closed).
	form     *kit2.Form
	formMode string
	formID   string

	// pending is the action the open confirmation dialog will run.
	pending *kit2.Action
	bar     *kit2.ActionBar

	// build is the live runtime-image build log (kit2 Stream: appends
	// preserve the operator's scroll offset).
	build       *kit2.Stream
	buildStream *connect.ServerStreamForClient[apiv1.BuildRuntimeImageResponse]
	buildCancel context.CancelFunc
	buildID     string
	building    bool
	buildStatus string

	notice string
}

// New builds the screen. The project-events subscription lives for the
// screen's lifetime and is closed by Close (tab switch = unsubscribe).
func New(cl *client.Clients, reg *subs.Registry, tenantID string) *Model {
	m := &Model{cl: cl, reg: reg, tenantID: tenantID, view: viewTree}
	m.NameStr = "work"
	m.AddSource(srcProjects, "Projects", m.fetchProjects)
	m.AddSource(srcWorkItems, "Work Items", m.fetchWorkItems)
	m.AddSource(srcImages, "Runtime Images", m.fetchImages)
	m.SetDetail(m.detail)
	m.Base.SetSourceEmpty(srcProjects, "no projects yet — press n to create one")
	m.Base.SetSourceEmpty(srcWorkItems, "no work items in this view — press n to create one, v to switch Tree/Board/Archive")
	m.Base.SetSourceEmpty(srcImages, "no runtime images yet — press n to define one, b to build")
	m.bar = kit2.NewActionBar()
	m.build = kit2.NewStream("build log", 80, 20)
	m.Base.SetStatuses([]screenkit.StatusMsg{
		{Name: "project-events", Status: "idle"},
	})
	return m
}

func (m *Model) Name() string { return "work" }

// EnsureSubscriptions starts the project-events live stream once
// (idempotent; the shell calls it on every switch to this tab).
func (m *Model) EnsureSubscriptions() {
	if m.sub == nil {
		m.sub = m.reg.ProjectEvents(m.cl, m.tenantID)
	}
}

// Close unsubscribes (useStream: navigating away unsubscribes) and tears
// down any in-flight build stream.
func (m *Model) Close() {
	if m.buildCancel != nil {
		m.buildCancel()
		m.buildCancel = nil
	}
	m.reg.CloseAll()
}

func (m *Model) SetSize(w, h int) {
	m.w, m.h = w, h
	m.Base.SetSize(w, h)
	if m.build != nil {
		m.build.SetSize(w-4, h-6)
	}
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.Load(), m.reg.WaitStatus("project-events"))
}

// ClaimsKeys reports whether the screen owns every key right now (an open
// form, confirmation dialog or build log). The shell consults it before its
// own routes so a typed character is never stolen ('q' would quit, space
// would open the tab menu, '/' the palette).
func (m *Model) ClaimsKeys() bool { return m.form != nil || m.Open != nil }

// ActiveForm returns the open form (nil when closed) — tests and the shell
// read the in-progress input through it.
func (m *Model) ActiveForm() *kit2.Form { return m.form }

// DialogOpen reports whether a confirmation dialog is up.
func (m *Model) DialogOpen() bool { return m.Open != nil }

// Notice returns the last action's status line ("" = none).
func (m *Model) Notice() string { return m.notice }

// ViewMode returns the work-items display grouping.
func (m *Model) ViewMode() viewMode {
	m.viewMu.Lock()
	defer m.viewMu.Unlock()
	return m.view
}

// Building reports whether a runtime-image build is streaming.
func (m *Model) Building() bool { return m.building }

// BuildLog returns the live build log text.
func (m *Model) BuildLog() string { return m.build.View() }

// ---------------- fetches ----------------

func (m *Model) fetchProjects(ctx context.Context, pageToken string) ([]kit2.Item, string, error) {
	resp, err := m.cl.Projects.ListProjects(ctx, connect.NewRequest(&apiv1.ListProjectsRequest{
		PageSize:  100,
		PageToken: pageToken,
	}))
	if err != nil {
		return nil, "", err
	}
	items := make([]kit2.Item, 0, len(resp.Msg.Projects))
	for _, p := range resp.Msg.Projects {
		meta := strings.ToLower(strings.TrimPrefix(p.GetStatus().String(), "PROJECT_STATUS_"))
		if dir := p.GetProjectDir(); dir != "" {
			meta += " · " + dir
		}
		items = append(items, kit2.Item{ID: p.GetId(), Title: p.GetName(), Meta: meta})
	}
	return items, resp.Msg.NextPageToken, nil
}

// fetchWorkItems renders the active work-items page through the selected
// display grouping (tree / board / archive).
func (m *Model) fetchWorkItems(ctx context.Context, pageToken string) ([]kit2.Item, string, error) {
	view := m.ViewMode()
	resp, err := m.cl.WorkItems.ListWorkItems(ctx, connect.NewRequest(&apiv1.ListWorkItemsRequest{
		PageSize:        200,
		PageToken:       pageToken,
		IncludeArchived: view == viewArchive,
		RecurringFilter: apiv1.RecurringFilter_RECURRING_FILTER_EXCLUDE_RECURRING,
		IdeaScope:       apiv1.IdeaScope_IDEA_SCOPE_EXCLUDE_IDEA,
	}))
	if err != nil {
		return nil, "", err
	}
	return rowsFor(view, resp.Msg.GetWorkItems()), resp.Msg.GetNextPageToken(), nil
}

func (m *Model) fetchImages(ctx context.Context, pageToken string) ([]kit2.Item, string, error) {
	resp, err := m.cl.Images.ListRuntimeImages(ctx, connect.NewRequest(&apiv1.ListRuntimeImagesRequest{
		PageSize:  100,
		PageToken: pageToken,
	}))
	if err != nil {
		return nil, "", err
	}
	items := make([]kit2.Item, 0, len(resp.Msg.GetRuntimeImages()))
	for _, img := range resp.Msg.GetRuntimeImages() {
		meta := strings.ToLower(strings.TrimPrefix(img.GetStatus().String(), "RUNTIME_IMAGE_STATUS_"))
		if img.GetBuiltVersion() > 0 && img.GetBuiltVersion() < img.GetVersion() {
			meta += " · rebuild pending"
		}
		items = append(items, kit2.Item{ID: img.GetId(), Title: img.GetName(), Meta: meta})
	}
	return items, resp.Msg.GetNextPageToken(), nil
}

// ---------------- detail ----------------

func (m *Model) detail(ctx context.Context, src, id string) (string, []kit2.Field, string, error) {
	switch src {
	case srcProjects:
		resp, err := m.cl.Projects.GetProject(ctx, connect.NewRequest(&apiv1.GetProjectRequest{Id: id}))
		if err != nil {
			return "", nil, "", err
		}
		p := resp.Msg.GetProject()
		fields := []kit2.Field{
			{Key: "id", Value: p.GetId()},
			{Key: "name", Value: p.GetName()},
			{Key: "slug", Value: p.GetSlug()},
			{Key: "status", Value: strings.ToLower(strings.TrimPrefix(p.GetStatus().String(), "PROJECT_STATUS_"))},
			{Key: "repo", Value: p.GetRepoSlug()},
			{Key: "project dir", Value: p.GetProjectDir()},
			{Key: "default image", Value: p.GetDefaultRuntimeImage()},
			{Key: "max concurrent", Value: screenkit.FmtInt(int(p.GetMaxConcurrentRuns()))},
			{Key: "created", Value: screenkit.FmtTime(p.GetCreatedAt())},
			{Key: "updated", Value: screenkit.FmtTime(p.GetUpdatedAt())},
		}
		return "Project: " + p.GetName(), fields, p.GetGoals(), nil

	case srcWorkItems:
		resp, err := m.cl.WorkItems.GetWorkItem(ctx, connect.NewRequest(&apiv1.GetWorkItemRequest{Id: id}))
		if err != nil {
			return "", nil, "", err
		}
		w := resp.Msg.GetWorkItem()
		fields := []kit2.Field{
			{Key: "id", Value: w.GetId()},
			{Key: "title", Value: w.GetTitle()},
			{Key: "kind", Value: kindBadge(w.GetKind())},
			{Key: "status", Value: statusPill(w.GetStatus())},
			{Key: "parent", Value: w.GetParentId()},
			{Key: "project", Value: w.GetProjectId()},
			{Key: "priority", Value: screenkit.FmtInt(int(w.GetPriority()))},
			{Key: "budgets", Value: w.GetBudgets()},
			{Key: "context window", Value: screenkit.FmtInt(int(w.GetContextWindow()))},
			{Key: "runtime image", Value: w.GetRuntimeImage()},
			{Key: "worker", Value: w.GetAssignedWorkerRef()},
			{Key: "workflow", Value: w.GetWorkflowId()},
			{Key: "workflow run", Value: w.GetWorkflowRunId()},
			{Key: "auto-start", Value: boolStr(w.GetAutoStartWorkflow())},
			{Key: "scheduled", Value: screenkit.FmtTime(w.GetScheduledStartAt())},
			{Key: "sort order", Value: strconv.FormatFloat(w.GetSortOrder(), 'g', -1, 64)},
			{Key: "archived from", Value: w.GetArchivedFromStatus()},
			{Key: "updated", Value: screenkit.FmtTime(w.GetUpdatedAt())},
		}
		return "Work Item: " + w.GetTitle(), fields, detailBody(w), nil

	case srcImages:
		resp, err := m.cl.Images.GetRuntimeImage(ctx, connect.NewRequest(&apiv1.GetRuntimeImageRequest{Id: id}))
		if err != nil {
			return "", nil, "", err
		}
		img := resp.Msg.GetRuntimeImage()
		fields := []kit2.Field{
			{Key: "id", Value: img.GetId()},
			{Key: "name", Value: img.GetName()},
			{Key: "slug", Value: img.GetSlug()},
			{Key: "tag", Value: img.GetTag()},
			{Key: "status", Value: strings.ToLower(strings.TrimPrefix(img.GetStatus().String(), "RUNTIME_IMAGE_STATUS_"))},
			{Key: "base", Value: img.GetBaseImageRef()},
			{Key: "source", Value: img.GetSource()},
			{Key: "version", Value: screenkit.FmtInt(int(img.GetVersion()))},
			{Key: "built version", Value: screenkit.FmtInt(int(img.GetBuiltVersion()))},
			{Key: "apt packages", Value: img.GetAptPackages()},
			{Key: "toolchains", Value: img.GetToolchains()},
			{Key: "env", Value: img.GetEnv()},
			{Key: "updated", Value: screenkit.FmtTime(img.GetUpdatedAt())},
		}
		body := img.GetDescription()
		if df := img.GetDockerfileOverride(); df != "" {
			body += "\n\ndockerfile override:\n" + df
		}
		if log := img.GetBuildLog(); log != "" {
			body += "\n\nlast build log:\n" + log
		}
		if f := img.GetFailureReason(); f != "" {
			body += "\n\nfailure: " + f
		}
		return "Runtime Image: " + img.GetName(), fields, strings.TrimSpace(body), nil
	}
	return "", nil, "", nil
}

// ---------------- actions (confirm + mutation executor) ----------------

// actionsForSelection builds the entity-bound actions for the focused row.
func (m *Model) actionsForSelection() []kit2.Action {
	switch m.ActiveSourceName() {
	case srcProjects:
		return m.projectActions()
	case srcImages:
		return m.imageActions()
	default:
		return m.itemActions()
	}
}

// openAction opens the confirmation dialog for an action that needs one, or
// runs it immediately.
func (m *Model) openAction(a kit2.Action) tea.Cmd {
	if !a.NeedsConfirm() {
		return m.runAction(a)
	}
	d := kit2.Confirm(a.Label, a.Confirm, a.Label)
	d.Danger = a.Danger
	m.Open = d
	pending := a
	m.pending = &pending
	m.OnDialog = func(choice string) tea.Cmd {
		pa := m.pending
		m.pending = nil
		m.OnDialog = nil
		if pa == nil || choice == "" {
			m.notice = "cancelled"
			return nil // dismissed
		}
		m.notice = choice + " confirmed"
		return m.runAction(*pa)
	}
	return nil
}

func (m *Model) runAction(a kit2.Action) tea.Cmd {
	return m.Mutate(mutate.Request{
		Name: a.Label, Source: a.Source,
		Apply: a.Apply, Rollback: a.Rollback, Do: a.Do,
	})
}

// actionByKey returns the action bound to a key for the focused source.
func (m *Model) actionByKey(key string) (kit2.Action, bool) {
	for _, a := range m.actionsForSelection() {
		if a.Key == key {
			return a, true
		}
	}
	return kit2.Action{}, false
}

// switchView changes the work-items display grouping and re-fetches. It
// never writes: a display grouping cannot renumber the sequence.
func (m *Model) switchView(v viewMode) tea.Cmd {
	m.viewMu.Lock()
	m.view = v
	m.viewMu.Unlock()
	m.notice = "work items: " + string(v) + " view"
	return m.Refresh(srcWorkItems)
}

// ---------------- update ----------------

func (m *Model) Update(msg tea.Msg) (screenkit.Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
		return m, nil

	case subs.StatusMsg:
		m.Base.SetStatus(msg.Name, string(msg.Status))
		// Re-arm the status wait; a transition back to open after a drop
		// means events may have been missed — refetch page 1.
		cmd := m.reg.WaitStatus("project-events")
		if msg.Status == "open" && m.reconnected {
			return m, tea.Batch(cmd, m.Load())
		}
		if msg.Status != "open" {
			m.reconnected = true
		}
		return m, cmd

	case itemFormMsg:
		if msg.err != nil {
			m.notice = "couldn't open the form: " + msg.err.Error()
			return m, nil
		}
		switch msg.mode {
		case formCreateItem:
			if len(msg.projects) == 0 {
				m.notice = "no projects yet — a work item belongs to a project"
				return m, nil
			}
			m.projects, m.workflows = msg.projects, msg.workflows
			m.form = m.newItemCreateForm()
		case formEditItem:
			m.form = m.newItemEditForm(msg.item)
		case formStatusItem:
			m.form = m.newItemStatusForm(msg.item)
		case formAssignItem:
			m.form = m.newItemAssignForm(msg.item)
		case formScheduleItem:
			m.form = m.newItemScheduleForm(msg.item)
		}
		m.formMode, m.formID = msg.mode, msg.item.GetId()
		m.notice = ""
		return m, nil

	case projectFormMsg:
		if msg.err != nil {
			m.notice = "couldn't open the form: " + msg.err.Error()
			return m, nil
		}
		switch msg.mode {
		case formCreateProject:
			m.form = m.newProjectCreateForm()
		case formEditProject:
			m.form = m.newProjectEditForm(msg.project)
		case formProjectDir:
			m.form = m.newProjectDirForm(msg.project)
		}
		m.formMode, m.formID = msg.mode, msg.project.GetId()
		m.notice = ""
		return m, nil

	case imageFormMsg:
		if msg.err != nil {
			m.notice = "couldn't open the form: " + msg.err.Error()
			return m, nil
		}
		switch msg.mode {
		case formCreateImage:
			m.form = m.newImageCreateForm()
		case formEditImage:
			m.form = m.newImageEditForm(msg.image)
		}
		m.formMode, m.formID = msg.mode, msg.image.GetId()
		m.notice = ""
		return m, nil

	case buildOpenMsg:
		if msg.err != nil {
			m.building = false
			m.notice = "build failed to start: " + msg.err.Error()
			return m, nil
		}
		m.buildStream, m.buildCancel, m.buildID = msg.stream, msg.cancel, msg.id
		m.building, m.buildStatus = true, "building"
		m.build.SetLines(nil)
		m.build.Append("building " + msg.tag + "…")
		m.MutateRow(srcImages, msg.id, func(r *kit2.Row) { r.Meta = "building" })
		m.renderBuildDetail(msg.tag)
		return m, m.readBuildChunk()

	case buildChunkMsg:
		return m, m.handleBuildChunk(msg)

	case tea.KeyMsg:
		// The open form owns every key while it is up (esc closes it; enter
		// on the last field submits through the form's own validation).
		if m.form != nil {
			if msg.String() == "esc" {
				m.form = nil
				m.notice = "cancelled"
				return m, nil
			}
			cmd, _ := m.form.HandleKey(msg)
			if m.form.Submitted {
				m.notice = "saved (" + m.formMode + ")"
				m.form = nil
			}
			return m, cmd
		}
		if m.Open != nil {
			break // the dialog owns every key — kit2.Base resolves it
		}
		if cmd, handled := m.handleKey(msg); handled {
			return m, cmd
		}
	}

	if handled, cmd := m.Base.Update(msg); handled {
		return m, cmd
	}
	return m, nil
}

// handleBuildChunk folds one streamed build chunk into the log + status.
// handled=false falls through when nothing is streaming.
func (m *Model) handleBuildChunk(msg buildChunkMsg) tea.Cmd {
	if msg.chunk != nil {
		c := msg.chunk
		for _, line := range strings.Split(strings.TrimRight(c.GetLog(), "\n"), "\n") {
			if line != "" {
				m.build.Append(line)
			}
		}
		if c.GetStatus() != apiv1.RuntimeImageStatus_RUNTIME_IMAGE_STATUS_UNSPECIFIED {
			m.buildStatus = strings.ToLower(strings.TrimPrefix(c.GetStatus().String(), "RUNTIME_IMAGE_STATUS_"))
			m.MutateRow(srcImages, m.buildID, func(r *kit2.Row) { r.Meta = m.buildStatus })
		}
		if c.GetError() != "" {
			m.build.Append("error: " + c.GetError())
		}
		if c.GetSkipped() {
			m.build.Append("build skipped — the spec is unchanged (image already up to date)")
		}
		if c.GetFailureReason() != "" {
			m.build.Append("failure: " + c.GetFailureReason())
		}
		m.renderBuildDetail(c.GetTag())
		if isTerminalBuildStatus(c.GetStatus()) {
			m.finishBuild()
			return m.Refresh(srcImages)
		}
		return m.readBuildChunk()
	}
	if msg.err != nil {
		m.build.Append("stream ended: " + msg.err.Error())
	}
	m.finishBuild()
	return m.Refresh(srcImages)
}

func isTerminalBuildStatus(s apiv1.RuntimeImageStatus) bool {
	return s == apiv1.RuntimeImageStatus_RUNTIME_IMAGE_STATUS_READY || s == apiv1.RuntimeImageStatus_RUNTIME_IMAGE_STATUS_FAILED
}

// finishBuild closes the stream and clears the in-flight state.
func (m *Model) finishBuild() {
	if m.buildCancel != nil {
		m.buildCancel()
		m.buildCancel = nil
	}
	m.buildStream = nil
	m.building = false
	m.notice = "build " + m.buildStatus
}

// renderBuildDetail pushes the live log into the detail pane (the Stream
// widget owns scroll preservation; the pane is where the operator sees it).
func (m *Model) renderBuildDetail(tag string) {
	title := "Build: " + tag
	m.Base.SetDetailContent(title, []kit2.Field{
		{Key: "status", Value: m.buildStatus},
		{Key: "scroll", Value: m.build.ScrollLabel()},
	}, m.build.View())
}

// handleKey implements the screen's own keys. handled=false falls through to
// the shared list/detail navigation.
func (m *Model) handleKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	src := m.ActiveSourceName()
	switch msg.String() {
	case "n":
		switch src {
		case srcProjects:
			return m.openForm(m.newProjectCreateForm(), formCreateProject, ""), true
		case srcImages:
			return m.openForm(m.newImageCreateForm(), formCreateImage, ""), true
		default:
			return m.prepCreateItem(), true
		}
	case "e":
		switch src {
		case srcProjects:
			return m.prepEditProject(formEditProject), true
		case srcImages:
			return m.prepEditImage(), true
		default:
			return m.prepEditItem(formEditItem), true
		}
	case "d":
		if src == srcProjects {
			return m.prepEditProject(formProjectDir), true
		}
	case "s":
		if src == srcWorkItems {
			return m.prepEditItem(formStatusItem), true
		}
	case "w":
		if src == srcWorkItems {
			return m.prepEditItem(formAssignItem), true
		}
	case "t":
		if src == srcWorkItems {
			return m.prepEditItem(formScheduleItem), true
		}
	case "b":
		if src == srcImages {
			return m.startBuild(), true
		}
	case "v":
		if src == srcWorkItems {
			return m.switchView(m.ViewMode().next()), true
		}
	case "T":
		if src == srcWorkItems {
			return m.switchView(viewTree), true
		}
	case "B":
		if src == srcWorkItems {
			return m.switchView(viewBoard), true
		}
	case "Z":
		if src == srcWorkItems {
			return m.switchView(viewArchive), true
		}
	case "J":
		if src == srcWorkItems && m.ViewMode() != viewBoard {
			return m.reorderChildren(1), true
		}
	case "K":
		if src == srcWorkItems && m.ViewMode() != viewBoard {
			return m.reorderChildren(-1), true
		}
	case "y", "W", "a", "R", "x":
		if a, ok := m.actionByKey(msg.String()); ok {
			return m.openAction(a), true
		}
	}
	return nil, false
}

// openForm installs a locally-built form (the project + image forms need no
// option-list round trip).
func (m *Model) openForm(f *kit2.Form, mode, id string) tea.Cmd {
	m.form, m.formMode, m.formID = f, mode, id
	m.notice = ""
	return nil
}

// ---------------- view ----------------

func (m *Model) View() string {
	m.refreshActionBar()

	body := m.Base.View()
	if m.notice != "" {
		body += "\n" + theme.HintText.Render(m.notice)
	}
	body += "\n" + m.HintLine()
	if m.w > 0 && m.h > 0 {
		body = kit2.FitLines(body, m.w, m.h)
		if m.form != nil {
			body = kit2.Center(body, formBox(m.form, m.w), m.w, m.h)
		} else if m.Open != nil {
			box := m.Open.Box(minInt(64, m.w-4), minInt(12, m.h-2))
			body = kit2.Center(body, box, m.w, m.h)
		}
		return kit2.FitLines(body, m.w, m.h)
	}
	if m.form != nil {
		return body + "\n" + formBox(m.form, 70)
	}
	return body
}

// formBox wraps the typed form in a titled dialog-sized box.
func formBox(f *kit2.Form, w int) string {
	bw := minInt(76, w-4)
	if bw < 24 {
		bw = 24
	}
	d := &kit2.Dialog{Title: f.Title, Body: f.View(), Buttons: []string{"submit", "cancel"}}
	return d.Box(bw, minInt(26, len(f.Specs)*2+5))
}

func (m *Model) refreshActionBar() {
	actions := m.actionsForSelection()
	m.bar.Actions = actions
	if m.bar.Sel >= len(actions) {
		m.bar.Sel = 0
	}
	m.Base.Bar = m.bar
}

// HintLine is the screen's key cheat-sheet.
func (m *Model) HintLine() string {
	switch m.ActiveSourceName() {
	case srcProjects:
		return theme.HintText.Render("n: new project · e: edit (name/goals/dir) · d: set+create project dir · enter: detail · ←/→: pane")
	case srcImages:
		return theme.HintText.Render("n: new image · e: edit spec · b: build (live logs) · x: delete (confirm) · enter: detail")
	default:
		return theme.HintText.Render("n: new · e: edit · s: status/priority · t: schedule · w: assign · W: unassign · y: auto-start · J/K: reorder · a: archive · x: delete · v/T/B/Z: tree/board/archive")
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// SelectSource focuses the named source (slash nav command support).
func (m *Model) SelectSource(name string) bool { return m.Base.SelectSource(name) }

// SelectItem selects the item by ID in the named source (slash arg
// jumps); detail loads via RequestDetail when the item is not paged in.
func (m *Model) SelectItem(src, id string) bool { return m.Base.SelectItem(src, id) }

// RequestDetail loads the detail view for (src, id) directly.
func (m *Model) RequestDetail(src, id string) tea.Cmd { return m.Base.RequestDetail(src, id) }

// ActiveSourceName / ActiveItem expose the Base focus state to the shell's
// context engine.
func (m *Model) ActiveSourceName() string           { return m.Base.ActiveSourceName() }
func (m *Model) ActiveItem() (screenkit.Item, bool) { return m.Base.ActiveItem() }
