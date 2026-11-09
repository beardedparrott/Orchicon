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
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

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
type projectOpt struct {
	ID, Name string
	// Mode is the project's execution mode, carried so the work-item form can say WHAT an inheriting
	// item inherits — the GUI's "Inherit from project (<mode>)".
	Mode apiv1.ExecutionMode
}
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

	// viewMu guards the work-items display grouping and the project filter (both
	// are read from the fetch closure, which runs off the update loop).
	viewMu sync.Mutex
	view   viewMode
	// projectFilter narrows the WORK ITEMS pane to one project, set by the shell from the active
	// workspace (SetActiveProject).
	projectFilter string
	// sort is the sibling DISPLAY order (the control next to the search box).
	sort sortMode

	// create-form option lists (loaded before the form opens).
	projects  []projectOpt
	workflows []workflowOpt
	// parents / images back the KPicker fields (parent work item, runtime
	// image): references chosen from a filtered list rather than typed ids.
	parents []kit2.Option
	images  []kit2.Option
	// parentKind maps a parent id to its kind, so the create form can DERIVE the child's kind from the
	// chosen parent.
	parentKind map[string]apiv1.WorkItemKind
	// parentProject maps a parent id to its project.
	parentProject map[string]string

	// names resolves the IDS the detail pane carries into the names an operator reads (workflow,
	// project, parent).
	names nameIndex

	// form is the open typed form (nil = closed).
	form     *kit2.Form
	formMode string
	formID   string

	// dtPicker is the open combined calendar + clock modal (a KDateTime field activated), layered ABOVE
	// the form it was opened from; dtField is the field it writes back into.
	dtPicker *kit2.DateTimePicker
	dtField  string

	// formLoading is true from the moment a modal is REQUESTED until its payload arrives.
	formLoading bool

	// bulkSetIDs is the SNAPSHOT of the marked ids taken when the bulk workflow/image picker was
	// opened.
	bulkSetIDs []string
	// parentIDs is the set of ids that have at least one child — the TUI's sequence-parent detector,
	// the counterpart of the GUI's parentIdSet.
	parentIDs map[string]bool

	// runsByItem is the page's PR index: work item id -> its runs' PR surfaces, newest first.
	runsByItem prIndex

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
	m := &Model{cl: cl, reg: reg, tenantID: tenantID, view: viewTree, sort: sortSequence}
	m.NameStr = "work"
	m.AddSource(srcProjects, "Projects", m.fetchProjects)
	m.AddSource(srcWorkItems, "Work Items", m.fetchWorkItems)
	m.AddSource(srcImages, "Runtime Images", m.fetchImages)
	m.SetDetail(m.detail)
	m.Base.SetSourceEmpty(srcProjects, "no projects yet — press n to create one")
	m.Base.SetSourceEmpty(srcWorkItems, "no work items in this view — press n to create one, or / to search")
	m.Base.SetSourceEmpty(srcImages, "no runtime images yet — press n to define one, b to build")
	// The mutation executor is installed HERE so every write has a feedback surface.
	m.Base.SetExecutor(&mutate.Executor{Sink: workSink{m}})
	// The Work Items list carries a search row ('/'), per the operator's "search
	// box at the top of the work items page for filter".
	m.Base.EnableFilter(srcWorkItems)
	// A WORK ITEM TREE OPENS COLLAPSED.
	m.Base.SetCollapsedByDefault(srcWorkItems, true)
	// ...and a MODE label on that same row, right beside the search box: the operator's "When in
	// archive or normal mode in the TUI.
	m.Base.SetCaption(srcWorkItems, func() string {
		if m.ViewMode() == viewArchive {
			return "ARCHIVE (archived items)"
		}
		return "TREE (active items)"
	})
	// ...and a clickable collapse/expand-all control next to it.
	m.syncRowActions()
	m.Base.OnItemsLanded = func() tea.Cmd {
		// Only the work-items pane has the control, and only its own landings matter — re-registering on
		// another source's landing would swap the actions out from under a pane the operator is looking at.
		if m.Base.ActiveSourceName() == srcWorkItems {
			m.syncRowActions()
		}
		return nil
	}
	m.bar = kit2.NewActionBar()
	m.build = kit2.NewStream("build log", 80, 20)
	// The inline detail editor reports its outcome here (the modal path did
	// this inline in Update; the editor lives in kit2.Base now).
	m.Base.SetOnEditDone(func(submitted bool) {
		if submitted {
			m.notice = "saved (" + m.formMode + ")"
			return
		}
		m.notice = "cancelled"
	})
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
	// CloseAll tears down EVERY subscription in the SHARED registry, so the screen must drop its own
	// handle too.
	m.sub = nil
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

// DropKeyClaim releases the screen's key claim so the focus chord can return the operator to the
// composer (see kit2.Base.DropKeyClaim).
func (m *Model) DropKeyClaim() {
	m.formLoading = false
	m.Base.DropKeyClaim()
}

// FormOpen reports whether a FORM is open — the inline details-pane editor or the modal
// image-create form.
func (m *Model) FormOpen() bool {
	return m.form != nil || m.Base.EditingDetail()
}

// ModalFormOpen reports a form drawn as its own centred WINDOW — on this screen only the
// runtime-image create form.
func (m *Model) ModalFormOpen() bool { return m.form != nil }

// ClaimsKeys reports whether the screen owns every key right now (a modal form, an inline detail
// editor, a confirmation dialog, or a modal that is still being PREPARED).
func (m *Model) ClaimsKeys() bool {
	// dtPicker participates for the same reason a form does: while the calendar is up it owns every
	// key, and a keystroke that reached the list behind it would act on the wrong thing.
	return m.form != nil || m.dtPicker != nil || m.Open != nil || m.formLoading ||
		m.Base.EditingDetail() || m.Base.Filtering()
}

// workSink adapts the screen to mutate.Sink.
type workSink struct{ m *Model }

func (s workSink) dock() (dockSink, bool) {
	d, ok := s.m.Shell().(dockSink)
	return d, ok
}

// Progress reports a mutation starting.
func (s workSink) Progress(msg string) {
	s.m.notice = msg
	if d, ok := s.dock(); ok {
		d.DockNotice(msg)
	}
}

// Notice reports a successful mutation.
func (s workSink) Notice(msg string) {
	s.m.notice = msg
	if d, ok := s.dock(); ok {
		d.DockNotice(msg)
	}
}

// Fail surfaces a FAILED mutation.
func (s workSink) Fail(msg string) {
	s.m.notice = "✗ " + msg
	if d, ok := s.dock(); ok {
		d.DockError(msg)
	}
}

type dockSink interface {
	DockError(msg string)
	DockNotice(msg string)
}

// mcpModalHost is the shell surface that owns the MCP + skills modal.
type mcpModalHost interface {
	OpenProjectMCPModal(projectID, name string) tea.Cmd
}

// mcpModalHost resolves the shell, when it offers the modal.
func (m *Model) mcpModalHost() (mcpModalHost, bool) {
	h, ok := m.Shell().(mcpModalHost)
	return h, ok
}

// ActiveForm returns the form currently open on this screen, whichever host draws it — the modal
// form (creates) or the inline detail-pane editor (edits).
func (m *Model) ActiveForm() *kit2.Form {
	if m.form != nil {
		return m.form
	}
	return m.Base.DetailForm()
}

// DialogOpen reports whether a confirmation dialog is up.
func (m *Model) DialogOpen() bool { return m.Open != nil }

// openDateTimePicker opens the host's combined calendar + clock for a KDateTime field, seeded from
// the field's current value so editing starts where the operator left it.
func (m *Model) openDateTimePicker(field, current string) tea.Cmd {
	var initial time.Time
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(current)); err == nil {
		initial = t
	}
	m.dtField = field
	p := kit2.NewDateTimePicker("Scheduled start", initial)
	p.SetScreen(m.w, m.h)
	m.dtPicker = p
	return nil
}

// finishDateTimePicker closes the calendar when it reports Done and writes the chosen instant into
// the field that opened it.
func (m *Model) finishDateTimePicker() tea.Cmd {
	if m.dtPicker == nil || !m.dtPicker.Done() {
		return nil
	}
	value, committed, field := m.dtPicker.Value(), m.dtPicker.Committed(), m.dtField
	m.dtPicker, m.dtField = nil, ""
	if !committed {
		return nil
	}
	// ActiveForm, NOT m.form: the scheduled-start field lives on the INLINE detail-pane editor (the `e`
	// gesture), where m.form is nil and the form is Base.DetailForm().
	if f := m.ActiveForm(); f != nil && field != "" {
		f.Set(field, value)
	}
	return nil
}

// Notice returns the last action's status line ("" = none).
func (m *Model) Notice() string { return m.notice }

// It reads ONLY the mutex-guarded index, which is fed by BOTH sources the operator's projects can
// arrive from: the Projects page (fetchProjects) and a work-item form prep (m.projects).
func (m *Model) projectLabel(id string) string { return m.names.projectName(id) }

// projectNameIndex / workflowNameIndex turn an option list into an id→name index for the name
// index above.
func projectNameIndex(opts []projectOpt) map[string]string {
	out := make(map[string]string, len(opts))
	for _, o := range opts {
		if o.ID != "" {
			out[o.ID] = o.Name
		}
	}
	return out
}

func workflowNameIndex(opts []workflowOpt) map[string]string {
	out := make(map[string]string, len(opts))
	for _, o := range opts {
		if o.ID != "" {
			out[o.ID] = o.Name
		}
	}
	return out
}

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
	names := make(map[string]string, len(resp.Msg.Projects))
	for _, p := range resp.Msg.Projects {
		meta := strings.ToLower(strings.TrimPrefix(p.GetStatus().String(), "PROJECT_STATUS_"))
		if dir := p.GetProjectDir(); dir != "" {
			meta += " · " + dir
		}
		items = append(items, kit2.Item{ID: p.GetId(), Title: p.GetName(), Meta: meta})
		// The same page that draws the Projects pane is the index the WORK ITEM
		// detail pane resolves `project` from. Free: no request of its own.
		if p.GetId() != "" {
			names[p.GetId()] = p.GetName()
		}
	}
	m.names.setProjects(names)
	return items, resp.Msg.NextPageToken, nil
}

// workItemPageSize is how many work items the screen asks for in ONE request.
//
// IT IS DELIBERATELY LARGE, AND DELIBERATELY NOT PAGED. The server's list RPC pages with a bare
// `id > token` cursor but orders the FIRST page (`sort_order NULLS LAST, created_at, id`) — the
// sequence chain — and every page AFTER it by ID alone. Those are different orders, so a cursor taken
// from row 200 of one order is at an arbitrary position in the other, and the two pages OVERLAP. Live
// measurement, project 01KYQXQ95C2BFGDT1AFXFX5875 (327 matching items, 200/page):
//
//	page 1: 200 rows   page 2: 14 rows   distinct union: 205   DUPLICATED: 9   MISSED: 122
//
// The duplicates are what the operator sees ("There are two of them showing up in the TUI but only
// one in the GUI"); the 122 MISSING rows are the more serious half of the same defect, and they are
// invisible by construction. The GUI never hits either because it asks for 1000 in a single request
// and never follows a token — which is exactly why the operator sees ONE of the duplicate rows in
// the GUI and two here.
//
// So this follows the GUI: one request large enough for a real tenant, no cursor. 1000 is the RPC's
// own PageSize maximum (`if f.PageSize <= 0 || f.PageSize > 1000 { f.PageSize = 100 }`), and it is
// the number the GUI sends, so both clients see the same set by construction.
//
// The screen still READS next_page_token and reports it up (below), so the count is honest if a
// tenant ever exceeds the cap — the fix is that we no longer WALK the broken cursor, not that we
// pretend more pages do not exist.
const workItemPageSize = 1000

// fetchWorkItems renders the work-items set through the selected display grouping (tree / archive).
func (m *Model) fetchWorkItems(ctx context.Context, pageToken string) ([]kit2.Item, string, error) {
	view := m.ViewMode()
	resp, err := m.cl.WorkItems.ListWorkItems(ctx, connect.NewRequest(&apiv1.ListWorkItemsRequest{
		PageSize:        workItemPageSize,
		PageToken:       pageToken,
		ProjectId:       m.projectScopeFilter(),
		IncludeArchived: view == viewArchive,
		RecurringFilter: apiv1.RecurringFilter_RECURRING_FILTER_EXCLUDE_RECURRING,
		IdeaScope:       apiv1.IdeaScope_IDEA_SCOPE_EXCLUDE_IDEA,
	}))
	if err != nil {
		return nil, "", err
	}
	// The workflow names are read HERE, inside a fetch this screen already makes, so the detail pane
	// can resolve `workflow` on a cold screen — with no form ever opened.
	m.loadWorkflowNames(ctx)
	// THE ACTIVE PAGE, in the Archive view only: it is what the archive tree's ghost anchors are
	// resolved from (an archived item whose parent is still active has no parent row in the archived
	// set.
	var active []*apiv1.WorkItem
	if view == viewArchive {
		if ar, aerr := m.cl.WorkItems.ListWorkItems(ctx, connect.NewRequest(&apiv1.ListWorkItemsRequest{
			PageSize:        workItemPageSize,
			ProjectId:       m.projectScopeFilter(),
			IncludeArchived: false,
			RecurringFilter: apiv1.RecurringFilter_RECURRING_FILTER_EXCLUDE_RECURRING,
			IdeaScope:       apiv1.IdeaScope_IDEA_SCOPE_EXCLUDE_IDEA,
		})); aerr == nil {
			active = ar.Msg.GetWorkItems()
		}
	}
	// Index the RAW page (before rowsFor turns it into display rows) so the detail pane can resolve a
	// `parent` to its title from what is already loaded — and, in the same pass.
	names := make(map[string]string, len(resp.Msg.GetWorkItems()))
	parents := map[string]bool{}
	for _, w := range resp.Msg.GetWorkItems() {
		if w.GetId() != "" {
			names[w.GetId()] = w.GetTitle()
		}
		if p := w.GetParentId(); p != "" {
			parents[p] = true
		}
	}
	m.names.setItems(names)
	m.viewMu.Lock()
	m.parentIDs = parents
	m.viewMu.Unlock()
	// The PR surface for this page: one extra call, the same one the GUI makes for the same reason, so
	// the list can say a run's PR merged and the details pane can carry its URL.
	prs := m.fetchItemPRs(ctx, resp.Msg.GetWorkItems())
	m.viewMu.Lock()
	m.runsByItem = prs
	m.viewMu.Unlock()
	return rowsFor(view, resp.Msg.GetWorkItems(), active, m.SortMode(), prs), resp.Msg.GetNextPageToken(), nil
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
		// The project's OWNED MCP servers, one row each — the TUI counterpart of the GUI panel's
		// "Configured servers".
		if m.cl != nil && m.cl.MCP != nil {
			if lr, err := m.cl.MCP.ListMCPServers(ctx, connect.NewRequest(&apiv1.MCPServerListRequest{ProjectId: p.GetId()})); err == nil {
				for _, s := range lr.Msg.GetServers() {
					fields = append(fields, kit2.Field{Key: "mcp: " + s.GetName(), Value: mcpRowSummary(s)})
				}
			}
		}
		// The project's skill files, so the operator can see them without opening
		// the edit form.
		if len(p.GetSkillFiles()) > 0 {
			fields = append(fields, kit2.Field{Key: "skill files", Value: strings.Join(p.GetSkillFiles(), ", ")})
		}
		return "Project: " + p.GetName(), fields, p.GetGoals(), nil

	case srcWorkItems:
		resp, err := m.cl.WorkItems.GetWorkItem(ctx, connect.NewRequest(&apiv1.GetWorkItemRequest{Id: id}))
		if err != nil {
			return "", nil, "", err
		}
		w := resp.Msg.GetWorkItem()
		// The pane's IDENTITY block. The `pr url` field is spliced in right after `status` — at the TOP, where
		// the operator asked for it: "add a field at the top of the details pane that shows the URL".
		fields := []kit2.Field{
			{Key: "id", Value: w.GetId()},
			{Key: "title", Value: w.GetTitle()},
			{Key: "kind", Value: kindBadge(w.GetKind())},
			{Key: "status", Value: statusPill(w.GetStatus())},
		}
		// NO PR ON RECORD IS NO FIELD — never a blank row and never an invented value, the same discipline
		// the workflow-run field follows.
		if url, state := m.itemPRLink(w.GetId()); url != "" {
			fields = append(fields, kit2.Field{Key: "pr url", Value: prDisplay(url, state)})
		}
		fields = append(fields,
			// The five IDENTIFIER fields below carry a resolved NAME with the id retained beside it, because
			// that is what an operator can act on (the raw guid "won't mean anything to anyone") while the id.
			kit2.Field{Key: "parent", Value: named(m.names.itemTitle(w.GetParentId()), w.GetParentId())},
			kit2.Field{Key: "project", Value: named(m.projectLabel(w.GetProjectId()), w.GetProjectId())},
			kit2.Field{Key: "priority", Value: screenkit.FmtInt(int(w.GetPriority()))},
			kit2.Field{Key: "budgets", Value: w.GetBudgets()},
			kit2.Field{Key: "context window", Value: screenkit.FmtInt(int(w.GetContextWindow()))},
			kit2.Field{Key: "runtime image", Value: w.GetRuntimeImage()},
			// WHERE THIS ITEM'S EXECUTIONS RUN — the EFFECTIVE mode (item override -> project -> tenant
			// default, resolved server-side in db.EffectiveExecutionMode).
			kit2.Field{Key: "execution mode", Value: executionModeDetail(w)},
			kit2.Field{Key: "workflow", Value: named(m.names.workflowName(w.GetWorkflowId()), w.GetWorkflowId())},
			// A workflow RUN has no name of its own — it is not a named entity — so it gets the GUI's
			// rendering: a shortened id, not the 26-character ULID, and not an invented label.
			kit2.Field{Key: "workflow run", Value: shortRunID(w.GetWorkflowRunId())},
			kit2.Field{Key: "auto-start", Value: boolStr(w.GetAutoStartWorkflow())},
			kit2.Field{Key: "scheduled", Value: screenkit.FmtTime(w.GetScheduledStartAt())},
			// The numeric sort_order is deliberately NOT shown: a bare float is meaningless to a human.
			kit2.Field{Key: "archived from", Value: w.GetArchivedFromStatus()},
			kit2.Field{Key: "updated", Value: screenkit.FmtTime(w.GetUpdatedAt())},
		)
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

// actionsForSelection builds the entity-bound actions for the focused row — or the BULK actions,
// when the operator has multi-selected.
func (m *Model) actionsForSelection() []kit2.Action {
	// The BULK rule is kit2's, in one place: more than one marked row means the operator is operating
	// on a SELECTION, not on the cursor row.
	if ids := m.Base.BulkIDs(); len(ids) > 0 {
		// ROUTED BY SOURCE, which it was not: every source went to bulkItemActions, so a projects selection
		// was offered "delete" that called the WORK ITEM RPC with project ids.
		switch m.ActiveSourceName() {
		case srcProjects:
			return m.bulkProjectActions(ids)
		default:
			// PER VIEW, because the two views' bulk vocabularies are disjoint: the archive's operation is
			// RESTORE, and archive/delete are no-ops or different outcomes there.
			if m.ViewMode() == viewArchive {
				return m.bulkArchiveActions(ids)
			}
			return m.bulkItemActions(ids)
		}
	}
	switch m.ActiveSourceName() {
	case srcProjects:
		return m.projectActions()
	case srcImages:
		return m.imageActions()
	default:
		return m.itemActions()
	}
}

// bulkItemActions are the operations that make sense on a whole selection.
func (m *Model) bulkItemActions(ids []string) []kit2.Action {
	n := len(ids)
	count := fmt.Sprintf("%d", n)
	label := func(verb string) string { return verb + " " + count + " selected" }
	// A write path must never PANIC on a missing client: a screen without a plane is a configuration
	// the rest of this screen already refuses gracefully.
	if m.cl == nil || m.cl.WorkItems == nil {
		return []kit2.Action{{
			Label: "no work-item client", Source: srcWorkItems,
			Do: func(context.Context) error { return fmt.Errorf("no work-item client") },
		}}
	}
	client := m.cl.WorkItems

	// The rows are removed locally only AFTER the writes land (there is no rollback that can restore
	// another item's server state), so these actions carry no optimistic Apply.
	archive := kit2.Action{
		Label: label("archive"), Key: "a", Danger: true, Source: srcWorkItems,
		Confirm: "Archive " + count + " items?\n" +
			"Each must be terminal and childless — the server rejects the rest, and the first\n" +
			"rejection stops the run. Reversible one at a time with restore.",
		Do: func(ctx context.Context) error {
			failed := 0
			for _, id := range ids {
				if _, err := client.ArchiveWorkItem(ctx, connect.NewRequest(&apiv1.ArchiveWorkItemRequest{Id: id})); err != nil {
					failed++
				}
			}
			if failed > 0 {
				return fmt.Errorf("archived %d of %d — %d were rejected (terminal, childless items only)",
					n-failed, n, failed)
			}
			return nil
		},
	}
	del := kit2.Action{
		Label: label("delete"), Key: kit2.DeleteChord, Danger: true, Source: srcWorkItems,
		Confirm: "Permanently delete " + count + " items and all their dependencies?\n" +
			"This cannot be undone. An item with children is refused rather than cascaded, and is\n" +
			"reported below.",
		Do: func(ctx context.Context) error {
			failed := 0
			for _, id := range ids {
				if _, err := client.HardDeleteWorkItem(ctx, connect.NewRequest(&apiv1.HardDeleteWorkItemRequest{Id: id})); err != nil {
					failed++
				}
			}
			if failed > 0 {
				return fmt.Errorf("deleted %d of %d — %d refused (an item with children must be emptied or archived first)",
					n-failed, n, failed)
			}
			return nil
		},
	}
	clear := kit2.Action{
		Label: "clear selection", Key: "esc", Source: srcWorkItems,
		// No confirm and no write: it is the escape hatch the operator named, and offering it
		// as an action makes it discoverable from the bar as well as the key.
		Do: func(context.Context) error { return nil },
		Apply: func() {
			m.Base.ClearMarks()
			m.notice = "selection cleared"
		},
	}
	// The bulk workflow/runtime-image set.
	setwf := kit2.Action{
		Label: label("set workflow & image"), Key: keyBulkSet, Source: srcWorkItems,
		// No RPC of its own: openAction intercepts this action and opens the picker instead.
		Do: func(context.Context) error { return nil },
	}
	return []kit2.Action{archive, del, setwf, clear}
}

// bulkArchiveActions is the ARCHIVE VIEW's bulk action set: RESTORE, and only restore.
func (m *Model) bulkArchiveActions(ids []string) []kit2.Action {
	n := len(ids)
	count := fmt.Sprintf("%d", n)
	if m.cl == nil || m.cl.WorkItems == nil {
		return []kit2.Action{{
			Label: "no work-item client", Source: srcWorkItems,
			Do: func(context.Context) error { return fmt.Errorf("no work-item client") },
		}}
	}
	cl := m.cl.WorkItems
	ordered := m.restoreOrder(ids)
	restore := kit2.Action{
		Label: "restore " + count + " selected", Key: "R", Source: srcWorkItems,
		Confirm: "Restore " + count + " items?\n" +
			"Each returns to the active views at the status it was archived from.\n" +
			"Children are restored BEFORE their parents so the hierarchy holds at every step,\n" +
			"and the first rejection stops the run.",
		Do: func(ctx context.Context) error {
			failed := 0
			for _, id := range ordered {
				if _, err := cl.RestoreWorkItem(ctx, connect.NewRequest(&apiv1.RestoreWorkItemRequest{Id: id})); err != nil {
					failed++
				}
			}
			if failed > 0 {
				return fmt.Errorf("restored %d of %d — %d were rejected", n-failed, n, failed)
			}
			return nil
		},
	}
	clear := kit2.Action{
		Label: "clear selection", Key: "esc", Source: srcWorkItems,
		Do: func(context.Context) error { return nil },
		Apply: func() {
			m.Base.ClearMarks()
			m.notice = "selection cleared"
		},
	}
	return []kit2.Action{restore, clear}
}

// restoreOrder returns the marked ids CHILD-FIRST, by each row's depth in the archive tree.
func (m *Model) restoreOrder(ids []string) []string {
	t := m.Base.ActiveTable()
	if t == nil {
		return ids
	}
	type ranked struct {
		id    string
		depth int
	}
	rows := make([]ranked, 0, len(ids))
	for _, id := range ids {
		d := 0
		if r, ok := t.RowByID(id); ok {
			d = r.Depth
		}
		rows = append(rows, ranked{id: id, depth: d})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].depth > rows[j].depth })
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.id)
	}
	return out
}

// openAction opens the confirmation dialog for an action that needs one, or
// runs it immediately.
func (m *Model) openAction(a kit2.Action) tea.Cmd {
	// PICKER-FIRST: the VALUE is chosen before the confirm, so the confirm can name it.
	if a.Key == keyBulkSet {
		m.bulkSetIDs = m.Base.BulkIDs()
		if len(m.bulkSetIDs) == 0 {
			return nil
		}
		return m.prepBulkSet()
	}
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
	// THE COLLAPSE DEFAULT IS PER VIEW (see New): the Tree opens collapsed because a deep hierarchy is
	// unreadable expanded.
	m.Base.SetCollapsedByDefault(srcWorkItems, v == viewTree)
	// …and RE-SEAT the rows already on screen.
	m.Base.ResetCollapseState(srcWorkItems)
	m.notice = "work items: " + string(v) + " view"
	m.syncRowActions()
	return m.Refresh(srcWorkItems)
}

// SortMode returns the work-items sibling display order.
func (m *Model) SortMode() sortMode {
	m.viewMu.Lock()
	defer m.viewMu.Unlock()
	return m.sort
}

// cycleSort advances the sort control and RE-FETCHES so the new order is actually applied — the bug
// the operator hit ("it goes through the different orderings but it doesn't actually apply.
func (m *Model) cycleSort() tea.Cmd {
	m.viewMu.Lock()
	m.sort = m.sort.next()
	next := m.sort
	m.viewMu.Unlock()
	// Re-ordering the list parks the cursor so the reload starts at the TOP.
	if t := m.Base.ActiveTable(); t != nil {
		t.ResetCursor()
	}
	m.notice = "sorted by " + string(next) + " (sequence = the run order; +/- changes it)"
	return m.Refresh(srcWorkItems)
}

// syncRowActions (re)installs the pane's clickable controls.
func (m *Model) syncRowActions() {
	if m.Base.ActiveSourceName() != srcWorkItems {
		return
	}
	acts := []kit2.RowAction{{
		// A STATE-reporting label: the control says what is on.
		Label: func() string { return m.SortMode().label() },
		Do:    func() tea.Cmd { return m.cycleSort() },
	}}
	if t := m.Base.ActiveTable(); t != nil && t.ExpandableCount() > 0 {
		acts = append(acts, kit2.RowAction{
			Label: func() string {
				t := m.Base.ActiveTable()
				if t == nil || t.AllExpanded() {
					return "collapse all"
				}
				return "expand all"
			},
			Do: func() tea.Cmd { return m.toggleAllTreeNodes() },
		})
	}
	m.Base.SetRowActions(srcWorkItems, acts)
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
		m.formLoading = false
		if msg.err != nil {
			m.notice = "couldn't open the form: " + msg.err.Error()
			return m, nil
		}
		// Only replace a list the message actually carries: the edit path fetches the item plus its
		// pickers, and must not clobber the project list the create form told us about.
		if len(msg.projects) > 0 {
			m.projects = msg.projects
		}
		if len(msg.workflows) > 0 {
			m.workflows = msg.workflows
		}
		// The form prep carries the SAME two lists the pickers are built from, so it is the other half of
		// the name index: workflows have no other source on this Model.
		m.names.setProjects(projectNameIndex(msg.projects))
		m.names.setWorkflows(workflowNameIndex(msg.workflows))
		if len(msg.parents) > 0 {
			m.parents = msg.parents
		}
		if len(msg.parentKinds) > 0 {
			m.parentKind = msg.parentKinds
			m.parentProject = msg.parentProjects
		}
		if len(msg.images) > 0 {
			m.images = msg.images
		}
		switch msg.mode {
		case formCreateItem:
			if len(msg.projects) == 0 {
				m.notice = "no projects yet — a work item belongs to a project"
				return m, nil
			}
			// Item 5: CREATE in the DETAILS PANE too, matching the worker forms and the edit path below.
			m.Base.BeginDetailEdit("New work item", m.newItemCreateForm())
		case formEditItem:
			// Edit in the DETAILS PANE, not a modal (the operator's ask).
			m.Base.BeginDetailEdit("Edit work item", m.newItemEditForm(msg.item))
		case formStatusItem:
			m.Base.BeginDetailEdit("Status & priority", m.newItemStatusForm(msg.item))
		case formBulkSetItem:
			// The MODAL host, not the details pane: a bulk write is not about the active row.
			m.form = m.newBulkSetForm(msg.workflows, msg.images)
			if msg.hiddenWorkflows > 0 {
				m.notice = fmt.Sprintf("%d workflow(s) hidden — not published, or has no steps", msg.hiddenWorkflows)
			}
		}
		m.formMode, m.formID = msg.mode, msg.item.GetId()
		if msg.mode != formBulkSetItem {
			m.notice = ""
		}
		return m, nil

	case projectFormMsg:
		m.formLoading = false
		if msg.err != nil {
			m.notice = "couldn't open the form: " + msg.err.Error()
			return m, nil
		}
		// Cache the runtime-image options so the next form costs no round trip — the same
		// treatment the work-item form's image list gets.
		if len(msg.data.images) > 0 {
			m.images = msg.data.images
		}
		switch msg.mode {
		case formCreateProject:
			// Same as create-item: the pane, not a modal.
			m.Base.BeginDetailEdit("New project", m.newProjectCreateFormWith(msg.data))
		case formEditProject:
			m.Base.BeginDetailEdit("Edit project", m.newProjectEditFormWith(msg.project, msg.data))
		case formProjectDir:
			m.Base.BeginDetailEdit("Project directory", m.newProjectDirForm(msg.project))
		}
		m.formMode, m.formID = msg.mode, msg.project.GetId()
		m.notice = ""
		return m, nil

	case imageFormMsg:
		m.formLoading = false
		if msg.err != nil {
			m.notice = "couldn't open the form: " + msg.err.Error()
			return m, nil
		}
		switch msg.mode {
		case formCreateImage:
			m.form = m.newImageCreateForm()
		case formEditImage:
			m.Base.BeginDetailEdit("Edit runtime image", m.newImageEditForm(msg.image))
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
		// THE CALENDAR IS THE TOPMOST MODAL, so it owns every key FIRST — above the search box and,
		// critically, above the inline detail editor, which is the surface it is usually opened FROM.
		if m.dtPicker != nil {
			_, cmd := m.dtPicker.HandleKey(msg)
			return m, tea.Batch(cmd, m.finishDateTimePicker())
		}
		// The search box (the operator typing into the filter row) owns every
		// key first: it is a text input.
		if m.Base.Filtering() {
			if handled, cmd := m.Base.Update(msg); handled {
				return m, cmd
			}
		}
		// The INLINE detail editor (an item/project/image being edited in the
		// details pane) owns every key first: it is the focused surface.
		if m.Base.EditingDetail() {
			if handled, cmd := m.Base.Update(msg); handled {
				return m, cmd
			}
		}
		// The open modal form owns every key while it is up (esc closes it; enter
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
	m.Base.SetDetailContentLaidOut(title, []kit2.Field{
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
			// THROUGH THE PREP, not straight to the form: the create form now offers the MCP selection, whose
			// options are a round trip.
			return m.prepProjectForm(formCreateProject, ""), true
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
	case "m":
		// IT REPLACES A CREATE-ONLY FORM.
		if src == srcProjects {
			if it, ok := m.ActiveItem(); ok {
				if h, ok := m.mcpModalHost(); ok {
					return h.OpenProjectMCPModal(it.ID, it.Title), true
				}
				m.notice = "the MCP + skills modal is unavailable here"
				return nil, true
			}
		}
	case "s":
		if src == srcWorkItems {
			return m.prepEditItem(formStatusItem), true
		}
	case "b":
		if src == srcImages {
			return m.startBuild(), true
		}
	case "v":
		if src == srcWorkItems {
			return m.switchView(m.ViewMode().next()), true
		}
	case "O":
		// The OVERALL collapse/expand toggle ('o' is the single-node one, handled by the table itself).
		if src == srcWorkItems {
			return m.toggleAllTreeNodes(), true
		}
	case "/":
		// Focus the search box at the top of the list.
		if m.Base.StartFilter() {
			return nil, true
		}
	case "+", "=":
		// Move the selected item one step EARLIER in its sibling sequence (its workflow then runs sooner).
		if src == srcWorkItems && m.ViewMode() == viewTree {
			return m.reorderChildren(-1), true
		}
	case "-", "_":
		// Move the selected item one step LATER in its sibling sequence.
		if src == srcWorkItems && m.ViewMode() == viewTree {
			return m.reorderChildren(1), true
		}
	case "y", "a", "R", keyBulkSet:
		if a, ok := m.actionByKey(msg.String()); ok {
			return m.openAction(a), true
		}
		// keyBulkSet IS THE ONE CHORD IN THIS GROUP THAT IS NOT ALWAYS OFFERED, so it is the one that could
		// fall through in silence.
		if msg.String() == keyBulkSet {
			m.notice = keyBulkSet + " sets a workflow + runtime image for SEVERAL items at once — " +
				"space to mark 2 or more first, or e to set one item's fields"
			return nil, true
		}
	case kit2.DeleteChord, kit2.DeleteChordAlias:
		// ONE delete gesture, TWO keys.
		if a, ok := m.actionByKey(kit2.DeleteChord); ok {
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
		if m.dtPicker != nil {
			// Re-stated every frame: the picker caches the screen size to size its box, and a resize
			// while it is open would otherwise leave it laid out for the terminal it was opened in.
			m.dtPicker.SetScreen(m.w, m.h)
			body = kit2.Center(body, m.dtPicker.View(), m.w, m.h)
		} else if m.form != nil {
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
	// The form's own width must reflect the box it is drawn in, or its horizontal window (and its
	// caret) would be computed against the terminal rather than the dialog's inner width.
	f.Width = bw - 4
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

// toggleAllTreeNodes flips every node of the work-item Tree at once — the operator's "overall
// collapse and expand".
func (m *Model) toggleAllTreeNodes() tea.Cmd {
	t := m.Base.ActiveTable()
	if t == nil || t.ExpandableCount() == 0 {
		return nil
	}
	open := !t.AllExpanded()
	n := t.ExpandAll(open)
	if open {
		m.notice = fmt.Sprintf("expanded %d node(s)", n)
	} else {
		m.notice = fmt.Sprintf("collapsed %d node(s)", n)
	}
	return nil
}

// HintLine is the screen's key cheat-sheet.
func (m *Model) HintLine() string {
	switch m.ActiveSourceName() {
	case srcProjects:
		return theme.HintText.Render("n: new project · e: edit · d: set+create dir · " +
			"m: MCP + skills (add, or add from catalog) · enter: detail · ←/→: pane")
	case srcImages:
		return theme.HintText.Render("n: new image · e: edit spec · b: build (live logs) · " +
			kit2.DeleteChord + ": delete · enter: detail")
	default:
		// THE BULK SET IS NAMED HERE, AND IT WAS NAMED NOWHERE.
		if n := m.Base.MarkCount(); n > 1 {
			if m.ViewMode() == viewArchive {
				return theme.HintText.Render(fmt.Sprintf("%d marked", n) + " · " +
					"R: restore · esc: clear · ↑↓: move · enter: detail")
			}
			return theme.HintText.Render(fmt.Sprintf("%d marked", n) + " · " +
				keyBulkSet + ": set workflow & image · esc: clear · ↑↓: move · enter: detail")
		}
		// THE ARCHIVE VIEW GETS ITS OWN LINE, because half of the tree line's chords do not exist there and
		// the one chord that DOES was named nowhere.
		if m.ViewMode() == viewArchive {
			return theme.HintText.Render("R: restore · /: search · enter: detail · " +
				"v: view (archive ⇄ tree) · o/O: collapse · ↑↓: move")
		}
		// THE STOP CHORD IS ADVERTISED ONLY WHEN THERE IS SOMETHING TO STOP.
		stop := ""
		if it, ok := m.ActiveItem(); ok && itemIsRunning(it) {
			stop = keyStopRun + ": stop execution · "
		}
		return theme.HintText.Render(stop + "n: new · /: search · e: edit · s: status · y: auto-start · +/-: move step · " +
			"a: archive · " + kit2.DeleteChord + ": delete · " + keyBulkSet + ": set workflow & image (space marks 2+) · " +
			"v: view (tree ⇄ archive) · o/O: collapse · enter: detail")
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

// itemIsRunning reports whether the pane's selected row is an item whose run is live.
func itemIsRunning(it screenkit.Item) bool {
	return strings.Contains(it.Meta, statusPill(apiv1.WorkItemStatus_WORK_ITEM_STATUS_RUNNING))
}

// StopRunHint reports the chord that stops the focused row's run, or "" when the focused row is not
// running.
func (m *Model) StopRunHint() string {
	if m.ActiveSourceName() != srcWorkItems {
		return ""
	}
	if it, ok := m.ActiveItem(); ok && itemIsRunning(it) {
		return keyStopRun + " stop execution"
	}
	return ""
}
