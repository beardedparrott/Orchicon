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
	// sort is the sibling DISPLAY order (the control next to the search box).
	sort sortMode

	// create-form option lists (loaded before the form opens).
	projects  []projectOpt
	workflows []workflowOpt
	// parents / images back the KPicker fields (parent work item, runtime
	// image): references chosen from a filtered list rather than typed ids.
	parents []kit2.Option
	images  []kit2.Option
	// parentKind maps a parent id to its kind, so the create form can DERIVE
	// the child's kind from the chosen parent. The hierarchy is deterministic
	// (epic > feature > task > subtask), so asking the operator to pick both
	// invited combinations the server rejects — which is how a create read as
	// "the item is nowhere to be found".
	parentKind map[string]apiv1.WorkItemKind
	// parentProject maps a parent id to its project. The server requires a
	// parent to be in the SAME project as its child, so the picker must only
	// offer parents from the chosen project (it used to list every project).
	parentProject map[string]string

	// form is the open typed form (nil = closed).
	form     *kit2.Form
	formMode string
	formID   string

	// dtPicker is the open combined calendar + clock modal (a KDateTime field activated), layered
	// ABOVE the form it was opened from; dtField is the field it writes back into. Nil when closed.
	//
	// It is the screen's own modal, like the automation screen's calendar, for the reason
	// kit2.ModelPicker.Done documents: a host callback closing a modal would mutate a copy of the model
	// that bubbletea has already replaced, so the SCREEN owns both the open and the close.
	dtPicker *kit2.DateTimePicker
	dtField  string

	// formLoading is true from the moment a modal is REQUESTED until its
	// payload arrives. The create/edit forms need an option-list round trip
	// (projects / workflows / images) before they can be drawn, and until
	// ClaimsKeys reports true the vertical keys fell through to the list
	// BEHIND the modal and scrolled it.
	formLoading bool

	// formMCPLoaded records whether the OPEN project form's MCP data arrived. It is
	// consulted at submit time to decide whether the MCP selection may be WRITTEN: the
	// field is absent when the load failed, and an absent field must not be read as "the
	// operator cleared it". See projects.go setProjectMCPServers.
	formMCPLoaded bool

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
	// The mutation executor is installed HERE so every write has a feedback
	// surface. Without it Base.Mutate fell back to a zero executor with a nil
	// Sink, so a REJECTED write rolled back silently and the operator only saw
	// an item that never appeared (the operator's "it is nowhere to be found").
	m.Base.SetExecutor(&mutate.Executor{Sink: workSink{m}})
	// The Work Items list carries a search row ('/'), per the operator's "search
	// box at the top of the work items page for filter".
	m.Base.EnableFilter(srcWorkItems)
	// ...and a clickable collapse/expand-all control next to it (Tree view only).
	m.syncRowActions()
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
	// CloseAll tears down EVERY subscription in the SHARED registry, so the
	// screen must drop its own handle too. EnsureSubscriptions guards on that
	// handle being nil, so a stale non-nil one meant the stream was never
	// recreated after the first visit: no further events arrived AND the footer
	// stayed frozen at the last status — the operator's "the connection status
	// is CONSTANTLY saying disconnected or reconnecting".
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

// DropKeyClaim releases the screen's key claim so the focus chord can return the
// operator to the composer (see kit2.Base.DropKeyClaim). It clears the two
// LATCHED reasons — a form still being PREPARED and the search box — and closes
// the inline editor. A confirm dialog is deliberately left alone: that is a modal
// decision the operator must resolve, and silently dismissing it on a focus
// chord would be worse than the chord not working.
func (m *Model) DropKeyClaim() {
	m.formLoading = false
	m.Base.DropKeyClaim()
}

// FormOpen reports whether a FORM is open — the inline details-pane editor or the
// modal image-create form. The shell's Tab hard chord consults it so that, while the
// operator is editing, Tab (and Shift+Tab) move through the FORM'S ITEMS rather than
// walking the top tab menu.
//
// This screen was missing the method entirely, and the absence was not benign: the chord
// does `if fs, ok := FormOpen(); ok && fs.FormOpen()` — with ok == false it falls through
// to `m.closeTabMenu(); m.tabRingNext()`, so on Work (and only Work) Tab walked OUT of an
// open editor instead of moving between its inputs. Every other screen with a form
// already implements it.
//
// The operator settled the rule explicitly: "ONCE IN EDIT/NEW MODE using down/up OR
// tab/shift+tab should move through the edit items as opposed to the top menu bar on
// every screen. Once you ctrl+s to save or hit Esc to get out of the editing mode,
// tab/shift+tab now affects the top tab menu again." Which is exactly what this does —
// the moment the form closes, the claim drops and Tab belongs to the bar again.
func (m *Model) FormOpen() bool {
	return m.form != nil || m.Base.EditingDetail()
}

// ModalFormOpen reports a form drawn as its own centred WINDOW — on this screen only the
// runtime-image create form. The distinction matters for the shell's Tab handling on
// screens that treat a windowed modal and an inline editor differently; here both take
// Tab for their fields, per the rule above.
func (m *Model) ModalFormOpen() bool { return m.form != nil }

// ClaimsKeys reports whether the screen owns every key right now (a modal
// form, an inline detail editor, a confirmation dialog, or a modal that is
// still being PREPARED). The shell consults it before its own routes so a
// typed character is never stolen ('q' would quit, space would open the tab
// menu, '/' the palette).
//
// formLoading participates on purpose: the create/edit forms fetch their
// option lists asynchronously, and in that window a form is not yet open —
// so without this the arrow keys reached the list behind the modal.
func (m *Model) ClaimsKeys() bool {
	// dtPicker participates for the same reason a form does: while the calendar is up it owns every
	// key, and a keystroke that reached the list behind it would act on the wrong thing.
	return m.form != nil || m.dtPicker != nil || m.Open != nil || m.formLoading ||
		m.Base.EditingDetail() || m.Base.Filtering()
}

// workSink adapts the screen to mutate.Sink. It is an adapter rather than
// methods on Model because Model already has a `Notice() string` accessor the
// shell reads; the Sink needs `Notice(msg string)`.
//
// EVERY outcome goes to the SHELL's dock as well as the screen's notice line,
// and that is not cosmetic: the screen's notice is appended to the body and
// then cut off by FitLines whenever the panes fill the height, so a successful
// create reported NOTHING the operator could see — which read as "new work
// items don't seem to be actually saving". The dock renders inside the composer
// box, which is never truncated.
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

// Fail surfaces a FAILED mutation. It is deliberately loud: a rejected write
// must never look like nothing happened, which is exactly how a server-side
// rejection ("a task must have a parent; only epics can be top-level") read as
// an item that vanished.
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

// ActiveForm returns the form currently open on this screen, whichever host
// draws it — the modal form (creates) or the inline detail-pane editor (edits).
// Tests and the shell read the in-progress input through it.
func (m *Model) ActiveForm() *kit2.Form {
	if m.form != nil {
		return m.form
	}
	return m.Base.DetailForm()
}

// DialogOpen reports whether a confirmation dialog is up.
func (m *Model) DialogOpen() bool { return m.Open != nil }

// openDateTimePicker opens the host's combined calendar + clock for a KDateTime field, seeded from the
// field's current value so editing starts where the operator left it.
//
// The seed is parsed as RFC3339 and handed over as an INSTANT; the picker converts to local wall clock
// itself, which is the whole point — the operator edits the day and the time they think in, and the
// value that comes back is the moment they meant. An EMPTY or unparseable seed falls back to now, so
// the common case (scheduling something soon) opens on the current day rather than on the zero date.
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

// finishDateTimePicker closes the calendar when it reports Done and writes the chosen instant into the
// field that opened it. The SCREEN owns the close (see kit2.ModelPicker.Done for why a host callback
// cannot).
//
// A CANCELLED modal writes nothing: esc must leave the field exactly as it was, or dismissing the
// calendar would silently reschedule the item to whatever the cursor happened to be sitting on.
func (m *Model) finishDateTimePicker() tea.Cmd {
	if m.dtPicker == nil || !m.dtPicker.Done() {
		return nil
	}
	value, committed, field := m.dtPicker.Value(), m.dtPicker.Committed(), m.dtField
	m.dtPicker, m.dtField = nil, ""
	if !committed {
		return nil
	}
	// ActiveForm, NOT m.form: the scheduled-start field lives on the INLINE detail-pane editor (the
	// `e` gesture), where m.form is nil and the form is Base.DetailForm(). Writing to m.form alone
	// would have dropped every picked value on the one path the operator actually uses — the calendar
	// would commit, close, and change nothing.
	if f := m.ActiveForm(); f != nil && field != "" {
		f.Set(field, value)
	}
	return nil
}

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

// fetchWorkItems renders the work-items set through the selected display
// grouping (tree / archive).
//
// ONE request, like the GUI (see workItemPageSize for why paging this RPC is broken and why that is
// the SERVER's ordering being page-dependent rather than anything this screen does). The display
// order is this screen's own: treeRows re-sorts every sibling set with its own sortSequence/sortTitle
// /sortStatus/sortPriority, and stepNumber derives the step from the stored chain, so the ORDER the
// server returns rows in has never mattered to what is drawn — only the cursor's reliability did.
func (m *Model) fetchWorkItems(ctx context.Context, pageToken string) ([]kit2.Item, string, error) {
	view := m.ViewMode()
	resp, err := m.cl.WorkItems.ListWorkItems(ctx, connect.NewRequest(&apiv1.ListWorkItemsRequest{
		PageSize:        workItemPageSize,
		PageToken:       pageToken,
		IncludeArchived: view == viewArchive,
		RecurringFilter: apiv1.RecurringFilter_RECURRING_FILTER_EXCLUDE_RECURRING,
		IdeaScope:       apiv1.IdeaScope_IDEA_SCOPE_EXCLUDE_IDEA,
	}))
	if err != nil {
		return nil, "", err
	}
	return rowsFor(view, resp.Msg.GetWorkItems(), m.SortMode()), resp.Msg.GetNextPageToken(), nil
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
			// The numeric sort_order is deliberately NOT shown: a bare float is
			// meaningless to a human. The step position is visible in the list
			// itself, where it can be compared against its siblings.
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

// actionsForSelection builds the entity-bound actions for the focused row — or the BULK
// actions, when the operator has multi-selected.
//
// The operator's rule is the whole design: "we need to have a consistent way to handle bulk
// operations on all forms. My suggestion would be spacebar can do multi-select and Esc
// clears the multi-select and then the bulk options shows up only after you have more than
// one item selected."
//
// So bulk replaces single-row AT MORE THAN ONE. One marked row is not a bulk operation — it
// is the row the cursor is on, and offering "archive 1 item" beside "archive" would just be
// a slower way to do the same thing. The action BAR, the hint LINE and the key dispatch all
// derive from this one list, so switching to bulk here switches everywhere at once.
func (m *Model) actionsForSelection() []kit2.Action {
	// The BULK rule is kit2's, in one place: more than one marked row means the operator is
	// operating on a SELECTION, not on the cursor row, so the single-row actions are replaced
	// rather than mixed with it. (This screen had its own `len(ids) > 1` until the Schedules pane
	// needed the same rule — two copies of a threshold is how "consistent bulk operations" stops
	// being consistent.)
	if ids := m.Base.BulkIDs(); len(ids) > 0 {
		// ROUTED BY SOURCE, which it was not: every source went to bulkItemActions, so a
		// projects selection was offered "delete" that called the WORK ITEM RPC with
		// project ids — failing on every call for a reason the operator could not see. A
		// row is a row to the table, but a project is not a work item.
		switch m.ActiveSourceName() {
		case srcProjects:
			return m.bulkProjectActions(ids)
		default:
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
//
// Deliberately only the ones the server can do per item and that an operator plausibly wants
// in bulk: ARCHIVE and DELETE. A bulk status change or reassignment would need a form per
// item (each carries its own acceptance review and worker binding), so it is not offered
// rather than offered misleadingly.
//
// One action, one confirm, and the confirm NAMES THE COUNT: a destructive operation on ten
// rows must not look like one on a single row. The writes are sequential and the FIRST
// failure stops and reports, because a partial bulk operation must say what it did rather
// than claim a clean sweep.
func (m *Model) bulkItemActions(ids []string) []kit2.Action {
	n := len(ids)
	count := fmt.Sprintf("%d", n)
	label := func(verb string) string { return verb + " " + count + " selected" }
	// A write path must never PANIC on a missing client: a screen without a plane is a
	// configuration the rest of this screen already refuses gracefully, and a crash in a
	// destructive bulk operation would take the whole TUI with it.
	if m.cl == nil || m.cl.WorkItems == nil {
		return []kit2.Action{{
			Label: "no work-item client", Source: srcWorkItems,
			Do: func(context.Context) error { return fmt.Errorf("no work-item client") },
		}}
	}
	client := m.cl.WorkItems

	// The rows are removed locally only AFTER the writes land (there is no rollback that can
	// restore another item's server state), so these actions carry no optimistic Apply: a
	// refresh is the honest reconciliation for a multi-row write.
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
		Confirm: "Delete " + count + " items?\n" +
			"This soft-deletes each (status → cancelled) and they leave every active view.",
		Do: func(ctx context.Context) error {
			failed := 0
			for _, id := range ids {
				if _, err := client.DeleteWorkItem(ctx, connect.NewRequest(&apiv1.DeleteWorkItemRequest{Id: id})); err != nil {
					failed++
				}
			}
			if failed > 0 {
				return fmt.Errorf("deleted %d of %d — %d failed", n-failed, n, failed)
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
	return []kit2.Action{archive, del, clear}
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
	m.syncRowActions()
	return m.Refresh(srcWorkItems)
}

// SortMode returns the work-items sibling display order.
func (m *Model) SortMode() sortMode {
	m.viewMu.Lock()
	defer m.viewMu.Unlock()
	return m.sort
}

// cycleSort advances the sort control and RE-FETCHES so the new order is
// actually applied — the bug the operator hit ("it goes through the different
// orderings but it doesn't actually apply the sort") was that the control's
// command was dropped.
func (m *Model) cycleSort() tea.Cmd {
	m.viewMu.Lock()
	m.sort = m.sort.next()
	next := m.sort
	m.viewMu.Unlock()
	// Re-ordering the list parks the cursor so the reload starts at the TOP.
	// Without this the cursor was restored to the old row's NEW position and the
	// window scrolled to keep it visible — the operator's "sort is working but
	// jumping to the bottom of the screen".
	if t := m.Base.ActiveTable(); t != nil {
		t.ResetCursor()
	}
	m.notice = "sorted by " + string(next) + " (sequence = the run order; +/- changes it)"
	return m.Refresh(srcWorkItems)
}

// syncRowActions (re)installs the pane's clickable controls. The tree's
// collapse/expand-all only means something in the Tree view, so the flat views
// carry no button rather than a dead one.
func (m *Model) syncRowActions() {
	acts := []kit2.RowAction{{
		// A STATE-reporting label: the control says what is on.
		Label: func() string { return m.SortMode().label() },
		Do:    func() tea.Cmd { return m.cycleSort() },
	}}
	if m.ViewMode() == viewTree {
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
		// Only replace a list the message actually carries: the edit path
		// fetches the item plus its pickers, and must not clobber the project
		// list the create form told us about.
		if len(msg.projects) > 0 {
			m.projects = msg.projects
		}
		if len(msg.workflows) > 0 {
			m.workflows = msg.workflows
		}
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
			// Item 5: CREATE in the DETAILS PANE too, matching the worker forms and
			// the edit path below. The operator: "if we are going to make new worker
			// go into the detail pane for editing (which I do like), then we should
			// mimic that for work items because work items still uses a popup
			// modal." Both halves of the work-item flow now share one host, so the
			// keys, the validation and the submit path cannot diverge between them.
			m.Base.BeginDetailEdit("New work item", m.newItemCreateForm())
		case formEditItem:
			// Edit in the DETAILS PANE, not a modal (the operator's ask).
			m.Base.BeginDetailEdit("Edit work item", m.newItemEditForm(msg.item))
		case formStatusItem:
			m.Base.BeginDetailEdit("Status & priority", m.newItemStatusForm(msg.item))
		}
		m.formMode, m.formID = msg.mode, msg.item.GetId()
		m.notice = ""
		return m, nil

	case projectFormMsg:
		m.formLoading = false
		if msg.err != nil {
			m.notice = "couldn't open the form: " + msg.err.Error()
			return m, nil
		}
		m.formMCPLoaded = msg.mcpLoaded
		switch msg.mode {
		case formCreateProject:
			// Same as create-item: the pane, not a modal.
			m.Base.BeginDetailEdit("New project", m.newProjectCreateFormWith(msg.mcpServers, msg.mcpSelected))
		case formEditProject:
			m.Base.BeginDetailEdit("Edit project", m.newProjectEditFormWith(msg.project, msg.mcpServers, msg.mcpSelected))
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
		//
		// THIS ORDERING IS NOT A DETAIL. The scheduled-start field lives on the INLINE detail editor
		// (the `e` gesture), and that editor claims every key before anything else — so with the
		// calendar checked after it, enter and esc went to the FORM: the modal could be neither
		// committed nor dismissed. It opened, and then ignored the operator entirely.
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
			// THROUGH THE PREP, not straight to the form: the create form now offers the
			// MCP selection, whose options are a round trip. Building it synchronously
			// would open a form that silently lacked the field.
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
	case "T":
		if src == srcWorkItems {
			return m.switchView(viewTree), true
		}
	case "Z":
		if src == srcWorkItems {
			return m.switchView(viewArchive), true
		}
	case "O":
		// The OVERALL collapse/expand toggle ('o' is the single-node one,
		// handled by the table itself). Tree only — Board/Archive are flat.
		if src == srcWorkItems && m.ViewMode() == viewTree {
			return m.toggleAllTreeNodes(), true
		}
	case "/":
		// Focus the search box at the top of the list. While it is focused Base
		// owns every key (typing narrows the list; esc clears the query and
		// leaves the box), so there is no esc case here to shadow it.
		if m.Base.StartFilter() {
			return nil, true
		}
	case "+", "=":
		// Move the selected item one step EARLIER in its sibling sequence (its
		// workflow then runs sooner). "=" is accepted because "+" is a shifted
		// key on most layouts and the shifted/unshifted pair is easy to hit with
		// the wrong finger.
		if src == srcWorkItems && m.ViewMode() == viewTree {
			return m.reorderChildren(-1), true
		}
	case "-", "_":
		// Move the selected item one step LATER in its sibling sequence.
		if src == srcWorkItems && m.ViewMode() == viewTree {
			return m.reorderChildren(1), true
		}
	case "y", "a", "R":
		if a, ok := m.actionByKey(msg.String()); ok {
			return m.openAction(a), true
		}
	case kit2.DeleteChord, kit2.DeleteChordAlias:
		// ONE delete gesture, TWO keys. `ctrl+x` is the client's canonical delete chord
		// (kit2.DeleteChord — the operator asked for it "across the board", and the
		// Execution panes have answered it ever since) and a bare `x` is what these panes
		// have always used. Accepting both is the point of this fix: the operator pressed
		// the chord they had asked for and NOTHING HAPPENED, because this screen only knew
		// its own.
		//
		// The action's Key is the canonical one, so the bar advertises `ctrl+x:delete` and
		// every pane in the client names the same binding.
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
	// The form's own width must reflect the box it is drawn in, or its
	// horizontal window (and its caret) would be computed against the terminal
	// rather than the dialog's inner width.
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

// toggleAllTreeNodes flips every node of the work-item Tree at once — the
// operator's "overall collapse and expand". A no-op on a flat list (no parents)
// and on Board/Archive, which have no tree.
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
		return theme.HintText.Render("n: new project · e: edit · d: set+create dir · enter: detail · ←/→: pane")
	case srcImages:
		return theme.HintText.Render("n: new image · e: edit spec · b: build (live logs) · x: delete · enter: detail")
	default:
		return theme.HintText.Render("n: new · /: search · e: edit · s: status · y: auto-start · +/-: move step · a: archive · x: delete · v/T/Z: view · o/O: collapse · enter: detail")
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
