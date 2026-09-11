// Package tui is orch's app shell: the six-tab shell (Ask, Work,
// Execution, Automation, Enforcement, Control) mirroring the GUI nav
// (frontend/src/lib/nav-config.ts + app-shell.tsx), with the global key
// router, status footer, and help overlay.
package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"
	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
	"github.com/beardedparrott/orchicon/internal/tui/diffs"
	"github.com/beardedparrott/orchicon/internal/tui/dock"
	"github.com/beardedparrott/orchicon/internal/tui/screens/ask"
	"github.com/beardedparrott/orchicon/internal/tui/screens/automation"
	"github.com/beardedparrott/orchicon/internal/tui/screens/control"
	"github.com/beardedparrott/orchicon/internal/tui/screens/enforcement"
	"github.com/beardedparrott/orchicon/internal/tui/screens/execution"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/screens/work"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
	"github.com/beardedparrott/orchicon/internal/version"
)

// TabID is a stable screen identifier.
type TabID string

// The six GUI nav domains.
const (
	TabAsk         TabID = "ask"
	TabWork        TabID = "work"
	TabExecution   TabID = "execution"
	TabAutomation  TabID = "automation"
	TabEnforcement TabID = "enforcement"
	TabControl     TabID = "control"
)

// Tab is one shell tab (GUI nav domain).
type Tab struct {
	ID      TabID
	Title   string // GUI nav label
	Chord   string // ctrl+<key>
	Ordinal string // "1"…"6" — numbered tab chrome (mockup parity)
}

// Tabs is the top tab bar, in GUI nav order. Ordinals number the tabs so
// the tab bar matches the mockup's "1 · 2 · 3…" chrome; clicking a tab
// (mouse) is wired in the shell dispatcher.
var Tabs = []Tab{
	{TabAsk, "Ask Orchicon", "ctrl+o", "1"},
	{TabWork, "Work", "ctrl+w", "2"},
	{TabExecution, "Execution", "ctrl+e", "3"},
	{TabAutomation, "Automation", "ctrl+a", "4"},
	{TabEnforcement, "Enforcement", "ctrl+f", "5"},
	{TabControl, "Control", "ctrl+t", "6"},
}

// Screen is the contract every area screen implements (alias of the
// screenkit interface so shell code stays short).
type Screen = screenkit.Screen

// focusMode is where keyboard focus lives: the content pane (screens
// keep tab chords + editing) or the chat composer (global tab chords
// fall through to readline editing inside the input).
type focusMode int

const (
	focusContent focusMode = iota
	focusComposer
)

// App is the root model.
type App struct {
	clients   *client.Clients
	profile   *config.Profile
	reg       *subs.Registry
	screens   map[TabID]Screen
	factories map[TabID]func() Screen
	active    TabID
	width     int
	height    int
	footer    footerModel
	help      helpModel
	routes    []KeyRoute
	quitting  bool

	// Chat dock state (feature: context-aware Ask Orchicon + slash).
	dock               dock.Model
	chat               *chat.Controller
	chatStore          *chatStore    // guarded chatItems (stream goroutine writes)
	chatWake           chan struct{} // live-chunk repaint poke (cap 1)
	chatCmds           chan tea.Cmd  // goroutine follow-ups (watch re-dial, poll)
	chatFocus          focusMode
	mouseEnabled       bool // tea.WithMouseCellMotion is on; footer shows "Mouse Enabled"
	palette            palette
	slash              *slashRegistry
	contextOverride    string // /context pin <desc>
	// Tab dropdown submenus (Phase 2a): per-tab menus built from nav
	// config; menuOpen = the tab whose dropdown is open ("" = closed).
	menus     map[TabID]*TabMenu
	menuOpen  TabID
	navReg    []NavEntry
	themes    []string
	reconnectRequested bool
	chatConvID         string                     // active conversation ("" = none yet)
	execSessions       map[string][]chat.ChatItem // execution id → durable session items
	pendingDetail      tea.Cmd
	lastScreenKeys     string

	// Diff sidebar (TUI sibling of the GUI DiffSidebar). The shell owns the
	// open/tab/selected state so it persists across SwitchTo (the GUI
	// persists it at the host). The pane is a left rail that slides out over
	// the content; when open, the main screen + chat dock reflow by
	// DiffPaneWidth.
	diffOpen bool
	diffPane *diffs.Model
	diffTab  diffs.Tab
	diffPath string

	// Ask conversations rail (GUI Ask sidebar). OPEN by default; collapsible
	// via ctrl+r toggle and a mouse click on the rail header. State persists
	// for the session. The diff pane is the LEFT rail; this is the RIGHT rail.
	conversations []chat.Conversation
	convRailOpen  bool
	convSel       int
	convScroll    int
	// Rail load state (Phase 2c finding 9): the rail is never a silent
	// empty box — a failed load holds an explicit error + retry state.
	convErr     string // last rail load failure (auth/API); "" = healthy
	convLoaded  bool   // a successful rail load has landed
	convLoading bool   // a rail load is in flight

	// rightRailOpen records the Ask screen's right-rail visibility; kept so
	// the renderer knows whether to draw the rail without re-deriving it.
	rightRailOpen bool

	// pendingDiffCmd carries the diff-pane owner-setup cmd out of a route
	// Handle (routes can't return a tea.Cmd; dispatch re-emits it).
	pendingDiffCmd tea.Cmd
	// pendingRailCmd carries the conversations-rail retry/reload cmd out of
	// a route or a mouse handler that cannot return one directly.
	pendingRailCmd tea.Cmd
}

// DiffPaneWidth is the left rail width (cells). Mirrors the GUI's ~480px
// rail proportionally at a typical 96-col terminal.
const DiffPaneWidth = 48

// NewApp builds the shell over an established client set.
func NewApp(cl *client.Clients, profile *config.Profile, serverVersion string) *App {
	m := &App{
		clients:      cl,
		profile:      profile,
		reg:          subs.NewRegistry(),
		screens:      map[TabID]Screen{},
		chatStore:    &chatStore{items: map[string][]chat.ChatItem{}},
		execSessions: map[string][]chat.ChatItem{},
		footer: footerModel{
			URL:           profile.URL,
			ServerVersion: serverVersion,
			ClientVersion: version.Current().Tag,
		},
	}
	m.dock = dock.New()
	if profile != nil && profile.Newline != "" {
		m.dock.Newlines = dock.ParseNewlineMode(profile.Newline)
	}
	// The diff pane shares the shell's client set + registry so its live
	// StreamFileEdits subscription follows the same reconnect/resume/dedup
	// semantics and is closed by CloseAll on screen close.
	m.diffPane = diffs.NewModel(cl, m.reg)
	m.diffTab = diffs.TabDiff
	m.chat = chat.NewController(cl)
	m.chatWake = make(chan struct{}, 1)
	m.chatCmds = make(chan tea.Cmd, 16)
	m.chat.Bind(&appEventStore{m: m}, m.chatCmds)
	// One factory per tab — screens construct lazily on first visit so
	// stream subscriptions only exist while their tab is active.
	// Screens take tenantID="" for their streams: the plane resolves the
	// tenant from the bearer credential (internal/project/service.go —
	// StreamProjectEvents ignores req.TenantId), so orch never guesses one.
	m.factories = map[TabID]func() Screen{
		TabAsk:         func() Screen { s := ask.New(cl, m.reg); s.SetShell(m); return s },
		TabWork:        func() Screen { return work.New(cl, m.reg, "") },
		TabExecution:   func() Screen { s := execution.New(cl, m.reg, ""); s.SetShell(m); return s },
		TabAutomation:  func() Screen { return automation.New(cl, m.reg, "") },
		TabEnforcement: func() Screen { s := enforcement.New(cl, m.reg, ""); s.SetShell(m); return s },
		TabControl:     func() Screen { return control.New(cl, m.reg) },
	}
	// The slash registry is generated from the screens' Sources() (the
	// no-drift source of truth), so factories must exist before it builds.
	// The nav registry (tab dropdown entries) shares that source — build it
	// ONCE here and let buildSlashRegistry consume it, so the two can never
	// disagree (and openTabMenu sees the same entries as /work-items).
	m.navReg = buildNavEntries(m)
	m.slash = buildSlashRegistry(m)
	m.routes = GlobalKeyRoutes(Tabs)
	m.mouseEnabled = true // cmd/orch runs tea.WithMouseCellMotion()
	m.convRailOpen = true // Ask conversations rail OPEN on default (GUI parity)
	m.rightRailOpen = true
	m.menus = map[TabID]*TabMenu{}
	m.themes = theme.Names()
	// Phase 2a (operator finding 3): the composer is the launch focus —
	// typing works immediately, no ctrl+g needed.
	m.chatFocus = focusComposer
	m.dock.Focus()
	m.footer.ComposerFocus = true
	if profile != nil && profile.Theme != "" && theme.Use(profile.Theme) {
		// config-selected theme already applied (styles are package state).
	} else {
		theme.Use(theme.DefaultName)
	}
	return m
}

// SetIdentity sets the footer identity (display name / subject).
func (m *App) SetIdentity(name string) { m.footer.Identity = name }

// RegisterScreen installs a screen for a tab (tests / custom wiring;
// overrides the factory).
func (m *App) RegisterScreen(id TabID, s Screen) {
	m.screens[id] = s
	if m.active == "" {
		m.active = id
	}
}

// SwitchTo activates a tab (lazily constructing its screen; the previous
// screen is closed = unsubscribed, useStream semantics).
func (m *App) SwitchTo(id TabID) {
	if m.active == id {
		// Re-selecting the OPEN tab's chord/click closes its dropdown
		// (toggle); with no menu it just re-arms streams.
		if m.menuOpen == id {
			m.closeTabMenu()
		} else {
			m.EnsureSubscriptions(id)
		}
		return
	}
	if old, ok := m.screens[m.active]; ok && old != nil {
		old.Close()
	}
	m.active = id
	if _, ok := m.screens[id]; !ok {
		if f, ok := m.factories[id]; ok {
			s := f()
			m.screens[id] = s
		}
	}
	// Always (re)apply the layout to the screen being activated: a screen
	// cached earlier (e.g. the nav registry's introspection build, or a
	// pre-resize visit) never saw a SetSize and would otherwise render
	// against a zero-sized content region.
	if s := m.screens[id]; s != nil && m.width > 0 {
		s.SetSize(m.contentWidth(), m.contentHeight())
	}
	// A tab switch swaps the chrome's submenu to the new tab's (the
	// dropdown follows focus, per the mockup's menu-under-tab pattern);
	// switching to the OPEN tab's neighbor while its menu is up moves the
	// menu. A fresh SwitchTo with no menu open leaves it closed (the
	// mouse route toggles explicitly around this call).
	if m.menuOpen != "" {
		if m.menuOpen == id {
			m.closeTabMenu()
		} else {
			m.openTabMenu(id)
		}
	}
	m.updateContextChip()
	// The diff pane's open state persists across SwitchTo (the GUI persists
	// it at the host). Re-point it at the new tab's owner (if any) so it
	// shows the active session without resetting open/tab/selected.
	if m.diffOpen {
		m.refreshDiffOwner()
	}
}

// EnsureSubscriptions starts the screen's live event streams if it has
// any and they are not already running (idempotent). Called on every tab
// chord (re-press = re-arm) and on the first layout, so the opening tab
// starts live without extra keys.
func (m *App) EnsureSubscriptions(id TabID) {
	if s := m.screens[id]; s != nil {
		if es, ok := s.(interface{ EnsureSubscriptions() }); ok {
			es.EnsureSubscriptions()
		}
	}
}

// ActiveTab returns the active tab ID.
func (m *App) ActiveTab() TabID { return m.active }

// NextTab / PrevTab cycle the tab bar.
func (m *App) NextTab() { m.cycle(1) }
func (m *App) PrevTab() { m.cycle(-1) }

func (m *App) cycle(delta int) {
	idx := 0
	for i, t := range Tabs {
		if t.ID == m.active {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(Tabs)) % len(Tabs)
	m.SwitchTo(Tabs[idx].ID)
}

func (m *App) contentHeight() int {
	// Fixed shell overhead (Phase 2a full-screen chrome): the centered tab
	// bar + underline rule (2), the ONE blank separator after the chrome,
	// the footer strip, and the composer pinned to the bottom (dock.Lines
	// = input + notice strips). Screens get every remaining row; the
	// content column is always m.height - 4 - dock.Lines() so the
	// composer lands flush against the footer on every screen.
	h := m.height - 4 - m.dock.Lines()
	if h < 1 {
		h = 1
	}
	return h
}

// contentWidth is the main content area width. The left diff pane and the
// Ask right conversations rail both consume columns; the screen and dock
// reflow into the remainder.
func (m *App) contentWidth() int {
	w := m.width
	if m.diffOpen {
		w -= DiffPaneWidth
	}
	if m.railVisible() {
		w -= ConversationsRailWidth
	}
	if w < 1 {
		w = 1
	}
	return w
}

// dockHeight is the rows the dock renders.
func (m *App) dockHeight() int { return m.dock.Lines() }

// diffOwner derives the pane's owner from the active context: the execution
// screen's selected detail (owner_kind "execution", owner_id = DetailID) or
// the ask screen's active conversation (owner_kind "ask_conversation",
// owner_id = chatConvID). Empty (kind, id) → the pane has no diff-relevant
// session and the toggle is a no-op.
func (m *App) diffOwner() (kind, id string) {
	switch m.active {
	case TabExecution:
		if s := m.screens[TabExecution]; s != nil {
			if d, ok := s.(interface{ DetailID() string }); ok && d.DetailID() != "" {
				return "execution", d.DetailID()
			}
		}
	case TabAsk:
		if m.chatConvID != "" {
			return "ask_conversation", m.chatConvID
		}
	}
	return diffs.NoneOwner, diffs.NoneOwner
}

// diffOwnerLive reports whether the owner's session is live (a running
// execution or an open ask conversation). A completed owner is not live —
// its diff is the durable, git-reconciled ledger only.
func (m *App) diffOwnerLive(kind, id string) bool {
	if kind == "execution" {
		running, _ := m.runningExecutionID()
		return running == id && running != ""
	}
	// ask_conversation: live while the conversation is active with a running
	// turn (chat.IsStreaming). Otherwise treat as durable-only.
	return m.chat != nil && m.chat.IsStreaming(id)
}

// openDiffPane opens the pane and points it at the active owner. If the
// owner changed, it refetches the durable ledger + (re)arms the live stream.
func (m *App) openDiffPane() tea.Cmd {
	if m.diffPane == nil {
		return nil
	}
	// NOTE: the composer-vs-content focus rule lives on the `d`/`D` ROUTE
	// (so `d` stays a literal character while composing); an explicit
	// request (the /diff command) opens the rail regardless of focus.
	kind, id := m.diffOwner()
	if kind == diffs.NoneOwner || id == diffs.NoneOwner {
		// No diff-relevant session on this screen — the toggle is a no-op.
		return nil
	}
	m.diffOpen = true
	m.restoreDiffPaneState()
	m.diffPane.SetSize(DiffPaneWidth, m.contentHeight()+m.dock.Lines())
	m.refreshLayout()
	// The pane keeps its previously selected path if it matches this owner's
	// files; otherwise the SetOwner fetch defaults it (see diffs.Model).
	return m.diffPane.SetOwner(kind, id, m.diffOwnerLive(kind, id))
}

// closeDiffPane closes the pane, tears down its live stream, and restores
// the previous layout (the content + dock widths are re-derived on the next
// render by contentWidth). The shell-owned open/tab/selected state is kept
// so re-opening restores the last view.
func (m *App) closeDiffPane() {
	if m.diffOpen {
		m.diffOpen = false
	}
	if m.diffPane != nil {
		// Persist the pane's current tab/selection back to the shell before
		// tearing down the live stream.
		m.syncDiffPaneState()
		m.diffPane.Close()
	}
	// Restore the full-width layout (the screen + dock reflow back).
	m.refreshLayout()
}

// reflowForDiff re-applies the current layout to the screen and dock given
// whether the diff pane is open (the pane consumes DiffPaneWidth columns).
// Called after toggling the pane, so the content reflows without waiting for
// the next terminal resize.
// reflowForDiff was unified into refreshLayout (Phase 2a): one layout
// applier for window resize, rail toggles, and diff-pane toggles.
func (m *App) refreshLayout() {
	if s := m.screens[m.active]; s != nil && m.width > 0 {
		s.SetSize(m.contentWidth(), m.contentHeight())
	}
	if m.width > 0 {
		m.dock.Width = m.contentWidth()
	}
	if m.diffPane != nil {
		m.diffPane.SetSize(DiffPaneWidth, m.contentHeight()+m.dock.Lines())
	}
}

// refreshDiffOwner re-points an already-open pane at the (possibly
// changed) active owner. Called when the active detail/conversation changes.
func (m *App) refreshDiffOwner() tea.Cmd {
	if !m.diffOpen || m.diffPane == nil || m.chatFocus != focusContent {
		return nil
	}
	kind, id := m.diffOwner()
	if kind == diffs.NoneOwner || id == diffs.NoneOwner {
		return nil
	}
	m.restoreDiffPaneState()
	return m.diffPane.SetOwner(kind, id, m.diffOwnerLive(kind, id))
}

// syncDiffPaneState captures the pane's current tab/selection into the
// shell-owned state (pane→shell). Called whenever the pane may have changed
// (key/mouse interactions, close) so the shell's open/tab/selected state
// stays authoritative and survives a later SwitchTo. It does NOT push the
// shell state into the pane — that direction (restoreDiffPaneState) runs on
// open/refresh so a reopened pane resumes the last view.
func (m *App) syncDiffPaneState() {
	if m.diffPane == nil {
		return
	}
	m.diffTab = m.diffPane.Tab
	if m.diffPane.SelectedPath != "" {
		m.diffPath = m.diffPane.SelectedPath
	}
}

// restoreDiffPaneState applies the shell-owned open/tab/selected state to the
// pane (shell→pane). Called when the pane is (re)opened or re-pointed at a
// new owner so the pane resumes the last view rather than resetting to the
// default diff tab.
func (m *App) restoreDiffPaneState() {
	if m.diffPane == nil {
		return
	}
	m.diffPane.SetTab(m.diffTab)
	if m.diffPath != "" {
		m.diffPane.SelectPath(m.diffPath)
	}
}

// reconnectStreams forces every live subscription to redial now.
func (m *App) reconnectStreams() { m.reg.ReconnectAll() }

func (m *App) updateContextChip() {
	if s := m.screens[m.active]; s != nil {
		if c, ok := s.(interface{ ContextChip() string }); ok {
			m.footer.ContextChip = c.ContextChip()
			m.dock.Chip = m.activeContextLabel()
			return
		}
	}
	m.footer.ContextChip = ""
	m.dock.Chip = ""
}

// activeContext is the context engine read: what the active screen is
// showing, as (tab, source, itemID, label). Used by outgoing message
// preambles and the /context command.
func (m *App) activeContext() (tab TabID, src, id, label string) {
	tab = m.active
	s := m.screens[m.active]
	if s == nil {
		return tab, "", "", ""
	}
	type ctxer interface {
		ActiveContext() (string, string, string)
	}
	if c, ok := s.(ctxer); ok {
		src, id, label = c.ActiveContext()
		return tab, src, id, label
	}
	// generic Base-derived fallback
	type sourcer interface {
		ActiveSourceName() string
	}
	type itemer interface {
		ActiveItem() (screenkit.Item, bool)
	}
	if sr, ok := s.(sourcer); ok {
		src = sr.ActiveSourceName()
		if it, ok2 := s.(itemer); ok2 {
			if it, found := it.ActiveItem(); found {
				id = it.ID
				label = it.Title
			}
		}
	}
	return tab, src, id, label
}

// activeContextLabel renders the chip: "<SourceTitle>: <item>" (or the
// pinned override), with a running prefix when the selected execution
// is live.
func (m *App) activeContextLabel() string {
	if m.contextOverride != "" {
		return m.contextOverride
	}
	_, src, id, label := m.activeContext()
	if src == "" {
		return ""
	}
	title := srcTitle(m, src)
	out := title
	if label != "" {
		out += ": " + label
	} else if id != "" {
		out += ": " + id
	}
	return out
}

// srcTitle resolves a source name to its display title across screens.
func srcTitle(m *App, src string) string {
	s := m.screens[m.active]
	if s == nil {
		return src
	}
	type sourcer interface{ Sources() []screenkit.SourceMeta }
	if sr, ok := s.(sourcer); ok {
		for _, sm := range sr.Sources() {
			if sm.Name == src {
				return sm.Title
			}
		}
	}
	return src
}

// contextPreamble builds the bracketed context line prepended to every
// outgoing chat message (the only context shape the Ask API accepts —
// plan fact 2). Empty when nothing is selected.
func (m *App) contextPreamble() string {
	label := m.contextOverride
	if label == "" {
		label = m.activeContextLabel()
	}
	if label == "" {
		return ""
	}
	return "[context: " + label + "]"
}

// contextPreambleLabel is the human-readable chip for /context.
func (m *App) contextPreambleLabel() string {
	if m.contextOverride != "" {
		return m.contextOverride
	}
	if l := m.activeContextLabel(); l != "" {
		return l
	}
	return "(none — no screen/entity selected)"
}

// selectScreenSource focuses the named source on the tab's screen.
func (m *App) selectScreenSource(tab TabID, src string) {
	s := m.screens[tab]
	if s == nil {
		return
	}
	type selSrc interface{ SelectSource(string) bool }
	if ss, ok := s.(selSrc); ok {
		ss.SelectSource(src)
	}
}

// selectScreenItem selects an item by ID and loads its detail (arg
// jumps). Falls back to RequestDetail when the item is not on the
// loaded page (detail works without a list hit — plan §2).
func (m *App) selectScreenItem(tab TabID, src, id string) {
	s := m.screens[tab]
	if s == nil {
		return
	}
	type selItem interface{ SelectItem(src, id string) bool }
	type reqDetail interface{ RequestDetail(src, id string) tea.Cmd }
	si, ok1 := s.(selItem)
	rd, ok2 := s.(reqDetail)
	if !ok1 && !ok2 {
		return
	}
	found := false
	if ok1 {
		found = si.SelectItem(src, id)
	}
	if !found && ok2 {
		m.pendingDetail = rd.RequestDetail(src, id)
	} else if ok2 {
		m.pendingDetail = rd.RequestDetail(src, id)
	}
}

// OpenAskConversation makes the conversation the active chat target and
// loads its durable transcript (the ask screen's onDetail hook; live
// chunks then stream into the open pane).
func (m *App) OpenAskConversation(id string) tea.Cmd {
	if id == "" {
		return nil
	}
	if m.chatConvID == id {
		return nil
	}
	m.chatConvID = id
	m.chat.SetActive(id)
	return m.chat.OpenConversation(id)
}

// onExecutionSessionLoaded paints the merged session view into the
// execution detail pane: durable GetExecutionSession parts + live
// StreamExecutionEvents (MergeSessionItems drops covered chunks, sorts,
// phase-groups).
func (m *App) onExecutionSessionLoaded(execID string, parts []*apiv1.ExecutionSessionPart, err error) tea.Cmd {
	if err != nil {
		m.setChatErrorPlain("session: " + err.Error())
		return nil
	}
	history := chat.HistoryItems(parts)
	m.execSessions[execID] = history
	return m.refreshExecutionSession(execID)
}

// refreshExecutionSession merges the cached durable parts with the
// screen's current live events and repaints the detail pane.
func (m *App) refreshExecutionSession(execID string) tea.Cmd {
	s := m.screens[TabExecution]
	if s == nil {
		return nil
	}
	ex, ok := s.(interface {
		SessionEvents(execID string) []*apiv1.StreamExecutionEventsResponse
		RenderSession(items []chat.ChatItem)
	})
	if !ok {
		return nil
	}
	if s.(interface{ DetailID() string }).DetailID() != execID {
		return nil
	}
	history := m.execSessions[execID]
	live := chat.LiveItems(ex.SessionEvents(execID))
	ex.RenderSession(chat.MergeSessionItems(history, live))
	return nil
}

// RefreshExecutionSession is the event-poke entry (live repaint).
func (m *App) RefreshExecutionSession(execID string, events []*apiv1.StreamExecutionEventsResponse) {
	_ = events // events come from the screen's SessionEvents; kept for testability
	_ = m.refreshExecutionSession(execID)
}

// OpenExecutionSession loads an execution's durable session (the
// execution screen's onDetail hook) and merges live events.
func (m *App) OpenExecutionSession(execID string) tea.Cmd {
	if execID == "" {
		return nil
	}
	cl := m.clients
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		resp, err := cl.Executions.GetExecutionSession(ctx, connect.NewRequest(&apiv1.GetExecutionSessionRequest{
			ExecutionId: execID,
			Limit:       200,
		}))
		if err != nil {
			return execSessionMsg{execID: execID, err: err}
		}
		return execSessionMsg{execID: execID, parts: resp.Msg.GetParts()}
	}
}

// execSessionMsg carries the durable session parts for one execution.
type execSessionMsg struct {
	execID string
	parts  []*apiv1.ExecutionSessionPart
	err    error
}

// navEntries returns (building once) the tab's dropdown rows from the
// screen inventory (buildNavEntries over Sources()) — the same no-drift
// source as the slash registry, per docs/tui-parity.md.
func (m *App) navEntries(tab TabID) []NavEntry {
	if m.navReg == nil {
		m.navReg = buildNavEntries(m)
	}
	var out []NavEntry
	for _, e := range m.navReg {
		if e.Tab == tab {
			out = append(out, e)
		}
	}
	return out
}

// streamStatus derives the footer state from the active screen's
// reported subscription statuses (worst wins).
func (m *App) streamStatus() streamStatusString {
	s := m.screens[m.active]
	if s == nil {
		return openStatus
	}
	rp, ok := s.(screenkit.StatusReporter)
	if !ok {
		return openStatus
	}
	worst := openStatus
	for _, st := range rp.ReportStatus() {
		if statusRank(streamStatusString(st.Status)) > statusRank(worst) {
			worst = streamStatusString(st.Status)
		}
	}
	return worst
}

// Init implements tea.Model.
func (m *App) Init() tea.Cmd {
	var cmds []tea.Cmd
	if s := m.screens[m.active]; s != nil {
		cmds = append(cmds, s.Init())
	}
	cmds = append(cmds, m.waitChat(), m.chat.LoadConversations())
	return tea.Batch(cmds...)
}

// Update implements tea.Model (via the router dispatch).
func (m App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	return m.dispatch(msg)
}

// passToScreen forwards to the active screen. When the diff pane is open it
// first lets the pane consume pane-scoped messages (its own async results,
// the file-edits live-stream status/pokes, and key/mouse events that land
// in the pane rail) before the screen sees them.
func (m *App) passToScreen(msg tea.Msg) (*App, tea.Cmd) {
	if m.help.open {
		if k, ok := msg.(tea.KeyMsg); ok && (k.String() == "esc" || k.String() == "?") {
			m.help.open = false
			return m, nil
		}
		return m, nil // overlay swallows keys
	}
	if _, ok := msg.(tea.WindowSizeMsg); ok {
		// handled in dispatch (App-level sizing)
		return m, nil
	}
	if m.diffOpen && m.diffPane != nil {
		if consumed, cmd := m.diffMsg(msg); consumed {
			return m, cmd
		}
	}
	s := m.screens[m.active]
	if s == nil {
		return m, nil
	}
	ns, cmd := s.Update(msg)
	m.screens[m.active] = ns
	// The Ask screen's detail pane is the transcript surface: whenever it
	// re-renders (e.g. a conversation detail just loaded — including from a
	// rail click), relay the merged transcript into it. No-op unless the
	// open detail is the active conversation (operator finding 9).
	if m.active == TabAsk {
		m.onChatWake()
	}
	m.updateContextChip()
	m.footer.StreamStatus = m.streamStatus()
	m.footer.Width = m.width
	m.footer.ComposerFocus = m.chatFocus == focusComposer
	return m, cmd
}

// diffMsg forwards a pane-scoped message to the diff pane's Update and
// reports whether the pane consumed it. Pane-scoped (consumed=true): the
// pane's own async results (FetchDoneMsg/OwnerSetMsg), the file-edits
// live-stream status/pokes, and key/mouse events that land in the left pane
// rail. Anything else returns (false, nil) so it falls through to the screen.
func (m *App) diffMsg(msg tea.Msg) (bool, tea.Cmd) {
	switch msg := msg.(type) {
	case diffs.FetchDoneMsg, diffs.OwnerSetMsg:
		return true, m.diffPane.Update(msg)
	case subs.EventPokeMsg:
		// Only the file-edits live stream is the pane's own; other pokes
		// belong to the screen and must fall through.
		if msg.Name == "file-edits" {
			return true, m.diffPane.Update(msg)
		}
		return false, nil
	case subs.StatusMsg:
		if msg.Name == "file-edits" {
			return true, m.diffPane.Update(msg)
		}
		return false, nil
	case tea.KeyMsg:
		// Global routes already consumed d/esc/y (and ctrl chords). Only
		// forward navigation keys to the pane when it is open (the pane is
		// the focus then); the screen's own list/detail navigation is
		// intentionally left to the pane while it is open. Both tab-switch
		// keys (h = previous, l = next) must reach the pane so the h/l
		// switcher is symmetric.
		switch msg.String() {
		case "h", "l", "j", "k", "up", "down", "pgup", "pgdown", "g", "G":
			cmd := m.diffPane.Update(msg)
			// The pane's tab/selection may have changed (tab-switch keys
			// h/l, or scroll state); copy it back so the shell-owned state
			// stays in sync and survives a later SwitchTo (the pane must not
			// reset the user's tab when navigating screens).
			m.syncDiffPaneState()
			return true, cmd
		}
		return false, nil
	case tea.MouseMsg:
		// Forward clicks/wheel that land in the left pane rail (x <
		// DiffPaneWidth). Motion/Release stay native (Shift+drag); the pane
		// ignores them anyway. Clicks right of the rail pass to the screen.
		if msg.X < DiffPaneWidth {
			cmd := m.diffPane.Update(msg)
			// A click may have hit the pane's ✕ close button (the mouse
			// toggle area). If so, close the pane (restore the layout) and
			// consume the event — mirroring the GUI's PanelLeftClose.
			if m.diffPane.TakeCloseRequest() {
				m.closeDiffPane()
				m.syncDiffPaneState()
				return true, cmd
			}
			// A click may switch the pane's tab or select a file; copy the
			// new tab/selection back to the shell-owned state so it persists
			// across navigation.
			m.syncDiffPaneState()
			return true, cmd
		}
		return false, nil
	}
	return false, nil
}

// View implements tea.Model. Full-screen takeover (Phase 2a): the view
// is EXACTLY m.height rows × m.width columns, every cell carrying the
// theme's solid background — zero terminal bleed-through on every screen.
// The budget is fixed chrome, not intrinsic content height:
//
//	row 0            centered tab bar
//	row 1            full-width underline rule
//	row 2            one blank separator (ScreenBg)
//	rows 3…          the active screen (contentHeight() rows — every
//	                 screen consumes the FULL budget, never its intrinsic
//	                 content height) with the dock block pinned beneath it
//	                 (rails join this region as extra columns)
//	last row         the one-line footer
//
// fillView then pins the assembled grid to exactly w×h through ScreenBg.
// The slash palette floats above the composer when open; the tab
// dropdown overlays the body region.
func (m App) View() string {
	if m.quitting {
		return ""
	}
	w, h := m.width, m.height
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	// ONE opaque full-viewport frame is painted FIRST on every path
	// (baseView); every overlay is a layer INSIDE that frame — never an
	// inline drawing over the terminal (Phase 3 overlay discipline). The
	// final fillView re-normalizes the composited result to exactly w×h so
	// no overlay can ever change the row count or open a hole.
	base := m.baseView(w, h)
	if m.help.open {
		overlay := m.help.view(m.routes) + "\n" + strings.Join(m.slash.helpLines(), "\n")
		return fillView(m.overlayCentered(base, overlay), w, h)
	}
	if m.palette.connectOpen {
		return fillView(m.overlayCentered(base, m.connectOverlayView()), w, h)
	}
	if m.palette.PaletteOpen() {
		base = m.paletteComposerView(base)
	}
	base = m.composeView(base)
	return fillView(base, w, h)
}

// baseView paints the opaque full-viewport shell grid: the centered tab
// bar, the underline rule, one separator row, the active screen block,
// the composer dock, and the one-line footer — every cell carrying the
// theme's solid background, so nothing bleeds through from beneath the
// alt-screen frame.
func (m App) baseView(w, h int) string {
	cw := m.contentWidth()
	screenRows := m.contentHeight()
	dockRows := m.dock.Lines()
	if screenRows < 1 {
		screenRows = 1
	}
	// Center column: the active screen block + the dock block beneath it,
	// each normalized to its budget row count (padded with the theme
	// background when short, truncated when over — the composer can never
	// be pushed off-screen by an over-tall screen render).
	screenBlock := normalizeBlock(safeView(m.screens[m.active]), cw, screenRows)
	dockBlock := normalizeBlockKeepTail(m.dock.View(), cw, dockRows)
	bodyLines := append(append([]string{}, screenBlock...), dockBlock...)
	body := strings.Join(bodyLines, "\n")
	// Left diff rail / right conversations rail: extra COLUMNS joined over
	// the screen+dock region (the gap row spans the full width alone).
	if m.diffOpen && m.diffPane != nil && m.diffPane.HasOwner() {
		pane := strings.Join(normalizeBlock(m.diffPane.View(), DiffPaneWidth, screenRows+dockRows), "\n")
		body = lipgloss.JoinHorizontal(lipgloss.Top, pane, body)
	}
	if m.railVisible() {
		rail := strings.Join(normalizeBlock(m.rightRailView(), ConversationsRailWidth, screenRows+dockRows), "\n")
		body = lipgloss.JoinHorizontal(lipgloss.Top, body, rail)
	}
	// Assemble the exact-height grid: chrome · gap · body · footer.
	gap := theme.ScreenBg.Render(strings.Repeat(" ", w))
	footerLine := normalizeBlock(m.footer.View(), w, 1)
	rows := make([]string, 0, h)
	rows = append(rows, m.centeredTabBarView(), m.tabBarUnderlineView(), gap)
	rows = append(rows, strings.Split(body, "\n")...)
	rows = append(rows, footerLine...)
	return fillView(strings.Join(rows, "\n"), w, h)
}

// safeView renders the active screen, tolerating a nil screen (the shell
// is briefly screenless before the first SwitchTo).
func safeView(s Screen) string {
	if s == nil {
		return theme.HintText.Render("select an area")
	}
	return s.View()
}

// normalizeBlock pins a rendered block to exactly h rows × w columns:
// over-long rows are ANSI-aware truncated, short rows background-padded
// (the padding cells are painted by fillView's ScreenBg pass), and the
// row count is exact — a short block never leaves the rows below it to
// drift up (the pre-Phase-2a bleed-through), an over-tall block never
// pushes the composer/footer off-screen.
func normalizeBlock(block string, w, h int) []string {
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	lines := strings.Split(block, "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	for i, l := range lines {
		lines[i] = padScreenLine(l, w)
	}
	for len(lines) < h {
		lines = append(lines, strings.Repeat(" ", w))
	}
	return lines
}

// normalizeBlockKeepTail is normalizeBlock for blocks whose LAST row is
// the load-bearing one (the dock: chip/notice strips sit above the input
// line). Overflow drops rows from the TOP (the chip first) so the
// composer input line survives an over-tall block.
func normalizeBlockKeepTail(block string, w, h int) []string {
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	lines := strings.Split(block, "\n")
	if len(lines) > h {
		lines = lines[len(lines)-h:]
	}
	for i, l := range lines {
		lines[i] = padScreenLine(l, w)
	}
	for len(lines) < h {
		lines = append([]string{strings.Repeat(" ", w)}, lines...)
	}
	return lines
}

// fillView pins content to the exact viewport: every rendered line is
// padded to w cells (truncated on overflow) and the whole grid is filled
// to exactly h rows, ALL through ScreenBg so every cell is opaque theme
// background — no terminal bleed-through anywhere.
func fillView(content string, w, h int) string {
	lines := strings.Split(content, "\n")
	for i, l := range lines {
		lines[i] = padScreenLine(l, w)
	}
	for len(lines) < h {
		lines = append(lines, strings.Repeat(" ", w))
	}
	if len(lines) > h {
		lines = lines[:h]
	}
	return strings.Join(lines, "\n")
}

// padScreenLine renders one row at exactly w cells: overlong lines are
// ANSI-aware truncated, short lines background-padded. Every cell —
// including padding — carries the theme's solid background.
func padScreenLine(l string, w int) string {
	cols := lipgloss.Width(l)
	if cols > w {
		l = ansi.Truncate(l, w, "")
		cols = w
	}
	if cols < w {
		l += strings.Repeat(" ", w-cols)
	}
	return theme.ScreenBg.Render(l)
}

// ReconnectRequested reports whether the shell exited for /connect
// (main.go re-opens the connection screen instead of quitting).
func (m *App) ReconnectRequested() bool { return m.reconnectRequested }

// Profile returns the active config profile (main.go re-reads it after
// a /connect cycle).
func (m *App) Profile() *config.Profile { return m.profile }

// Footer returns footer internals for tests.
func (m App) Footer() footerModel { return m.footer }

// StreamStatus exposes the footer's stream state.
func (m App) StreamStatus() streamStatusString { return m.streamStatus() }

// --- chat dock: shell-side handlers ---------------------------------------

// waitChat arms the shell's chat waiter: one tea.Cmd parked on the
// wake/cmd channels, returning whichever fires first. The shell re-arms
// after every handled chat message (same re-armable pattern as the
// subs status channels).
func (m *App) waitChat() tea.Cmd {
	wake, cmds := m.chatWake, m.chatCmds
	return func() tea.Msg {
		select {
		case <-wake:
			return chatWakeMsg{}
		case c := <-cmds:
			return chatCmdMsg{cmd: c}
		}
	}
}

// chatWakeMsg: live chat chunks landed (repaint the open transcript).
type chatWakeMsg struct{}

// chatCmdMsg carries a goroutine-produced follow-up (watch re-dial,
// transcript poll) into the tea loop.
type chatCmdMsg struct{ cmd tea.Cmd }

// appEventStore adapts the App into chat.EventStore (live chunks land
// in the per-conversation item list). Mutations go through chatStore
// (mutex-guarded); chatWake pokes the tea loop for a repaint.
type appEventStore struct{ m *App }

func (s appEventStore) AppendLiveItem(convID string, item chat.ChatItem) {
	s.m.chatStore.append(convID, item)
	select {
	case s.m.chatWake <- struct{}{}:
	default:
	}
}

func (s appEventStore) SetReconnecting(convID string, on bool) {
	// The dock mutates on the tea loop (onChatWake) — this goroutine
	// only records state + pokes (bubbletea's value-model copies make a
	// direct dock write here invisible).
	s.m.chatStore.setReconnecting(convID, on)
	select {
	case s.m.chatWake <- struct{}{}:
	default:
	}
}

// onConversations ingests the loaded conversation list for the Ask
// screen's CONVERSATIONS rail. The rail is OPEN by default (GUI parity),
// but NO conversation is auto-opened on launch — the shell lands on a
// fresh chat context (GUI default behavior); existing conversations are
// reached only deliberately (rail click, slash command, /context, chat).
func (m *App) onConversations(msg chat.ConversationsMsg) tea.Cmd {
	m.convLoading = false
	if msg.Err != "" {
		// Explicit rail failure state (finding 9): the rail renders the
		// error + a retry affordance instead of an empty box.
		m.convErr = msg.Err
		m.setChatErrorPlain(msg.Err)
		return nil
	}
	m.convErr = ""
	m.convLoaded = true
	m.conversations = msg.Convs
	if m.convSel >= len(m.conversations) {
		m.convSel = 0
	}
	if m.convScroll > len(m.conversations) {
		m.convScroll = 0
	}
	return nil
}

// reloadConversations re-fetches the conversations rail from the live API
// (the rail's explicit retry path). Safe to call on the tea loop.
func (m *App) reloadConversations() tea.Cmd {
	if m.chat == nil {
		return nil
	}
	m.convLoading = true
	m.convLoaded = false
	return m.chat.LoadConversations()
}

// drainRailCmd returns (once) the staged conversations-rail reload cmd.
func (m *App) drainRailCmd() tea.Cmd {
	c := m.pendingRailCmd
	m.pendingRailCmd = nil
	return c
}

// inDockRows reports whether an absolute terminal row is inside the dock
// (composer) block — the chip/notice strip(s) and the input line. Used by
// the mouse router: clicking the composer must FOCUS it (finding 7), not
// just rely on the launch default.
func (m *App) inDockRows(y int) bool {
	dockRows := m.dock.Lines()
	return y >= m.height-1-dockRows && y <= m.height-2
}

// chatStore guards the per-conversation live item lists (the stream
// goroutine appends; the tea loop reads). Held by pointer so App keeps
// its value-receiver tea.Model methods.
type chatStore struct {
	mu           sync.Mutex
	items        map[string][]chat.ChatItem
	reconnecting map[string]bool
}

func (s *chatStore) append(convID string, item chat.ChatItem) {
	s.mu.Lock()
	s.items[convID] = append(s.items[convID], item)
	s.mu.Unlock()
}

func (s *chatStore) isReconnecting(convID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reconnecting[convID]
}

func (s *chatStore) setReconnecting(convID string, on bool) {
	s.mu.Lock()
	if s.reconnecting == nil {
		s.reconnecting = map[string]bool{}
	}
	s.reconnecting[convID] = on
	s.mu.Unlock()
}

func (s *chatStore) mergeHistory(convID string, history []chat.ChatItem) {
	s.mu.Lock()
	live := s.items[convID]
	s.items[convID] = append(append([]chat.ChatItem{}, history...), live...)
	s.mu.Unlock()
}

// replace swaps the conversation's items for the durable transcript —
// the completion authority (GUI semantics: the poll replaces the live
// buffer once the turn resolves, so nothing renders twice).
func (s *chatStore) replace(convID string, items []chat.ChatItem) {
	s.mu.Lock()
	s.items[convID] = append([]chat.ChatItem{}, items...)
	s.mu.Unlock()
}

func (s *chatStore) snapshot(convID string) []chat.ChatItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := s.items[convID]
	out := make([]chat.ChatItem, len(items))
	copy(out, items)
	return out
}

// onTranscript ingests the durable transcript. When the turn is no
// longer streaming the transcript REPLACES the live buffer (completion
// authority — no duplicated reply); mid-turn (reconnect refresh) it
// merges under the live chunks.
func (m *App) onTranscript(msg chat.TranscriptMsg) tea.Cmd {
	if msg.Err != "" {
		m.setChatErrorPlain(msg.Err)
		return nil
	}
	if m.chat.IsStreaming(msg.ConvID) {
		m.chatStore.mergeHistory(msg.ConvID, msg.Items)
	} else {
		m.chatStore.replace(msg.ConvID, msg.Items)
	}
	return m.onChatWake()
}

// onChatWake repaints the open ask-conversation detail pane with the
// merged, phase-grouped transcript + live chunks. Cheap (no RPC): the
// conversation detail's fields render from the last GetConversation —
// we rebuild the body only.
func (m *App) onChatWake() tea.Cmd {
	// dock-level banner first (any screen): reconnecting state from the
	// shared store, applied on the tea loop.
	if m.chatConvID != "" {
		if m.chatStore.isReconnecting(m.chatConvID) {
			m.dock.SetNotice("connection lost — re-attaching to the running turn…")
		} else if m.dock.Notice == "connection lost — re-attaching to the running turn…" {
			m.dock.SetNotice("")
		}
	}
	s := m.screens[TabAsk]
	if s == nil || m.chatConvID == "" {
		return nil
	}
	type detailIDer interface{ DetailID() string }
	type setter interface {
		SetDetailContent(title string, fields []screenkit.Field, body string)
	}
	dr, ok1 := s.(detailIDer)
	st, ok2 := s.(setter)
	if !ok1 || !ok2 || dr.DetailID() != m.chatConvID {
		return nil // detail pane is showing something else
	}
	if askS, ok := s.(interface {
		RenderTranscript([]chat.ChatItem) (title string, fields []screenkit.Field)
	}); ok {
		items := m.chatStore.snapshot(m.chatConvID)
		title, fields := askS.RenderTranscript(items)
		body := chat.RenderItems(chat.GroupByPhase(items), s.(interface{ DetailWidth() int }).DetailWidth())
		st.SetDetailContent(title, fields, body)
	}
	return nil
}

// onStreamDone resolves a finished turn: the slot clears (future sends
// go out as fresh ChatStreams, not interjections) and the poll fetches
// the final persisted transcript (completion authority).
func (m *App) onStreamDone(msg chat.StreamDoneMsg) tea.Cmd {
	m.chat.EndStream(msg.ConvID)
	return m.chat.Poll(msg.ConvID)
}

// setChatError maps a chat failure to the dock error strip (401 gets
// the re-auth prompt naming the in-place fix; the app keeps running).
func (m *App) setChatError(where string, err error) {
	if chat.IsAuthExpired(err) {
		m.setReauthBanner()
		return
	}
	m.dock.SetError(where + ": " + err.Error())
}

func (m *App) setChatErrorPlain(errText string) {
	if errText == "" {
		return
	}
	if strings.Contains(errText, "Unauthenticated") || strings.Contains(errText, "unauthenticated") {
		m.setReauthBanner()
		return
	}
	m.dock.SetError(errText)
}

// reauthBannerText is the ONE global re-auth banner. The re-auth UX is
// never duplicated: it renders either inline in the pane/rail that hit
// the 401 or once here above the composer — never both.
const reauthBannerText = "session needs re-authentication — run /connect (esc cancels; the shell reconnects in place)"

// setReauthBanner raises the single global re-auth banner. When an inline
// retry state is already on screen (the active screen's panes or the
// conversations rail) the banner is suppressed — that inline state IS the
// banner, so the operator sees exactly one.
func (m *App) setReauthBanner() {
	if m.authRetryInline() {
		return
	}
	m.dock.SetError(reauthBannerText)
}

// authRetryInline reports whether the shell already renders the re-auth
// retry state inline (a pane's fetch error the rail's own retry row).
func (m *App) authRetryInline() bool {
	if m.convErr != "" && isAuthErrText(m.convErr) {
		return true
	}
	type authRetrier interface{ HasAuthRetry() bool }
	if s := m.screens[m.active]; s != nil {
		if ar, ok := s.(authRetrier); ok && ar.HasAuthRetry() {
			return true
		}
	}
	return false
}

// sendChat sends text to the conversation with the context preamble.
func (m *App) sendChat(convID, text, preamble string) tea.Cmd {
	return m.chat.Send(convID, text, preamble)
}

// createConversationAndSend creates the first conversation lazily and
// sends the message into it (GUI CreateConversation pattern).
func (m *App) createConversationAndSend(text, preamble string) tea.Cmd {
	cl := m.clients
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		resp, err := cl.Ask.CreateConversation(ctx, connect.NewRequest(&apiv1.CreateConversationRequest{
			InitialMessage: text,
		}))
		if err != nil {
			return chat.ErrMsg{Where: "create conversation", Err: err}
		}
		convID := resp.Msg.GetConversation().GetId()
		return chatConvCreatedMsg{convID: convID, text: text, preamble: preamble}
	}
}

// chatConvCreatedMsg carries the lazy-create result back into dispatch.
type chatConvCreatedMsg struct {
	convID   string
	text     string
	preamble string
}

// runningExecutionID reports the selected execution when it is RUNNING
// (interjection context). Status comes from the execution screen.
func (m *App) runningExecutionID() (string, bool) {
	if m.active != TabExecution {
		return "", false
	}
	type running interface {
		RunningExecutionID() (string, bool)
	}
	if r, ok := m.screens[m.active].(running); ok {
		return r.RunningExecutionID()
	}
	return "", false
}

// interjectExecution sends a mid-run message into the execution's live
// session (the TUI equivalent of orchicon_send_execution_message; the
// same API path the GUI's useSendExecutionMessage uses). Reply streams
// back through the execution screen's existing ExecutionEvents sub.
func (m *App) interjectExecution(execID, text string) tea.Cmd {
	cl := m.clients
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := cl.Executions.SendExecutionMessage(ctx, connect.NewRequest(&apiv1.SendExecutionMessageRequest{
			ExecutionId: execID,
			Message:     text,
		}))
		if err != nil {
			return chat.ErrMsg{Where: "interject", Err: err}
		}
		return interjectOKMsg{execID: execID}
	}
}

// interjectOKMsg confirms the interjection landed (dock notice).
type interjectOKMsg struct{ execID string }

// EnforcementDockNotice / EnforcementDockError are the enforcement screen's
// dock surface (the screen holds the shell as `any` and type-asserts these):
// every write's outcome lands in the composer dock. A plane refusal is passed
// through VERBATIM — an approval/policy decision the plane rejects is never a
// silent no-op.
func (m *App) EnforcementDockNotice(s string) {
	if s == "" {
		return
	}
	m.dock.SetError("")
	m.dock.SetNotice(s)
}

// EnforcementDockError puts a refusal on the dock's error strip verbatim.
func (m *App) EnforcementDockError(s string) {
	if s == "" {
		return
	}
	m.dock.SetError(s)
}

// setFocus moves focus between the content pane and the composer,
// syncing the dock's textarea.
func (m *App) setFocus(f focusMode) {
	m.chatFocus = f
	if f == focusComposer {
		m.dock.Focus()
	} else {
		m.dock.Blur()
	}
}

// sendFromComposer routes composer text: slash commands dispatch,
// running-execution context interjects, everything else sends to Ask
// Orchicon (conversation lazily created on first send).
func (m *App) sendFromComposer(text string) tea.Cmd {
	if handled, cmd := m.dispatchSlash(text); handled {
		return cmd
	}
	// escaped literal slash (`\/…`) — strip the escape, send as chat
	if strings.HasPrefix(text, "\\/") {
		text = strings.TrimPrefix(text, "\\")
	}
	m.dock.SetError("")
	preamble := m.contextPreamble()
	if preamble != "" {
		m.dock.SetNotice("context injected: " + strings.Trim(preamble, "[]"))
	}
	if id, ok := m.runningExecutionID(); ok {
		return m.interjectExecution(id, text)
	}
	if m.chatConvID == "" {
		return m.createConversationAndSend(text, preamble)
	}
	m.chatStore.append(m.chatConvID, chat.ChatItem{Kind: chat.KindUser, Text: text, At: time.Now().UnixMilli(), Key: fmt.Sprintf("draft-%d", time.Now().UnixNano()), Live: true})
	return tea.Batch(m.sendChat(m.chatConvID, text, preamble), m.onChatWake())
}
