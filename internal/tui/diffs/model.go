package diffs

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/stream"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// Tab is the pane's tab switcher.
type Tab string

const (
	TabDiff     Tab = "diff"
	TabTree     Tab = "tree"
	TabTimeline Tab = "timeline"
)

// Paneless sentinel for a pane with no owner (the toggle is a no-op when the
// active screen has no diff-relevant session).
const NoneOwner = ""

// Model is the pane's bubbletea sub-model. It owns the Store (tenant resolve
// + durable fetch), the live StreamFileEdits subscription (registered on the
// shell's registry), the scroll viewport, the tab switcher, and the OSC 52
// `y` copy binding.
//
// The shell ("App") owns open/tab/selected state so it survives tab switches
// (the GUI persists open/tab/selected at the host), and calls SetOwner to
// point the pane at the active session when the screen changes.
type Model struct {
	Store *Store
	reg   *subs.Registry
	cl    *client.Clients

	Width  int
	Height int

	SelectedPath string
	Tab          Tab

	// Data (rebuilt on Load/SetOwner).
	groups  []FileGroup
	rows    []Row
	maxSeq  int64
	Status  string
	Loading bool
	Err     string

	scroll int

	// closeReq is set when the user clicks the pane's "✕" close button
	// (the mouse toggle area). The shell polls it after forwarding a mouse
	// event and, when set, closes the pane and consumes the request.
	closeReq bool

	// OSC 52 copy pending (emitted once on the next View()).
	copyBuf    string
	copyActive bool

	sub         *stream.Sub[*apiv1.StreamFileEditsResponse]
	ownerKind   string
	ownerID     string
	isLive      bool
	streamArmed bool
}

// NewModel builds a pane over the shell's client set + registry.
func NewModel(cl *client.Clients, reg *subs.Registry) *Model {
	return &Model{
		Store:  NewStore(cl),
		reg:    reg,
		cl:     cl,
		Tab:    TabDiff,
		Status: "idle",
	}
}

// SetSize updates the pane dimensions (content width excludes the border).
func (m *Model) SetSize(w, h int) {
	m.Width, m.Height = w, h
}

// Open activates the pane (renders its frame). Does not refetch.
func (m *Model) Open() { m.Status = "open" }

// Close tears down the live stream.
func (m *Model) Close() {
	if m.sub != nil {
		m.sub.Close()
		m.sub = nil
	}
	m.streamArmed = false
}

// SetOwner points the pane at a session's ledger. When ownerID changes it
// re-fetches the durable ledger and (re)arms the live StreamFileEdits stream
// only when the owner is live (a running execution / open Ask conversation).
// Returns the tea.Cmd(s) to run.
func (m *Model) SetOwner(kind, id string, isLive bool) tea.Cmd {
	if kind == "" || id == "" {
		// No diff-relevant owner — clear the pane. SetOwner always runs on
		// the tea loop (route/appMsg), so mutate directly (no background
		// goroutine like the fetch closure below).
		m.clear()
		return nil
	}
	if m.ownerID == id && m.ownerKind == kind && m.streamArmed {
		return nil // same owner, already set up
	}
	// Tear down any previous live sub.
	if m.sub != nil {
		m.sub.Close()
		m.sub = nil
	}
	m.ownerKind, m.ownerID, m.isLive = kind, id, isLive
	m.streamArmed = false
	// Bubbletea runs the returned Cmd in a background goroutine, so the fetch
	// closure must NOT mutate the model — it only fetches and hands the data
	// back in the message; Update applies it on the tea loop below. Setting
	// Loading here (on the tea loop) is safe.
	m.Loading = true

	fetch := func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		snap, err := m.Store.Fetch(ctx, kind, id)
		return FetchDoneMsg{Snapshot: snap, Err: err}
	}
	// If the tenant is already resolved (a prior fetch cached it), arm the
	// live stream immediately on the tea loop. Otherwise arm it in the
	// FetchDoneMsg handler once the durable fetch caches the tenant.
	if isLive && m.Store.Tenant() != "" {
		m.armStream()
	}
	return fetch
}

// armStream subscribes to StreamFileEdits for the current owner via the
// shell's registry (mirrors the execution screen's EnsureSubscriptions).
// MUST run on the tea loop (it reads / mutates m.sub and reads the cached
// tenant), never inside a stream goroutine — avoiding a data race and the
// empty-tenant rejection.
func (m *Model) armStream() {
	if m.streamArmed || m.ownerID == "" || !m.isLive {
		return
	}
	tenant := m.Store.Tenant()
	if tenant == "" {
		return // tenant not resolved yet; arm after the next FetchDoneMsg
	}
	m.sub = m.reg.FileEdits(m.cl, tenant, m.ownerKind, m.ownerID)
	m.streamArmed = true
}

func (m *Model) clear() {
	m.groups = nil
	m.rows = nil
	m.maxSeq = 0
	m.SelectedPath = ""
	m.Status = "idle"
	m.Err = ""
}

// rowsForSelected parses the selected file's latest unified diff.
func (m *Model) rowsForSelected() []Row {
	for _, g := range m.groups {
		if g.Path == m.SelectedPath {
			if len(g.Edits) == 0 {
				return nil
			}
			last := g.Edits[len(g.Edits)-1]
			return ParseUnifiedDiff(last.GetUnifiedDiff())
		}
	}
	return nil
}

// SelectPath selects a file (its diff becomes the pane's rows).
func (m *Model) SelectPath(path string) {
	if path == m.SelectedPath {
		return
	}
	m.SelectedPath = path
	m.rows = m.rowsForSelected()
	m.scroll = 0
}

// SetTab switches the pane tab.
func (m *Model) SetTab(t Tab) { m.Tab = t }

// Live returns whether the current owner is live.
func (m *Model) Live() bool { return m.isLive }

// Owner returns the current owner (kind, id).
func (m *Model) Owner() (kind, id string) { return m.ownerKind, m.ownerID }

// HasOwner reports whether the pane has a diff-relevant owner loaded.
func (m *Model) HasOwner() bool { return m.ownerID != "" && m.ownerKind != "" }

// Scroll moves the viewport by delta lines.
func (m *Model) Scroll(delta int) {
	m.scroll += delta
	if m.scroll < 0 {
		m.scroll = 0
	}
	if total := m.lineCount(); total > 0 && m.scroll > total-1 {
		m.scroll = total - 1
	}
}

// lineCount is the number of rendered diff rows.
func (m *Model) lineCount() int { return len(m.rows) }

// viewHeight is the number of body rows the pane can show (the pane's Height
// minus the tab-bar row that occupies the first content line). Used for
// page-scroll and the diff-body viewport so the pane never renders more rows
// than it has room for.
func (m *Model) viewHeight() int {
	h := m.Height - 1 // tab bar row
	if h < 1 {
		h = 1
	}
	return h
}

// CopySelectedDiff stages an OSC 52 copy of the selected file's unified diff
// (copied verbatim from the ledger — not a re-render) so `y` works over SSH.
func (m *Model) CopySelectedDiff() bool {
	for _, g := range m.groups {
		if g.Path != m.SelectedPath || len(g.Edits) == 0 {
			continue
		}
		last := g.Edits[len(g.Edits)-1]
		diff := last.GetUnifiedDiff()
		if diff == "" {
			return false
		}
		m.copyBuf = "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(diff)) + "\x1b\\"
		m.copyActive = true
		return true
	}
	return false
}

// Update handles pane-scoped key/mouse/status messages. It receives the
// messages the shell forwarded while the pane is open AND any status/poke
// events from the registry. Returns a tea.Cmd.
func (m *Model) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
		return nil
	case FetchDoneMsg:
		m.Loading = false
		if msg.Err != nil {
			m.Err = msg.Err.Error()
			m.Status = "error"
			return nil
		}
		snap := msg.Snapshot
		if snap != nil {
			m.maxSeq = snap.MaxDurableSeq
			m.groups = GroupByFile(snap.Edits)
			m.rows = m.rowsForSelected()
			m.Err = ""
			m.Status = "ready"
			// Default the tree selection to the first changed file (if any).
			if m.SelectedPath == "" && len(m.groups) > 0 {
				m.SelectedPath = m.groups[0].Path
				m.rows = m.rowsForSelected()
			}
		}
		// The durable fetch caches the tenant; if this owner is live, arm the
		// live stream now (on the tea loop) and park a re-armable event poke.
		if m.isLive && !m.streamArmed && m.Store.Tenant() != "" {
			m.armStream()
		}
		if m.streamArmed {
			return m.reg.WaitEventPoke("file-edits")
		}
		return nil
	case subs.EventPokeMsg:
		if msg.Name != "file-edits" || !m.streamArmed {
			return nil
		}
		// Merge live events into the groups and re-derive rows.
		m.mergeLive()
		return m.reg.WaitEventPoke("file-edits")
	case subs.StatusMsg:
		if msg.Name == "file-edits" {
			m.Status = string(msg.Status)
		}
		return m.reg.WaitStatus("file-edits")
	case tea.KeyMsg:
		return m.handleKey(msg)
	case tea.MouseMsg:
		return m.handleMouse(msg)
	}
	return nil
}

func (m *Model) handleKey(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "up", "k":
		m.Scroll(-1)
		return nil
	case "down", "j":
		m.Scroll(1)
		return nil
	case "pgup":
		m.Scroll(-m.viewHeight() / 2)
		return nil
	case "pgdown":
		m.Scroll(m.viewHeight() / 2)
		return nil
	case "g":
		m.scroll = 0
		return nil
	case "G":
		m.scroll = m.lineCount() - 1
		return nil
	case "h", "l":
		// Switch tab: l = next, h = previous (the two directions must be
		// asymmetric — TabDiff→TabTree on `l`, TabDiff→TabTimeline on `h`).
		// A single branch that maps both h and l to the same next tab is wrong.
		if k.String() == "l" {
			switch m.Tab {
			case TabDiff:
				m.Tab = TabTree
			case TabTree:
				m.Tab = TabTimeline
			case TabTimeline:
				m.Tab = TabDiff
			}
		} else {
			switch m.Tab {
			case TabDiff:
				m.Tab = TabTimeline
			case TabTree:
				m.Tab = TabDiff
			case TabTimeline:
				m.Tab = TabTree
			}
		}
		return nil
	case "y":
		m.CopySelectedDiff()
		return nil
	}
	return nil
}

func (m *Model) handleMouse(ev tea.MouseMsg) tea.Cmd {
	if ev.Action == tea.MouseActionMotion || ev.Action == tea.MouseActionRelease {
		return nil // drag/release never consumed: Shift+drag stays native
	}
	switch ev.Button {
	case tea.MouseButtonWheelUp:
		m.Scroll(-3)
	case tea.MouseButtonWheelDown:
		m.Scroll(3)
	case tea.MouseButtonLeft:
		m.click(ev.X, ev.Y)
	}
	return nil
}

// click resolves a mouse click to a tab / file row within the pane.
// (x, y) are terminal-global coordinates as forwarded by the shell. The pane
// is a left rail rendered below the shell tab bar: the shell tab bar occupies
// row 0 and its bottom border row 1 (the TabBar style has a bottom border),
// so the pane's content starts at terminal row 2 with its left border at
// column 0 — its tab bar is terminal row 2 and its body starts at row 3.
func (m *Model) click(x, y int) {
	if m.Width <= 0 {
		return
	}
	// The pane's content starts one column right of the left border, so a
	// content-relative X is the terminal X minus the border column.
	contentX := x - 1
	// Tab bar is the pane's first content row — terminal row 2 (shell tab bar
	// row 0 + its bottom border row 1).
	if y == 2 {
		// The far-right "✕" close button is the last hit region before the
		// padding to the pane's content width (it sits right of the last tab).
		if m.closeAt(contentX) {
			m.closeReq = true
			return
		}
		m.clickTab(contentX)
		return
	}
	if y < 3 {
		return
	}
	// Body row: terminal row 3 is body row 0 (after the tab bar at row 2).
	row := y - 3
	if m.Tab != TabDiff && row >= 0 && row < len(m.groups) {
		m.SelectPath(m.groups[row].Path)
	}
}

// clickTab selects the tab under a content-relative X click, reproducing the
// tabBar() layout: a leading space, then each padded label with a 1-space
// separator. The hit region covers the label text AND its 1-cell horizontal
// padding (the whole clickable button, matching the GUI tab button), so a
// click anywhere on a tab activates it. Computed rather than hard-coded so
// tab clicks stay correct if the theme padding ever changes.
func (m *Model) clickTab(contentX int) {
	// Layout constants mirror tabBar(): leading " " (1), per-label
	// horizontal padding (1 each side), and a 1-space separator between tabs.
	const leadingSpace = 1
	const tabPadding = 1
	const tabSep = 1
	// First label's text starts after the leading space + its left padding.
	textStart := leadingSpace + tabPadding
	for _, t := range []Tab{TabDiff, TabTree, TabTimeline} {
		end := textStart + len(t)
		// Widen the hit to cover the label's padding so the whole button is
		// clickable (the padding cells do not overlap other tabs — separated
		// by the 1-space separator).
		if contentX >= textStart-tabPadding && contentX < end+tabPadding {
			m.Tab = t
			return
		}
		// Advance past: this label's text + right padding (tabPadding) +
		// separator (tabSep) + next label's left padding (tabPadding).
		textStart = end + tabPadding + tabSep + tabPadding
	}
}

// closeAt reports whether a content-relative X click lands on the docked
// "✕" close button. The glyph sits after the three tab labels + their
// separators, at the content-relative column glyphX() (measured from the
// RENDERED tab-bar prefix so it tracks the actual layout and any theme padding
// change). A click on the glyph (or its 1-cell padding) is the mouse-toggle
// close; the trailing padding to the pane's content width stays inert.
func (m *Model) closeAt(contentX int) bool {
	if m.Width <= 0 {
		return false
	}
	g := m.glyphX()
	const tabPadding = 1
	return contentX >= g && contentX < g+ansi.StringWidth("✕")+tabPadding
}

// GlyphX is the content-relative column of the docked "✕" close button in
// the tab bar. Exported so the shell (and its tests) can hit the mouse
// toggle area at the same coordinate the pane draws it.
func (m *Model) GlyphX() int { return m.glyphX() }

// glyphX is the content-relative column of the first "✕" glyph cell in the
// tab bar. It reproduces tabBar()'s label/separator/leading-space layout so
// the click hit-region matches what is drawn, without depending on the pane
// width or the active tab (both tab styles pad identically).
func (m *Model) glyphX() int {
	var b strings.Builder
	b.WriteString(" ")
	for i, t := range []Tab{TabDiff, TabTree, TabTimeline} {
		if t == m.Tab {
			b.WriteString(theme.DiffTabActive.Render(string(t)))
		} else {
			b.WriteString(theme.DiffTabInactive.Render(string(t)))
		}
		if i < len([]Tab{TabDiff, TabTree, TabTimeline})-1 {
			b.WriteString(" ")
		}
	}
	b.WriteString(" ") // the separating space before the glyph
	return ansi.StringWidth(b.String())
}

// TakeCloseRequest reports and clears a pending mouse-toggle close request
// (set when the user clicks the pane's ✕). The shell calls it after
// forwarding a mouse event; when true it closes the pane.
func (m *Model) TakeCloseRequest() bool {
	if m.closeReq {
		m.closeReq = false
		return true
	}
	return false
}

// mergeLive folds the buffered live stream events into the groups and
// re-derives the selected file's rows.
func (m *Model) mergeLive() {
	if m.sub == nil || m.ownerID == "" {
		return
	}
	live := m.sub.Events()
	edits := make([]*apiv1.FileEdit, 0, len(live))
	for _, e := range live {
		if e.GetEvent() != nil {
			edits = append(edits, e.GetEvent())
		}
	}
	if len(edits) == 0 {
		return
	}
	durable := flattenGroups(m.groups)
	merged := MergeEdits(durable, edits)
	m.groups = GroupByFile(merged)
	m.rows = m.rowsForSelected()
}

// flattenGroups collapses the grouped edits back into a flat, seq-ordered
// list (the durable side of MergeEdits).
func flattenGroups(groups []FileGroup) []*apiv1.FileEdit {
	var out []*apiv1.FileEdit
	for _, g := range groups {
		out = append(out, g.Edits...)
	}
	return out
}

// View renders the pane frame (tabs header + body), emitting any pending
// OSC 52 copy once.
func (m *Model) View() string {
	var b strings.Builder
	// If a copy is pending, emit it once at the top of the frame.
	if m.copyActive {
		b.WriteString(m.copyBuf)
		b.WriteString("\n")
		m.copyActive = false
	}
	b.WriteString(theme.DiffPanel.Render(m.rawView()))
	return b.String()
}

// rawView builds the pane content (header + body) without the outer panel.
func (m *Model) rawView() string {
	var b strings.Builder
	b.WriteString(m.tabBar())
	b.WriteString("\n")
	if m.Err != "" {
		b.WriteString(theme.ErrorText.Render("  " + truncate(m.Err, m.Width)))
		return b.String()
	}
	if m.Loading {
		b.WriteString(theme.HintText.Render("  loading…"))
		return b.String()
	}
	switch m.Tab {
	case TabDiff:
		b.WriteString(m.diffBody())
	case TabTree:
		b.WriteString(m.treeBody())
	case TabTimeline:
		b.WriteString(m.timelineBody())
	}
	return b.String()
}

func (m *Model) tabBar() string {
	tabs := []Tab{TabDiff, TabTree, TabTimeline}
	var b strings.Builder
	b.WriteString(" ")
	for i, t := range tabs {
		label := string(t)
		if t == m.Tab {
			b.WriteString(theme.DiffTabActive.Render(label))
		} else {
			b.WriteString(theme.DiffTabInactive.Render(label))
		}
		if i < len(tabs)-1 {
			b.WriteString(" ")
		}
	}
	// Docked close button on the FAR RIGHT of the tab bar (the GUI's
	// PanelLeftClose mirror). Right-padded to the pane's content width so the
	// pane never renders wider than its rail.
	b.WriteString(" ")
	b.WriteString(theme.DiffClose.Render("✕"))
	// The tab bar must not exceed the pane's content width (no horizontal
	// overflow / tearing); pad the remainder so the underline stays flush.
	if cur := ansi.StringWidth(b.String()); cur < m.Width {
		b.WriteString(strings.Repeat(" ", m.Width-cur))
	}
	return b.String()
}

func (m *Model) diffBody() string {
	if len(m.rows) == 0 {
		return theme.HintText.Render("  select a file (tree) to view its diff")
	}
	content := RenderPane(m.rows, m.Width, currentProfile())
	lines := strings.Split(content, "\n")
	// The pane has Height rows total, but the tab bar (rawView's first row)
	// consumes one, so the diff body gets Height-1 rows — otherwise the pane
	// would render Height+1 rows and overflow (tearing / pushing the footer).
	viewH := m.viewHeight()
	start := m.scroll
	if start > len(lines) {
		start = len(lines)
	}
	end := start + viewH
	if end > len(lines) {
		end = len(lines)
	}
	if start > end {
		start = end
	}
	visible := lines[start:end]
	return strings.Join(visible, "\n")
}

func (m *Model) treeBody() string {
	if len(m.groups) == 0 {
		return theme.HintText.Render("  no changed files yet")
	}
	var b strings.Builder
	for i, g := range m.groups {
		sel := g.Path == m.SelectedPath
		line := fmt.Sprintf(" %s +%d −%d", g.Path, g.Adds, g.Dels)
		if sel {
			b.WriteString(theme.DiffFileSel.Render(line))
		} else {
			b.WriteString(line)
		}
		if i < len(m.groups)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

func (m *Model) timelineBody() string {
	if len(m.groups) == 0 {
		return theme.HintText.Render("  no edits yet")
	}
	var b strings.Builder
	for i, g := range m.groups {
		line := fmt.Sprintf(" %s %s (%s)", g.Path, g.Kind, g.LastTool)
		b.WriteString(line)
		if i < len(m.groups)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// FetchDoneMsg / OwnerSetMsg are the pane's internal async completions
// (exported so the shell can forward them to the pane's Update).
// FetchDoneMsg carries the fetched snapshot (and any error) rather than
// mutating the model inside a background goroutine — bubbletea runs each
// Cmd off the tea loop, so Update applies the data on the loop instead.
type FetchDoneMsg struct {
	Snapshot *Snapshot
	Err      error
}
type OwnerSetMsg struct{}
