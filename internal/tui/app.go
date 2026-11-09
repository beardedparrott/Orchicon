// Package tui is orch's app shell: the seven-tab shell (Ask, Overview, Work, Execution, Automation,
// Enforcement, Control) mirroring the GUI nav (frontend/src/lib/nav-config.ts + app-shell.tsx).
package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/toolclass"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
	"github.com/beardedparrott/orchicon/internal/tui/diffs"
	"github.com/beardedparrott/orchicon/internal/chatlimits"
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
	"github.com/beardedparrott/orchicon/internal/tui/stream"
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

// DockError / DockNotice are the shell hooks screens use to surface mutation feedback (the kit2
// mutate.Executor sink) in the always-present chat dock.
func (m *App) DockError(msg string)  { m.dock.SetError(msg) }
func (m *App) DockNotice(msg string) { m.dock.SetNotice(msg) }

// focusMode is where keyboard focus lives: the content pane (screens keep tab chords + editing) or
// the chat composer (global tab chords fall through to readline editing inside the input).
type focusMode int

const (
	focusContent focusMode = iota
	focusComposer
	// focusTabs means the TAB BAR holds the keyboard — nothing below it has been chosen yet.
	focusTabs
)

// App is the root model.
type App struct {
	// deadNewlineChord records that the profile's configured newline chord cannot be delivered on this
	// platform and was replaced by the platform default.
	deadNewlineChord string

	clients *client.Clients
	profile *config.Profile
	reg     *subs.Registry
	// A constructed screen is never built without its shell reference: newScreen injects this into
	// every screen it creates (see the helper's comment for the bug that omission caused).
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
	// plane is the last PLANE-STATE probe (plane_lifecycle.go).
	plane planeInfo
	// planeProbed says the first-layout probe has been armed, so a resize storm does not re-probe.
	planeProbed bool
	// planeDownErrText is the EXACT strip text applyPlaneState wrote for a down plane, so leaving the
	// down state clears that message and never one somebody else wrote.
	planeDownErrText string
	// planeReprobeAt is when the last disconnect-driven re-probe was armed — planeReprobeCmd's cooldown.
	planeReprobeAt time.Time
	help           helpModel
	// msgMenu is the per-message edit menu (undo / revert to here), opened by a RIGHT-CLICK on one of the
	// operator's own messages. See msgmenu.go.
	msgMenu  *msgMenu
	routes   []KeyRoute
	quitting bool
	// launchDir is the directory the process was launched from — a FACT ABOUT THE PROCESS (os.Getwd),
	// set by tui.WithLaunchDir and empty in tests and embedders.
	launchDir string
	// launchPromptArmed says whether THIS shell may raise the launch-time project PROMPT (launch.go).
	launchPromptArmed bool
	// launch is the launch-time project prompt while it is up (nil = not showing).
	launch *launchPrompt
	// refreshGen identifies the CURRENT rolling-refresh chain.
	refreshGen uint64

	// keepalive refreshes the access token BEFORE it expires, so a password-mode session never meets an
	// expiry during an idle stretch (see client.StartKeepalive).
	keepalive *client.Keepalive

	// shellCtx is the shell's lifetime context: cancelled when the App is torn down, so background
	// work (the keepalive above) exits with it rather than outliving the screen that started it.
	shellCtx context.Context

	// Chat dock state (feature: context-aware Ask Orchicon + slash).
	dock      dock.Model
	chat      *chat.Controller
	chatStore *chatStore // guarded chatItems (stream goroutine writes)
	// renderCache remembers each transcript item's rendered segment, so a repaint pays for the items
	// that CHANGED rather than for every item in the conversation.
	renderCache  *chat.RenderCache
	chatWake     chan struct{} // live-chunk repaint poke (cap 1)
	chatCmds     chan tea.Cmd  // goroutine follow-ups (watch re-dial, poll)
	chatFocus    focusMode
	mouseEnabled bool // tea.WithMouseCellMotion is on; footer shows "Mouse Enabled"

	// permGrants is the conversation's active session grants AS LAST REPORTED BY THE SERVER, keyed by
	// conversation.
	permGrants       map[string][]chat.SessionGrant
	permGrantsLoaded map[string]bool
	permGrantsErr    map[string]string
	// pendingConsentRevoke carries the revoke command the Ask screen's SYNCHRONOUS host call cannot
	// return (ConsentRevoke must answer "did that work" with an error, and the RPC is asynchronous).
	pendingConsentRevoke tea.Cmd
	// permStore is the PERSISTENT allow/deny list (the FILE is the source of truth; storage is the
	// sibling policy task's).
	permStore chat.PermissionStore

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

	// modelPicker is the /models modal (nil = closed).
	modelPicker *kit2.ModelPicker
	// renameConv is the conversation-rename modal (nil = closed), hosted here for the same reason as
	// the model picker: the surface it names (the conversations rail) belongs to the SHELL.
	renameConv *kit2.Form
	// convScopeForm is the conversation-scope MCP modal (/mcp define|edit|secret|install), hosted here
	// for the same reason: the surface it edits belongs to the SHELL's open conversation.
	convScopeForm *kit2.Form
	// scope is the CONVERSATION SCOPE modal (/scope, /mcp, /skills): the conversation's MCP definitions
	// and skill files in one list, with the project's shown read-only above them (scope_modal.go).
	scope        *scopeModal
	renameConvID string
	// Categories (worker / workflow / conversation groupings).
	categories   []*apiv1.Category
	assignForm   *kit2.Form
	assignTarget apiv1.CategoryTargetType
	// catAssignedBy maps a TARGET-TYPE-SCOPED entity key (catEntityKey) to the grouping that entity is
	// in.
	catAssignedBy map[string]string
	// catForm is the shell's PREFILLED grouping-rename form (nil = closed), hosted here for the same
	// reason as the rename/assign modals: the grouping it edits is drawn by whoever shows it.
	catForm   *kit2.Form
	catFormID string
	// assignEntities is what the modal is pointed at — a LIST, because the rail can bulk-assign a whole
	// marked selection as well as one row.
	assignEntities []string
	pendingCatCmd  tea.Cmd
	// convMarked is the rail's multi-selection, keyed by conversation ID (never by index: the rolling
	// refresh re-seats rows by id.
	convMarked map[string]bool
	// convCollapsed is the conversations rail's collapsed folders, keyed by category id.
	convCollapsed map[string]bool

	// reasoningFolded is the set of FOLDED reasoning blocks in the open conversation, keyed by the
	// ChatItem's Key (stable per block, because the controller assigns it).
	reasoningFolded map[string]bool

	// pendingAttach holds the files acquired for the NEXT turn — pasted screenshots and attached paths
	// that have not been sent yet.
	pendingAttach []attachment
	// bulkConfirm hosts the confirm dialog for a destructive bulk rail operation (nil = closed), and
	// bulkConfirmRun is what the affirmative choice dispatches.
	bulkConfirm    *kit2.Dialog
	bulkConfirmRun func() tea.Cmd
	// metrics is the open conversation's usage roll-up feeding the composer's stat strip.
	metrics      sessionMetrics
	ctxWindowFor string
	ctxWindow    int64
	// askDefaultModel is the tenant's DefaultAskOrchiconModel (settings).
	askDefaultModel string
	execSessions    map[string][]chat.ChatItem // execution id → durable session items

	// Transcript Stream widgets (kit2): one per conversation.
	chatStreams     map[string]*kit2.Stream
	transcriptLines map[string][]string
	// transcriptSpans is each conversation's rendered item geometry, from the same render that produced
	// transcriptLines. It is what a click resolves against.
	transcriptSpans map[string][]chat.ItemSpan
	pendingDetail   tea.Cmd
	lastScreenKeys  string

	// Diff sidebar (TUI sibling of the GUI DiffSidebar).
	diffOpen bool
	diffPane *diffs.Model
	diffTab  diffs.Tab
	diffPath string
	// diffArmedFor is the owner+liveness the pane's live stream was last SETTLED for, as
	// "kind|id|live".
	diffArmedFor string
	// It is the ONLY extra input to diffPaneWidth, so every consumer of the width — contentWidth,
	// refreshLayout, both SetSize calls, the mouse hit-tests, the selection-region guard.
	diffPaneW int
	// listSharePct is the operator's master-detail split preference for the screens (0 = the default
	// even split).
	listSharePct int
	// diffScrollDragging is a LIVE scrollbar drag on the rail (see dispatchMouse): the press jumped the
	// viewport and every motion follows the pointer until the release.
	diffScrollDragging bool
	// diffResizing is true from a divider press until its release.
	diffResizing bool
	// diffResizeMoved records whether the live gesture actually MOVED the rail, and diffResizePrev is
	// the override in force when it began (0 = auto).
	diffResizeMoved bool
	diffResizePrev  int

	// Ask conversations rail (GUI Ask sidebar).
	askMode askMode // Ask's presentation: launch page vs conversations
	// askSearch is the OPEN transcript search query for the Ask pane (transcript_search.go).
	askSearch   string
	askMatchIdx int
	// findBox is the FIND MODAL (nil = closed) — the box ctrl+f opens and the field the query is typed
	// into (findbox.go).
	findBox *findBox
	// Slide-out conversation strip (chatpanel.go): open when the operator engages the composer on a
	// screen other than Ask, so a send is VISIBLE.
	panelOpen     bool
	panelScroll   int
	conversations []chat.Conversation
	// railProjects is the tenant's projects, for the rail's PROJECT grouping (railprojects.go).
	railProjects []railProject
	// projectScope is the ACTIVE PROJECT the rail is filtered by — the operator's "Projects are
	// WORKSPACES" and "/project to set the active project".
	projectScope string
	// projectScopeChosen records that the operator picked the workspace THEMSELVES, which is what stops
	// the launch-directory default from overruling them.
	projectScopeChosen bool
	// railProjectsLoaded records that a project load SUCCEEDED, so a failed one is retried by the next
	// conversations load rather than leaving an empty picker for the session.
	railProjectsLoaded bool
	// pendingProjectName is a workspace selection parked until the project list arrives
	// (projectselect.go).
	pendingProjectName string
	// projectThemes mirrors the config file's top-level project_themes map: project id -> palette name,
	// verbatim (see config.Config.ProjectThemes).
	projectThemes map[string]string
	// themePinned marks that the ACTIVE palette was chosen by an explicit /theme (or the Themes pane),
	// not by the project-scope follower.
	themePinned bool
	// degradedPaletteWarned dedupes the "this project's palette is unknown to this build" notice per
	// (project id, palette name) pair.
	degradedPaletteWarned map[string]bool
	// projectPick is the /project modal (nil = closed). It chooses the scope, or moves one conversation — see
	// projectpick.go for why the two share the control.
	projectPick  *projectPicker
	convRailOpen bool
	// askPane records which Ask pane holds the keyboard.
	askPane askPaneID

	convSel    int
	convScroll int
	// Rail load state (Phase 2c finding 9): the rail is never a silent
	// empty box — a failed load holds an explicit error + retry state.
	convErr    string // last rail load failure (auth/API); "" = healthy
	convLoaded bool   // a successful rail load has landed
	// scopeHiddenNoticed is the number of conversations the ACTIVE scope was last REPORTED to be
	// hiding.
	scopeHiddenNoticed int
	convLoading        bool // a rail load is in flight

	// failedTurnRestored latches the message id of a durable failed turn whose draft has been put back
	// in the composer, so the restore happens ONCE per failed row and not on every poll.
	failedTurnRestored map[string]bool

	// rightRailOpen records the Ask screen's right-rail visibility; kept so
	// the renderer knows whether to draw the rail without re-deriving it.
	rightRailOpen bool

	// pendingDiffCmd carries the diff-pane owner-setup cmd out of a route
	// Handle (routes can't return a tea.Cmd; dispatch re-emits it).
	pendingDiffCmd tea.Cmd
	// pendingRailCmd carries the conversations-rail retry/reload cmd out of
	// a route or a mouse handler that cannot return one directly.
	pendingRailCmd tea.Cmd
	// pendingStopCmd carries the Stop (ctrl+y) abort out of its global key route — a KeyRoute.Handle
	// returns only a bool, so it cannot return a Cmd directly (same constraint.
	pendingStopCmd tea.Cmd
	// pendingFindCmd carries the find box's caret-blink cmd out of the ctrl+f route — the same constraint
	// as pendingStopCmd (a route Handle returns only a bool); drainStaged re-emits it.
	pendingFindCmd tea.Cmd
	// pendingAttachCmd carries an ATTACHMENT read out of its global key route (ctrl+v pastes a
	// clipboard image).
	pendingAttachCmd tea.Cmd
	// pendingClipCmd carries the OSC 52 WRITE for ctrl+a's copy.
	pendingClipCmd tea.Cmd
	// pendingAttachClear says the composer's text was a PATH being attached, so a SUCCESSFUL attach
	// should clear the box — otherwise the path would also be sent as prose.
	pendingAttachClear bool
	// lastSentAttachments holds the attachments of the turn currently in flight, so a FAILED send can
	// put them back (see restoreAttachments).
	lastSentAttachments []*apiv1.AttachmentInput
	// pendingScreenCmd carries a newly activated screen's first-load cmd
	// out of SwitchTo (which cannot return one).
	pendingScreenCmd tea.Cmd
	// Screens are constructed eagerly for the nav registry, so without this bookkeeping ONLY the
	// startup tab ever fetched its lists — every other tab rendered "nothing here" with no error.
	loaded map[TabID]bool
}

// DiffRailMinWidth is the left rail's MINIMUM width (cells).
const DiffRailMinWidth = 48

// diffPaneAutoWidth is the PROPORTIONAL width the pane gets when the operator has set no override:
// the auto share of the available columns.
func (m *App) diffPaneAutoWidth() int {
	return m.clampDiffPaneWidth(m.diffAvailWidth() * 45 / 100)
}

// diffAvailWidth is the column budget the diff rail and the content pane share: the terminal width,
// less the conversations rail when it is showing.
func (m *App) diffAvailWidth() int {
	avail := m.width
	if avail < 1 {
		avail = DiffRailMinWidth * 2
	}
	// The conversation rail, when present, is not ours to spend.
	if m.railVisible() {
		avail -= ConversationsRailWidth
	}
	return avail
}

// clampDiffPaneWidth is THE width clamp — the floor and the cap are expressed in this one function
// and nowhere else, so neither an override nor the auto path can invent a third rule.
func (m *App) clampDiffPaneWidth(width int) int {
	avail := m.diffAvailWidth()
	if width < DiffRailMinWidth {
		width = DiffRailMinWidth
	}
	if max := avail / 2; width > max {
		width = max
	}
	if width < 20 {
		width = 20
	}
	return width
}

// diffPaneWidth returns the left rail's APPLIED width for the current terminal: the operator's
// override when they have set one, the proportional width otherwise, clamped in exactly one place.
func (m *App) diffPaneWidth() int {
	w := m.diffPaneAutoWidth()
	if m.diffPaneW > 0 {
		w = m.diffPaneW
	}
	return m.clampDiffPaneWidth(w)
}

// diffResizeStep is how many cells one keyboard resize chord moves the rail. Fixed rather than
// proportional so the chord's effect is predictable on any terminal.
const diffResizeStep = 4

// setDiffPaneW records the operator's chosen rail width (the pane's right-edge column + 1) and
// re-lays the layout out.
func (m *App) setDiffPaneW(cells int) {
	if cells < DiffRailMinWidth {
		cells = DiffRailMinWidth
	}
	m.diffPaneW = cells
	m.refreshLayout()
}

// diffRailWidthStep grows (delta > 0) or shrinks (delta < 0) the rail by one step.
func (m *App) diffRailWidthStep(delta int) bool {
	if !m.diffOpen || m.diffPane == nil {
		return false
	}
	m.setDiffPaneW(m.diffPaneWidth() + delta*diffResizeStep)
	m.persistDiffRailWidth()
	return true
}

// splitAdjustable reports whether there is a split here for the chord to resize: the diff rail when
// it is open, otherwise the active screen's tree/detail split.
func (m *App) splitAdjustable() bool {
	if m.diffOpen && m.diffPane != nil {
		return true
	}
	s := m.screens[m.active]
	if s == nil {
		return false
	}
	sa, ok := s.(interface{ SplitAdjustable() bool })
	return ok && sa.SplitAdjustable()
}

// splitWidthStep widens or narrows the split IN FRONT OF THE OPERATOR.
func (m *App) splitWidthStep(delta int) bool {
	if m.diffRailWidthStep(delta) {
		return true
	}
	if !m.splitAdjustable() {
		// NOTHING TO RESIZE HERE, so the chord is NOT claimed — the rail's own step returns false in the
		// same situation, and a chord that silently changed a width nobody draws is worse than one.
		return false
	}
	s := m.screens[m.active]
	if s == nil {
		return false
	}
	nr, ok := s.(interface{ NudgeListShare(int) bool })
	if !ok {
		return false
	}
	nr.NudgeListShare(delta)
	// Read the value back rather than computing it here, so the clamp and the bounds live in ONE place
	// (kit2.Base) and the App stores exactly what the screen applied.
	if g, ok := s.(interface{ ListSharePct() int }); ok {
		m.listSharePct = g.ListSharePct()
	}
	m.persistListShare()
	// CONSUMED EVEN AT A BOUND: the chord belongs to the split, so pressing it when the split cannot move
	// further must not fall through to a screen and do something unrelated.
	return true
}

// splitWidthReset returns the split to its default: the rail to automatic when it is open, otherwise the
// active screen's tree/detail split to an even share. Same precedence, same reason.
func (m *App) splitWidthReset() bool {
	if m.diffRailWidthReset() {
		return true
	}
	if !m.splitAdjustable() {
		return false
	}
	s := m.screens[m.active]
	if s == nil {
		return false
	}
	lr, ok := s.(interface{ SetListSharePct(int) })
	if !ok {
		return false
	}
	lr.SetListSharePct(0)
	m.listSharePct = 0
	m.persistListShare()
	return true
}

// diffRailWidthReset returns the rail to the automatic/proportional width and clears the persisted
// override, so a restart does not resurrect the width the operator just abandoned.
func (m *App) diffRailWidthReset() bool {
	if !m.diffOpen || m.diffPane == nil {
		return false
	}
	m.diffPaneW = 0
	m.refreshLayout()
	m.persistDiffRailWidth()
	return true
}

// diffDividerHit reports whether a frame CELL is the pane's right-most column — the divider a drag
// grabs — over the pane's body rows.
func (m *App) diffDividerHit(x, y int) bool {
	if !m.diffOpen || m.diffPane == nil {
		return false
	}
	if x != m.diffPaneWidth()-1 {
		return false
	}
	top := tabBarRows + 1 // row 0 the tab bar, row 1 the rule, row 2 the blank separator
	bottom := top + m.screenRows() + m.panelRows() + m.dock.Lines() - 1
	if last := m.height - 2; bottom > last {
		bottom = last
	}
	return y >= top && y <= bottom
}

// AppOption customizes App construction.
type AppOption func(*App)

// WithLaunchDir sets the launch DIRECTORY — where the process was started.
func WithLaunchDir(dir string) AppOption {
	return func(m *App) {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			return
		}
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		m.launchDir = filepath.Clean(dir)
	}
}

// WithLaunchPrompt arms the launch-time project prompt (launch.go) for THIS shell.
func WithLaunchPrompt() AppOption {
	return func(m *App) { m.launchPromptArmed = true }
}

// NewApp builds the shell over an established client set.
func NewApp(cl *client.Clients, profile *config.Profile, serverVersion string, opts ...AppOption) *App {
	m := &App{
		clients: cl,
		profile: profile,
		reg:     subs.NewRegistry(),
		// The shell's lifetime context. Cancelled on teardown so the token keep-alive (and anything
		// else started for the shell's lifetime) exits with it.
		shellCtx: context.Background(),
		// The SELECTION and its clipboard live on a shared pointer: App is a value model, so View and
		// Update each receive a COPY.
		clip:    &clipState{},
		screens: map[TabID]Screen{},
		chatStore: &chatStore{
			items:     map[string][]chat.ChatItem{},
			toolCalls: map[string][]toolclass.Call{},
		},
		renderCache:        chat.NewRenderCache(),
		permGrants:         map[string][]chat.SessionGrant{},
		permGrantsLoaded:   map[string]bool{},
		permGrantsErr:      map[string]string{},
		failedTurnRestored: map[string]bool{},
		execSessions:       map[string][]chat.ChatItem{},
		loaded:             map[TabID]bool{},
		chatStreams:        map[string]*kit2.Stream{},
		transcriptLines:    map[string][]string{},
		reasoningFolded:    map[string]bool{},
		footer: footerModel{
			URL:           profile.URL,
			ServerVersion: serverVersion,
			ClientVersion: version.Current().Tag,
		},
	}
	m.dock = dock.New()
	// THE COMPOSER KNOWS THE MESSAGE BOUND, so it can warn BEFORE a send is refused. At 10,000 the limit was
	// discovered mid-paste, after the text was already in the box and the send button was pressed.
	m.dock.SetCharLimit(chatlimits.MaxMessageChars)
	// IT WAS NEVER SET, so the zero value stood and a fresh shell opened on unassignedScope ("") — the
	// picker's "No project" — and filterConversationsByScope then showed ONLY chats with no project.
	m.projectScope = projectScopeAll
	if profile != nil && profile.Newline != "" {
		// A PROFILE CAN CARRY A CHORD THIS PLATFORM CANNOT DELIVER — the common case being a profile saved
		// on Linux with `newline = "alt+enter"`.
		nl := dock.ParseNewlineMode(profile.Newline)
		if !dock.ChordReachesOrch(nl) {
			def := dock.DefaultNewlineMode()
			m.deadNewlineChord = string(nl) + " does not reach orch on this platform (the terminal consumes it), " +
				"so the newline chord has been set to " + string(def)
			nl = def
		}
		m.dock.Newlines = nl
	}
	// A PASTE THAT IS A FILE PATH ATTACHES THE FILE.
	m.dock.SetPastePathHook(func(text string) tea.Cmd {
		if !isProbablyPath(text) {
			return nil
		}
		m.pendingAttachClear = false // the paste IS the path; do not also clear what is already in the box
		return attachFileFromPrompt(text)
	})
	// The diff pane shares the shell's client set + registry so its live StreamFileEdits subscription
	// follows the same reconnect/resume/dedup semantics and is closed by CloseAll on screen close.
	m.diffPane = diffs.NewModel(cl, m.reg)
	m.diffTab = diffs.TabDiff
	m.chat = chat.NewController(cl)
	m.chatWake = make(chan struct{}, 1)
	m.chatCmds = make(chan tea.Cmd, 16)
	m.chat.Bind(&appEventStore{m: m}, m.chatCmds)
	// One factory per tab — screens construct lazily on first visit so stream subscriptions only exist
	// while their tab is active.
	m.factories = map[TabID]func() Screen{
		TabAsk:         func() Screen { return ask.New(m.clients, m.reg) },
		TabOverview:    func() Screen { return overview.New(m.clients, m.reg, "") },
		TabWork:        func() Screen { return work.New(m.clients, m.reg, "") },
		TabExecution:   func() Screen { return execution.New(m.clients, m.reg, "") },
		TabAutomation:  func() Screen { return automation.New(m.clients, m.reg, "") },
		TabEnforcement: func() Screen { return enforcement.New(m.clients, m.reg, "") },
		// SERVERS IS OFFERED ONLY FOR A LOCAL PLANE (D6).
		TabControl: func() Screen {
			_, local := instanceForProfile(m.profile)
			return control.New(m.clients, m.reg, control.WithServers(local))
		},
	}
	// The slash registry is generated from the screens' Sources() (the no-drift source of truth), so
	// factories must exist before it builds.
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
	// THE TOKEN-REFRESH RECOVERY HOOK, WIRED.
	m.wireSessionRecovery(m.clients)
	// THE TERMINAL'S OWN BACKGROUND IS ASKED FOR ONCE, HERE, before bubbletea takes the tty.
	theme.PrimeTerminalGround()
	// THE THEME IS APPLIED AND THE COMPOSER RE-PINNED, in that order and in one place.
	if profile != nil && profile.Theme != "" {
		m.applyThemeAndRefresh(profile.Theme)
	} else {
		m.applyThemeAndRefresh(theme.DefaultName)
	}
	// The persisted fold state comes from the same file, at the same moment, for the same reason: a
	// display preference that only takes effect after the operator has re-toggled it is the bug.
	m.loadCollapsedGroups()
	// The diff rail's width is the SAME kind of preference in the SAME file, so it is restored at the SAME
	// moment and by the same silent-on-failure rule (see prefs.go loadDiffRailWidth).
	m.loadDiffRailWidth()
	// The project→palette bindings are the SAME kind of preference too, loaded now so they are already in
	// memory by the time applyLaunchDirScope resolves the launch directory's project (see projecttheme.go).
	m.loadProjectThemes()
	// The master-detail split is the same kind of preference again (see prefs.go loadListShare).
	m.loadListShare()
	// Caller options LAST, so anything they set wins over the defaults above.
	for _, o := range opts {
		o(m)
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

// rebindScreens re-points every live screen's shell reference at THIS App.
func (m *App) rebindScreens() {
	for _, s := range m.screens {
		if ss, ok := s.(interface{ SetShell(any) }); ok {
			ss.SetShell(m)
		}
		// The split is the App's preference, so every screen is brought to it here — the same pass that
		// fixes a stale shell reference, for the same reason.
		if lr, ok := s.(interface{ SetListSharePct(int) }); ok {
			lr.SetListSharePct(m.listSharePct)
		}
	}
	// THE ACTIVE WORKSPACE, on the same pass and for the same reason: a screen built during an earlier
	// Update is not otherwise reachable.
	if c := m.pushProjectScope(); c != nil {
		m.pendingScreenCmd = tea.Batch(m.pendingScreenCmd, c)
	}
}

// pushProjectScope tells every screen which project is the active workspace, and returns the reload
// commands of the ones whose filter actually changed.
func (m *App) pushProjectScope() tea.Cmd {
	id := m.activeProjectID()
	var cmds []tea.Cmd
	for _, s := range m.screens {
		if ap, ok := s.(interface{ SetActiveProject(string) tea.Cmd }); ok {
			if c := ap.SetActiveProject(id); c != nil {
				cmds = append(cmds, c)
			}
		}
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// newScreen constructs a tab's screen and injects the shell reference.
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
	// operator has left.
	m.refreshGen++
	if _, ok := m.screens[id]; !ok {
		if s := m.newScreen(id); s != nil {
			m.screens[id] = s
		}
	}
	// Always (re)apply the layout to the screen being activated: a screen cached earlier (e.g. the nav
	// registry's introspection build.
	if s := m.screens[id]; s != nil && m.width > 0 {
		s.SetSize(m.contentWidth(), m.screenRows())
	}
	// A tab switch swaps the chrome's submenu to the new tab's (the dropdown follows focus, per the
	// mockup's menu-under-tab pattern).
	if m.menuOpen != "" {
		if m.menuOpen == id {
			m.closeTabMenu()
		} else {
			m.openTabMenu(id)
		}
	}
	m.updateContextChip()
	m.refreshComposerHint()
	// The diff pane's open state persists across SwitchTo (the GUI persists it at the host).
	if m.diffOpen {
		m.refreshDiffOwner()
	}
	// A screen first reached this session runs its first load now: the nav registry builds every screen
	// eagerly, so a lazily-activated screen would otherwise never fetch (every pane "nothing here").
	m.ensureLoaded(id)
}

// EnsureSubscriptions starts the screen's live event streams if it has any and they are not already
// running (idempotent).
func (m *App) EnsureSubscriptions(id TabID) {
	if s := m.screens[id]; s != nil {
		if es, ok := s.(interface{ EnsureSubscriptions() }); ok {
			es.EnsureSubscriptions()
		}
	}
}

// ensureLoaded runs a screen's first load (Init → Load) exactly once, when it first becomes
// activatable.
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

// drainStaged returns (once) every cmd staged by a route or mouse handler that cannot return one
// directly: screen first-loads, diff-pane owner setup, and conversations-rail reloads.
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
	if m.pendingStopCmd != nil {
		cmds = append(cmds, m.pendingStopCmd)
		m.pendingStopCmd = nil
	}
	if m.pendingFindCmd != nil {
		cmds = append(cmds, m.pendingFindCmd)
		m.pendingFindCmd = nil
	}
	if m.pendingAttachCmd != nil {
		cmds = append(cmds, m.pendingAttachCmd)
		m.pendingAttachCmd = nil
	}
	if m.pendingClipCmd != nil {
		cmds = append(cmds, m.pendingClipCmd)
		m.pendingClipCmd = nil
	}
	if m.pendingCatCmd != nil {
		cmds = append(cmds, m.pendingCatCmd)
		m.pendingCatCmd = nil
	}
	if m.pendingConsentRevoke != nil {
		cmds = append(cmds, m.pendingConsentRevoke)
		m.pendingConsentRevoke = nil
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// newChat drops the active conversation and returns Ask to its hero: the next composer send creates
// a fresh conversation (the GUI's New chat).
func (m *App) showAskConversations() {
	m.askMode = askConversations
	if m.active != TabAsk {
		m.SwitchTo(TabAsk)
	}
	m.EnsureSubscriptions(TabAsk)
	m.refreshLayout()
	// The rail only loads when it is shown, so the first time it appears it may never have been fetched
	// (or may have failed) — fetch it now.
	if m.convErr != "" || !m.convLoaded {
		m.pendingRailCmd = m.reloadConversations()
	}
}

func (m *App) newChat() {
	m.askMode = askNew
	m.chatConvID = ""
	// A new chat has no scope to show: the modal is per-conversation and the conversation it named is
	// gone.
	m.closeScopeModal()
	if m.chat != nil {
		m.chat.SetActive("")
		// A NEW CONVERSATION STARTS AT THE DEFAULT MODE.
		m.chat.SetPendingMode(apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM)
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

// openRenameConversation opens the rename modal for a conversation, PREFILLED with its current
// title.
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
			// Stay open and SAY why, rather than closing on a no-op (the GUI's saveRenameConv silently returns
			// when the trimmed value is empty.
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

// modalPanelFittingContent wraps a body in a modal panel WIDE ENOUGH TO SHOW IT,
// starting from the standard modal width.
//
// It exists for the one overlay whose body is hardcoded prose rather than a form:
// the connection screen writes its hint paragraphs as literal strings with their own
// line breaks, so it cannot reflow to a narrower box. A Panel truncates each body
// line to its interior width (kit2.Panel: innerW = w-2, then ansi.Truncate), so the
// standard 72-cell cap cut the last few words off three of those hints — a NEW
// artifact, introduced by giving this overlay a solid panel instead of splicing it
// raw across the full viewport.
//
// So the panel takes the width its content needs, clamped to the viewport with the
// same 4-cell margin modalWidth uses. Sizing a box to its content cannot clip it; a
// fixed-cap box can, and did.
func (m *App) modalPanelFittingContent(body string) string {
	w := m.modalWidth()
	if need := widestLineWidth(body) + 2; need > w { // +2 for the border cells
		if limit := m.width - 4; need > limit {
			need = limit
		}
		if need > w {
			w = need
		}
	}
	return m.modalPanel(body, w)
}

// widestLineWidth is the display width of the widest line in s.
func widestLineWidth(s string) int {
	w := 0
	for _, l := range strings.Split(s, "\n") {
		if n := lipgloss.Width(l); n > w {
			w = n
		}
	}
	return w
}

// renameConvKey drives the rename modal.
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

// scrollActiveDetail scrolls the active screen's detail pane when it has one (every list+detail
// screen embeds screenkit.Base.ScrollDetail).
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

// tabRingNext advances the operator focus ring: chat prompt FIRST, then the six area tabs in order,
// then back to the prompt.
func (m *App) tabRingNext() {
	// One action per press: an open dropdown just closes (no focus move).
	if m.TabMenu() != nil {
		m.closeTabMenu()
		return
	}
	// Tab lands on the TAB BAR, from the composer AND from the content.
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
	// WRAP.
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
func (m *App) toggleSideRails() {
	if m.diffOpen {
		m.closeDiffPane()
		return
	}
	if kind, id := m.diffOwner(); kind == diffs.NoneOwner || id == diffs.NoneOwner {
		// No diff-relevant session yet: still bring the pane OUT so Shift+Tab is a predictable toggle (it
		// renders its empty state) rather than a silent no-op.
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
	// Fixed shell overhead (Phase 2a full-screen chrome): the centered tab bar + underline rule (2),
	// the ONE blank separator after the chrome, the footer strip.
	h := m.height - 4 - m.dock.Lines()
	if h < 1 {
		h = 1
	}
	return h
}

// contentWidth is the main content area width.
func (m *App) contentWidth() int {
	w := m.width
	if m.diffOpen {
		w -= m.diffPaneWidth()
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

// diffOwner derives the pane's owner from the active context: the execution screen's selected
// detail (owner_kind "execution".
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

// ensureDiffStreamArmed arms an ALREADY-OPEN pane's live stream once its owner becomes live, and
// does nothing otherwise.
func (m *App) ensureDiffStreamArmed() tea.Cmd {
	if !m.diffOpen || m.diffPane == nil {
		return nil
	}
	kind, id := m.diffOwner()
	if kind == diffs.NoneOwner || id == diffs.NoneOwner {
		m.diffArmedFor = ""
		return nil
	}
	// THE KEY IS WHAT KEEPS THIS CHEAP.
	live := m.diffOwnerLive(kind, id)
	key := kind + "|" + id + "|" + strconv.FormatBool(live)
	if key == m.diffArmedFor {
		return nil
	}
	m.diffArmedFor = key
	if m.diffPane.StreamArmed() || !live {
		// Already subscribed, or nothing live to subscribe to (a finished run /
		// idle conversation has a durable ledger and no stream).
		return nil
	}
	return m.diffPane.SetOwner(kind, id, true)
}

// diffOwnerLive reports whether the owner's session is live (a running execution or an open ask
// conversation).
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
	// NOTE: the composer-vs-content focus rule lives on the `d`/`D` ROUTE (so `d` stays a literal
	// character while composing).
	kind, id := m.diffOwner()
	if kind == diffs.NoneOwner || id == diffs.NoneOwner {
		// No diff-relevant session on this screen — the toggle is a no-op.
		return nil
	}
	m.diffOpen = true
	m.restoreDiffPaneState()
	m.diffPane.SetSize(m.diffPaneWidth(), m.contentHeight()+m.dock.Lines())
	m.refreshLayout()
	// The pane keeps its previously selected path if it matches this owner's
	// files; otherwise the SetOwner fetch defaults it (see diffs.Model).
	return m.diffPane.SetOwner(kind, id, m.diffOwnerLive(kind, id))
}

// applyThemeAndRefresh applies a theme AND re-pins every component that CAPTURES styles, rather
// than reading them at render time.
func (m *App) applyThemeAndRefresh(name string) bool {
	if !theme.Use(name) {
		return false
	}
	// The composer captures textarea/cursor styles at construction, so a switch must re-pin them
	// (otherwise the box keeps the old palette). dock.Model is a value, so there is no nil case.
	m.dock.ApplyTheme()
	return true
}

// closeDiffPane closes the pane, tears down its live stream, and restores the previous layout (the
// content + dock widths are re-derived on the next render by contentWidth).
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
	// The DOCK WIDTH comes first: the composer's height (dock.Lines) depends on how its hint wraps at
	// that width, and the screens are sized from the rows the dock leaves.
	if m.width > 0 {
		m.dock.Width = m.contentWidth()
	}
	// AND THE DOCK'S HEIGHT BUDGET: the composer's growth ceiling is derived from the viewport, so the
	// dock has to know how tall the terminal is before anything asks it how many rows it needs (see.
	if m.height > 0 {
		m.dock.SetViewportRows(m.height)
	}
	if s := m.screens[m.active]; s != nil && m.width > 0 {
		s.SetSize(m.contentWidth(), m.screenRows())
	}
	if m.diffPane != nil {
		m.diffPane.SetSize(m.diffPaneWidth(), m.screenRows()+m.dock.Lines()+m.panelRows())
	}
	// THE ASK TRANSCRIPT IS RE-MEASURED HERE, for the same reason the dock's width is set first: its
	// SIZE depends on the dock's height, and the dock's height depends on its own content.
	if m.active == TabAsk && m.chatConvID != "" {
		m.remeasureTranscript()
	}
}

// remeasureTranscript re-sizes the open conversation's transcript to the pane's CURRENT body
// height, preserving the operator's scroll.
func (m *App) remeasureTranscript() {
	str := m.chatStreams[m.chatConvID]
	if str == nil {
		return // nothing rendered yet; onChatWake will size it on its first paint
	}
	s := m.screens[TabAsk]
	if s == nil {
		return
	}
	bh, ok := s.(interface{ DetailBodyHeight(int, bool) int })
	if !ok {
		return
	}
	fieldRows := 0
	if fp, ok := s.(interface{ DetailFieldCount() int }); ok {
		fieldRows = fp.DetailFieldCount()
	}
	str.SetSize(str.Width, bh.DetailBodyHeight(fieldRows, true))
	// A resize CLAMPS the offset, so a stream taller than the pane keeps its newest row on screen. Without
	// this the operator's scroll could sit past the last line after a shrink.
	str.ScrollLabel()
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

// syncDiffPaneState captures the pane's current tab/selection into the shell-owned state
// (pane→shell).
func (m *App) syncDiffPaneState() {
	if m.diffPane == nil {
		return
	}
	m.diffTab = m.diffPane.Tab
	if m.diffPane.SelectedPath != "" {
		m.diffPath = m.diffPane.SelectedPath
	}
}

// restoreDiffPaneState applies the shell-owned open/tab/selected state to the pane (shell→pane).
func (m *App) restoreDiffPaneState() {
	if m.diffPane == nil {
		return
	}
	m.diffPane.SetTab(m.diffTab)
	if m.diffPath != "" {
		m.diffPane.SelectPath(m.diffPath)
	}
}

// wireSessionRecovery installs the session's OnRefreshed hook on a client set: on a successful
// token refresh, redial every live stream so it picks up the fresh bearer.
func (m *App) wireSessionRecovery(cl *client.Clients) {
	if cl == nil || cl.Session == nil {
		return
	}
	cl.Session.OnRefreshed = m.reconnectStreams

	// AND KEEP THE TOKEN ALIVE, so the operator never meets an expiry at all.
	if m.keepalive != nil {
		m.keepalive.Stop()
		m.keepalive = nil
	}
	m.keepalive = cl.StartKeepalive(m.shellCtx, m.tokenLifetime(), m.reconnectStreams)
}

// tokenLifetime is the access token's TTL, as the server reported it at login/refresh.
func (m *App) tokenLifetime() time.Duration {
	if m.profile == nil || m.profile.TokenExpiresIn <= 0 {
		return 0
	}
	return time.Duration(m.profile.TokenExpiresIn) * time.Second
}

// reconnectStreams forces every live subscription to redial now.
func (m *App) reconnectStreams() { m.reg.ReconnectAll() }

// refreshComposerHint fills the composer's affordance row with the ACTIVE screen's shortcut list,
// so the guidance follows the page the operator is on (the screen's own HintLine is the single.
func (m *App) refreshComposerHint() {
	// THE STOP AFFORDANCE IS DERIVED, NOT STORED, and it is derived HERE — before the row is measured
	// below — so a hint that gains the stop segment also gains its row (the Lines() comparison.
	m.dock.SetReplyInFlight(m.chat != nil && m.chatConvID != "" && m.chat.IsStreaming(m.chatConvID))
	// THE RUN'S STOP IS DERIVED HERE TOO, and for the same reason: the composer must advertise the
	// chord that stops the thing the operator is looking.
	m.dock.SetStopRunHint(m.stopRunHint())
	m.dock.SetAbortRunHint(m.abortRunHint())
	ctx := ""
	// AN APP-LEVEL MODAL OWNS THE KEYBOARD TOO, so it must win over the screen's hint for the same
	// reason an in-screen form does — and it is checked FIRST.
	if m.findBox != nil {
		// THE FIND BOX OWNS THE KEYBOARD TOO, and it is checked FIRST because it is claimed FIRST in
		// dispatch: whatever takes the keys is what the row has to describe.
		ctx = findComposerHint
	} else if m.renameConv != nil || m.assignForm != nil || m.bulkConfirm != nil || m.catForm != nil {
		ctx = formComposerHint
	} else if s := m.screens[m.active]; s != nil {
		// AN OPEN FORM OWNS THE KEYBOARD, SO THE HINT MUST DESCRIBE THE FORM.
		if fo, ok := s.(interface{ FormOpen() bool }); ok && fo.FormOpen() {
			ctx = formComposerHint
		} else if h, ok := s.(interface{ HintLine() string }); ok {
			ctx = ansi.Strip(strings.TrimSpace(h.HintLine()))
		}
	}
	// THE RAIL'S CHORD LIST BELONGS HERE, NOT INSIDE THE RAIL PANE.
	if rail := m.railHintLine(); rail != "" {
		if ctx == "" {
			ctx = rail
		} else {
			ctx = rail + " · " + ctx
		}
	}
	// THE TRANSCRIPT'S OWN AFFORDANCE, and it belongs to the composer because that is where this client
	// advertises what the keys and the mouse can do.
	if m.active == TabAsk && m.chatConvID != "" && m.findBox == nil {
		// THE TRANSCRIPT'S FIND + JUMP KEYS, ADVERTISED — because a chord nobody can see is a chord nobody
		// has.
		for _, seg := range []string{transcriptFindHint, transcriptCopyHint} {
			if ctx == "" {
				ctx = seg
			} else {
				ctx += " · " + seg
			}
		}
	}
	// THE WIDTH CHORD, WHEREVER THERE IS A SPLIT TO RESIZE.
	if m.splitAdjustable() {
		if ctx == "" {
			ctx = splitResizeHint
		} else {
			ctx += " · " + splitResizeHint
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

// transcriptCopyHint is the composer's advertisement for the transcript's click gesture.
const transcriptCopyHint = "click your message to copy"

// transcriptFindHint is the composer's advertisement for the transcript's find + jump chords.
const transcriptFindHint = "ctrl+f: find · home/end: top·bottom"

// findComposerHint is what the composer advertises while the FIND BOX is up. It names only keys the box
// honours, so the row is true in that state — the rule formComposerHint follows.
const findComposerHint = "find: type the term · ↑/↓: step matches · enter: keep searching · esc: cancel"

// splitResizeHint advertises the width chord. Short because the affordance row wraps rather than scrolls, and
// phrased for what the operator wants ("resize panes") rather than for what the code calls it (a share).
const splitResizeHint = "ctrl+←/→: resize panes"

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

// activeContext is the context engine read: what the active screen is showing, as (tab, source,
// itemID, label).
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

// activeContextLabel renders the chip: "<SourceTitle>: <item>" (or the pinned override), with a
// running prefix when the selected execution is live.
func (m *App) activeContextLabel() string {
	if m.contextOverride != "" {
		return m.contextOverride
	}
	_, src, id, label := m.activeContext()
	if src == "" {
		return ""
	}
	// The Ask tab's conversations list IS the chat's own conversation: its title is self-referential
	// inside that conversation's own messages, so it is not context.
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

// contextPreamble builds the bracketed context line prepended to every outgoing chat message (the
// only context shape the Ask API accepts — plan fact 2).
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

// selectScreenItem selects an item by ID and loads its detail (arg jumps).
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

// OpenAskConversation makes the conversation the active chat target and loads its durable
// transcript (the ask screen's onDetail hook; live chunks then stream into the open pane).
func (m *App) OpenAskConversation(id string) tea.Cmd {
	if id == "" {
		return nil
	}
	if m.chatConvID == id {
		return nil
	}
	// THE SCOPE MODAL BELONGS TO ONE CONVERSATION: opening a different one closes it rather than
	// leaving a pane on screen that edits the previous chat's servers (the GUI's Scope disclosure
	// closes.
	m.closeScopeModal()
	m.askMode = askConversations
	m.chatConvID = id
	// OPENING A CONVERSATION SELECTS IT FOR THE KEYBOARD.
	m.askPane = askPaneConversation
	m.syncAskPaneFocus()
	// THE HIGHLIGHT MUST FOLLOW WHAT WAS JUST OPENED.
	m.railSyncSelectionToOpenConversation()
	m.chat.SetActive(id)
	// DECLARE THE PANE'S CONTENT.
	if s := m.screens[TabAsk]; s != nil {
		if sid, ok := s.(interface{ SetDetailID(string) }); ok {
			sid.SetDetailID(id)
		}
	}
	// The right rail appears with a conversation, which SHRINKS contentWidth() — and every pane's width
	// is assigned from it in refreshLayout, which is only otherwise run on a window resize.
	m.refreshLayout()
	// RE-ATTACH TO A TURN THAT IS STILL RUNNING SERVER-SIDE.
	re := m.reattachRunningTurn(id)
	// A conversation switch changes whose usage the strip reports, so re-read it
	// (the previous conversation's numbers must never linger under a new chat).
	return tea.Batch(m.chat.OpenConversation(id), m.refreshMetrics(), re)
}

// reattachRunningTurn restores the stream slot for a conversation whose turn the server still
// reports as running, when this client has no live stream for it.
func (m *App) reattachRunningTurn(convID string) tea.Cmd {
	if m.chat == nil {
		return nil
	}
	// DISCOVERY IS UNCONDITIONAL, the re-attach is not.
	discover := m.chat.DiscoverPendingAsks(convID)
	c, ok := m.conversationByID(convID)
	if !ok || !c.TurnInFly || c.PendingReplyID == "" {
		return discover
	}
	return tea.Batch(discover, m.chat.Reattach(convID, c.PendingReplyID))
}

// onExecutionSessionLoaded paints the merged session view into the execution detail pane: durable
// GetExecutionSession parts + live StreamExecutionEvents (MergeSessionItems drops covered chunks.
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
		// The server returns the tail NEWEST-first (seq DESC); the transcript reads oldest-first, so it is
		// reversed here rather than at every render (chat.MergeSessionItems sorts.
		parts := resp.Msg.GetParts()
		chronological := make([]*apiv1.ExecutionSessionPart, len(parts))
		for i, p := range parts {
			chronological[len(parts)-1-i] = p
		}
		return execSessionMsg{execID: execID, parts: chronological}
	}
}

// sessionTailFromSeq is the "from the end" sentinel for GetExecutionSession's before_seq: every
// part whose seq is below this, i.e. all of them, newest-first.
const sessionTailFromSeq = int64(^uint64(0) >> 1)

// sessionPartLimit bounds how much of a transcript is fetched.
const sessionPartLimit = 10000

// execSessionMsg carries the durable session parts for one execution.
type execSessionMsg struct {
	execID string
	parts  []*apiv1.ExecutionSessionPart
	err    error
}

// navEntries returns (building once) the tab's dropdown rows from the screen inventory
// (buildNavEntries over Sources()) — the same no-drift source as the slash registry, per
// docs/tui-parity.md.
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

// refreshStreamStatus updates the footer AND surfaces the underlying reason a stream is down, once
// per distinct message.
func (m *App) refreshStreamStatus() {
	m.footer.StreamStatus = m.streamStatus()
	m.surfaceStreamError()
}

// surfaceStreamError pushes the active screen's stream error into the dock's error strip whenever
// the footer is not "open".
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
		text := st.Name + ": " + errText
		// A BARE "connection refused" IS THE FAILURE THIS FEATURE WAS FILED FOR.
		if m.plane.State == planeStateDown {
			text += " — " + planeRemedy(m.plane)
		}
		m.dock.SetError(text)
		return
	}
}

// streamStatus derives the footer state from the active screen's declared subscriptions.
func (m *App) streamStatus() streamStatusString {
	// This used to aggregate ONLY over the statuses the active screen declares — and a screen that
	// declares none reports nothing, so the worst-wins loop started and ended at "open".
	worst := openStatus
	if st, _ := m.reg.WorstStatus(); st != "" {
		worst = streamStatusString(st)
	}
	s := m.screens[m.active]
	if s == nil {
		return worst
	}
	rp, ok := s.(screenkit.StatusReporter)
	if !ok {
		return worst
	}
	// The screen's own report can still RAISE the severity (a stream it holds that the registry has not
	// seen), which is why it is folded in rather than dropped.
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
	cmds = append(cmds, m.waitChat(), m.chat.LoadConversations(), m.loadRailProjects(), m.fetchAskDefaultModel())
	// The category list is loaded ONCE at startup alongside everything else, so the first assignment
	// the operator makes already has its picker populated.
	if c := m.loadCategories(); c != nil {
		cmds = append(cmds, c)
	}
	// ARM THE ROLLING REFRESH WINDOW (refresh.go).
	cmds = append(cmds, refreshCmd(m.refreshGen))
	// THE LAUNCH CHECK — one question, and only when this shell may ask it and the directory is
	// unattached.
	if m.launchDir != "" && m.launchPromptArmed {
		cmds = append(cmds, m.checkLaunchProject())
	}
	if c := m.drainStaged(); c != nil {
		cmds = append(cmds, c)
	}
	// THE SIZE PROBE'S CHAIN (resize_probe.go). Nil wherever the platform delivers its own resize events,
	// which is every platform but Windows — so this appends nothing on Linux/macOS.
	if c := sizeProbeTickCmd(); c != nil {
		cmds = append(cmds, c)
	}
	// TELL THE OPERATOR IF THEIR CONFIGURED NEWLINE CHORD COULD NOT BE USED (applyProfile).
	if m.deadNewlineChord != "" {
		m.dock.SetNotice(m.deadNewlineChord)
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
func (m App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.dispatch(msg)
	// The CONTENT WIDTH is not a pure function of a resize: railVisible() depends on STATE (askMode /
	// chatConvID), so the conversations rail appearing shrinks the content area.
	next.syncLayoutWidth()
	// The composer's caret BLINK STARTER (captured when the dock took focus) has to reach the runtime,
	// and focus is taken from several places — ctrl+g, a click, keyboard navigation.
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

// passToScreen forwards to the active screen.
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
	// The Ask screen's detail pane is the transcript surface: whenever it re-renders (e.g. a
	// conversation detail just loaded — including from a rail click), relay the merged transcript into
	// it.
	if m.active == TabAsk {
		m.onChatWake()
	}
	m.updateContextChip()
	m.refreshComposerHint()
	m.footer.StreamStatus = m.streamStatus()
	m.footer.Width = m.width
	// footer.ComposerFocus is NOT recomputed here: it is derived in setFocus, the one place focus
	// changes (see setFocus).
	return m, cmd
}

// diffRailOwnsX reports whether a terminal column belongs to the diff rail.
func (m *App) diffRailOwnsX(x int) bool {
	return m.diffOpen && m.diffPane != nil && x < m.diffPaneWidth()
}

// diffScrollbarHit reports whether a terminal (x, y) is on the diff rail's SCROLLBAR — the cell the
// operator grabs to jump or drag the viewport.
func (m *App) diffScrollbarHit(x, y int) bool {
	return m.diffOpen && m.diffPane != nil && m.diffPane.ScrollbarHit(x, y)
}

// diffMsg forwards a pane-scoped message to the diff pane's Update and reports whether the pane
// consumed it.
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
		// Global routes already consumed d/esc/y (and ctrl chords).
		switch msg.String() {
		case "h", "l", "j", "k", "up", "down", "pgup", "pgdown", "g", "G":
			cmd := m.diffPane.Update(msg)
			// The pane's tab/selection may have changed (tab-switch keys h/l, or scroll state); copy it back so
			// the shell-owned state stays in sync and survives a later SwitchTo (the pane must not reset.
			m.syncDiffPaneState()
			return true, cmd
		}
		return false, nil
	case tea.MouseMsg:
		// Forward clicks/wheel that land in the left pane rail (x < the pane width).
		if m.diffRailOwnsX(msg.X) {
			cmd := m.diffPane.Update(msg)
			// A click may have hit the pane's ✕ close button (the mouse toggle area).
			if m.diffPane.TakeCloseRequest() {
				m.closeDiffPane()
				m.syncDiffPaneState()
				return true, cmd
			}
			// A click may switch the pane's tab or select a file; copy the new tab/selection back to the
			// shell-owned state so it persists across navigation.
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
	// ONE opaque full-viewport frame is painted FIRST on every path (baseView); every overlay is a
	// layer INSIDE that frame — never an inline drawing over the terminal (Phase 3 overlay discipline).
	if m.launch != nil {
		return m.launchView(w, h)
	}
	base := m.baseView(w, h)
	if m.help.open {
		overlay := m.help.view(m.routes) + "\n" + strings.Join(m.slash.helpLines(), "\n")
		return fillView(m.overlayCentered(base, overlay), w, h)
	}
	if m.palette.connectOpen {
		// SOLID, LIKE EVERY OTHER MODAL.
		return fillView(m.overlayCentered(base, m.modalPanelFittingContent(m.connectOverlayView())), w, h)
	}
	if m.palette.PaletteOpen() {
		base = m.paletteComposerView(base)
	}
	base = m.composeView(base)
	// The /models picker is spliced LAST (above every other layer).
	if m.modelPicker != nil {
		m.modelPicker.SetScreen(w, h)
		base = m.overlayCentered(base, m.modelPicker.View())
	}
	// The rename modal goes in the same layer.
	if m.renameConv != nil {
		base = m.renameConvView(base, w, h)
	}
	// THE MESSAGE EDIT MENU, drawn above the transcript it edits and centred by the same compositor
	// every other overlay uses — so it cannot change the frame's row count.
	if m.msgMenu != nil {
		base = m.overlayCentered(base, m.msgMenuView())
	}
	// THE CONVERSATION SCOPE MODAL IS DRAWN FIRST OF THE TWO, so a form raised FROM it lands ON TOP.
	if m.scope != nil {
		base = m.scopeView(base, w, h)
	}
	if m.convScopeForm != nil {
		base = m.convScopeView(base, w, h)
	}
	if m.assignForm != nil {
		base = m.assignCategoryView(base, w, h)
	}
	// The /project picker sits in the same layer: the shell owns the surface it names, and it must cover the
	// rail whose scope it is changing.
	if m.projectPick != nil {
		base = m.overlayCentered(base, m.projectPickerView())
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
	// THE FIND BOX IS COMPOSED LAST — over every other modal — because it is CLAIMED FIRST in dispatch.
	if m.findBox != nil {
		base = m.findBoxView(base, w, h)
	}
	return fillView(base, w, h)
}

// baseView paints the opaque full-viewport shell grid: the centered tab bar, the underline rule,
// one separator row, the active screen block, the composer dock, and the one-line footer.
func (m App) baseView(w, h int) string {
	cw := m.contentWidth()
	screenRows := m.contentHeight()
	dockRows := m.dock.Lines()
	if screenRows < 1 {
		screenRows = 1
	}
	// Center column: the active screen block + the dock block beneath it, each normalized to its budget
	// row count (padded with the theme background when short, truncated when over.
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
		pane := strings.Join(normalizeBlock(m.diffPane.View(), m.diffPaneWidth(), screenRows+dockRows), "\n")
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

// welcomeMode reports whether to render the centered launch layout: the Ask tab showing "New" with
// no conversation open.
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

// brandGlyphs is a 5-row block font.
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

// welcomeBrand is "ORCHICON" composed from brandGlyphs: each glyph padded to brandGlyphW and
// separated by one space, so every row is the same width by construction.
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

// welcomeComposer is WHERE the launch page's centered composer box is DRAWN, in FRAME coordinates.
//
// It exists because the launch page is the one layout where the composer is NOT at the bottom of the
// frame: it is centered inside the body region and narrower than the layout width. Two things need to
// know that geometry — the paint, and a click that has to be converted from frame coordinates into the
// dock's own — and the operator's report is exactly what happens when only one of them knows it:
//
//	"Clicking into the composer at a specific coordinate should move the cursor to that coordinate.
//	 This WAS working but is no longer working again."
//
// It works when the composer is DOCKED (composerTopRow is the true row) and not on the launch page,
// where composerTopRow() pointed at a row the box is not on and the click missed it entirely.
//
// ONE computation, read by both (see welcomeLayout), so a change to the centering cannot desync the
// caret from the box an operator clicked.
type welcomeComposer struct {
	// Top is the frame row of the box's FIRST line (its top border); Left is its first CELL.
	// Width is the box's width and Rows how many rows it spans.
	Top, Left, Width, Rows int
}

// contains reports whether a frame coordinate lands inside the drawn box.
func (g welcomeComposer) contains(x, y int) bool {
	return x >= g.Left && x < g.Left+g.Width && y >= g.Top && y < g.Top+g.Rows
}

// bodyTopRow is the frame row of the body region's FIRST row.
func bodyTopRow() int { return tabBarRows + 1 }

// welcomeLayout builds the launch block — the large brand lockup, the composer box, and the tagline
// — vertically centered in the w×h body region.
func (m App) welcomeLayout(w, h int) ([]string, welcomeComposer) {
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

	// The wordmark falls back to a plain bold title on terminals too narrow for the block letters (they
	// are ~63 cells wide), so the launch screen never wraps or truncates mid-glyph.
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
	blockRow := len(block) // the box's first row WITHIN the block
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
	// The box's LEFT edge.
	boxLeft := (w - boxW) / 2
	if boxLeft < 0 {
		boxLeft = 0
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
	return out, welcomeComposer{
		Top:   bodyTopRow() + top + blockRow,
		Left:  boxLeft,
		Width: boxW,
		Rows:  len(boxLines),
	}
}

// centeredWelcomeView renders the centered launch block (see welcomeLayout, which owns the layout).
func (m App) centeredWelcomeView(w, h int) []string {
	rows, _ := m.welcomeLayout(w, h)
	return rows
}

// safeView renders the active screen, tolerating a nil screen (the shell
// is briefly screenless before the first SwitchTo).
func safeView(s Screen) string {
	if s == nil {
		return theme.HintText.Render("select an area")
	}
	return s.View()
}

// normalizeBlock pins a rendered block to exactly h rows × w columns: over-long rows are ANSI-aware
// truncated, short rows background-padded (the padding cells are painted by fillView's ScreenBg.
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

// normalizeBlockKeepTail is normalizeBlock for blocks whose LAST row is the load-bearing one (the
// dock: chip/notice strips sit above the input line).
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

// fillView pins content to the exact viewport: every rendered line is padded to w cells (truncated
// on overflow) and the whole grid is filled to exactly h rows.
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
func bgOpaque(l string) string {
	const reset = "\x1b[0m"
	bg := theme.ScreenBg
	// The re-assert sequence: the same SGR ScreenBg emits, minus its own trailing reset.
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
	// The foreground repair sequence, derived the same way so it always matches the active theme and
	// colour profile.
	fgPaint := lipgloss.NewStyle().Foreground(theme.Text).Render("")
	fgOpen := ""
	if strings.HasSuffix(fgPaint, reset) {
		fgOpen = strings.TrimSuffix(fgPaint, reset)
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
		// Re-assert the FOREGROUND and the BACKGROUND unless the next SGR establishes them.
		//
		// This used to skip the repair whenever ANY SGR followed, on the reasoning that "its own
		// sequence will establish state". That is only true of a sequence that sets the same
		// ATTRIBUTE — and almost all of them set only a foreground:
		//
		//	...\x1b[0m\x1b[38;2;157;171;190mhttps://…\x1b[0m
		//	    └ reset; the next SGR is fg-only, so the BACKGROUND stayed cleared for the whole URL
		//
		// Two different symptoms came out of that one omission:
		//
		//   - a cell with no BACKGROUND shows the terminal's own — the "black background box" on the
		//     footer, "the black around the adapter and provider selection", and the black blocks at
		//     the end of a line in an execution (which is also why the clipboard copy returned spaces
		//     and a bare │: those cells hold no glyphs);
		//   - a cell with no FOREGROUND shows the terminal's default, which is the "terminal font
		//     color coming through in various areas" — green on this operator's terminal, and
		//     unreadable on a light theme. The markdown renderer is attribute-only BY DESIGN (it
		//     composes with a host that supplies the colours) and the detail pane's viewport supplies
		//     NONE, so every markdown body and every raw output block rendered in the terminal's
		//     colour.
		//
		// Repairing both here is the general fix: no cell of the frame is left to the terminal.
		if !sgrSetsForeground(rest) {
			b.WriteString(fgOpen)
		}
		if !sgrSetsBackground(rest) {
			b.WriteString(open)
		}
		l = rest
	}
}

// sgrSetsBackground reports whether rest begins with an SGR sequence that establishes a BACKGROUND.
func sgrSetsBackground(rest string) bool {
	return sgrHasParam(rest, "48", "7")
}

// sgrSetsForeground is the same test for the foreground: 38;… sets a colour and 7 swaps the pair.
// 39 (the default-foreground reset) does NOT count — it is the very thing being repaired.
func sgrSetsForeground(rest string) bool {
	return sgrHasParam(rest, "38", "7")
}

// sgrHasParam reports whether rest begins with an SGR sequence carrying any of the given parameters.
func sgrHasParam(rest string, params ...string) bool {
	if !strings.HasPrefix(rest, "\x1b[") {
		return false
	}
	end := strings.IndexByte(rest, 'm')
	if end < 0 {
		return false
	}
	for _, p := range strings.Split(rest[2:end], ";") {
		for _, want := range params {
			if p == want {
				return true
			}
		}
	}
	return false
}

// padScreenLine renders one row at exactly w cells: overlong lines are ANSI-aware truncated, short
// lines background-padded.
func padScreenLine(l string, w int) string {
	cols := lipgloss.Width(l)
	if cols > w {
		l = ansi.Truncate(l, w, "")
		cols = w
	}
	if cols < w {
		l += strings.Repeat(" ", w-cols)
	}
	// BOTH attributes, because each is independently lost: an inner style ends with a reset and the
	// next one frequently restores only the foreground, which is how the background went missing.
	return bgOpaque(screenBase().Render(l))
}

// screenBase is the style every row of the frame is wrapped in: the theme's body text on the
// theme's screen background.
func screenBase() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(theme.Text).Background(theme.Bg)
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

// waitChat arms the shell's chat waiter: one tea.Cmd parked on the wake/cmd channels, returning
// whichever fires first.
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

// appEventStore adapts the App into chat.EventStore (live chunks land in the per-conversation item
// list).
type appEventStore struct{ m *App }

func (s appEventStore) AppendLiveItem(convID string, item chat.ChatItem) {
	s.m.chatStore.append(convID, item)
	s.pokeChat()
}

func (s appEventStore) SetReconnecting(convID string, on bool) {
	// The dock mutates on the tea loop (onChatWake) — this goroutine only records state + pokes
	// (bubbletea's value-model copies make a direct dock write here invisible).
	s.m.chatStore.setReconnecting(convID, on)
	s.pokeChat()
}

// ShowConsentAsk puts a pending ask's card into the conversation's transcript, from the STREAM
// goroutine. ONLY POINTER AND CHANNEL FIELDS ARE TOUCHED.
func (s appEventStore) ShowConsentAsk(ask chat.PermissionAsk) {
	if ask.ConvID == "" {
		// The controller always stamps the turn's conversation (see the PermissionAsk wire arm), so an
		// empty id here is a locally built ask and there is no live shell to fall back to.
		return
	}
	s.m.chatStore.drawConsentAsk(ask.ConvID, ask)
	s.pokeChat()
}

// SettleConsentAsk records an ask that was settled SOMEWHERE ELSE, so this client's card stops being a
// choice. Straight to the store, for the same reason: a dropped resolution left a live-looking, inert card.
func (s appEventStore) SettleConsentAsk(convID, askID, outcome, answer string) {
	s.m.chatStore.settleAsk(convID, askID, outcome, answer)
	s.pokeChat()
}

// ShowDiscoveredAsks draws the open asks the server just reported. IT ONLY EVER ADDS.
//
// THIS IS THE GUI'S RULE, and adopting it verbatim is the fix. The GUI's discovery effect is:
//
//	if (!pendingAsks || pendingAsks.length === 0) return;
//	for (const ask of pendingAsks) applyAsk(activeConvId, ask);
//
// It adds what the server lists and never retires anything because the list did not mention it.
//
// WHAT THE TUI DID INSTEAD, and why it flickered. This method used to settle every pending card whose id
// was ABSENT from the server's answer. Absence is not evidence: the answer is a point-in-time read over a
// poll, and an empty or partial one (a read that raced the raise, a conversation the server was still
// indexing, a poll that landed between the persist and the registry write) retired LIVE cards. The next
// poll then drew them again — a settle → redraw cycle at poll cadence, which is exactly the flicker the
// operator reported, and which the GUI never showed because it cannot settle this way at all. The
// operator's own observation localised it: "Looks like just in the TUI."
//
// SO WHAT STILL SETTLES A CARD? Only positive evidence, never absence:
//   - a RECORDED OUTCOME in the durable page (onTranscript → TranscriptMsg.SettledAsks), which is what
//     survives a reload and covers an expiry at finalize and a decision taken in the other client;
//   - a DECISION THIS CLIENT MADE (SettleConsentAsk, from the resolve event or its own reply).
//
// The other direction is the safe one: a card the server has genuinely dropped but that no outcome record
// covers stays answerable — a stale card the operator can still click, which resolves to "already answered"
// on the server, rather than a live card that vanished. A dead card that can be dismissed is a far smaller
// harm than a live turn parked behind a card nobody can see.
//
// drawConsentAsk is idempotent by ask id and re-arms a card settled in error, so drawing on every poll is
// correct and never duplicates. One poke, after every draw, so no repaint can observe a half-applied state.
func (s appEventStore) ShowDiscoveredAsks(convID string, asks []chat.PermissionAsk) {
	for _, ask := range asks {
		if ask.ConvID == "" {
			ask.ConvID = convID
		}
		s.m.chatStore.drawConsentAsk(ask.ConvID, ask)
	}
	s.pokeChat()
}

// pokeChat wakes the tea loop for a repaint.
func (s appEventStore) pokeChat() {
	select {
	case s.m.chatWake <- struct{}{}:
	default:
	}
}

// onConversations ingests the loaded conversation list for the Ask screen's CONVERSATIONS rail.
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
	// THE LIST'S OWN GROUPINGS COME WITH IT.
	m.applyCategorySet(apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION, msg.Categories, msg.Assignments)
	// AND THE PROJECT LIST, IF IT NEVER LANDED.
	m.conversations = msg.Convs
	// The list is the ONLY place a conversation created in the other client becomes known, and the
	// scope's own notice is written when the scope is SET — so without this.
	m.noteScopeHidesNewRows()
	if !m.railProjectsLoaded && m.clients != nil {
		// The composer strip is pushed from the list it derives from (AC 9): the
		// mode pill reads currentModeLabel(), which reads THIS list.
		m.syncComposerStats()
		return tea.Batch(m.onChatWake(), m.loadRailProjects(), m.waitChat(), m.reattachFromRail())
	}
	if m.convSel >= len(m.railRows()) {
		m.convSel = 0
	}
	if m.convScroll > len(m.conversations) {
		m.convScroll = 0
	}
	// Marks are reconciled against the new list: a reload is the honest reconciliation for a bulk
	// delete, and a mark on a conversation that no longer exists would overstate the selection and aim.
	m.pruneConvMarks()
	// THE COMPOSER STRIP IS PUSHED FROM ITS OWN SOURCE (AC 9).
	m.syncComposerStats()
	// The detail header reads its title + message count out of THIS list, so a refresh has to repaint
	// the open pane or the new values sit unrendered until the next unrelated wake.
	wake := m.onChatWake()
	// AND ATTACH TO A TURN THAT STARTED WHILE WE WERE LOOKING — see reattachFromRail.
	re := m.reattachFromRail()
	// The MODEL + STATS fields derive from m.metrics, which a rail reload does NOT refresh: when the
	// open conversation's row now names a different model (a `/model` set in the other client).
	if m.composerModelDiverged() {
		return tea.Batch(wake, m.refreshMetrics(), re)
	}
	return tea.Batch(wake, re)
}

// reattachFromRail attaches this client to a turn the rail reports as running on the OPEN conversation, when no
// live local stream owns it. Nil when there is nothing to attach to.
//
// THE GAP IT CLOSES, reported twice by the operator and structural rather than a flake:
//
//	"the permission ask card pops up in the GUI but it doesn't pop up in the TUI. It just sits at
//	 'orchicon is thinking'. ... It's not consistent in the TUI."
//
// A turn's asks ride THAT TURN'S stream, and asks are STREAM-ONLY — the transcript records an ask's OUTCOME,
// never the open ask, so "a permission ask has no durable per-ask row to reconcile against" (see
// chat/controller.go). This client opened a stream in exactly two situations: when the TUI ITSELF sent a
// message, and when a conversation was OPENED whose rail row said the turn was in flight. So a turn started
// anywhere else — from the GUI, or by an interjection made in the other client — while the TUI sat on that very
// conversation had NO stream here, and therefore no way to receive its cards. The GUI showed the card; this
// client showed nothing and could not poll its way back, because there is nothing durable to poll.
//
// WHY THE RAIL IS THE RIGHT TRIGGER. It is the freshest server-side view the shell holds, it carries
// turn_in_flight and the pending assistant id, and it is ALREADY reloaded every 5s by the rolling refresh
// window — so this needs no new poll, no new RPC and no new subscription. The server half is already built:
// WatchTurnStream re-emits every still-OPEN ask to a late subscriber (chat.go), which is what makes attaching
// late sufficient, and the controller dedupes by ask id so a replay cannot draw a second card.
//
// IT IS SAFE TO CALL ON EVERY RELOAD. Reattach refuses when a live local stream already owns the slot, so a
// turn started in THIS client keeps its own stream and only a gap is ever filled; and a row with no turn in
// flight (or no pending id) returns nil.
func (m *App) reattachFromRail() tea.Cmd {
	return m.reattachRunningTurn(m.chatConvID)
}

// reloadConversations re-fetches the conversations rail from the live API
// (the rail's explicit retry path). Safe to call on the tea loop.
func (m *App) reloadConversations() tea.Cmd {
	if m.chat == nil {
		return nil
	}
	m.convLoading = true
	return tea.Batch(m.chat.LoadConversations(), m.loadRailProjects())
}

// drainRailCmd returns (once) the staged conversations-rail reload cmd.
func (m *App) drainRailCmd() tea.Cmd {
	c := m.pendingRailCmd
	m.pendingRailCmd = nil
	return c
}

// inDockRows reports whether an absolute terminal row is inside the dock (composer) block — the
// chip/notice strip(s) and the input line.
func (m *App) inDockRows(y int) bool {
	dockRows := m.dock.Lines()
	return y >= m.height-1-dockRows && y <= m.height-2
}

// chatStore guards the per-conversation live item lists (the stream goroutine appends; the tea loop
// reads).
type chatStore struct {
	mu           sync.Mutex
	items        map[string][]chat.ChatItem
	reconnecting map[string]bool
	// WHOLESALE SET, NEVER APPEND.
	toolCalls map[string][]toolclass.Call
	// orderAt is the ARRIVAL ANCHOR map: convID -> MESSAGE key -> the ordering timestamp every item of
	// that message must sort by, overriding the timestamp each item was built with.
	orderAt map[string]map[string]int64
}

func (s *chatStore) append(convID string, item chat.ChatItem) {
	s.mu.Lock()
	s.items[convID] = append(s.items[convID], item)
	s.mu.Unlock()
}

// consentState finds a pending ask's live state by id.
func (s *chatStore) consentState(convID, askID string) *chat.ConsentState {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items[convID] {
		it := &s.items[convID][i]
		if it.Kind == chat.KindConsent && it.AskID == askID {
			return it.Consent
		}
	}
	return nil
}

// hasPendingConsent reports whether the conversation carries an unresolved ask.
func (s *chatStore) hasPendingConsent(convID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range s.items[convID] {
		if it.Kind == chat.KindConsent && it.Consent != nil && it.Consent.Pending() {
			return true
		}
	}
	return false
}

// drawConsentAsk puts a pending ask's card into the conversation's transcript, and reports whether it drew
// one (false = this conversation already holds a card for that ask).
//
// IT IS THE ONE PLACE A CARD ENTERS THE TRANSCRIPT, called by both the shell (App.ShowConsentAsk, from the tea
// loop) and the stream (appEventStore.ShowConsentAsk, from the turn's goroutine), so the two paths cannot
// disagree about the ask's identity, about the conversation it lands in, or about drawing it twice.
//
// A CARD THE SERVER RAISED IS ALWAYS DRAWN. This used to be suppressed by the shell's own grant mirror, and
// the suppression could only ever swallow a decision the operator needed to make:
//
//   - the server consults its grant store BEFORE it raises an ask (permpolicy's SessionGranted input), so an
//     ask that arrives is one the server genuinely has no consent for. A client that mutes it does not remove
//     a question — it removes the ANSWER, and the turn parks with nothing on screen to act on. The operator,
//     watching both clients: "the permission ask card pops up in the GUI but it doesn't pop up in the TUI. It
//     just sits at 'orchicon is thinking'."
//   - the mirror's inputs were all unreliable: the server's grants are IN MEMORY and are dropped by every plane
//     restart (the prod plane restarted 14 times in the two days this was measured); a decision this client
//     sent may have been REFUSED (Applied=false, Expired=true) while the card was still settled locally; and
//     the server matches a grant over its whole SUBTREE while the mirror matched the directory EXACTLY.
//
// A duplicate card is answerable; a missing one hangs the turn. Where the mirror used to silence a repeat, the
// SERVER does it correctly — it will not raise the ask again once the grant is applied.
//
// Idempotence is kept for the SAME ask, which is not hypothetical: a dropped socket re-dials through
// WatchTurnStream, whose replay re-emits every pending ask (chat.go), so without this the card the operator
// already has would be drawn a second time.
func (s *chatStore) drawConsentAsk(convID string, ask chat.PermissionAsk) bool {
	if convID == "" {
		return false
	}
	if ask.ID == "" {
		ask.ID = fmt.Sprintf("ask-%d", time.Now().UnixNano())
	}
	if s.hasConsent(convID, ask.ID) {
		// A HELD ID IS NOT ALWAYS A HELD CHOICE.
		if s.rearmSettledConsent(convID, ask.ID) {
			return true
		}
		return false
	}
	// Stamped NOW, so the card sorts to the END of the transcript and stays there. Without a timestamp it
	// sorted to the top on the next poll — see ConsentItem.
	s.append(convID, chat.ConsentItem(ask, time.Now().UnixMilli()))
	return true
}

// rearmSettledConsent turns a SETTLED card for this ask back into a pending choice, and reports
// whether it found one to re-arm.
func (s *chatStore) rearmSettledConsent(convID, askID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items[convID] {
		it := &s.items[convID][i]
		if it.Kind != chat.KindConsent || it.AskID != askID || it.Consent == nil {
			continue
		}
		if it.Consent.Pending() {
			return false
		}
		it.Consent.Decision = ""
		it.Consent.Choice = ""
		it.Consent.Note = ""
		it.Consent.OtherMode = false
		return true
	}
	return false
}

// hasConsent reports whether this conversation already holds a card for the ask.
func (s *chatStore) hasConsent(convID, askID string) bool {
	if askID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range s.items[convID] {
		if it.Kind == chat.KindConsent && it.AskID == askID {
			return true
		}
	}
	return false
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

// snapshotToolCalls returns the conversation's durable tool calls as of the last transcript page.
func (s *chatStore) snapshotToolCalls(convID string) []toolclass.Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.toolCalls[convID]
}

// setToolCalls replaces the conversation's durable tool calls with the page just loaded (see the field
// comment: wholesale, never appended).
func (s *chatStore) setToolCalls(convID string, calls []toolclass.Call) {
	s.mu.Lock()
	if s.toolCalls == nil {
		s.toolCalls = map[string][]toolclass.Call{}
	}
	s.toolCalls[convID] = calls
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
//
// IT MUST ALSO BE IDEMPOTENT, BECAUSE IT IS CALLED REPEATEDLY ON THE SAME BUFFER, and that is
// what this function got wrong. The merge writes its result back into the live store, so after
// ONE pass the buffer already contains the durable rows. A second pass then appended the
// transcript to a buffer that already had it:
//
//	merge 1 -> 1 item    (history only; the echo was dropped)
//	merge 2 -> 2 items   (the same durable row twice)
//	merge 3 -> 3 items   (the operator's screenshot)
//
// It compounds because the merge runs on EVERY mid-turn transcript load, and a load happens on
// each re-entry into the conversation — which is the exact shape of the report: "when I click out
// of an ongoing session and back into it, it duplicated the user message in the stream". Once per
// click, and every durable row was affected, not just the user's (the reasoning parts, now that
// they are durable rows, duplicated with it).
//
// The fix is to dedupe by IDENTITY rather than only by text: anything the incoming transcript
// already carries — matched on its key, which the server derives from the message id
// ("m-<id>", "m-<id>-r<part>") — is not live, whatever else it looks like.
func (s *chatStore) mergeHistory(convID string, history []chat.ChatItem) {
	s.mu.Lock()
	live := s.items[convID]
	// The durable transcript's identity: the keys already present, plus the user texts an
	// optimistic echo has to be matched against.
	already := make(map[string]bool, len(history))
	var durableUser []string
	// THE DURABLE MIRROR OF THE IN-FLIGHT REPLY, if it has one. See below for why this is the thing that
	// stops the reply rendering twice.
	var mirrorText, mirrorReasoning string
	for _, it := range history {
		if it.Key != "" {
			already[it.Key] = true
		}
		if it.Kind == chat.KindUser {
			durableUser = append(durableUser, it.Text)
		}
		// The assistant row's key is "m-<messageID>"; the per-part reasoning keys are "m-<id>-r<j>". The
		// LONGEST text for the message is what the mirror currently holds.
		if it.Kind == chat.KindText && strings.HasPrefix(it.Key, "m-") && !strings.Contains(it.Key, "-r") {
			if len(it.Text) > len(mirrorText) {
				mirrorText = it.Text
			}
		}
		if it.Kind == chat.KindReasoning && strings.HasPrefix(it.Key, "m-") {
			if len(it.Text) > len(mirrorReasoning) {
				mirrorReasoning = it.Text
			}
		}
	}
	kept := make([]chat.ChatItem, 0, len(live))
	for _, it := range live {
		// ALREADY MERGED: this row came from a previous pass, so adding it again IS the duplication.
		if it.Key != "" && already[it.Key] {
			continue
		}
		// THE OPTIMISTIC ECHO: not yet durable, and superseded by the incoming copy of the same
		// words. Its key is generated locally, so it is never in `already`.
		if it.Kind == chat.KindUser && strings.HasPrefix(it.Key, "draft-") && matchesAny(durableUser, it.Text) {
			continue
		}
		// THE LIVE HALF OF A REPLY THAT IS ALSO DURABLE — the duplication this function still had, and why
		// a mid-turn poll showed the reply twice.
		if (it.Kind == chat.KindText || it.Kind == chat.KindReasoning) && it.Live {
			mirror := mirrorText
			if it.Kind == chat.KindReasoning {
				mirror = mirrorReasoning
			}
			if mirror != "" && strings.Contains(mirror, it.Text) {
				continue // the durable mirror already carries this text
			}
		}
		kept = append(kept, it)
	}
	// INTERLEAVE by timestamp — never "history then live".
	s.anchorGrowingMessages(convID, history)
	merged := append(append([]chat.ChatItem{}, history...), kept...)
	s.applyOrderAnchors(convID, merged)
	chat.SortChronologically(merged)
	// AND LAST, THE PROMPT. A pending card is moved after its own history so it cannot be pushed off the
	// top of the pane by a growing reply — see chat.PendingCardsLast for the reported failure.
	merged = chat.PendingCardsLast(merged)
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

// replace swaps the conversation's items for the durable transcript — the completion authority (GUI
// semantics: the poll replaces the live buffer once the turn resolves, so nothing renders twice).
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
		// A PENDING consent card is LIVE-ONLY, so the durable view can never carry it and this loop is its
		// only chance to survive.
		if it.Kind == chat.KindConsent && it.Consent != nil && it.Consent.Pending() {
			out = append(out, it)
			continue
		}
		// A SETTLED card is kept as its own RECORD — "SETTLED, NOT REMOVED" (see settleStaleConsent).
		if it.Kind == chat.KindConsent && it.Consent != nil && !durableRendersAsk(items, it) {
			out = append(out, it)
		}
	}
	// The anchors survive the replace: without applying them here the in-flight reply would JUMP back
	// up to its row-creation time the moment the turn completed.
	s.anchorGrowingMessages(convID, items)
	s.applyOrderAnchors(convID, out)
	if len(out) != len(items) || len(s.orderAt[convID]) > 0 {
		chat.SortChronologically(out)
	}
	// The same rule on the completion path, or a card that survived the replace would be ordered back
	// above the reply the moment the turn's poll landed.
	out = chat.PendingCardsLast(out)
	s.items[convID] = out
	s.mu.Unlock()
}

// durableRendersAsk reports whether the durable page already renders this live consent card's ask,
// so a retained settled copy would draw it TWICE. ONLY A QUESTION HAS A DURABLE TWIN.
func durableRendersAsk(items []chat.ChatItem, live chat.ChatItem) bool {
	if live.Consent == nil || live.Consent.Ask.Kind != chat.AskQuestion {
		return false
	}
	q := strings.TrimSpace(live.Consent.Ask.Question)
	if q == "" {
		return false
	}
	for _, it := range items {
		if it.Kind == chat.KindAsk && it.Ask != nil && strings.TrimSpace(it.Ask.Question) == q {
			return true
		}
	}
	return false
}

// settleAsk records a decision that was made SOMEWHERE ELSE, so this client's copy of the card
// stops being a choice.
func (s *chatStore) settleAsk(convID, askID, outcome, answer string) {
	if s == nil || askID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items[convID] {
		it := &s.items[convID][i]
		if it.Kind != chat.KindConsent || it.AskID != askID || it.Consent == nil {
			continue
		}
		it.Consent.Decision = consentDecisionFromOutcome(outcome)
		it.Consent.Choice = answer
		it.Consent.Note = ""
		it.Consent.OtherMode = false
		return
	}
}

// consentDecisionFromOutcome maps the wire's outcome name onto the local decision the renderer
// records.
func consentDecisionFromOutcome(outcome string) chat.ConsentDecision {
	switch outcome {
	case "allow_once":
		return chat.DecisionAllowOnce
	case "allow_session":
		return chat.DecisionAllowSession
	case "deny":
		return chat.DecisionDeny
	case "answered":
		return chat.DecisionAnswer
	case "unanswered":
		// A QUESTION NOBODY ANSWERED is not a decision against it.
		return chat.DecisionUnanswered
	case "expired":
		return chat.DecisionDeny
	default:
		return chat.DecisionSettled
	}
}

// reArmAskDraftClaim hands the keyboard back to a RECORDED card's open free-text row, after a click
// on that row.
func (m *App) reArmAskDraftClaim() {
	s := m.screens[TabAsk]
	if s == nil {
		return
	}
	if c, ok := s.(interface{ ReArmAskDraftClaim() }); ok {
		c.ReArmAskDraftClaim()
	}
}

// beginAskDraft opens the free-text row on a RECORDED ask card, by the key of the item under the
// click.
func (s *chatStore) askIDForKey(convID, key string) string {
	if s == nil || key == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items[convID] {
		it := &s.items[convID][i]
		if it.Kind != chat.KindAsk || it.Key != key || it.Ask == nil {
			continue
		}
		return it.Ask.ID
	}
	return ""
}

// AskReply answers a RECORDED card through the reply RPC — the same call the GUI makes
// (askOrchiconClient.replyPermissionAsk).
func (m *App) AskReply(askID, answer string) tea.Cmd {
	if m.chat == nil || strings.TrimSpace(askID) == "" {
		return nil
	}
	return m.chat.ReplyPermissionAsk(m.chatConvID, askID,
		apiv1.PermissionChoice_PERMISSION_CHOICE_UNSPECIFIED, answer)
}

// selectAskRow moves a RECORDED card's cursor to the row the operator just CLICKED.
func (s *chatStore) selectAskRow(convID, key, label string) {
	if s == nil || key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items[convID] {
		it := &s.items[convID][i]
		if it.Kind != chat.KindAsk || it.Key != key || it.Ask == nil {
			continue
		}
		for j, o := range it.Ask.Options {
			if o.Label == label {
				it.Ask.Sel = j
				return
			}
		}
		if it.Ask.AllowOther {
			// The Other row sits immediately after the options (see renderAskCardSpans).
			it.Ask.Sel = len(it.Ask.Options)
		}
		return
	}
}

// reArmAskCardClaim hands the keyboard back to a RECORDED card's cursor after a click on one of its
// rows — the mirror of reArmAskDraftClaim, and the same reason.
func (m *App) reArmAskCardClaim() {
	s := m.screens[TabAsk]
	if s == nil {
		return
	}
	if c, ok := s.(interface{ ReArmAskCardClaim() }); ok {
		c.ReArmAskCardClaim()
	}
}

func (s *chatStore) beginAskDraft(convID, key string) bool {
	if s == nil || key == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items[convID] {
		it := &s.items[convID][i]
		if it.Kind != chat.KindAsk || it.Key != key || it.Ask == nil {
			continue
		}
		if it.Ask.Answered || !it.Ask.AllowOther {
			return false
		}
		it.Ask.Drafting = true
		it.Ask.Draft = ""
		return true
	}
	return false
}

func (s *chatStore) snapshot(convID string) []chat.ChatItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := s.items[convID]
	out := make([]chat.ChatItem, len(items))
	copy(out, items)
	return out
}

// messageKeyOf is the MESSAGE an item was derived from, or "" for an item that is not part of a
// durable message.
func messageKeyOf(key string) string {
	if !strings.HasPrefix(key, "m-") {
		return ""
	}
	if i := strings.Index(key, "-r"); i > 0 {
		return key[:i]
	}
	return strings.TrimSuffix(key, "-ask")
}

// ONLY A MESSAGE THE STORE HAS ALREADY SEEN IS ANCHORED.
func (s *chatStore) anchorGrowingMessages(convID string, items []chat.ChatItem) {
	grown := map[string]bool{}
	seen := map[string]int{}
	for _, it := range s.items[convID] {
		if k := messageKeyOf(it.Key); k != "" {
			if n := len(it.Text); n > seen[k] {
				seen[k] = n
			}
		}
	}
	for _, it := range items {
		k := messageKeyOf(it.Key)
		if k == "" {
			continue
		}
		was, ok := seen[k]
		if !ok {
			continue
		}
		if len(it.Text) > was {
			grown[k] = true
		}
	}
	if len(grown) == 0 {
		return
	}
	if s.orderAt == nil {
		s.orderAt = map[string]map[string]int64{}
	}
	byMsg := s.orderAt[convID]
	if byMsg == nil {
		byMsg = map[string]int64{}
		s.orderAt[convID] = byMsg
	}
	for k := range grown {
		byMsg[k] = time.Now().UnixMilli()
	}
}

// applyOrderAnchors rewrites every anchored item's ordering timestamp. Call with
// s.mu held, BEFORE sorting.
func (s *chatStore) applyOrderAnchors(convID string, items []chat.ChatItem) {
	byMsg := s.orderAt[convID]
	if len(byMsg) == 0 {
		return
	}
	for i := range items {
		if at, ok := byMsg[messageKeyOf(items[i].Key)]; ok {
			items[i].At = at
		}
	}
}

// onTranscript ingests the durable transcript.
func (m *App) onTranscript(msg chat.TranscriptMsg) tea.Cmd {
	if msg.Err != "" {
		m.setChatErrorPlain(msg.Err)
		return nil
	}
	// MID-TURN IS EITHER HALF, AND THAT IS THE FIX FOR "my user messages are being swallowed up when I
	// send them ... and permission cards no longer let me click on items".
	//
	// The choice used to be made on THIS CLIENT'S STREAM SLOT alone. replace() keeps only two kinds of
	// live-only row — a "draft-" echo and a PENDING consent card — so any other row that exists only live
	// was DROPPED by a delivery: a recorded clarifying-question card (KindAsk), an in-flight text or
	// reasoning chunk, an artifact. All of those have no durable row yet BY DESIGN (the transcript records
	// outcomes, and the server persists the reply as it goes), so the operator's own message and their
	// card disappeared from screen and came back only when the server caught up — "not until the agent
	// starts reasoning".
	//
	// A DURABLE TRANSCRIPT IS INCOMPLETE BY DEFINITION WHILE A TURN RUNS, so mid-turn the live buffer must
	// be preserved. The plane states whether a turn is running (turn_in_flight) and the shell already
	// trusts that field elsewhere (the rail's marker, re-attach, the activity line), so the decision uses
	// BOTH halves — the same either-half rule the activity line uses. The REPLACE is reserved for the state
	// it was written for: the turn is over, the durable transcript IS the authority, and any surviving live
	// row would render twice.
	// THE RETRY AFFORDANCE IS THE SHELL'S TO CLAIM (see chat.RetryAffordanceLine): the composer it refers to
	// is THIS shell's, so only a failed turn THIS client sent — its own user row matching the dock's lastSent —
	// may say the message is back. A HISTORICAL failed turn (another session, the other client) is drawn with
	// its reason and model but makes NO composer claim, because its draft is deliberately not injected. Done
	// BEFORE the store so the stamped row is what every later read sees, and repeated on EVERY poll so the
	// line cannot be dropped when the next durable copy replaces the live one.
	// THE DURABLE PAGE SETTLES WHAT IT RECORDS, on every load. An ask settled in the OTHER client (or
	// expired when its turn finalized) has no other path into this client: the resolution event only
	// reaches a watcher of that turn, and an unrelated replacement card must not be inferred from a
	// close. The page is the record, so the record is what settles.
	for _, sa := range msg.SettledAsks {
		m.chatStore.settleAsk(msg.ConvID, sa.AskID, sa.Outcome, "")
	}
	m.stampRetryAffordance(msg.ConvID, msg.Items)
	midTurn := m.runningFor(msg.ConvID)
	if midTurn {
		m.chatStore.mergeHistory(msg.ConvID, msg.Items)
	} else {
		m.chatStore.replace(msg.ConvID, msg.Items)
	}
	// A FAILED TURN PUTS THE DRAFT BACK, VISIBLY. The GUI does this in its completion effect (it copies the
	// sent text to the clipboard AND signals the composer to restore it) so the operator never loses what
	// they typed and knows it can be re-sent. The TUI restored the draft only on the PRE-ACK path
	// (setChatError), which is the OTHER failure: a turn that failed AFTER the ack persists an error row and
	// lands here, and its draft was never put back — so the error row's "your message is back in the composer"
	// line would have been a false claim on exactly the case the operator hit. See restoreDraftForFailedTurn.
	m.restoreDraftForFailedTurn(msg.ConvID, msg.Items)
	// THE DURABLE TOOL CALLS LAND WITH THE PAGE THEY CAME ON.
	m.chatStore.setToolCalls(msg.ConvID, msg.ToolCalls)
	// ASK THE SERVER WHAT THIS CONVERSATION IS ALLOWED, once, when its transcript lands — the grants
	// header makes a claim about exactly that, and the list is the server's to state.
	if !m.permGrantsLoaded[msg.ConvID] {
		return tea.Batch(m.onChatWake(), m.loadConsentGrants(msg.ConvID))
	}
	return m.onChatWake()
}

// conversationByID returns the conversations-rail row for id, when the shell holds one.
func (m *App) conversationByID(id string) (chat.Conversation, bool) {
	for _, c := range m.conversations {
		if c.ID == id {
			return c, true
		}
	}
	return chat.Conversation{}, false
}

// turnInFlight reports whether the PLANE says this conversation has a turn in flight.
func (m *App) turnInFlight(convID string) bool {
	c, ok := m.conversationByID(convID)
	return ok && c.TurnInFly
}

// And it is a UNION, not a LOCAL-ONLY check: a turn started in the OTHER client has no local slot,
// so TurnInFly must still mark the row.
func (m *App) runningFor(convID string) bool {
	return m.chat.IsStreaming(convID) || m.turnInFlight(convID)
}

// transcriptStatusLine is the Ask pane's fixed footer: the conversation's status line, ONE slot.
func (m *App) transcriptStatusLine(items []chat.ChatItem) string {
	if m.chatConvID == "" {
		return ""
	}
	switch {
	case m.chatStore.isReconnecting(m.chatConvID):
		return "reconnecting…"
	case m.planeUnreachable():
		// THE PANE ITSELF SAYS THE CONNECTION IS DOWN — the operator's ask, verbatim: "if a connection
		// dies, the GUI tells you.
		return "⚠ disconnected — replies will resume when the plane returns (r retries now)"
	}
	// AN ACTIVE SEARCH OWNS THE FOOTER, once the CONNECTION arms above are settled: it is a deliberate
	// operator mode, and the match count IS the status they asked for.
	if strings.TrimSpace(m.askSearch) != "" {
		matches, total := transcriptSearchCount(items, m.askSearch)
		return transcriptSearchStatus(m.askSearch, matches, total)
	}
	// THE CARD IS THE REASON NOTHING IS HAPPENING, AND THE FOOTER MUST SAY SO.
	if m.chatStore.hasPendingConsent(m.chatConvID) {
		if line := m.cardWaitingLine(); line != "" {
			return line
		}
	}
	if m.runningFor(m.chatConvID) {
		// THE SERVER'S CLOCK FEEDS THE ROTATING VERB (see turnActivityNotice).
		serverTime, _ := m.chat.ServerTimeSince(m.chatConvID)
		// THE COUNTER COMES FROM WHAT THE CLIENT ALREADY POLLS.
		summary := toolclass.SummarizeCalls(
			m.chatStore.snapshotToolCalls(m.chatConvID), noticeNow(), toolclass.DefaultWindow)
		return turnActivityNotice(m.chat.SilenceSince(m.chatConvID), serverTime, summary, m.askWidth())
	}
	return ""
}

// noticeNow is the activity line's clock seam.
var noticeNow = time.Now

// cardWaitingLine is the pane footer while a card is waiting for the operator: WHAT is being asked,
// in one row, and the fact that the turn cannot move until it is answered.
func (m *App) cardWaitingLine() string {
	_, st, ok := m.pendingConsentItem()
	if !ok {
		// Raced away between the case test and here (answered, or settled): say nothing rather than claim a
		// card the store no longer holds. The next repaint takes the ordinary arms.
		return ""
	}
	label := "approval"
	if st.Ask.Kind == chat.AskQuestion {
		label = "answer"
	}
	head := "⏸ waiting for your " + label
	subject := chat.ConsentSubject(st.Ask)
	const hint = " — Orchicon is paused until you answer"
	width := m.askWidth()
	// Unbounded (no measured pane): say everything rather than guess a width.
	if width <= 0 {
		if subject == "" {
			return head + hint
		}
		return head + " · " + subject + hint
	}
	if subject == "" {
		if ansi.StringWidth(head+hint) <= width {
			return head + hint
		}
		return truncateSingle(head, width)
	}
	full := head + " · " + subject + hint
	if ansi.StringWidth(full) <= width {
		return full
	}
	named := head + " · " + subject
	if ansi.StringWidth(named) <= width {
		return named
	}
	// The subject still has to share the row with the label, so give it what is left — and only if what is
	// left is worth reading (a two-character stub of a path names nothing).
	if room := width - ansi.StringWidth(head) - 3; room >= minSubjectCells {
		return head + " · " + truncateSingle(subject, room)
	}
	return truncateSingle(head, width)
}

// minSubjectCells is the fewest cells of a subject worth drawing beside the label. Below it the row is the
// label alone: a path cut to "wr…" is noise dressed as information.
const minSubjectCells = 12

// askWidth is the Ask pane's detail render width, read from the same surface askStatusLine reads
// its footer from.
func (m *App) askWidth() int {
	if s := m.screens[TabAsk]; s != nil {
		if f, ok := s.(interface{ DetailWidth() int }); ok {
			return f.DetailWidth()
		}
	}
	return 0
}

// askStatusLine reads the Ask pane's fixed footer — where the activity/status line now lives.
func (m *App) askStatusLine() string {
	if s := m.screens[TabAsk]; s != nil {
		if f, ok := s.(interface{ DetailFooter() string }); ok {
			return f.DetailFooter()
		}
	}
	return ""
}

// onChatWake repaints the open ask-conversation detail pane with the merged, phase-grouped
// transcript + live chunks.
func (m *App) onChatWake() tea.Cmd {
	// dock-level banner first (any screen): reconnecting state from the shared store, applied on the
	// tea loop.
	if m.chatConvID != "" {
		switch {
		case m.chatStore.isReconnecting(m.chatConvID):
			m.dock.SetNotice("connection lost — re-attaching to the running turn…")
		case m.planeUnreachable():
			m.dock.SetNotice("connection lost — retrying… (r reconnects now)")
		default:
			// Clear only OUR banners, never an unrelated notice the operator is being shown.
			if isConnBanner(m.dock.Notice) {
				m.dock.SetNotice("")
			}
		}
	}
	s := m.screens[TabAsk]
	if s == nil || m.chatConvID == "" {
		// NO CONVERSATION, NO STATUS: clear the band rather than leaving the previous chat's line sitting
		// under a fresh transcript.
		if s != nil {
			if stf, ok := s.(interface{ SetDetailFooter(string) }); ok {
				stf.SetDetailFooter("")
			}
		}
		return nil
	}
	type detailIDer interface{ DetailID() string }
	// The transcript body is ALREADY LAID OUT by the shell (padded bands, collapsible structure), so
	// the pane is asked for the laid-out form.
	type setter interface {
		SetDetailContentLaidOut(title string, fields []screenkit.Field, body string)
	}
	dr, ok1 := s.(detailIDer)
	st, ok2 := s.(setter)
	if !ok1 || !ok2 {
		return nil
	}
	// A DRIFTED DETAIL ID IS RE-ASSERTED, NOT TREATED AS "SHOWING SOMETHING ELSE".
	if dr.DetailID() != m.chatConvID {
		sid, ok := s.(interface{ SetDetailID(string) })
		if !ok {
			return nil
		}
		sid.SetDetailID(m.chatConvID)
	}
	if askS, ok := s.(interface {
		RenderTranscript([]chat.ChatItem, chat.Conversation, bool) (title string, fields []screenkit.Field)
	}); ok {
		items := m.chatStore.snapshot(m.chatConvID)
		// AN ACTIVE SEARCH FILTERS WHAT IS RENDERED, not what is COUNTED: the header and the footer still
		// describe the whole conversation.
		visibleItems := m.searchVisibleItems(items)
		// The rail's row for this conversation is the freshest header data the shell holds: it is reloaded
		// after every send, and it carries the title and the message count.
		live, haveLive := m.conversationByID(m.chatConvID)
		title, fields := askS.RenderTranscript(items, live, haveLive)
		w := s.(interface{ DetailWidth() int }).DetailWidth()
		// ONE SCROLL ROW ALWAYS, so the field count is STABLE and the pane's body height can be computed
		// BEFORE the stream is sized (below).
		fields = append(fields, screenkit.Field{Key: "scroll", Value: ""})

		// THE ACTIVITY LINE IS THE PANE'S FIXED FOOTER, AND IT IS SET *BEFORE* THE STREAM IS SIZED.
		if stf, ok := s.(interface{ SetDetailFooter(string) }); ok {
			stf.SetDetailFooter(m.transcriptStatusLine(items))
		}

		// THE STREAM IS SIZED TO THE PANE'S BODY, NOT THE CONTENT REGION.
		strH := m.contentHeight()
		if _, ok := s.(interface{ DetailBodyHeight(int, bool) int }); ok {
			// LAY OUT BEFORE MEASURING.
			//
			// The pane's body height is derived from ITS height, which the shell assigns in refreshLayout
			// from the rows the DOCK leaves — and the dock's row count depends on its own content (the chip
			// row, the hint's wrap, the notice strip). So a paint that measures before laying out sizes the
			// stream against a pane height that is about to change:
			//
			//	MEASURED: dock 7 rows -> 8 after a reload -> the pane's body 22 -> 21
			//
			// and a stream sized 22 while the pane draws 21 has its BOTTOM row clipped by the pane's own
			// viewport — which is the notice, drawn last. Re-laying-out here makes the measurement and the
			// paint agree by construction, rather than depending on whoever changed the dock's content
			// remembering to re-measure.
			m.refreshLayout()
			bh := s.(interface{ DetailBodyHeight(int, bool) int })
			// THE FIELD COUNT MUST BE THE PANE'S REAL ONE, not the count implied by the fields the shell just
			// built.
			fieldRows := len(fields)
			if fp, ok := s.(interface{ DetailFieldCount() int }); ok {
				if n := fp.DetailFieldCount(); n > fieldRows {
					fieldRows = n
				}
			}
			strH = bh.DetailBodyHeight(fieldRows, true)
		}
		str := m.newTranscriptStream(m.chatConvID, w, strH)
		m.syncTranscript(m.chatConvID, str, visibleItems, w)
		// Surface the scroll position when the transcript is taller than the pane.
		fields[len(fields)-1].Value = str.ScrollLabel()
		st.SetDetailContentLaidOut(title, fields, str.View())
	}
	// THE PANE RE-EVALUATES ITS OWNER'S LIVENESS ON EVERY WAKE, which is what makes it follow a turn
	// that starts AFTER it was opened — see ensureDiffStreamArmed for the operator report this closes.
	return m.ensureDiffStreamArmed()
}

// planeUnreachable reports whether the PLANE itself is not answering — as opposed to one turn
// having lost its socket.
func (m *App) planeUnreachable() bool {
	st, _ := m.reg.WorstStatus()
	switch st {
	case stream.StatusReconnecting, stream.StatusError, stream.StatusClosed:
		return true
	}
	return false
}

// isConnBanner reports whether a dock notice is one of the CONNECTION banners this shell writes, so
// clearing on recovery cannot wipe an unrelated message the operator is being shown (a send ack.
func isConnBanner(notice string) bool {
	return strings.HasPrefix(notice, "connection lost —")
}

// awaitingReply reports whether the conversation is waiting for the model's first content — i.e. a
// user message has been sent and NO model text has arrived after it.
func awaitingReply(items []chat.ChatItem) bool {
	lastUser := -1
	for i, it := range items {
		if it.Kind == chat.KindUser {
			lastUser = i
		}
	}
	if lastUser < 0 {
		return false // nothing was sent; there is no reply to wait for
	}
	for _, it := range items[lastUser+1:] {
		// The model's own words. Tool rows and reasoning do not count: the question is answered when
		// there is prose, and the GUI waits for the same thing.
		if it.Kind == chat.KindText {
			return false
		}
	}
	return true
}

// newTranscriptStream builds the conversation's transcript stream, sized to the pane body, with
// OVERFLOW WRAPPING ON.
func (m *App) newTranscriptStream(convID string, w, h int) *kit2.Stream {
	str := m.transcriptStream(convID, w, h)
	// See kit2.Stream.WrapOverflow — the row count is the price, readability is
	// the point.
	str.WrapOverflow()
	return str
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

// syncTranscript renders the grouped transcript and folds it into the stream: when the new render
// EXTENDS the previous one the extra lines are appended (preserving scroll offset / following only.
func (m *App) syncTranscript(convID string, str *kit2.Stream, items []chat.ChatItem, w int) {
	if m.transcriptLines == nil {
		m.transcriptLines = map[string][]string{}
	}
	// RenderItemsSpansWithCopy, not RenderItemsSpans: the copy glyph marks the two things on THIS
	// surface that a click copies — the operator's own message and each code block.
	body, spans := m.renderCache.Render(convID, chat.GroupByPhase(items), w, chat.CopyGlyph, m.foldedReasoning, m.askSearch)
	if m.transcriptSpans == nil {
		m.transcriptSpans = map[string][]chat.ItemSpan{}
	}
	m.transcriptSpans[convID] = spans
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
		// ReplaceLines, NOT SetLines: the common case here is a reply growing IN PLACE — the durable
		// poll rewrites the assistant message once a second and its last line changes rather than a new
		// line appearing — so this branch runs repeatedly during a live turn. SetLines re-pins to the
		// bottom, which would drag an operator who scrolled up back down every second. ReplaceLines
		// keeps their place unless they were already following the tail.
		str.ReplaceLines(lines)
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

// turnActivityNotice is the transcript's activity line while a turn is streaming: what the turn is
// doing, plus how long the stream has been quiet.
func turnActivityNotice(silent time.Duration, effectiveServerTimeMs int64, summary string, width int) string {
	const (
		// ONE SECOND, not five.
		showAgeAfter = time.Second
		// Past the server's heartbeat interval (15s) plus slack, silence is worth stating plainly: a LIVE
		// stream can never reach it, because the heartbeat keeps arriving.
		warnAfter = 25 * time.Second
		// The watchdog re-dials at 40s (askStreamStalled). Naming that just before it happens turns an
		// unexplained stall into a stated one.
		reDialAfter = 35 * time.Second
	)
	// ONE WORD, FROM ONE PURE FUNCTION.
	verb := "Orchicon is " + chat.ActivityVerb(effectiveServerTimeMs) + "…"
	secs := int(silent.Round(time.Second) / time.Second)
	var want string
	switch {
	case silent >= reDialAfter:
		// ESCALATION OUTRANKS THE COUNTER, AND IT IS THE POINT OF THE WHOLE RULE.
		want = fmt.Sprintf("%s · no output for %ds — the stream will re-attach if it stays silent", verb, secs)
	case silent >= warnAfter:
		want = fmt.Sprintf("%s · no output for %ds", verb, secs)
	case summary != "":
		// THE HEALTHY BAND, AND THE SUMMARY'S OWN TRAILING "newest call Ns ago" IS THE AGE.
		want = verb + " · " + summary
	case silent <= 0 || silent < showAgeAfter:
		// No counted work and no age yet — the moment between sending and the stream's first event.
		return fitNotice(verb, verb, width)
	default:
		want = fmt.Sprintf("%s · last activity %ds ago", verb, secs)
	}
	return fitNotice(want, verb, width)
}

// fitNotice trims ONE row to the pane's width by dropping text in priority order:
//
//	summary  <  escalation band  <  verb
//
// WHY IT HAS TO EXIST. The footer is a single row (screenkit.Detail.footerRows counts "\n"), and the
// host Panel CLIPS THE TAIL of an over-wide row. The tail is where the escalation band and the counter
// live, and the band is the half that carries the watchdog's verdict — so on a narrow terminal a naive
// append would silently eat the more important text. Degrade, do not clip: the summary goes first, then
// the band, and the verb is NEVER dropped (the line's oldest invariant: it is present at every stage of
// the turn, and it says what the turn is doing).
//
// It mirrors the composer's own documented precedence (statline_test.go): the load-bearing text outranks
// the informative text.
func fitNotice(line, verb string, width int) string {
	// A zero or negative width means "unbounded" (the caller has no pane to fit), and a line that already
	// fits is returned as it stands: the function only ever REMOVES text.
	if width <= 0 || ansi.StringWidth(line) <= width {
		return line
	}
	// STEP 1 — the summary goes first.
	if strings.HasPrefix(line, verb) {
		rest := line[len(verb):]
		if !strings.Contains(rest, "no output for") {
			if ansi.StringWidth(verb) <= width {
				// Dropping the summary leaves the verb and nothing else, because the summary IS everything
				// after the verb on a healthy line.
				return verb
			}
		}
	}
	// STEP 2 — the band goes next, leaving the verb.
	if ansi.StringWidth(verb) <= width {
		return verb
	}
	// STEP 3 — a terminal narrower than the verb itself.
	runes := []rune(verb)
	if width >= len(runes) {
		return verb
	}
	if width < 1 {
		width = 1
	}
	return string(runes[:width])
}

// transcriptUserMessageAtFrameRow resolves a click at a FRAME row to the operator's own message text,
// when the click landed on one.
//
// THREE COORDINATE SPACES, and each is a place this can be wrong:
//
//	frame row  → body row   (subtract where the transcript's first line is drawn)
//	body row   → body LINE  (kit2.Stream.LineAtRow, because a wrapped row is not a line)
//	body line  → the ITEM   (chat.ItemSpan, from the render that drew it)
//
// The first is derived rather than guessed: the screen block starts one row below the tab chrome
// (tabBarRows + 1), the pane's border is one row, the pane's title is one row, the detail's fields take
// one row each, and a blank separator sits between the fields and the body — the same rows Detail.View
// writes and Detail.BodyHeightFor subtracts. A test pins the derivation against a real rendered frame,
// so a layout change fails there rather than silently copying the wrong message.
func (m *App) transcriptUserMessageAtFrameRow(frameRow int) (string, bool) {
	str := m.TranscriptStream(m.chatConvID)
	if str == nil {
		return "", false
	}
	line := str.LineAtRow(frameRow - m.transcriptBodyTopRow())
	if line < 0 {
		return "", false
	}
	for _, sp := range m.transcriptSpans[m.chatConvID] {
		// ONLY the operator's own messages, which is what was asked for: the useful gesture is "get MY
		// message back" — to re-send it, quote it, or paste it somewhere else.
		if sp.Kind == chat.KindUser && sp.Contains(line) && strings.TrimSpace(sp.Text) != "" {
			return sp.Text, true
		}
	}
	return "", false
}

// transcriptUserMessageIDAtFrameRow is transcriptUserMessageAtFrameRow for the edit menu: it
// returns the message's DURABLE ID as well as its text.
func (m *App) transcriptUserMessageIDAtFrameRow(frameRow int) (string, string, bool) {
	str := m.TranscriptStream(m.chatConvID)
	if str == nil {
		return "", "", false
	}
	line := str.LineAtRow(frameRow - m.transcriptBodyTopRow())
	if line < 0 {
		return "", "", false
	}
	for _, sp := range m.transcriptSpans[m.chatConvID] {
		if sp.Kind != chat.KindUser || !sp.Contains(line) {
			continue
		}
		// A SPAN WITH NO MESSAGE ID IS NOT EDITABLE: it is a live fragment, not a persisted row, and the server
		// could not be told which message to remove. The menu simply does not open there.
		if sp.MessageID == "" {
			return "", "", false
		}
		return sp.MessageID, sp.Text, true
	}
	return "", "", false
}

// lastEditableUserMessage resolves the MOST RECENT of the operator's own messages, for the slash
// commands.
func (m *App) lastEditableUserMessage() (string, string, bool) {
	if m.chatConvID == "" {
		return "", "", false
	}
	spans := m.transcriptSpans[m.chatConvID]
	for i := len(spans) - 1; i >= 0; i-- {
		sp := spans[i]
		if sp.Kind != chat.KindUser || sp.MessageID == "" || strings.TrimSpace(sp.Text) == "" {
			continue
		}
		return sp.MessageID, sp.Text, true
	}
	return "", "", false
}

// composerTopRow is the frame row of the composer dock's FIRST line: the chrome, then the screen block, then
// the slide-out strip, then the dock — the same order baseView paints them in.
func (m *App) composerTopRow() int {
	return tabBarRows + 1 + m.screenRows() + m.panelRows()
}

// composerClickAt routes a click in the composer to the caret, reporting whether it landed there.
func (m *App) composerClickAt(x, y int) bool {
	if m.welcomeMode() {
		_, g := m.welcomeLayout(m.contentWidth(), m.contentHeight()+m.dock.Lines())
		if !g.contains(x, y) {
			// Outside the drawn box — the centered margins, or the brand/tagline above and below.
			return false
		}
		// ClickAt measures the box it is placing the caret in (the prompt offset, the input rows, the
		// textarea's width), so it MUST be given the width the box was DRAWN at rather than the layout
		// width.
		prev := m.dock.Width
		m.dock.Width = g.Width
		ok := m.dock.ClickAt(x-g.Left, y-g.Top)
		m.dock.Width = prev
		return ok
	}
	if y < m.composerTopRow() {
		return false
	}
	left := 0
	if m.diffOpen && m.diffPane != nil {
		left = m.diffPaneWidth()
		if x < left {
			return false // the diff pane's own columns
		}
	}
	if x >= left+m.contentWidth() {
		return false // the conversations rail's own columns
	}
	return m.dock.ClickAt(x-left, y-m.composerTopRow())
}

// transcriptCodeBlockAtFrameRow resolves a click at a FRAME row to the SOURCE of a code block under
// it.
func (m *App) transcriptCodeBlockAtFrameRow(frameRow int) (string, bool) {
	str := m.TranscriptStream(m.chatConvID)
	if str == nil {
		return "", false
	}
	line := str.LineAtRow(frameRow - m.transcriptBodyTopRow())
	if line < 0 {
		return "", false
	}
	// The ITEM is found first, then the block within it: an item's block offsets are relative to the item, so
	// the item has to answer for the line before its blocks can be asked about it.
	for _, sp := range m.transcriptSpans[m.chatConvID] {
		if sp.Contains(line) {
			return sp.CodeAt(line)
		}
	}
	return "", false
}

// THE PERMISSION CARD USED TO BE KEYBOARD-ONLY.
func (m *App) transcriptCardOptionAtFrameRow(frameRow int) (chat.ItemKind, string, string, bool) {
	str := m.TranscriptStream(m.chatConvID)
	if str == nil {
		return "", "", "", false
	}
	line := str.LineAtRow(frameRow - m.transcriptBodyTopRow())
	if line < 0 {
		return "", "", "", false
	}
	for _, sp := range m.transcriptSpans[m.chatConvID] {
		if sp.Kind != chat.KindAsk && sp.Kind != chat.KindConsent {
			continue
		}
		if !sp.Contains(line) {
			continue
		}
		label, ok := sp.OptionAt(line)
		if !ok {
			return "", "", "", false // the card's body or header: not a choice
		}
		// A SETTLED card is no longer a choice.
		if sp.Kind == chat.KindAsk && m.askCardSettled(sp.Key) {
			return "", "", "", false
		}
		if sp.Kind == chat.KindConsent {
			if _, st, ok := m.pendingConsentItem(); !ok || !st.Pending() {
				return "", "", "", false
			}
		}
		return sp.Kind, sp.Key, label, true
	}
	return "", "", "", false
}

// pendingConsentItem finds the open permission/question card in the transcript, with the id and the
// live state a decision needs.
func (m *App) pendingConsentItem() (string, *chat.ConsentState, bool) {
	if m.chatStore == nil || m.chatConvID == "" {
		return "", nil, false
	}
	for _, it := range m.chatStore.snapshot(m.chatConvID) {
		if it.Kind == chat.KindConsent && it.Consent != nil && it.Consent.Pending() {
			return it.AskID, it.Consent, true
		}
	}
	return "", nil, false
}

// consentDecideFromRow applies a CLICK on a card row: a question's label goes through the ordinary
// send path, a permission's label becomes the decision.
func (m *App) consentDecideFromRow(label string) tea.Cmd {
	askID, st, ok := m.pendingConsentItem()
	if !ok {
		return nil
	}
	// THE ROW IS RESOLVED BY ITS INDEX, AND THE DECISION COMES FROM THE INDEX TOO.
	row := -1
	for i, l := range st.Ask.OptionLabels() {
		if l == label {
			row = i
			break
		}
	}
	if row < 0 {
		return nil
	}
	st.Sel = row
	// A QUESTION card rides the same KindConsent item, and a click on it ANSWERS rather than decides.
	if st.Ask.Kind == chat.AskQuestion {
		// THE FREE-TEXT ROW IS A PROMPT FOR AN ANSWER, NOT AN ANSWER.
		if label == chat.ConsentOther {
			st.OtherMode = true
			st.OtherInput = ""
			// THE CLICK IS AN ACT ON THE CARD, so the card takes the keyboard back: a card whose claim was
			// released by ctrl+g would otherwise draw an input row that collects nothing while the typing it
			// asks.
			m.reArmConsentClaim()
			return m.onChatWake()
		}
		return m.ConsentResolve(askID, chat.DecisionAnswer, label)
	}
	dec, ok := st.Ask.DecisionForRow(row)
	if !ok {
		// A non-choice row (a disabled session row): the click is a no-op rather than a
		// decision nobody made.
		return nil
	}
	return m.ConsentResolve(askID, dec, label)
}

// transcriptAskOptionAtFrameRow resolves a click at a FRAME row to the LABEL of the
// clarifying-question option under it, when the click landed on an option of an UNANSWERED card.
func (m *App) transcriptAskOptionAtFrameRow(frameRow int) (string, bool) {
	str := m.TranscriptStream(m.chatConvID)
	if str == nil {
		return "", false
	}
	line := str.LineAtRow(frameRow - m.transcriptBodyTopRow())
	if line < 0 {
		return "", false
	}
	for _, sp := range m.transcriptSpans[m.chatConvID] {
		if sp.Kind != chat.KindAsk || !sp.Contains(line) {
			continue
		}
		label, ok := sp.OptionAt(line)
		if !ok {
			return "", false // the question text or the card's header: not a choice
		}
		if m.askCardSettled(sp.Key) {
			return "", false
		}
		return label, true
	}
	return "", false
}

// askCardSettled reports whether a clarifying-question card has been ANSWERED.
func (m *App) askCardSettled(key string) bool {
	if m.chatStore == nil || key == "" {
		return false
	}
	for _, it := range m.chatStore.snapshot(m.chatConvID) {
		if it.Key != key {
			continue
		}
		return it.Ask != nil && it.Ask.Answered
	}
	return false
}

// transcriptBodyTopRow is the frame row at which the transcript's FIRST body line is drawn.
func (m *App) transcriptBodyTopRow() int {
	fieldRows := 0
	if s := m.screens[TabAsk]; s != nil {
		if fp, ok := s.(interface{ DetailFieldCount() int }); ok {
			fieldRows = fp.DetailFieldCount()
		}
	}
	// screen top + pane border + pane title + the detail's fields + the blank separator.
	return tabBarRows + 1 + 1 + 1 + fieldRows + 1
}

// ScrollTranscript wheels the open transcript by delta lines (the operator
// scroll offset the Stream preserves across appends).
func (m *App) ScrollTranscript(delta int) {
	if str := m.chatStreams[m.chatConvID]; str != nil {
		str.Wheel(delta)
	}
}

// onConversationMutated reconciles the conversations rail after a rename / delete / mode write: a
// delete drops the row locally (and clears the active conversation when it was the deleted one).
func (m *App) onConversationMutated(msg chat.ConversationMutatedMsg) tea.Cmd {
	if msg.Err != "" {
		m.dock.SetError(msg.Op + " failed: " + msg.Err)
		return m.reloadConversations()
	}
	if msg.Op == "compact" {
		// Compaction rewrites the server-side history, so the transcript the pane shows is now stale (and
		// the summary is a new assistant message).
		if msg.ID == m.chatConvID && msg.Detail != "" {
			m.dock.SetNotice("compact: " + msg.Detail)
			return tea.Batch(m.chat.Poll(msg.ID), m.reloadConversations())
		}
		return m.reloadConversations()
	}
	if msg.Op == "remove" {
		// A MESSAGE WAS REMOVED: the transcript the pane shows is now stale, so re-poll it.
		if msg.Detail != "" {
			m.dock.SetNotice("remove: " + msg.Detail)
		}
		if msg.ID == m.chatConvID && m.chat != nil {
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
			m.closeScopeModal()
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
	// THE GENERATION IS PASSED THROUGH, and it is what keeps an interjection from ending its own turn:
	// the superseded stream closes too.
	ended := m.chat.EndStream(msg.ConvID, msg.Gen)
	if ended {
		// A STREAM CLOSE IS NOT EVIDENCE ABOUT AN ASK, SO NOTHING IS SETTLED HERE.
	}
	// A finished turn is when new usage lands, so this is the LIVE update: the stat strip re-reads the
	// session's tokens / cache / cost and refreshes.
	m.refreshComposerHint()
	// AND THE SEND ACK SETTLES WITH THE TURN.
	m.dock.SettleSendingAck()
	next := []tea.Cmd{m.chat.Poll(msg.ConvID), m.refreshMetrics(), m.chat.LoadConversations()}
	if ended {
		// The server's open-ask list is the settle authority (see the note above).
		next = append(next, m.chat.DiscoverPendingAsks(msg.ConvID))
	}
	return tea.Batch(next...)
}

// restoreDraftForFailedTurn puts the operator's message back in the composer when a DURABLE transcript shows a
// turn FAILED — the POST-ack half of the retry affordance.
//
// The pre-ack path (setChatError) already restores the draft, because that is where a failed SEND lands. A
// turn that failed AFTER the ack is different: it is persisted as an assistant row with metadata.error set and
// lands on the transcript by a poll, so nothing restored the draft — yet the error row tells the operator
// their message is back. The GUI does restore it (its completion effect copies the text to the clipboard and
// signals the composer), so this is the TUI's missing half, and it is what makes the error row's retry line
// TRUE on the case the operator actually hit rather than only on the pre-ack case.
//
// IT IS SCOPED TO THE TURN THIS CLIENT ACTUALLY SENT, and that guard is not optional:
//
//	A durable KindError row is HISTORICAL. Opening any conversation whose transcript ends in a turn that
//	failed — a previous session, the OTHER client, hours ago — delivers exactly that row through this same
//	path, and an unscoped restore would then dump the text THIS session last sent into the composer of a
//	chat that never carried it (measured: open a conversation with an old failed turn after sending in
//	another chat and the composer came back holding the OTHER chat's message). The GUI is immune for the
//	same reason its completion effect keys on `pendingReplyId`: it only restores for the turn its own
//	slot was tracking.
//
// SO THE RESTORE REQUIRES THAT THE FAILED TURN'S OWN USER MESSAGE IS WHAT THIS CLIENT LAST SENT. The
//
//	error row's preceding user row is the turn's message (chronologically ordered), and it is compared with
//	dock.LastSent() through matchesAny — the SAME suffix rule the transcript merge uses — so a prepended
//	context preamble does not defeat it. Anything else is a failure that was not this client's send, and it
//	gets the error ROW (drawn by conversationItems) but not a composer injection.
//
// IT IS LATCHED PER ROW: the transcript is re-read every second while a turn runs and again on completion, so
// the same failed row arrives many times. dock.RestoreDraft already refuses to clobber text typed since, but the
// latch keeps the restore to the moment the row FIRST appears, so a later poll cannot re-clobber a draft the
// operator started typing after dismissing the error.
func (m *App) restoreDraftForFailedTurn(convID string, items []chat.ChatItem) {
	if convID == "" {
		return
	}
	// The text of the user row the current error row belongs to, tracked as the items are walked (they
	// are chronologically ordered).
	turnText := ""
	for _, it := range items {
		switch it.Kind {
		case chat.KindUser:
			turnText = it.Text
			continue
		case chat.KindError:
		default:
			continue
		}
		if it.Key == "" || m.failedTurnRestored[it.Key] {
			continue
		}
		m.failedTurnRestored[it.Key] = true
		// Only for the OPEN conversation: a failure in a background chat must not put its text in this
		// one's composer.
		if convID != m.chatConvID {
			continue
		}
		// AND ONLY FOR THE TURN THIS CLIENT SENT — see the doc above. A historical failure, or one this
		// client never sent, is drawn (conversationItems) but does not touch the composer.
		if !m.ownFailedTurn(turnText) {
			continue
		}
		m.restoreAttachments()
		m.dock.RestoreDraft()
	}
}

// ownFailedTurn reports whether a failed turn is the one THIS client sent — i.e. the turn's own
// user text is what the dock last sent (suffix-matched through matchesAny.
func (m *App) ownFailedTurn(turnText string) bool {
	last := m.dock.LastSent()
	return last != "" && matchesAny([]string{turnText}, last)
}

// stampRetryAffordance appends the retry line to the failed-turn rows that are THIS client's own
// send, on the OPEN conversation.
func (m *App) stampRetryAffordance(convID string, items []chat.ChatItem) {
	if convID == "" || convID != m.chatConvID {
		return
	}
	turnText := ""
	for i := range items {
		switch items[i].Kind {
		case chat.KindUser:
			turnText = items[i].Text
		case chat.KindError:
			if m.ownFailedTurn(turnText) {
				items[i].Text = chat.WithRetryAffordance(items[i].Text)
			}
		}
	}
}

// modelRefForConv is the model ref the composer's chain would resolve for a SPECIFIC conversation:
// that conversation's own row from the shell's list, else the pending selection.
func (m *App) modelRefForConv(convID string) string {
	if convID != "" {
		if c, ok := m.conversationByID(convID); ok && c.ModelRef != "" {
			return c.ModelRef
		}
	}
	// No row for that conversation (a failed create, or a list that has not landed): fall back to the
	// composer's own resolution, which is the best available answer for "what would this have been sent to".
	return m.currentAskModel()
}

// IT IS LIVE-ONLY BY CONSTRUCTION, and that is correct: there is no server row to reconcile
// against, and a later successful turn's durable transcript REPLACES the local buffer
// (chatStore.replace.
func (m *App) surfaceTurnFailure(convID string, err error) tea.Cmd {
	if convID == "" || m.chatStore == nil {
		return nil
	}
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	if strings.TrimSpace(errText) == "" {
		return nil
	}
	// THE RETRY CLAIM IS MADE ONLY FOR THE OPEN CONVERSATION, whose composer setChatError just put the
	// text back into.
	text := chat.FailedTurnText(errText, m.modelRefForConv(convID))
	if convID == m.chatConvID {
		text = chat.WithRetryAffordance(text)
	}
	m.chatStore.append(convID, chat.ChatItem{
		Kind: chat.KindError,
		Text: text,
		At:   time.Now().UnixMilli(),
		Key:  fmt.Sprintf("fail-%d", time.Now().UnixNano()),
		Live: true,
	})
	// REPAINT IF IT IS THE OPEN CONVERSATION. Same guard as the send's own echo: a stale failure for another
	// conversation must not repaint this one (the store keeps it for when that conversation is opened).
	if convID == m.chatConvID {
		return m.onChatWake()
	}
	return nil
}

// setChatError maps a chat failure to the dock error strip (401 gets
// the re-auth prompt naming the in-place fix; the app keeps running).
func (m *App) setChatError(where string, err error) {
	// Settle the composer's send ack.
	m.dock.SetNotice("")
	// A failed send must not lose the operator's message: put the draft back in the composer
	// (RestoreDraft never clobbers text typed since).
	m.restoreAttachments()
	if chat.IsAuthExpired(err) {
		m.setReauthBanner()
		m.dock.RestoreDraft()
		return
	}
	m.dock.SetError(where + ": " + err.Error())
	m.dock.RestoreDraft()
}

// restoreAttachments puts the last turn's attachments back into the pending set after a failed
// send.
func (m *App) restoreAttachments() {
	if len(m.lastSentAttachments) == 0 {
		return
	}
	if len(m.pendingAttach) > 0 {
		return // the operator has moved on; do not resurrect the old set over it
	}
	for _, f := range m.lastSentAttachments {
		m.pendingAttach = append(m.pendingAttach, attachment{
			Name: f.GetName(), MimeType: f.GetMimeType(), Data: f.GetData(),
		})
	}
	m.lastSentAttachments = nil
	m.refreshComposerHint()
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

// reauthBannerText is the ONE global re-auth banner.
const reauthBannerText = "session needs re-authentication — run /connect (esc cancels; the shell reconnects in place)"

// setReauthBanner raises the single global re-auth banner.
func (m *App) setReauthBanner() {
	if m.authRetryInline() {
		return
	}
	m.dock.SetError(reauthBannerText)
}

// authRetryInline reports whether the shell already renders the re-auth
// retry state inline (a pane's fetch error the rail's own retry row).
func (m *App) authRetryInline() bool {
	// The rail's error only suppresses the shell banner while the rail is actually ON SCREEN.
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

// RepaintTranscript re-sizes and repaints the open conversation's transcript from what the shell
// already holds.
func (m *App) RepaintTranscript() tea.Cmd { return m.onChatWake() }

// EVERY caller of dock.Update MUST go through it, and the reason is that the dock's Enter path does
// TWO things at once.
func (m *App) composerKey(msg tea.Msg) (handled bool, cmd tea.Cmd) {
	handled, cmd = m.dock.Update(msg)
	if text := m.dock.SendRequest(); text != "" {
		// Batched with whatever the key produced, so the edit and the send it triggered cannot be separated
		// (or one of them lost).
		cmd = tea.Batch(cmd, m.sendFromComposer(text))
		m.refreshStreamStatus()
	}
	return handled, cmd
}

// sendChat sends text to the conversation with the context preamble AND the pending attachments.
func (m *App) sendChat(convID, text, preamble string) tea.Cmd {
	files := m.wireAttachments()
	m.lastSentAttachments = files
	m.clearPendingAttachments()
	return m.chat.SendWithAttachments(convID, text, preamble, files)
}

// createConversationAndSend creates the first conversation lazily and
// sends the message into it (GUI CreateConversation pattern).
func (m *App) createConversationAndSend(text, preamble string) tea.Cmd {
	cl := m.clients
	// SEED the model on create, never leave it empty.
	model := m.currentAskModel()
	mode := m.chat.PendingMode()
	// THE ACTIVE WORKSPACE, READ HERE — on the tea loop, where the shell's state is current — and
	// captured by the command below.
	projectID := m.activeProjectID()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		// Create EMPTY, then send once through the stream below.
		resp, err := cl.Ask.CreateConversation(ctx, connect.NewRequest(&apiv1.CreateConversationRequest{
			ModelRef:  model,
			Mode:      mode,
			ProjectId: projectID,
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

// pinTranscriptToTail points the open transcript back at its newest line.
func (m *App) pinTranscriptToTail() {
	if m.chatConvID == "" {
		return
	}
	if str := m.chatStreams[m.chatConvID]; str != nil {
		str.ScrollToBottom()
	}
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

// interjectExecution sends a mid-run message into the execution's live session (the TUI equivalent
// of orchicon_send_execution_message; the same API path the GUI's useSendExecutionMessage uses).
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
	// THE FOOTER'S FOCUS HINT IS DERIVED HERE, at the ONE chokepoint every focus change passes through,
	// rather than at the handful of call sites somebody remembered.
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

// askPaneKey handles left/right as PANE SELECTION on the Ask tab, reporting whether it owned the
// key.
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

// stopReply interrupts the in-flight reply on the open conversation: the composer's ctrl+y, the
// TUI's counterpart to the GUI's Stop button (ask-orchicon.tsx: handleStopStreaming -> abortTurn).
func (m *App) stopReply() tea.Cmd {
	if m.chatConvID == "" {
		m.dock.SetNotice("no conversation open — nothing to stop")
		return nil
	}
	if m.chat == nil || !m.chat.IsStreaming(m.chatConvID) {
		m.dock.SetNotice("no reply in flight — nothing to stop")
		return nil
	}
	// The ack is written BEFORE the RPC, like the composer's "sending …", so the key press is visible
	// at once; the outcome ("reply stopped" / a stop failure) replaces it when AbortTurnMsg lands.
	m.dock.SetNotice("stopping the reply…")
	return m.chat.AbortTurn(m.chatConvID)
}

// sendFromComposer routes composer text: slash commands dispatch, running-execution context
// interjects, everything else sends to Ask Orchicon (conversation lazily created on first send).
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
	m.chatStore.append(m.chatConvID, chat.ChatItem{
		Kind: chat.KindUser, Text: text, At: time.Now().UnixMilli(),
		Key: fmt.Sprintf("draft-%d", time.Now().UnixNano()), Live: true,
		// The markers ride the echo, so the operator sees "[image]" on their own message the moment it
		// appears — which is the only on-screen record that the turn carried a file.
		Attachments: m.pendingAttachMarkers(),
	})
	// SHOW IT. See pinTranscriptToTail: on a long transcript the follow intent may be off, and the echo
	// plus the activity line would land below the fold.
	m.pinTranscriptToTail()
	// The turn is in flight as soon as sendChat is evaluated (chat.Send flips the slot synchronously),
	// so the composer's stop affordance appears with it.
	m.refreshComposerHint()
	return tea.Batch(m.sendChat(m.chatConvID, text, preamble), m.onChatWake())
}

// ---------------------------------------------------------------------------
// consent card: the shell's half (see internal/tui/screens/ask/consent.go)
// ---------------------------------------------------------------------------

// SetPermissionStore wires the persistent allow/deny list. Called by the host
// once the plane's policy store exists; absent, the TUI says "unavailable".
func (m *App) SetPermissionStore(s chat.PermissionStore) { m.permStore = s }

// ConsentStore exposes the persistent store to the Ask screen's overlays.
func (m *App) ConsentStore() (chat.PermissionStore, bool) {
	if m.permStore == nil {
		return nil, false
	}
	return m.permStore, true
}

// ConsentGrants lists the session grants for a conversation, AS THE SERVER HOLDS THEM.
//
// The second answer is deliberately three-valued, because "no grants", "not asked yet" and "the plane could not
// answer" are three different facts, and a surface that shows them as one is lying to the operator about what
// they have allowed:
//
//	available=false  the last fetch FAILED — say so; do not show an empty list as the truth
//	available=true   the list IS the server's (empty because nothing has been granted yet)
func (m *App) ConsentGrants(convID string) ([]chat.SessionGrant, bool) {
	if convID == "" || !m.permGrantsLoaded[convID] {
		return nil, true // never fetched: an empty list, reported as a real one
	}
	if m.permGrantsErr[convID] != "" {
		return nil, false
	}
	return m.permGrants[convID], true
}

// ConsentGrantsLoaded reports whether the conversation's grants have been fetched yet (see ConsentGrants).
func (m *App) ConsentGrantsLoaded(convID string) bool { return m.permGrantsLoaded[convID] }

// ConsentRevoke withdraws a session grant ON THE SERVER (see Controller.RevokePermissionGrant); the
// refreshed list arrives as a chat.PermissionGrantsMsg.
func (m *App) ConsentRevoke(convID, directory string) error {
	if m.chat == nil || convID == "" || directory == "" {
		return nil
	}
	m.pendingConsentRevoke = m.chat.RevokePermissionGrant(convID, directory)
	return nil
}

// loadConsentGrants fetches the conversation's grants from the server.
func (m *App) loadConsentGrants(convID string) tea.Cmd {
	if m.chat == nil || convID == "" {
		return nil
	}
	return m.chat.LoadPermissionGrants(convID)
}

// applyConsentGrants ingests a fetched (or revoked) grant list, keeps a FAILURE visible instead of presenting an
// empty list as the truth, and re-reads any open grants overlay.
func (m *App) applyConsentGrants(msg chat.PermissionGrantsMsg) tea.Cmd {
	if msg.ConvID == "" {
		return nil
	}
	if msg.Err != "" {
		m.permGrantsErr[msg.ConvID] = msg.Err
	} else {
		m.permGrantsErr[msg.ConvID] = ""
		m.permGrants[msg.ConvID] = msg.Grants
	}
	m.permGrantsLoaded[msg.ConvID] = true
	if msg.Notice != "" {
		m.dock.SetNotice(msg.Notice)
	}
	// The overlay's rows came from the cache this replaced, so they are re-read here rather than waiting for the
	// operator to reopen it.
	m.refreshGrantsOverlay()
	return m.onChatWake()
}

// refreshGrantsOverlay asks the Ask screen to re-read its session-grant rows, through the same
// narrow type assertion the shell's other screen hooks use.
func (m *App) refreshGrantsOverlay() {
	s := m.screens[TabAsk]
	if s == nil {
		return
	}
	if g, ok := s.(interface{ RefreshGrants() }); ok {
		g.RefreshGrants()
	}
}

// ConsentSend sends text as the next user message (the clarifying-question
// card's answer).
func (m *App) ConsentSend(text string) tea.Cmd { return m.SendUserMessage(text) }

// SendUserMessage sends text as the next user message through the COMPOSER'S OWN FUNNEL — the same
// optimistic echo, the same context preamble, the same stream, the same wake.
func (m *App) SendUserMessage(text string) tea.Cmd {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	preamble := m.contextPreamble()
	if id, ok := m.runningExecutionID(); ok {
		return m.interjectExecution(id, text)
	}
	if m.chatConvID == "" {
		return m.createConversationAndSend(text, preamble)
	}
	m.chatStore.append(m.chatConvID, chat.ChatItem{
		Kind: chat.KindUser, Text: text, At: time.Now().UnixMilli(),
		Key: fmt.Sprintf("draft-%d", time.Now().UnixNano()), Live: true,
	})
	m.pinTranscriptToTail()
	m.refreshComposerHint()
	return tea.Batch(m.sendChat(m.chatConvID, text, preamble), m.onChatWake())
}

// ShowConsentAsk surfaces a pending ask as a transcript CARD. IT IS THE SHELL'S ENTRY POINT ONLY.
func (m *App) ShowConsentAsk(ask chat.PermissionAsk) tea.Cmd {
	convID := ask.ConvID
	if convID == "" {
		convID = m.chatConvID
	}
	if convID == "" {
		return nil
	}
	if !m.chatStore.drawConsentAsk(convID, ask) {
		return nil // already drawn — see drawConsentAsk's idempotence
	}
	return m.onChatWake()
}

// ConsentResolve settles a card: it records a session grant, sends a clarifying answer, notices a
// denial, and repaints.
func (m *App) ConsentResolve(askID string, dec chat.ConsentDecision, choice string) tea.Cmd {
	cmds := []tea.Cmd{}
	st := m.chatStore.consentState(m.chatConvID, askID)
	if st != nil {
		ask := st.Ask
		// SETTLE THE CARD HERE, at the ONE place every decision passes through.
		st.Decision = dec
		st.Choice = choice
		st.Note = ""
		st.OtherMode = false
		switch dec {
		case chat.DecisionAllowSession:
			dir := ask.Directory
			if dir == "" {
				dir = ask.Target
			}
			// THE GRANT IS NOT RECORDED HERE ANY MORE.
			st.Note = "session · " + dir + " (pending)"
		case chat.DecisionDeny:
			m.dock.SetNotice("denied · " + strings.TrimSpace(ask.Tool+" "+ask.Target))
		}

		// TELL THE SERVER.
		if m.chat != nil && m.chatConvID != "" {
			var pc apiv1.PermissionChoice
			answer := ""
			switch dec {
			case chat.DecisionAllowOnce:
				pc = apiv1.PermissionChoice_PERMISSION_CHOICE_ALLOW_ONCE
			case chat.DecisionAllowSession:
				pc = apiv1.PermissionChoice_PERMISSION_CHOICE_ALLOW_SESSION
			case chat.DecisionDeny:
				pc = apiv1.PermissionChoice_PERMISSION_CHOICE_DENY
			case chat.DecisionAnswer:
				// UNSPECIFIED: a question has no permission choice. The server branches
				// on the ask being a question and reads `answer`.
				pc = apiv1.PermissionChoice_PERMISSION_CHOICE_UNSPECIFIED
				answer = choice
			}
			cmds = append(cmds, m.chat.ReplyPermissionAsk(m.chatConvID, askID, pc, answer))
		}
	}
	cmds = append(cmds, m.onChatWake())
	return tea.Batch(cmds...)
}

// settleConsentScope reconciles a decided card with the SERVER's verdict on it.
func (m *App) settleConsentScope(convID, askID, appliedPrefix string) string {
	if convID == "" || askID == "" {
		return ""
	}
	st := m.chatStore.consentState(convID, askID)
	if st == nil {
		return ""
	}
	ask := st.Ask
	dir := ask.Directory
	if dir == "" {
		dir = ask.Target
	}
	if appliedPrefix == "" || st.Decision != chat.DecisionAllowSession || dir == "" {
		if st.Decision == chat.DecisionAllowSession {
			st.Note = "session · not applied"
		}
		return ""
	}
	// NO LOCAL GRANT IS RECORDED — not even on the applied verdict.
	st.Note = appliedPrefix + dir
	return dir
}

// reArmConsentClaim hands the keyboard back to a card that is still pending, after a MOUSE action
// on it.
func (m *App) reArmConsentClaim() {
	s := m.screens[TabAsk]
	if s == nil {
		return
	}
	if c, ok := s.(interface{ ReArmConsentClaim() }); ok {
		c.ReArmConsentClaim()
	}
}

// runAskOverlay runs one of the Ask screen's list surfaces.
func (m *App) runAskOverlay(kind string) tea.Cmd {
	s := m.screens[TabAsk]
	if s == nil {
		return nil
	}
	op, ok := s.(interface{ OpenAskOverlay(kind string) tea.Cmd })
	if !ok {
		return nil
	}
	m.SwitchTo(TabAsk)
	cmd := op.OpenAskOverlay(kind)
	if kind == "grants" {
		// ALWAYS FETCH ON OPEN.
		return tea.Batch(cmd, m.loadConsentGrants(m.chatConvID))
	}
	return cmd
}

// stopRunHint asks the ACTIVE screen whether it is showing something stop­pable, and returns the
// chord to advertise.
func (m *App) stopRunHint() string {
	s := m.screens[m.active]
	if s == nil {
		return ""
	}
	if h, ok := s.(interface{ StopRunHint() string }); ok {
		return h.StopRunHint()
	}
	return ""
}

// abortRunHint is the same probe for the RUN-level abort, which the Execution screen answers while
// the Runs pane holds a live run.
func (m *App) abortRunHint() string {
	s := m.screens[m.active]
	if s == nil {
		return ""
	}
	if h, ok := s.(interface{ AbortRunHint() string }); ok {
		return h.AbortRunHint()
	}
	return ""
}
