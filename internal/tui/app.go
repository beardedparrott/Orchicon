// Package tui is orch's app shell: the seven-tab shell (Ask, Overview,
// Work, Execution, Automation, Enforcement, Control) mirroring the GUI nav
// (frontend/src/lib/nav-config.ts + app-shell.tsx), with the global key
// router, status footer, and help overlay.
package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

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
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/overview"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/screens/work"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
	"github.com/beardedparrott/orchicon/internal/version"
)

// TabID is a stable screen identifier.
type TabID string

// The seven GUI nav domains.
const (
	TabAsk         TabID = "ask"
	TabOverview    TabID = "overview"
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
	Chord   string // "f1"…"f7" — the tab chord: the SAME key the bar PRINTS, lowercased
	Ordinal string // "F1"…"F7" — the key the bar prints, UNDERLINED: it IS the chord, not decoration
}

// Tabs is the top tab bar, in GUI nav order. Ordinals number the tabs so
// the tab bar matches the mockup's "1 · 2 · 3…" chrome; clicking a tab
// (mouse) is wired in the shell dispatcher.
//
// THE CHORDS ARE F1 … F7, and the label the bar prints IS the chord (only the case differs), so the
// key the operator presses and the text beside the tab cannot disagree — the same invariant the
// chord list has carried since the ctrl+letter chords were replaced, when a letter per tab sat next
// to an unrelated number: "I think we should get rid of the ctrl+letter for the tab menus up top".
//
// HOW WE GOT TO F-KEYS — three candidates, each MEASURED through a pty and bubbletea's OWN parser
// (the same one the running program uses; the probe is validated by its delivery of plain letters
// and of the chord under test), never assumed:
//
//  1. ctrl+<digit> DOES NOT EXIST as a key. ctrl+1 arrives as ctrl+q, ctrl+2 as ctrl+@, and — the
//     reason this is more than cosmetic — **ctrl+3 arrives as ESC and ctrl+8 as BACKSPACE**, because
//     a control byte is `digit & 0x1f` and 3 and 8 collide with the escape and delete bytes.
//     ctrl+4/5/6/7 arrive as ctrl+\ / ctrl+] / ctrl+^ / ctrl+_. The protocols that would
//     disambiguate (xterm's modifyOtherKeys, kitty CSI-u) are NOT negotiated by bubbletea and return
//     nothing at all.
//  2. alt+<digit> IS delivered correctly — measured: alt+1 genuinely arrives as alt+1 — but the
//     operator's EMULATOR claims it first: "I use Konsole and I bet a lot of other people do as well
//     and when I hit alt+number it moves to a different terminal tab as opposed to actually
//     affecting orch". That is claimed at the emulator's level, before any program sees the byte, so
//     no binding here can recover it. The operator's own suggestion, shift+<digit>, is ALSO not
//     bindable: shift+1 arrives as the SHIFTED SYMBOL ("!"), so binding the digits' shifted forms
//     would break typing punctuation everywhere. (Measured alongside: shift+1 → "!", ctrl+alt+1 →
//     nothing, and both disambiguating protocols → nothing.)
//  3. F1 … F7 IS delivered cleanly — measured: F1 → "f1". One keystroke, not claimed by the
//     emulator, and it collides with nothing here (no screen binds an F-key).
//
// WHICH IS WHY THE BAR NO LONGER NAMES A MODIFIER. "alt+ 1·Ask Orchicon" existed because a bare
// number beside a tab does not say what to hold down — it was a label explaining the MODIFIER. An
// F-key needs no explanation: the label printed at the tab ("F1") already names the whole key, so a
// modifier prefix would be five cells of noise in front of the operator's own tabs. The label is
// gone, and the label printed at each tab is what the operator presses.
var Tabs = []Tab{
	{TabAsk, "Ask Orchicon", "f1", "F1"},
	{TabOverview, "Overview", "f2", "F2"},
	{TabWork, "Work", "f3", "F3"},
	{TabExecution, "Execution", "f4", "F4"},
	{TabAutomation, "Automation", "f5", "F5"},
	{TabEnforcement, "Enforcement", "f6", "F6"},
	{TabControl, "Control", "f7", "F7"},
}

// Screen is the contract every area screen implements (alias of the
// screenkit interface so shell code stays short).
type Screen = screenkit.Screen

// DockError / DockNotice are the shell hooks screens use to surface
// mutation feedback (the kit2 mutate.Executor sink) in the always-present
// chat dock.
func (m *App) DockError(msg string)  { m.dock.SetError(msg) }
func (m *App) DockNotice(msg string) { m.dock.SetNotice(msg) }

// focusMode is where keyboard focus lives: the content pane (screens
// keep tab chords + editing) or the chat composer (global tab chords
// fall through to readline editing inside the input).
type focusMode int

const (
	focusContent focusMode = iota
	focusComposer
	// focusTabs means the TAB BAR holds the keyboard — nothing below it has been
	// chosen yet.
	//
	// It exists because "the default Projects view under Work captures the down
	// arrows" was the pane receiving keys before the operator had selected
	// anything: Tab put them straight into the content, whose default source was
	// already focused. With this state Tab moves the top-level selection only; a
	// submenu opens on Enter, and SELECTING an entry is what moves focus into the
	// pane (see MenuSelect).
	focusTabs
)

// App is the root model.
type App struct {
	clients *client.Clients
	profile *config.Profile
	reg     *subs.Registry
	// A constructed screen is never built without its shell reference: newScreen
	// injects this into every screen it creates (see the helper's comment for
	// the bug that omission caused).
	shellOwner *App
	screens    map[TabID]Screen
	factories  map[TabID]func() Screen
	active     TabID
	width      int
	height     int
	footer     footerModel
	// lastStreamErr is the last stream error already surfaced in the dock, so
	// the same failure is not re-reported on every status update.
	lastStreamErr string
	help          helpModel
	routes        []KeyRoute
	quitting      bool
	// refreshGen identifies the CURRENT rolling-refresh chain. Every tick carries the generation that
	// armed it and is dropped when it no longer matches, so switching tabs cannot leave the old chain
	// running (which would multiply the refresh rate on every switch) — see refresh.go.
	refreshGen uint64

	// Chat dock state (feature: context-aware Ask Orchicon + slash).
	dock         dock.Model
	chat         *chat.Controller
	chatStore    *chatStore    // guarded chatItems (stream goroutine writes)
	chatWake     chan struct{} // live-chunk repaint poke (cap 1)
	chatCmds     chan tea.Cmd  // goroutine follow-ups (watch re-dial, poll)
	chatFocus    focusMode
	mouseEnabled bool // tea.WithMouseCellMotion is on; footer shows "Mouse Enabled"

	// clip is the frame the renderer last painted plus the drag-select in progress over it.
	// A POINTER because App is copied on every Update/View (clipboard.go).
	clip            *clipState
	palette         palette
	slash           *slashRegistry
	contextOverride string // /context pin <desc>
	// Tab dropdown submenus (Phase 2a): per-tab menus built from nav
	// config; menuOpen = the tab whose dropdown is open ("" = closed).
	menus              map[TabID]*TabMenu
	menuOpen           TabID
	navReg             []NavEntry
	themes             []string
	reconnectRequested bool
	chatConvID         string // active conversation ("" = none yet)

	// modelPicker is the /models modal (nil = closed). The composer lives on the
	// App (not on a screen), so the SHELL hosts this picker and owns its keys and
	// mouse while it is open — the same kit2.ModelPicker the screens use.
	modelPicker *kit2.ModelPicker
	// renameConv is the conversation-rename modal (nil = closed), hosted here for the same reason as the
	// model picker: the surface it names (the conversations rail) belongs to the SHELL, so no screen
	// can own its keys.
	//
	// The operator: "We can't rename conversations in the TUI." The RPC was already wired and reachable
	// by `/rename <title>` — but that is a command you have to KNOW, and it cannot show you the current
	// title to edit. The GUI prefills an input with the existing title (startRenameConv), which is what
	// this form does, and `ctrl+n` opens it from the rail itself so the gesture is available where the
	// operator is looking.
	renameConv   *kit2.Form
	renameConvID string
	// Categories (worker / workflow / conversation groupings). The CACHE is one slice for every target
	// type because the picker needs whichever type its item belongs to and the Control pane lists all
	// three; assignForm is the assign-or-create modal (nil = closed); assignTarget/assignEntities record
	// what it is pointed at so a selection change behind it cannot retarget the write.
	categories   []*apiv1.Category
	assignForm   *kit2.Form
	assignTarget apiv1.CategoryTargetType
	// catAssignedBy maps a TARGET-TYPE-SCOPED entity key (catEntityKey) to the grouping that entity is in.
	//
	// THIS is what makes a grouping visible on the item. Without it the shell knew the categories but not
	// which item belonged to which, so the operator's "I created a conversation category and assigned a
	// conversation to it, but it is not showing up in the UI" was literally true: the assignment was
	// fetched in the same response as the categories and thrown away before anything could render it.
	catAssignedBy map[string]string
	// catForm is the shell's PREFILLED grouping-rename form (nil = closed), hosted here for the same
	// reason as the rename/assign modals: the grouping it edits is drawn by whoever shows it, and the
	// shell owns the modal host and the cache. catFormID is the grouping it was opened for, so a
	// refresh that re-seats the cursor cannot retarget the write.
	catForm   *kit2.Form
	catFormID string
	// assignEntities is what the modal is pointed at — a LIST, because the rail can bulk-assign a whole
	// marked selection as well as one row. It is a field rather than a value read from the shell at
	// submit time so a selection change behind the modal cannot retarget the write.
	assignEntities []string
	pendingCatCmd  tea.Cmd
	// convMarked is the rail's multi-selection, keyed by conversation ID (never by index: the rolling
	// refresh re-seats rows by id, and an index-keyed mark would silently move to a different
	// conversation). nil = nothing marked.
	convMarked map[string]bool
	// convCollapsed is the conversations rail's collapsed folders, keyed by category id. Session state,
	// exactly as the GUI's per-page collapse is LOCAL state (`orchicon.categories.<page>.collapsed`) and
	// not server state — neither client makes a grouping's open/closed a tenant fact.
	convCollapsed map[string]bool
	// bulkConfirm hosts the confirm dialog for a destructive bulk rail operation (nil = closed), and
	// bulkConfirmRun is what the affirmative choice dispatches.
	bulkConfirm    *kit2.Dialog
	bulkConfirmRun func() tea.Cmd
	// metrics is the open conversation's usage roll-up feeding the composer's
	// stat strip. ctxWindowFor/ctxWindow cache the resolved context window per
	// model ref: the window costs a provider round trip and changes only when
	// the model does.
	metrics      sessionMetrics
	ctxWindowFor string
	ctxWindow    int64
	// askDefaultModel is the tenant's DefaultAskOrchiconModel (settings). It is
	// the LAST link in the composer's model chain after the conversation's own
	// model_ref and a pending selection — the GUI's ordering.
	askDefaultModel string
	execSessions    map[string][]chat.ChatItem // execution id → durable session items

	// Transcript Stream widgets (kit2): one per conversation. Live chunks
	// APPEND (preserving the operator's scroll offset; following the tail
	// only when already pinned at the bottom) instead of resetting the pane
	// on every repaint. transcriptLines is each stream's last rendered line
	// set, so an extension is an Append and anything else a reload.
	chatStreams     map[string]*kit2.Stream
	transcriptLines map[string][]string
	pendingDetail   tea.Cmd
	lastScreenKeys  string

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
	askMode askMode // Ask's presentation: launch page vs conversations
	// Slide-out conversation strip (chatpanel.go): open when the operator
	// engages the composer on a screen other than Ask, so a send is VISIBLE.
	// panelScroll is in lines up from the transcript tail (0 = following).
	panelOpen     bool
	panelScroll   int
	conversations []chat.Conversation
	convRailOpen  bool
	// askPane records which Ask pane holds the keyboard. Left/right select between the
	// conversations rail and the open conversation, and the vertical keys follow the
	// selection: on the rail they move the rail's cursor, in the conversation they scroll
	// the transcript. The operator: "When the conversation is selected up/down scrolls as
	// well as the wheel. When the list pane is selected up/down and scroll scrolls the
	// list of the conversation."
	//
	// It exists because focusContent is ONE stop covering both panes, so without it the rail
	// claimed the vertical keys unconditionally whenever it was visible — which is whenever a
	// conversation is open — leaving the transcript unscrollable by keyboard.
	askPane askPaneID

	convSel    int
	convScroll int
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
	// pendingScreenCmd carries a newly activated screen's first-load cmd
	// out of SwitchTo (which cannot return one).
	pendingScreenCmd tea.Cmd
	// loaded records which screens have run their first load. Screens are
	// constructed eagerly for the nav registry, so without this bookkeeping
	// ONLY the startup tab ever fetched its lists — every other tab
	// rendered "nothing here" with no error.
	loaded map[TabID]bool
}

// DiffPaneWidth is the left rail width (cells). Mirrors the GUI's ~480px
// rail proportionally at a typical 96-col terminal.
const DiffPaneWidth = 48

// NewApp builds the shell over an established client set.
func NewApp(cl *client.Clients, profile *config.Profile, serverVersion string) *App {
	m := &App{
		clients: cl,
		profile: profile,
		reg:     subs.NewRegistry(),
		// The SELECTION and its clipboard live on a shared pointer: App is a value model, so
		// View and Update each receive a COPY — only a shared pointer lets the frame the
		// renderer painted be read back by the mouse handler that arrives after it
		// (clipboard.go).
		clip:            &clipState{},
		screens:         map[TabID]Screen{},
		chatStore:       &chatStore{items: map[string][]chat.ChatItem{}},
		execSessions:    map[string][]chat.ChatItem{},
		loaded:          map[TabID]bool{},
		chatStreams:     map[string]*kit2.Stream{},
		transcriptLines: map[string][]string{},
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
	// The factory reads m.clients AT CALL TIME, not the cl captured here:
	// /connect rebuilds the client set, and a factory closing over the ORIGINAL
	// pointer handed every reconstructed screen a stale, unauthorised client —
	// which is why reconnecting left the screens blank.
	m.factories = map[TabID]func() Screen{
		TabAsk:         func() Screen { return ask.New(m.clients, m.reg) },
		TabOverview:    func() Screen { return overview.New(m.clients, m.reg, "") },
		TabWork:        func() Screen { return work.New(m.clients, m.reg, "") },
		TabExecution:   func() Screen { return execution.New(m.clients, m.reg, "") },
		TabAutomation:  func() Screen { return automation.New(m.clients, m.reg, "") },
		TabEnforcement: func() Screen { return enforcement.New(m.clients, m.reg, "") },
		TabControl:     func() Screen { return control.New(m.clients, m.reg) },
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

// newScreen constructs a tab's screen and injects the shell reference.
//
// This used to be hand-written inside each factory and Control's was MISSING,
// so Control.Shell() returned nil — which meant its Themes pane switched the
// palette in-session but could never persist it (applyTheme fell through to
// theme.Use), and its mutation notices were silently dropped (Notice reports
// through the shell's dock sink). Both failures were invisible: no error, no
// message, just a theme that did not come back. Constructing through this ONE
// helper makes forgetting impossible.
func (m *App) newScreen(id TabID) Screen {
	f, ok := m.factories[id]
	if !ok {
		return nil
	}
	s := f()
	if ss, ok := s.(interface{ SetShell(any) }); ok {
		ss.SetShell(m)
	}
	return s
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
		m.ensureLoaded(id)
		m.refreshComposerHint()
		return
	}
	if old, ok := m.screens[m.active]; ok && old != nil {
		old.Close()
	}
	m.active = id
	// Bumping the generation makes any tick still in flight stale, so it cannot act on the tab the
	// operator has left. The single chain adopts the new generation on its next fire (refresh.go), so
	// no new chain is armed here — one chain for the session's lifetime.
	m.refreshGen++
	if _, ok := m.screens[id]; !ok {
		if s := m.newScreen(id); s != nil {
			m.screens[id] = s
		}
	}
	// Always (re)apply the layout to the screen being activated: a screen
	// cached earlier (e.g. the nav registry's introspection build, or a
	// pre-resize visit) never saw a SetSize and would otherwise render
	// against a zero-sized content region.
	if s := m.screens[id]; s != nil && m.width > 0 {
		s.SetSize(m.contentWidth(), m.screenRows())
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
	m.refreshComposerHint()
	// The diff pane's open state persists across SwitchTo (the GUI persists
	// it at the host). Re-point it at the new tab's owner (if any) so it
	// shows the active session without resetting open/tab/selected.
	if m.diffOpen {
		m.refreshDiffOwner()
	}
	// A screen first reached this session runs its first load now: the nav
	// registry builds every screen eagerly, so a lazily-activated screen
	// would otherwise never fetch (every pane "nothing here").
	m.ensureLoaded(id)
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

// ensureLoaded runs a screen's first load (Init → Load) exactly once, when
// it first becomes activatable. Screens are constructed eagerly for the nav
// registry (buildNavEntries → screenForNav), so relying on App.Init alone
// means the startup tab loads and every other tab renders an empty list.
func (m *App) ensureLoaded(id TabID) {
	if id == "" || m.loaded[id] {
		return
	}
	s := m.screens[id]
	if s == nil {
		return
	}
	m.loaded[id] = true
	m.pendingScreenCmd = tea.Batch(m.pendingScreenCmd, s.Init())
}

// drainStaged returns (once) every cmd staged by a route or mouse handler
// that cannot return one directly: screen first-loads, diff-pane owner
// setup, and conversations-rail reloads.
func (m *App) drainStaged() tea.Cmd {
	var cmds []tea.Cmd
	if m.pendingScreenCmd != nil {
		cmds = append(cmds, m.pendingScreenCmd)
		m.pendingScreenCmd = nil
	}
	if m.pendingDiffCmd != nil {
		cmds = append(cmds, m.pendingDiffCmd)
		m.pendingDiffCmd = nil
	}
	if m.pendingRailCmd != nil {
		cmds = append(cmds, m.pendingRailCmd)
		m.pendingRailCmd = nil
	}
	if m.pendingCatCmd != nil {
		cmds = append(cmds, m.pendingCatCmd)
		m.pendingCatCmd = nil
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// newChat drops the active conversation and returns Ask to its hero: the
// next composer send creates a fresh conversation (the GUI's New chat).
// The transcript starts EMPTY (the detail pane is cleared and the stream
// for the new conversation has no lines yet).
// showAskConversations switches Ask to its conversation view: the transcript
// pane plus the conversations rail on the right, so the operator can continue a
// previous session. This is the Ask tab's "Conversations" menu row.
func (m *App) showAskConversations() {
	m.askMode = askConversations
	if m.active != TabAsk {
		m.SwitchTo(TabAsk)
	}
	m.EnsureSubscriptions(TabAsk)
	m.refreshLayout()
	// The rail only loads when it is shown, so the first time it appears it may
	// never have been fetched (or may have failed) — fetch it now. Never a
	// silent empty rail.
	if m.convErr != "" || !m.convLoaded {
		m.pendingRailCmd = m.reloadConversations()
	}
}

func (m *App) newChat() {
	m.askMode = askNew
	m.chatConvID = ""
	if m.chat != nil {
		m.chat.SetActive("")
	}
	m.SwitchTo(TabAsk)
	m.EnsureSubscriptions(TabAsk)
	if s := m.screens[TabAsk]; s != nil {
		if nc, ok := s.(interface{ NewChat() }); ok {
			nc.NewChat()
		}
		if st, ok := s.(interface {
			SetDetailContent(title string, fields []screenkit.Field, body string)
		}); ok {
			st.SetDetailContent("New chat", nil, "")
		}
	}
	m.updateContextChip()
}

// openRenameConversation opens the rename modal for a conversation, PREFILLED with its current title.
//
// Prefilling is the whole point, and it is the GUI's behaviour ("Conversation rename state" /
// startRenameConv selects the existing title). A rename box that opened EMPTY would make the operator
// retype a title they cannot see, which is worse than the `/rename <title>` command it replaces.
func (m *App) openRenameConversation(id, current string) {
	if id == "" {
		m.dock.SetError("no conversation to rename — open one first")
		return
	}
	f := kit2.NewForm("Rename conversation", kit2.FieldSpec{
		Name:     "title",
		Label:    "Title",
		Kind:     kit2.KText,
		Required: true,
		// Prefilled, and the caret is placed at the END by the form, so typing appends and ctrl+u
		// clears — both useful on a title.
		Initial: current,
	})
	prev := current
	f.OnSubmit = func(vals map[string]string, _ map[string][]string) (tea.Cmd, error) {
		title := strings.TrimSpace(vals["title"])
		if title == "" {
			// Stay open and SAY why, rather than closing on a no-op (the GUI's saveRenameConv
			// silently returns when the trimmed value is empty — a save that does nothing is the
			// silent-rejection class this codebase keeps having to fix).
			return nil, fmt.Errorf("a title is required")
		}
		if title == prev {
			return nil, nil // unchanged: valid, and nothing to write (the GUI does the same)
		}
		return m.chat.RenameConversation(id, title), nil
	}
	f.Width = m.modalWidth()
	m.renameConv = f
	m.renameConvID = id
	m.refreshComposerHint()
}

// modalWidth sizes an App-hosted modal to the viewport, with a margin so it never touches the edges.
func (m *App) modalWidth() int {
	w := m.width - 8
	if w > 72 {
		w = 72
	}
	if w < 24 {
		w = 24
	}
	return w
}

// renameConvKey drives the rename modal. The modal OWNS every key while it is open (like the model
// picker): a form whose keystrokes could reach the composer behind it would let a save chord land in
// a message.
func (m *App) renameConvKey(k tea.KeyMsg) (*App, tea.Cmd) {
	if m.renameConv == nil {
		return m, nil
	}
	switch k.String() {
	case "esc":
		// Cancel: the modal closes and the modal's ID is cleared with it, so a later ctrl+s cannot
		// write to a conversation the operator has stopped looking at.
		m.renameConv = nil
		m.renameConvID = ""
		m.refreshComposerHint()
		return m, nil
	case "ctrl+c":
		// Quit still works with a modal up (the hard escape).
		m.quitting = true
		return m, tea.Quit
	}
	cmd, _ := m.renameConv.HandleKey(k)
	if m.renameConv != nil && m.renameConv.Submitted {
		m.renameConv = nil
		m.renameConvID = ""
		m.refreshComposerHint()
	}
	return m, cmd
}

// renameConvView composes the modal over the base view, inside a SOLID panel.
func (m *App) renameConvView(base string, w, h int) string {
	if m.renameConv == nil {
		return base
	}
	// The form lays itself out in the PANEL'S INTERIOR, so its rows fit inside the border rather than
	// being truncated by two columns on the right.
	m.renameConv.Width = m.modalInnerWidth()
	return m.overlayCentered(base, m.modalPanel(m.renameConv.View(), m.modalWidth()))
}

// conversationTitle looks up a rail conversation's current title ("" when it is not loaded).
func (m *App) conversationTitle(id string) string {
	for _, c := range m.conversations {
		if c.ID == id {
			return c.Title
		}
	}
	return ""
}

// scrollKeyDelta maps the vertical keys to a line delta (0 = not a scroll
// key).
func scrollKeyDelta(key string) int {
	switch key {
	case "up":
		return -3
	case "down":
		return 3
	case "pgup":
		return -12
	case "pgdown":
		return 12
	}
	return 0
}

// scrollActiveDetail scrolls the active screen's detail pane when it has
// one (every list+detail screen embeds screenkit.Base.ScrollDetail). On the
// Ask tab the transcript is the kit2 Stream, so vertical keys scroll IT
// (the operator's scroll offset is what the Stream preserves across
// appends).
func (m *App) scrollActiveDetail(delta int) {
	if m.active == TabAsk && m.chatConvID != "" {
		m.ScrollTranscript(delta)
		return
	}
	if s := m.screens[m.active]; s != nil {
		if sc, ok := s.(interface{ ScrollDetail(int) }); ok {
			sc.ScrollDetail(delta)
		}
	}
}

// ActiveTab returns the active tab ID.
func (m *App) ActiveTab() TabID { return m.active }

// NextTab / PrevTab cycle the tab bar.
func (m *App) NextTab() { m.cycle(1) }
func (m *App) PrevTab() { m.cycle(-1) }

// tabRingNext advances the operator focus ring: chat prompt FIRST, then
// the six area tabs in order, then back to the prompt. From the composer
// Tab drops to the Ask tab's content; each further Tab advances to the
// next tab's content; after Control it wraps back to the composer. Left /
// right still switch tabs directly (content focus).
func (m *App) tabRingNext() {
	// One action per press: an open dropdown just closes (no focus move).
	if m.TabMenu() != nil {
		m.closeTabMenu()
		return
	}
	// Tab lands on the TAB BAR, from the composer AND from the content.
	//
	// Landing in the content is what let a pane take the arrows before the operator
	// had chosen anything. Returning from CONTENT is step 5 of the operator's model
	// — "Tab breaks that and moves through submenu again" — and it is what makes the
	// submenu reachable after a selection: Enter on the bar opens it. Without it,
	// Tab from a pane jumped to the NEXT tab, leaving no way back to the menu of the
	// row just picked except cycling the whole ring.
	if m.chatFocus != focusTabs {
		m.setFocus(focusTabs)
		return
	}
	idx := 0
	for i, t := range Tabs {
		if t.ID == m.active {
			idx = i
			break
		}
	}
	// WRAP. The operator: "once you hit the end of the tab menu it stops. It
	// should rotate back around." Past the last tab the ring returns to the
	// first rather than stopping.
	next := (idx + 1) % len(Tabs)
	m.SwitchTo(Tabs[next].ID)
	m.EnsureSubscriptions(Tabs[next].ID)
}

// tabRingPrev is tabRingNext in reverse, for Shift+Tab.
func (m *App) tabRingPrev() {
	if m.TabMenu() != nil {
		m.closeTabMenu()
		return
	}
	if m.chatFocus != focusTabs {
		m.setFocus(focusTabs)
		return
	}
	idx := 0
	for i, t := range Tabs {
		if t.ID == m.active {
			idx = i
			break
		}
	}
	prev := (idx - 1 + len(Tabs)) % len(Tabs)
	m.SwitchTo(Tabs[prev].ID)
	m.EnsureSubscriptions(Tabs[prev].ID)
}

// toggleSideRails is the Shift+Tab action: toggle the LEFT diff pane.
//
// The operator's ask was explicit — "Shift+Tab should honestly just bring out
// the diff pane back and forth" — so the conversations rail is NOT part of
// this toggle (it is always on for MVP1; see railVisible).
func (m *App) toggleSideRails() {
	if m.diffOpen {
		m.closeDiffPane()
		return
	}
	if kind, id := m.diffOwner(); kind == diffs.NoneOwner || id == diffs.NoneOwner {
		// No diff-relevant session yet: still bring the pane OUT so Shift+Tab
		// is a predictable toggle (it renders its empty state) rather than a
		// silent no-op — the operator's "Shift+Tab should just bring out the
		// diff pane back and forth".
		m.diffOpen = true
		m.refreshLayout()
		return
	}
	m.pendingDiffCmd = m.openDiffPane()
}

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

// syncLayoutWidth re-applies the layout when the CONTENT WIDTH changed since it
// was last applied.
//
// The width is not a pure function of a terminal resize: contentWidth() subtracts
// ConversationsRailWidth whenever railVisible() is true, and railVisible() depends
// on SHELL STATE (askMode / chatConvID). So the conversations rail APPEARING — the
// launch page becoming the conversation view, or the rail being toggled — changes
// the content width with no WindowSizeMsg to drive refreshLayout().
//
// The dock and the screen then keep the pre-rail (wider) width, and baseView's
// normalizeBlock TRUNCATES their right edge down to the real width. That silently
// cuts off everything RIGHT-ALIGNED:
//
//   - the composer's stat strip — the context/token/cache figures and the mode
//     pill live at the bottom-RIGHT ("the context information and model picker on
//     the bottom right are off the screen and I can't see it");
//   - the operator's OWN message bubbles, which are right-aligned — so a just-sent
//     message was missing from a transcript that plainly CONTAINED it, while the
//     left-aligned reply rendered normally;
//   - and it sliced long LEFT-aligned lines mid-sentence, because the renderer had
//     wrapped them to a pane wider than the one they were finally drawn in.
//
// Checks the applied width rather than a change counter, so it is self-correcting:
// every path that alters rail visibility is covered without each such path having
// to remember to re-layout, and once corrected the check is a no-op.
func (m *App) syncLayoutWidth() {
	if m.width <= 0 {
		return
	}
	if m.dock.Width == m.contentWidth() {
		return
	}
	m.refreshLayout()
}

// reflowForDiff was unified into refreshLayout (Phase 2a): one layout
// applier for window resize, rail toggles, and diff-pane toggles.
func (m *App) refreshLayout() {
	// The DOCK WIDTH comes first: the composer's height (dock.Lines) depends on
	// how its hint wraps at that width, and the screens are sized from the rows
	// the dock leaves. Sizing the screen first measured the dock at its previous
	// width, so a hint that gained a row pushed the screen out of its budget.
	if m.width > 0 {
		m.dock.Width = m.contentWidth()
	}
	if s := m.screens[m.active]; s != nil && m.width > 0 {
		s.SetSize(m.contentWidth(), m.screenRows())
	}
	if m.diffPane != nil {
		m.diffPane.SetSize(DiffPaneWidth, m.screenRows()+m.dock.Lines()+m.panelRows())
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

// refreshComposerHint fills the composer's affordance row with the ACTIVE
// screen's shortcut list, so the guidance follows the page the operator is on
// (the screen's own HintLine is the single source of truth) and always leads
// with the ctrl+g focus chord. A screen with no HintLine just clears the
// context.
func (m *App) refreshComposerHint() {
	ctx := ""
	// AN APP-LEVEL MODAL OWNS THE KEYBOARD TOO, so it must win over the screen's hint for the same
	// reason an in-screen form does — and it is checked FIRST, because while the modal is up the screen
	// underneath is not the thing reading the operator's keystrokes.
	if m.renameConv != nil || m.assignForm != nil || m.bulkConfirm != nil || m.catForm != nil {
		ctx = formComposerHint
	} else if s := m.screens[m.active]; s != nil {
		// AN OPEN FORM OWNS THE KEYBOARD, SO THE HINT MUST DESCRIBE THE FORM.
		//
		// The operator: "There is no ctrl+s - save guidance in the composer under providers or secrets
		// like other pages". The screen's own HintLine is a BROWSING cheat-sheet — for the providers
		// pane it says "n: new custom · e: edit · t: enable/disable · s: set token · c: clear ·
		// x: delete" — and it said exactly that while a provider form had the keyboard. Every one of
		// those chords is INERT in that state (the form consumes every key), and the two that DO work,
		// ctrl+s and esc, were not mentioned. So the hint was not merely incomplete, it was wrong:
		// it advertised six keys that do nothing and hid the one that saves.
		//
		// The fix is here rather than in each screen's HintLine because this is the ONE place that
		// knows a form is open for EVERY screen (the same FormOpen probe the Tab chord uses), so the
		// form's keys cannot be forgotten by a screen that adds a form later.
		if fo, ok := s.(interface{ FormOpen() bool }); ok && fo.FormOpen() {
			ctx = formComposerHint
		} else if h, ok := s.(interface{ HintLine() string }); ok {
			ctx = ansi.Strip(strings.TrimSpace(h.HintLine()))
		}
	}
	// THE RAIL'S CHORD LIST BELONGS HERE, NOT INSIDE THE RAIL PANE.
	//
	// It was drawn inside the rail, and that was wrong twice over: the rail's inner text width is 28
	// cells against a 34-cell chord list, so it was TRUNCATED mid-word ("ctrl+n: rename · ctrl+t: ca…"
	// — the operator's screenshot), and the rail is not where the keys live. The composer is: the
	// rail's keys are driven from there (an empty box is what makes the arrows move the rail at all),
	// and the operator asked for the move.
	if rail := m.railHintLine(); rail != "" {
		if ctx == "" {
			ctx = rail
		} else {
			ctx = rail + " · " + ctx
		}
	}
	before := m.dock.Lines()
	m.dock.SetContext(ctx)
	// A longer hint can gain a row, which changes the rows the dock leaves for
	// the screen — re-apply the layout so the screen still fills its budget.
	if m.dock.Lines() != before {
		m.refreshLayout()
	}
}

// formComposerHint is what the composer advertises while ANY screen has a form open. It names only
// keys the form actually honours, so the hint is true in that state — see refreshComposerHint.
const formComposerHint = "ctrl+s: save · esc: cancel · tab/↑↓: next field · enter: next field (or open a picker/list)"

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
	// The Ask tab's conversations list IS the chat's own conversation: its
	// title is self-referential inside that conversation's own messages, so
	// it is not context. Injecting it made every send carry a useless
	// "[context: Conversations: <its own title>]" header (the operator's
	// report). The GUI injects nothing here either.
	if m.active == TabAsk && src == "conversations" {
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
	m.askMode = askConversations
	m.chatConvID = id
	// OPENING A CONVERSATION SELECTS IT FOR THE KEYBOARD. Opening one is a deliberate
	// "I want to read this", and leaving the rail selected would make the operator press
	// right before their arrows did anything to what they just opened. Left still returns
	// to the rail.
	m.askPane = askPaneConversation
	m.syncAskPaneFocus()
	m.chat.SetActive(id)
	// The right rail appears with a conversation, which SHRINKS contentWidth()
	// — and every pane's width is assigned from it in refreshLayout, which is
	// only otherwise run on a window resize. Without re-laying-out here the dock
	// kept the pre-rail width: it built its rows for more columns than the body
	// gave it, normalizeBlock trimmed the TAIL of each row, and the stat row's
	// numbers and the mode pill were cut off (the operator's "the context is off
	// the screen").
	m.refreshLayout()
	// A conversation switch changes whose usage the strip reports, so re-read it
	// (the previous conversation's numbers must never linger under a new chat).
	return tea.Batch(m.chat.OpenConversation(id), m.refreshMetrics())
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
//
// THE TAIL, NOT THE HEAD — and that is the whole point of beforeSeq here. The operator: "I don't
// think it is showing the entire execution. In fact, I don't think any of the executions in the TUI
// are showing the entire execution."
//
// This asked for `Limit: 200` with NO beforeSeq, which the server answers with the FIRST 200 parts
// in seq order (db.ListExecutionSessionParts: `ORDER BY seq` when beforeSeq is unset). A real
// execution is far bigger than that — the one the operator named has 17,574 parts — so the pane
// showed the opening 200 and nothing else: no late tool calls, no final answer, no follow-up reply.
// Every execution looked truncated because every execution was.
//
// The GUI solved this the same way and for the same reason (frontend/src/api/executions.ts):
//
//	beforeSeq: 9223372036854775807n,   // max int64 = from the END
//	limit: 10000,
//	return [...res.parts].reverse();   // DESC → chronological
//
// so the server's DESC-tail query returns the NEWEST parts and the client puts them back in
// reading order. MaxInt64 as beforeSeq means "everything up to the end", which is the idiom the
// generated API already encodes (the GUI passes it as a bigint for exactly this).
//
// The limit is the GUI's 10000 rather than a number of my own: the two clients must agree about
// how much of a transcript is "the transcript", or the operator sees a different execution
// depending on which window they are in.
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
			BeforeSeq:   sessionTailFromSeq,
			Limit:       sessionPartLimit,
		}))
		if err != nil {
			return execSessionMsg{execID: execID, err: err}
		}
		// The server returns the tail NEWEST-first (seq DESC); the transcript reads oldest-first,
		// so it is reversed here rather than at every render (chat.MergeSessionItems sorts, but it
		// should be handed a coherent sequence to merge).
		parts := resp.Msg.GetParts()
		chronological := make([]*apiv1.ExecutionSessionPart, len(parts))
		for i, p := range parts {
			chronological[len(parts)-1-i] = p
		}
		return execSessionMsg{execID: execID, parts: chronological}
	}
}

// sessionTailFromSeq is the "from the end" sentinel for GetExecutionSession's before_seq: every
// part whose seq is below this, i.e. all of them, newest-first. It is math.MaxInt64, the same value
// the GUI sends, so both clients ask the server the identical question.
const sessionTailFromSeq = int64(^uint64(0) >> 1)

// sessionPartLimit bounds how much of a transcript is fetched. 10000 is the GUI's number (see
// useGetExecutionSession): generous enough to cover a long run's tail, bounded enough that a
// pathological session cannot stall the pane.
const sessionPartLimit = 10000

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

// refreshStreamStatus updates the footer AND surfaces the underlying reason a
// stream is down, once per distinct message. Without this a dead endpoint (or a
// rejected subscription) produced a bare "connecting…" that could never be
// diagnosed from inside the UI.
func (m *App) refreshStreamStatus() {
	m.footer.StreamStatus = m.streamStatus()
	m.surfaceStreamError()
}

// surfaceStreamError pushes the active screen's stream error into the dock's
// error strip whenever the footer is not "open", so the operator reads the
// actual reason (e.g. "connection refused") instead of guessing.
func (m *App) surfaceStreamError() {
	if m.footer.StreamStatus == openStatus {
		return
	}
	rp, ok := m.screens[m.active].(screenkit.StatusReporter)
	if !ok {
		return
	}
	for _, st := range rp.ReportStatus() {
		errText := m.reg.LatestError(st.Name)
		if errText == "" || errText == m.lastStreamErr {
			continue
		}
		m.lastStreamErr = errText
		m.dock.SetError(st.Name + ": " + errText)
		return
	}
}

// streamStatus derives the footer state from the active screen's declared
// subscriptions. The VALUE comes from the registry's latest report, not from
// what the screen last received: a delivery can be routed to a different screen
// than the one that armed the read, and the wake-up queue drops on overflow —
// either way the screen's copy could freeze on an old value (the footer stuck on
// "connecting…" for the rest of the session).
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
		live := st.Status
		if v := m.reg.LatestStatus(st.Name); v != "" {
			live = v
		}
		if statusRank(streamStatusString(live)) > statusRank(worst) {
			worst = streamStatusString(live)
		}
	}
	return worst
}

// Init implements tea.Model.
func (m *App) Init() tea.Cmd {
	var cmds []tea.Cmd
	// The startup tab loads here; every other tab loads on first activation
	// (ensureLoaded, called from SwitchTo).
	m.ensureLoaded(m.active)
	cmds = append(cmds, m.waitChat(), m.chat.LoadConversations(), m.fetchAskDefaultModel())
	// The category list is loaded ONCE at startup alongside everything else, so the first assignment the
	// operator makes already has its picker populated. The assign modal also triggers a load if the
	// cache is empty (a session that started before the server had any, or a failed first load), so this
	// is the fast path rather than the only path.
	if c := m.loadCategories(); c != nil {
		cmds = append(cmds, c)
	}
	// ARM THE ROLLING REFRESH WINDOW (refresh.go). One chain for the session, re-armed by its own
	// handler, refreshing whatever the active tab is showing every few seconds — because an event
	// poke only arrives when the server chooses to emit one, and the follow-up reply that made this
	// necessary is written durably without any live event at all.
	cmds = append(cmds, refreshCmd(m.refreshGen))
	if c := m.drainStaged(); c != nil {
		cmds = append(cmds, c)
	}
	return tea.Batch(cmds...)
}

// askDefaultSettingsMsg carries the tenant's default Ask model — the fallback the
// composer reports when the conversation itself has no model_ref.
type askDefaultSettingsMsg struct {
	model string
	err   error
}

// fetchAskDefaultModel reads the tenant's DefaultAskOrchiconModel.
//
// The TUI previously never consulted it: the composer's model came only from the
// active conversation's model_ref (then the pending one for a new chat), so a
// conversation created WITHOUT a model_ref reported no model at all — and, since
// the context window is resolved from that ref, the strip showed a bare
// occupancy with no limit. The GUI has always used the full chain
// (`conv.modelRef || settings.defaultAskOrchiconModel || fallback`); this mirrors it.
func (m *App) fetchAskDefaultModel() tea.Cmd {
	cl := m.clients
	return func() tea.Msg {
		if cl == nil || cl.Settings == nil {
			return askDefaultSettingsMsg{}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		resp, err := cl.Settings.GetSettings(ctx, connect.NewRequest(&apiv1.GetSettingsRequest{}))
		if err != nil {
			return askDefaultSettingsMsg{err: err}
		}
		return askDefaultSettingsMsg{model: resp.Msg.GetSettings().GetDefaultAskOrchiconModel()}
	}
}

// Update implements tea.Model (via the router dispatch).
// Update implements tea.Model.
//
// It is the ONE choke point every input path funnels through, so it also
// flushes any cmd a handler staged but did not return. Staging exists because
// SwitchTo must run the newly-activated screen's first load, and SwitchTo
// cannot return a tea.Cmd — handlers that switch tabs then return their own
// cmd and silently DROPPED the staged load. That is why every domain rendered
// its empty state forever when tabs were reached by mouse (the chord routes
// happened to drain it; the mouse tab-click, the dropdown chord and MenuSelect
// did not). Rather than depend on every present and future caller remembering,
// flush here.
func (m App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.dispatch(msg)
	// The CONTENT WIDTH is not a pure function of a resize: railVisible()
	// depends on STATE (askMode / chatConvID), so the conversations rail
	// appearing shrinks the content area by ConversationsRailWidth with no
	// WindowSizeMsg to trigger refreshLayout(). Re-apply the layout whenever the
	// width it implies has changed — see syncLayoutWidth.
	next.syncLayoutWidth()
	// The composer's caret BLINK STARTER (captured when the dock took focus) has
	// to reach the runtime, and focus is taken from several places — ctrl+g, a
	// click, keyboard navigation — most of which return their own command or nil.
	// Draining it here, once, after every dispatch, is the only place that covers
	// all of them: without it the blink loop was never started, so the caret sat
	// solid until a keypress happened to change the cursor position and bubbles
	// started the loop itself (the operator's "it only blinks if I type something
	// then backspace it to nothing").
	if b := next.dock.TakeBlinkStart(); b != nil {
		if cmd == nil {
			cmd = b
		} else {
			cmd = tea.Batch(cmd, b)
		}
	}
	if staged := next.drainStaged(); staged != nil {
		if cmd == nil {
			return next, staged
		}
		return next, tea.Batch(cmd, staged)
	}
	return next, cmd
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
	m.refreshComposerHint()
	m.footer.StreamStatus = m.streamStatus()
	m.footer.Width = m.width
	// footer.ComposerFocus is NOT recomputed here: it is derived in setFocus, the one place focus
	// changes (see setFocus). Recomputing it on this path was one of only two partial sources for
	// the flag, and the KEYBOARD paths — exactly the ones that move focus — reached neither.
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
	frame := m.viewFrame()
	if m.clip == nil {
		return frame
	}
	m.clip.setFrame(frame)
	return m.clip.decorate(frame)
}

func (m App) viewFrame() string {
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
	// The /models picker is spliced LAST (above every other layer). Its origin is
	// recomputed from the same w×h the overlay uses, so a click addresses the box
	// that was actually drawn.
	if m.modelPicker != nil {
		m.modelPicker.SetScreen(w, h)
		base = m.overlayCentered(base, m.modelPicker.View())
	}
	// The rename modal goes in the same layer. It and the model picker are mutually exclusive in
	// practice (the picker opens from the composer's model chip, the rename box from the rail), and
	// both are hosted here rather than on a screen, so ordering is not load-bearing.
	if m.renameConv != nil {
		base = m.renameConvView(base, w, h)
	}
	if m.assignForm != nil {
		base = m.assignCategoryView(base, w, h)
	}
	// The bulk confirm (a destructive rail operation) sits in the same layer, for the same reason: the
	// shell owns the surface it names.
	if m.bulkConfirm != nil {
		base = m.bulkConfirmView(base, w, h)
	}
	// The grouping-rename form sits in the same layer: the shell owns the grouping cache, and the modal
	// is opened from a category row in whichever pane is showing it.
	if m.catForm != nil {
		base = m.catAdminView(base, w, h)
	}
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
	var bodyLines []string
	if m.welcomeMode() {
		// opencode-style launch: the composer sits in the MIDDLE of the
		// viewport with the brand above it, until a session starts.
		bodyLines = m.centeredWelcomeView(cw, screenRows+dockRows)
	} else {
		screenBlock := normalizeBlock(safeView(m.screens[m.active]), cw, m.screenRows())
		var panelBlock []string
		if pRows := m.panelRows(); pRows > 0 {
			// The slide-out strip sits BETWEEN the screen and the composer, so
			// its rows come out of the screen's budget (never over content).
			panelBlock = normalizeBlockKeepTail(m.chatPanelView(cw), cw, pRows)
		}
		dockBlock := normalizeBlockKeepTail(m.dock.View(), cw, dockRows)
		bodyLines = append(append(screenBlock, panelBlock...), dockBlock...)
	}
	body := strings.Join(bodyLines, "\n")
	// Left diff rail / right conversations rail: extra COLUMNS joined over
	// the screen+dock region (the gap row spans the full width alone).
	if m.diffOpen && m.diffPane != nil {
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

// askMode is the Ask tab's presentation.
//
// Ask is the only tab whose dropdown is a pair of VERBS rather than a list of
// sources, because its list (conversations) lives in the right rail:
//
//	askNew         — the launch page: the centered composer, no rail. This is
//	                 what orch shows on start.
//	askConversations — the conversation view: the transcript pane with the
//	                 conversations rail on the right, for continuing sessions.
//
// Opening or creating a conversation implies askConversations, so a send from
// the launch page lands in the conversation view naturally.
type askMode int

const (
	askNew askMode = iota
	askConversations
)

// welcomeMode reports whether to render the centered launch layout: the Ask
// tab showing "New" with no conversation open. Choosing Conversations (or
// opening one) leaves it.
func (m App) welcomeMode() bool {
	if m.active != TabAsk || m.chatConvID != "" {
		return false
	}
	if s := m.screens[TabAsk]; s != nil {
		if d, ok := s.(interface{ DetailID() string }); ok && d.DetailID() != "" {
			return false
		}
	}
	return m.askMode != askConversations
}

// brandGlyphs is a 5-row block font. EVERY glyph is EXACTLY brandGlyphW cells
// wide in every row, and welcomeBrand below COMPOSES the word from them.
//
// Hand-writing one aligned block is what broke the previous wordmark: a single
// row came out one cell short, which shifted the H's crossbar and every glyph
// after it on that row and sheared the whole thing (the operator's "looks like
// I'm tripping on acid"). Composing per-glyph makes width drift impossible,
// and TestWelcomeBrandRowsAreUniform asserts it.
const brandGlyphW = 8

var brandGlyphs = map[rune][]string{
	'O': {" ██████ ", "██    ██", "██    ██", "██    ██", " ██████ "},
	'R': {"███████ ", "██    ██", "███████ ", "██  ██  ", "██   ██ "},
	'C': {" ██████ ", "██    ██", "██      ", "██    ██", " ██████ "},
	'H': {"██    ██", "██    ██", "████████", "██    ██", "██    ██"},
	'I': {"████████", "   ██   ", "   ██   ", "   ██   ", "████████"},
	'N': {"██    ██", "███   ██", "██ ██ ██", "██  ████", "██    ██"},
}

// brandRows is the glyph stack height.
const brandRows = 5

// welcomeBrand is "ORCHICON" composed from brandGlyphs: each glyph padded to
// brandGlyphW and separated by one space, so every row is the same width by
// construction.
var welcomeBrand = composeBrand("ORCHICON")

// composeBrand lays out word in the block font, one string per row.
func composeBrand(word string) []string {
	rows := make([]string, brandRows)
	for i, r := range word {
		g, ok := brandGlyphs[r]
		if !ok {
			continue // unmapped rune: skipped rather than emitted at a wrong width
		}
		if i > 0 {
			for row := range rows {
				rows[row] += " "
			}
		}
		for row := 0; row < brandRows; row++ {
			cell := g[row]
			if w := len([]rune(cell)); w < brandGlyphW {
				cell += strings.Repeat(" ", brandGlyphW-w)
			}
			rows[row] += cell
		}
	}
	return rows
}

const welcomeTagline = "Ask Orchicon anything. Plan, execute, and govern with real-time clarity and thin control."

// wrapPlain word-wraps s to width runes (used for the launch tagline).
func wrapPlain(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	words := strings.Fields(s)
	var out []string
	line := ""
	for _, w := range words {
		if line == "" {
			line = w
			continue
		}
		if len([]rune(line))+1+len([]rune(w)) <= width {
			line += " " + w
			continue
		}
		out = append(out, line)
		line = w
	}
	if line != "" {
		out = append(out, line)
	}
	return out
}

// centeredWelcomeView renders the launch block — the large brand lockup, the
// composer box, and the tagline — vertically centered in the w×h body region.
// The composer keeps its own rendering (border, hint row, palette target);
// only its width and position change, so typing, slash commands and drafts
// behave identically to the docked layout.
func (m App) centeredWelcomeView(w, h int) []string {
	boxW := w * 2 / 3
	if boxW > 76 {
		boxW = 76
	}
	if boxW < 30 {
		boxW = 30
	}
	if boxW > w {
		boxW = w
	}
	d := m.dock
	d.Width = boxW
	boxLines := strings.Split(d.View(), "\n")

	// The wordmark falls back to a plain bold title on terminals too narrow for
	// the block letters (they are ~63 cells wide), so the launch screen never
	// wraps or truncates mid-glyph.
	brandW := lipgloss.Width(welcomeBrand[0])
	var brand []string
	if w >= brandW+4 {
		for _, l := range welcomeBrand {
			brand = append(brand, theme.ListTitle.Render(l))
		}
	} else {
		brand = append(brand, theme.ListTitle.Render("Orchicon"))
	}

	// The tagline wraps to the composer's width so it reads as subtext rather
	// than a single over-long line.
	var tagline []string
	for _, l := range wrapPlain(welcomeTagline, boxW) {
		tagline = append(tagline, theme.HintText.Render(l))
	}

	var block []string
	block = append(block, "")
	block = append(block, brand...)
	block = append(block, "")
	block = append(block, boxLines...)
	block = append(block, "")
	block = append(block, tagline...)

	// Vertically center the block in the region, then center each line
	// horizontally inside w cells.
	out := make([]string, 0, h)
	top := (h - len(block)) / 2
	if top < 0 {
		top = 0
	}
	for i := 0; i < top; i++ {
		out = append(out, "")
	}
	for _, l := range block {
		pad := (w - lipgloss.Width(l)) / 2
		if pad < 0 {
			pad = 0
		}
		out = append(out, strings.Repeat(" ", pad)+l)
	}
	for len(out) < h {
		out = append(out, "")
	}
	if len(out) > h {
		out = out[:h]
	}
	return out
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

// bgOpaque re-asserts the theme background after every SGR reset inside l.
//
// Why: padScreenLine wraps a rendered line in ScreenBg once, but ANY inner
// style (HintText, DetailValue, …) emits its own trailing \x1b[0m reset,
// which turns the background OFF for the rest of the line — including all
// of padScreenLine's padding spaces. On a terminal whose own background is
// not the theme background (most terminals; transparent-background setups
// especially), every cell after an inner reset renders UNPAINTED and the
// terminal's content bleeds straight through the "opaque" frame.
//
// The fix mirrors what bubbletea's renderer does for its own cells: after
// each reset, immediately re-emit the background SGR so every subsequent
// cell (content or padding) stays on the theme background. A reset that is
// immediately followed by another escape needs no repair (the next SGR
// sets its own state and the next reset is caught then).
func bgOpaque(l string) string {
	const reset = "\x1b[0m"
	bg := theme.ScreenBg
	// The re-assert sequence: the same SGR ScreenBg emits, minus its own
	// trailing reset. We derive it once from a zero-width render so the
	// repair always matches the active theme + color profile.
	paint := bg.Render("")
	if paint == "" {
		return l // profile off / no background: nothing to re-assert
	}
	// paint is "<SGR>…<reset>"; strip the trailing reset to get the open seq.
	if !strings.HasSuffix(paint, reset) {
		return l
	}
	open := strings.TrimSuffix(paint, reset)
	if open == "" {
		return l
	}
	var b strings.Builder
	for {
		i := strings.Index(l, reset)
		if i < 0 {
			b.WriteString(l)
			return b.String()
		}
		rest := l[i+len(reset):]
		b.WriteString(l[:i+len(reset)])
		// Re-assert the background unless another SGR follows immediately
		// (its own sequence will establish state, and its eventual reset
		// gets repaired on the next loop iteration).
		if !strings.HasPrefix(rest, "\x1b[") {
			b.WriteString(open)
		}
		l = rest
	}
}

// padScreenLine renders one row at exactly w cells: overlong lines are
// ANSI-aware truncated, short lines background-padded. Every cell —
// including padding — carries the theme's solid background. Any inner
// style's reset is followed by a background re-assert (bgOpaque), so the
// padding can never render unpainted.
func padScreenLine(l string, w int) string {
	cols := lipgloss.Width(l)
	if cols > w {
		l = ansi.Truncate(l, w, "")
		cols = w
	}
	if cols < w {
		l += strings.Repeat(" ", w-cols)
	}
	return bgOpaque(theme.ScreenBg.Render(l))
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
	// THE LIST'S OWN GROUPINGS COME WITH IT. Applying them here is what keeps the rail's folders current
	// when the grouping was created in the OTHER client: the shell's cache is loaded once at startup, so
	// before this the rail could only ever show groupings that existed when the TUI began.
	m.applyCategorySet(apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION, msg.Categories, msg.Assignments)
	m.conversations = msg.Convs
	if m.convSel >= len(m.railRows()) {
		m.convSel = 0
	}
	if m.convScroll > len(m.conversations) {
		m.convScroll = 0
	}
	// Marks are reconciled against the new list: a reload is the honest reconciliation for a bulk
	// delete, and a mark on a conversation that no longer exists would overstate the selection and
	// aim the next bulk action at a row the server would reject.
	m.pruneConvMarks()
	// The detail header reads its title + message count out of THIS list, so a
	// refresh has to repaint the open pane or the new values sit unrendered
	// until the next unrelated wake.
	return m.onChatWake()
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

// mergeHistory folds the durable transcript UNDER the live chunks: the
// durable rows are authoritative for everything already persisted, and the
// live rows carry only what has not landed yet.
//
// Dedupe is required here: the composer appends an OPTIMISTIC user row
// (Key "draft-*") the moment a message is sent, and the durable transcript then
// arrives containing that same user message. Blind concatenation printed the
// operator's text twice — and, because live rows were appended AFTER history,
// the optimistic copy landed at the BOTTOM, below the reply.
//
// The match must be a SUFFIX, not equality: the message that reaches the plane
// is the context preamble prepended to the text (chat.Controller.Send does
// `full = preamble + "\n" + text`), while the optimistic row carries the raw
// text. Comparing them exactly never matched, so the dedupe silently did
// nothing and the duplicate came back (the operator's "it sent my message twice
// and put it below the model's response").
func (s *chatStore) mergeHistory(convID string, history []chat.ChatItem) {
	s.mu.Lock()
	live := s.items[convID]
	// Durable user texts, for matching an optimistic echo.
	var durableUser []string
	for _, it := range history {
		if it.Kind == chat.KindUser {
			durableUser = append(durableUser, it.Text)
		}
	}
	kept := make([]chat.ChatItem, 0, len(live))
	for _, it := range live {
		if it.Kind == chat.KindUser && strings.HasPrefix(it.Key, "draft-") && matchesAny(durableUser, it.Text) {
			continue // the durable copy supersedes the optimistic echo
		}
		kept = append(kept, it)
	}
	// INTERLEAVE by timestamp — never "history then live". Appending the live
	// buffer after the durable transcript put every live row at the BOTTOM, so
	// a just-sent user message rendered BELOW the model's reply (the
	// operator's "user messages are printing AFTER the model's messages").
	// Any live row the dedupe above does not drop has to land in its real
	// chronological place.
	merged := append(append([]chat.ChatItem{}, history...), kept...)
	chat.SortChronologically(merged)
	s.items[convID] = merged
	s.mu.Unlock()
}

// matchesAny reports whether want equals, or is a suffix of, any candidate.
// Suffix covers the prepended context preamble without needing to parse it.
func matchesAny(candidates []string, want string) bool {
	if want == "" {
		return false
	}
	for _, c := range candidates {
		if c == want || strings.HasSuffix(c, want) {
			return true
		}
	}
	return false
}

// replace swaps the conversation's items for the durable transcript —
// the completion authority (GUI semantics: the poll replaces the live
// buffer once the turn resolves, so nothing renders twice).
//
// EXCEPT: a live optimistic user echo (Key "draft-*") that the incoming durable
// list does NOT yet contain is KEPT. A durable fetch can legitimately land before
// the user's own row is visible to it — at conversation creation the transcript
// load races the turn that writes the message — and a blind replace then wiped
// the operator's just-sent text, leaving a conversation whose FIRST user message
// never appeared while the reply streamed in normally (the operator's "it is
// still not showing the initial user message on a new conversation"). The durable
// copy supersedes the echo as soon as it actually contains it, so this preserves
// exactly one copy.
func (s *chatStore) replace(convID string, items []chat.ChatItem) {
	s.mu.Lock()
	live := s.items[convID]
	var durableUser []string
	for _, it := range items {
		if it.Kind == chat.KindUser {
			durableUser = append(durableUser, it.Text)
		}
	}
	out := append([]chat.ChatItem{}, items...)
	for _, it := range live {
		if it.Kind == chat.KindUser && strings.HasPrefix(it.Key, "draft-") && !matchesAny(durableUser, it.Text) {
			out = append(out, it) // the durable view has not caught up yet
		}
	}
	if len(out) != len(items) {
		chat.SortChronologically(out)
	}
	s.items[convID] = out
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

// conversationByID returns the conversations-rail row for id, when the shell
// holds one. It is the freshest conversation metadata available to the shell: the
// rail is reloaded after every send, so its row carries the title and message
// count that a detail pane's one-shot GetConversation may have read before they
// existed.
func (m *App) conversationByID(id string) (chat.Conversation, bool) {
	for _, c := range m.conversations {
		if c.ID == id {
			return c, true
		}
	}
	return chat.Conversation{}, false
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
		RenderTranscript([]chat.ChatItem, chat.Conversation, bool) (title string, fields []screenkit.Field)
	}); ok {
		items := m.chatStore.snapshot(m.chatConvID)
		// The rail's row for this conversation is the freshest header data the
		// shell holds: it is reloaded after every send, and it carries the title
		// and the message count, which the screen's cached GetConversation can
		// predate for a brand-new conversation.
		live, haveLive := m.conversationByID(m.chatConvID)
		title, fields := askS.RenderTranscript(items, live, haveLive)
		w := s.(interface{ DetailWidth() int }).DetailWidth()
		// The transcript renders through the kit2 Stream widget: an
		// extension of the previous render APPENDS (the operator's scroll
		// offset is preserved; the tail is followed only when already at the
		// bottom), anything else (durable reload / turn resolution) resets.
		str := m.transcriptStream(m.chatConvID, w, m.contentHeight())
		m.syncTranscript(m.chatConvID, str, items, w)
		if m.chatStore.isReconnecting(m.chatConvID) {
			str.Notice = "reconnecting…"
		} else {
			str.Notice = ""
		}
		// Surface the scroll position when the transcript is taller than the pane.
		//
		// The transcript follows the TAIL, so a reply longer than the pane scrolls
		// the operator's own earlier messages out of view. Nothing on screen said
		// the transcript was scrolled, and on the Ask tab the rail owned both the
		// wheel and the keyboard — so a message that was merely OFF-SCREEN read as
		// a message that was never stored (the operator's "I still do not see my
		// initial test user message", while the header reported the right count).
		//
		// The label names the visible window and the total, so "4-23/35" says
		// plainly that 22 lines sit above. Same field shape the Work screen's
		// build log already uses.
		if str.Overflowing() {
			fields = append(fields, screenkit.Field{Key: "scroll", Value: str.ScrollLabel()})
		}
		st.SetDetailContent(title, fields, str.View())
	}
	return nil
}

// transcriptStream returns (creating + sizing) the kit2 Stream backing a
// conversation's transcript.
func (m *App) transcriptStream(convID string, w, h int) *kit2.Stream {
	if m.chatStreams == nil {
		m.chatStreams = map[string]*kit2.Stream{}
	}
	str := m.chatStreams[convID]
	if str == nil {
		str = kit2.NewStream("transcript", w, h)
		m.chatStreams[convID] = str
	}
	str.SetSize(w, h)
	return str
}

// syncTranscript renders the grouped transcript and folds it into the
// stream: when the new render EXTENDS the previous one the extra lines are
// appended (preserving scroll offset / following only at the bottom); any
// other change replaces the lines (a reload re-pins to the tail).
func (m *App) syncTranscript(convID string, str *kit2.Stream, items []chat.ChatItem, w int) {
	if m.transcriptLines == nil {
		m.transcriptLines = map[string][]string{}
	}
	body := chat.RenderItems(chat.GroupByPhase(items), w)
	var lines []string
	if body != "" {
		lines = strings.Split(strings.TrimRight(body, "\n"), "\n")
	}
	prev := m.transcriptLines[convID]
	if len(lines) >= len(prev) && linesPrefix(lines, prev) {
		if len(lines) > len(prev) {
			str.Append(lines[len(prev):]...)
		}
	} else {
		str.SetLines(lines)
	}
	m.transcriptLines[convID] = append([]string{}, lines...)
}

// linesPrefix reports whether prefix is a line-for-line prefix of lines.
func linesPrefix(lines, prefix []string) bool {
	if len(prefix) > len(lines) {
		return false
	}
	for i, p := range prefix {
		if lines[i] != p {
			return false
		}
	}
	return true
}

// TranscriptStream returns the Stream backing a conversation's transcript
// (nil when that conversation has never rendered).
func (m *App) TranscriptStream(convID string) *kit2.Stream { return m.chatStreams[convID] }

// ScrollTranscript wheels the open transcript by delta lines (the operator
// scroll offset the Stream preserves across appends).
func (m *App) ScrollTranscript(delta int) {
	if str := m.chatStreams[m.chatConvID]; str != nil {
		str.Wheel(delta)
	}
}

// onConversationMutated reconciles the conversations rail after a rename /
// delete / mode write: a delete drops the row locally (and clears the
// active conversation when it was the deleted one), then the rail refetches
// from the live API so it can never drift.
func (m *App) onConversationMutated(msg chat.ConversationMutatedMsg) tea.Cmd {
	if msg.Err != "" {
		m.dock.SetError(msg.Op + " failed: " + msg.Err)
		return m.reloadConversations()
	}
	if msg.Op == "compact" {
		// Compaction rewrites the server-side history, so the transcript the
		// pane shows is now stale (and the summary is a new assistant message).
		// Surface the outcome, then re-poll so the collapsed history is visible
		// instead of the pre-compaction transcript.
		if msg.ID == m.chatConvID && msg.Detail != "" {
			m.dock.SetNotice("compact: " + msg.Detail)
			return tea.Batch(m.chat.Poll(msg.ID), m.reloadConversations())
		}
		return m.reloadConversations()
	}
	if msg.Op == "delete" {
		kept := m.conversations[:0]
		for _, c := range m.conversations {
			if c.ID != msg.ID {
				kept = append(kept, c)
			}
		}
		m.conversations = kept
		if m.convSel >= len(m.conversations) {
			m.convSel = max(0, len(m.conversations)-1)
		}
		if m.chatConvID == msg.ID {
			m.chatConvID = ""
			m.chat.SetActive("")
			if s := m.screens[TabAsk]; s != nil {
				if st, ok := s.(interface {
					SetDetailContent(title string, fields []screenkit.Field, body string)
				}); ok {
					st.SetDetailContent("New chat", nil, "")
				}
			}
		}
	}
	if msg.Op == "model" {
		// The model changed: drop the cached window so it is re-resolved for the
		// NEW ref, and re-read the strip (the ref it displays just changed).
		m.ctxWindowFor = ""
		return tea.Batch(m.reloadConversations(), m.refreshMetrics())
	}
	return m.reloadConversations()
}

// onStreamDone resolves a finished turn: the durable transcript is re-read (the
// completion authority), the header is refreshed, and the stat strip updates.
func (m *App) onStreamDone(msg chat.StreamDoneMsg) tea.Cmd {
	m.chat.EndStream(msg.ConvID)
	// A finished turn is when new usage lands, so this is the LIVE update: the
	// stat strip re-reads the session's tokens / cache / cost and refreshes.
	//
	// The conversations RAIL is refreshed for the same reason, and it is what
	// repairs the Detail header: that header overlays the rail's row (title +
	// message count), and for a brand-new conversation the pane's one-shot
	// GetConversation ran BEFORE the first send had named the conversation and
	// written any message — so it reported "title —" and "messages 0" until the
	// rail caught up. Without this the header could stay stale for the whole
	// turn, showing a blank title and a zero count beside a transcript that
	// plainly had content (the operator's screenshot: "messages 0" next to a
	// populated transcript).
	return tea.Batch(m.chat.Poll(msg.ConvID), m.refreshMetrics(), m.chat.LoadConversations())
}

// setChatError maps a chat failure to the dock error strip (401 gets
// the re-auth prompt naming the in-place fix; the app keeps running).
func (m *App) setChatError(where string, err error) {
	// Settle the composer's send ack. The dock writes "sending …" the instant
	// Enter fires, so every terminal outcome has to replace it — otherwise a
	// FAILED send reads as one still in flight, forever (the operator's "it
	// says sending but nothing ever opens"). The banner/error below IS the
	// outcome, and it is set after this clear so it wins.
	m.dock.SetNotice("")
	// A failed send must not lose the operator's message: put the draft
	// back in the composer (RestoreDraft never clobbers text typed since).
	if chat.IsAuthExpired(err) {
		m.setReauthBanner()
		m.dock.RestoreDraft()
		return
	}
	m.dock.SetError(where + ": " + err.Error())
	m.dock.RestoreDraft()
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
	// The rail's error only suppresses the shell banner while the rail is
	// actually ON SCREEN. m.convErr is the CONVERSATIONS RAIL's load failure,
	// and on the Ask launch page the rail is not rendered at all
	// (railVisible() is false in welcome mode). Treating it as "the inline
	// state IS the banner" therefore made the banner suppress ITSELF, and the
	// operator got neither: an Enter that failed showed a stuck "sending …"
	// ack with the draft restored into the box and no explanation anywhere.
	if m.convErr != "" && isAuthErrText(m.convErr) && m.railVisible() {
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
	// SEED the model on create, never leave it empty.
	//
	// This used `m.chat.PendingModel()`, which is empty unless the operator ran
	// /models — so a conversation created straight from the New page stored an
	// EMPTY model_ref. The composer's context window resolves from the ref
	// (currentAskModel → the model's live metadata), so an empty ref meant no
	// denominator until the operator picked a model by hand: "It seems I must
	// first do a /models in new mode before I will see the actual 1 million
	// context."
	//
	// currentAskModel already walks the documented chain — the open
	// conversation's ref, else the pending selection, else the TENANT DEFAULT —
	// so a send from the New page now binds the tenant's default Ask model to the
	// new conversation and the strip is correct from the first render.
	model := m.currentAskModel()
	mode := m.chat.PendingMode()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		// Create EMPTY, then send once through the stream below. Passing
		// InitialMessage AND then sending persisted the operator's first message
		// TWICE and started two turns (the operator's "it sent two messages
		// instead of one on the first message"). This mirrors the GUI, which
		// calls createConversation({mode}) with no initialMessage and then
		// sendStreaming(conv.id, text) — see ask-orchicon.tsx.
		resp, err := cl.Ask.CreateConversation(ctx, connect.NewRequest(&apiv1.CreateConversationRequest{
			ModelRef: model,
			Mode:     mode,
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

// setFocus moves focus between the content pane and the composer,
// syncing the dock's textarea.
func (m *App) setFocus(f focusMode) {
	m.chatFocus = f
	if f == focusComposer {
		m.dock.Focus()
	} else {
		m.dock.Blur()
	}
	// THE FOOTER'S FOCUS HINT IS DERIVED HERE, at the ONE chokepoint every focus change passes
	// through, rather than at the handful of call sites somebody remembered.
	//
	// It was set in three places in the MOUSE handler and recomputed in passToScreen — neither of
	// which covers the keyboard. So pressing Tab to the tab bar (or an F-key, which now lands there)
	// left the footer claiming "composer" while the composer was blurred and typing went nowhere:
	// the focus indicator said the opposite of the truth at exactly the moment the operator needed
	// it. It is the only on-screen evidence of where the keyboard is, so a wrong value is worse than
	// no value.
	m.footer.ComposerFocus = f == focusComposer
}

// askPaneID is which PANE holds the keyboard on the Ask tab: the conversations rail, or
// the open conversation itself.
type askPaneID int

const (
	// askPaneRail is the conversations rail — the list of conversations.
	askPaneRail askPaneID = iota
	// askPaneConversation is the open conversation's transcript.
	askPaneConversation
)

// askPaneKey handles left/right as PANE SELECTION on the Ask tab, reporting whether it
// owned the key.
//
// The operator: "make left/right actually select the full conversation versus the pane and
// then up/down works accordingly between the rail and conversation". Tab is deliberately
// NOT used — it belongs to the tab bar — so left/right is the pane gesture on this tab,
// matching what left/right already mean on every other one ("left+right should move
// between the two panes below the menus").
//
// It also drives the panes' BORDERS through Base.SetPaneFocus, so the selection is VISIBLE. Without
// that the operator had "no ... highlight [on] the pane letting you know you have focus".
//
// THE DIRECTIONS FOLLOW THE LAYOUT, NOT THE NAMES. The rail is joined to the RIGHT of the content
// (View: JoinHorizontal(body, rail)), so RIGHT selects the rail and LEFT the conversation — the arrow
// points at the pane. I had this inverted at first and the operator reported it immediately: "the
// arrows are in reverse. You have to hit right from the rail to focus on the conversation even though
// the conversation pane is on the left".
func (m *App) askPaneKey(key string) (bool, tea.Cmd) {
	if m.active != TabAsk || !m.railVisible() {
		return false, nil
	}
	switch key {
	case "right":
		m.askPane = askPaneRail
	case "left":
		m.askPane = askPaneConversation
	default:
		return false, nil
	}
	m.setFocus(focusContent)
	m.syncAskPaneFocus()
	m.refreshStreamStatus()
	return true, nil
}

// syncAskPaneFocus makes the pane borders agree with askPane. Called wherever askPane changes, so the
// two panes can never disagree about which one holds the keyboard.
func (m *App) syncAskPaneFocus() {
	if m.active != TabAsk {
		return
	}
	if b, ok := m.screens[TabAsk].(interface{ SetPaneFocus(bool) }); ok {
		b.SetPaneFocus(m.askPane == askPaneConversation)
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
