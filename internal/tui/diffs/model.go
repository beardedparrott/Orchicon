package diffs

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

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

// Model is the pane's bubbletea sub-model.
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
	// The live scrollbar DRAG's anchor: the terminal row the grab started at and the scroll offset the
	// content had then.
	dragActive bool
	dragFromY  int
	dragScroll int

	// renderGen bumps whenever the ROWS change, and renderCache holds the rendered physical lines they
	// produced at a given width and profile.
	renderGen   int
	renderCache renderCache

	// closeReq is set when the user clicks the pane's "✕" close button (the mouse toggle area).
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
	m.clampScroll()
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

// StreamArmed reports whether the live StreamFileEdits subscription is up for the current owner.
func (m *Model) StreamArmed() bool { return m.streamArmed }

// SetOwner points the pane at a session's ledger.
func (m *Model) SetOwner(kind, id string, isLive bool) tea.Cmd {
	if kind == "" || id == "" {
		// No diff-relevant owner — clear the pane.
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
	// Bubbletea runs the returned Cmd in a background goroutine, so the fetch closure must NOT mutate
	// the model — it only fetches and hands the data back in the message.
	m.Loading = true

	fetch := func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		snap, err := m.Store.Fetch(ctx, kind, id)
		return FetchDoneMsg{Snapshot: snap, Err: err}
	}
	// If the tenant is already resolved (a prior fetch cached it), arm the live stream immediately on
	// the tea loop.
	if isLive && m.Store.Tenant() != "" {
		m.armStream()
	}
	return fetch
}

// MUST run on the tea loop (it reads / mutates m.sub and reads the cached tenant), never inside a
// stream goroutine — avoiding a data race and the empty-tenant rejection.
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
	m.setRows(nil)
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
	m.setRows(m.rowsForSelected())
	m.scroll = 0
}

// SetTab switches the pane tab.
func (m *Model) SetTab(t Tab) {
	m.Tab = t
	m.clampScroll()
}

// Live returns whether the current owner is live.
func (m *Model) Live() bool { return m.isLive }

// Owner returns the current owner (kind, id).
func (m *Model) Owner() (kind, id string) { return m.ownerKind, m.ownerID }

// HasOwner reports whether the pane has a diff-relevant owner loaded.
func (m *Model) HasOwner() bool { return m.ownerID != "" && m.ownerKind != "" }

// Scroll moves the viewport by delta lines, clamped to the wrapped extent.
func (m *Model) Scroll(delta int) {
	m.scroll += delta
	m.clampScroll()
}

// clampScroll pins m.scroll into [0, maxScroll] so the viewport can never sit past the last
// rendered page (the mismatch that let `G`/pgdn/scroll overshoot once wrapping made rendered lines.
func (m *Model) clampScroll() {
	if m.scroll < 0 {
		m.scroll = 0
	}
	if ms := m.maxScroll(); m.scroll > ms {
		m.scroll = ms
	}
}

// maxScroll is the greatest valid scroll offset: the last rendered page's top
// line. Zero when the content fits the viewport.
func (m *Model) maxScroll() int {
	ms := m.visibleLines() - m.viewHeight()
	if ms < 0 {
		ms = 0
	}
	return ms
}

// visibleLines is the number of PHYSICAL lines the ACTIVE tab shows at the pane's current body
// width.
type renderCache struct {
	gen     int
	width   int
	profile termenv.Profile
	lines   []string
}

// setRows is the ONLY way the pane's rows change, so the render cache cannot be left stale by a caller that
// forgot to invalidate it. Every assignment goes through here.
func (m *Model) setRows(rows []Row) {
	m.rows = rows
	m.renderGen++
}

// renderedLines renders the rows — or returns what the last render produced, when nothing it
// depends on has changed.
func (m *Model) renderedLines() []string {
	width := m.bodyWidth()
	profile := currentProfile()
	c := m.renderCache
	if c.gen == m.renderGen && c.width == width && c.profile == profile && c.lines != nil {
		return c.lines
	}
	lines := RenderLines(m.rows, width, profile)
	m.renderCache = renderCache{gen: m.renderGen, width: width, profile: profile, lines: lines}
	return lines
}

func (m *Model) visibleLines() int {
	switch m.Tab {
	case TabDiff:
		if len(m.rows) == 0 {
			return 0
		}
		return len(m.renderedLines())
	default:
		// Tree/Timeline: one rendered line per group (D4 — list rows do not
		// wrap, so the group<->row click mapping stays 1:1).
		return len(m.groups)
	}
}

// lineCount is the number of rendered lines for the active tab (kept as the
// scroll-extent accessor the existing tests assert against).
func (m *Model) lineCount() int { return m.visibleLines() }

// paneInnerWidth is the width of the pane's inner content — the pane's budget minus the DiffPanel's
// 1-cell LEFT border.
func (m *Model) paneInnerWidth() int {
	w := m.Width - 1
	if w < 1 {
		w = 1
	}
	return w
}

// bodyWidth is the width available to BODY TEXT: the pane's inner content minus the scrollbar's
// reserved 1-cell column AND the cell the shell's rail-resize handle uses.
func (m *Model) bodyWidth() int {
	w := m.Width - 3
	if w < 1 {
		w = 1
	}
	return w
}

// ScrollbarHit reports whether a terminal (x, y) is on the pane's scrollbar column — the cell the
// operator grabs to jump or to drag the viewport.
func (m *Model) ScrollbarHit(x, y int) bool {
	if m.Width <= 0 {
		return false
	}
	// contentX is the terminal X minus the panel's left border.
	if x-1 != m.bodyWidth() {
		return false
	}
	return y >= paneBodyRow && y < paneBodyRow+m.viewHeight()
}

// ScrollbarJumpTo moves the viewport so the terminal row it is given maps to the same relative
// position.
func (m *Model) ScrollbarJumpTo(termY int) {
	m.jumpToBodyRow(termY - paneBodyRow)
}

// ScrollbarThumbAt reports whether a terminal row is on the THUMB rather than the track.
func (m *Model) ScrollbarThumbAt(termY int) bool {
	start, length := scrollbarThumb(m.scroll, m.viewHeight(), m.visibleLines())
	if length == 0 {
		return false
	}
	row := termY - paneBodyRow
	return row >= start && row < start+length
}

// ScrollbarDragStart begins a bar gesture, returning whether the grab landed on the THUMB.
func (m *Model) ScrollbarDragStart(termY int) bool {
	onThumb := m.ScrollbarThumbAt(termY)
	if !onThumb {
		m.ScrollbarJumpTo(termY)
	}
	m.dragFromY = termY
	m.dragScroll = m.scroll
	m.dragActive = true
	return onThumb
}

// ScrollbarDragTo moves the viewport with the pointer, so the grabbed point of the bar stays under
// it.
func (m *Model) ScrollbarDragTo(termY int) {
	if !m.dragActive {
		return
	}
	viewH := m.viewHeight()
	total := m.visibleLines()
	if viewH < 1 || total <= viewH {
		return
	}
	_, thumbLen := scrollbarThumb(m.scroll, viewH, total)
	travel := viewH - thumbLen // the thumb's own range: the rows it can move through
	reach := m.maxScroll()
	if travel <= 0 || reach <= 0 {
		return
	}
	m.scroll = m.dragScroll + (termY-m.dragFromY)*reach/travel
	m.clampScroll()
}

// ScrollbarDragEnd ends the gesture, so the pane stops following a pointer that is no longer held.
func (m *Model) ScrollbarDragEnd() {
	m.dragActive = false
}

// viewHeight is the number of body rows the pane can show (the pane's Height minus the tab-bar row
// that occupies the first content line).
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

// Update handles pane-scoped key/mouse/status messages.
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
			m.setRows(m.rowsForSelected())
			m.Err = ""
			m.Status = "ready"
			// Default the tree selection to the first changed file (if any).
			if m.SelectedPath == "" && len(m.groups) > 0 {
				m.SelectedPath = m.groups[0].Path
				m.setRows(m.rowsForSelected())
			}
			// The rows (and therefore the rendered line count) just changed underneath the scroll — a fetch
			// that returns a SHORTER diff (a new owner.
			m.clampScroll()
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
		m.scroll = m.maxScroll()
		return nil
	case "h", "l":
		// Switch tab: l = next, h = previous (the two directions must be asymmetric — TabDiff→TabTree on
		// `l`, TabDiff→TabTimeline on `h`).
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
		// The two tabs index DIFFERENT things (physical diff lines vs one line per group), so a Diff scroll
		// must not leak into a short Tree list (which would leave scroll past the end and render a blank.
		m.clampScroll()
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

// Shell chrome geometry the pane's hit-tests depend on.
const (
	paneTopRow  = 3 // the pane's tab bar / first content row
	paneBodyRow = 4 // the pane's first body row (below its tab bar)
)

// click resolves a mouse click to a tab / file row within the pane.
func (m *Model) click(x, y int) {
	if m.Width <= 0 {
		return
	}
	// The pane's content starts one column right of the left border, so a
	// content-relative X is the terminal X minus the border column.
	contentX := x - 1
	// ROW MATH: the shell paints row 0 = centered tab bar, row 1 = its underline rule, row 2 = the
	// one-row gap, and the BODY starts at row 3.
	if y == paneTopRow {
		// The far-right "✕" close button is the last hit region before the
		// padding to the pane's content width (it sits right of the last tab).
		if m.closeAt(contentX) {
			m.closeReq = true
			return
		}
		m.clickTab(contentX)
		return
	}
	if y < paneBodyRow {
		return
	}
	// Body row: terminal row 4 is body row 0 (after the tab bar at row 3).
	bodyRow := y - paneBodyRow
	// SCROLLBAR HIT, resolved BEFORE the body hit-test: a press on the reserved bar column (content X
	// == bodyWidth — one cell INSIDE the panel edge.
	if contentX >= m.bodyWidth() {
		// ONE IMPLEMENTATION with the shell's drag: the shell claims the bar's press (so a selection region
		// is never set over the rail) and calls the same method.
		m.ScrollbarJumpTo(y)
		return
	}
	// VIEWPORT-AWARE row: the list is scrolled, so the row under the cursor is the scroll offset plus
	// the row within the viewport.
	row := m.scroll + bodyRow
	if m.Tab != TabDiff && row >= 0 && row < len(m.groups) {
		// Parity with the GUI mounts (DiffSidebar.tsx:109,112 / :138,141): a file-row click selects AND
		// focuses the diff, in one event.
		m.SelectPath(m.groups[row].Path)
		m.SetTab(TabDiff)
	}
}

// jumpToBodyRow resolves a click on the scrollbar's reserved column to a proportional scroll
// offset, so the operator can throw the viewport to where the bar was pressed.
func (m *Model) jumpToBodyRow(bodyRow int) {
	viewH := m.viewHeight()
	total := m.visibleLines()
	if viewH < 1 || total <= viewH {
		return // everything fits — the bar is inert
	}
	if bodyRow < 0 {
		bodyRow = 0
	}
	if bodyRow > viewH-1 {
		bodyRow = viewH - 1
	}
	reach := m.maxScroll()
	if viewH == 1 || reach <= 0 {
		m.scroll = 0
		m.clampScroll()
		return
	}
	m.scroll = bodyRow * reach / (viewH - 1)
	m.clampScroll()
}

// clickTab selects the tab under a content-relative X click, reproducing the tabBar() layout: a
// leading space, then each padded label with a 1-space separator.
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
		// Widen the hit to cover the label's padding so the whole button is clickable (the padding cells do
		// not overlap other tabs — separated by the 1-space separator).
		if contentX >= textStart-tabPadding && contentX < end+tabPadding {
			// SetTab (not a bare m.Tab =) so the stale-scroll clamp runs: a mouse tab switch out of a scrolled
			// Diff must not leave the list tabs scrolled past their (much shorter) extent.
			m.SetTab(t)
			return
		}
		// Advance past: this label's text + right padding (tabPadding) +
		// separator (tabSep) + next label's left padding (tabPadding).
		textStart = end + tabPadding + tabSep + tabPadding
	}
}

// closeAt reports whether a content-relative X click lands on the docked "✕" close button.
func (m *Model) closeAt(contentX int) bool {
	if m.Width <= 0 {
		return false
	}
	g := m.glyphX()
	const tabPadding = 1
	return contentX >= g && contentX < g+ansi.StringWidth("✕")+tabPadding
}

// GlyphX is the content-relative column of the docked "✕" close button in the tab bar.
func (m *Model) GlyphX() int { return m.glyphX() }

// glyphX is the content-relative column of the first "✕" glyph cell in the tab bar.
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

// TakeCloseRequest reports and clears a pending mouse-toggle close request (set when the user
// clicks the pane's ✕).
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
	m.setRows(m.rowsForSelected())
	// Live events rebuild the rows (and so the rendered line count) while the
	// operator is scrolled: clamp, or a shrinking diff leaves a blank viewport.
	m.clampScroll()
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
		b.WriteString(theme.ErrorText.Render("  " + truncate(m.Err, m.paneInnerWidth()-2)))
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
	// Docked close button on the FAR RIGHT of the tab bar (the GUI's PanelLeftClose mirror).
	b.WriteString(" ")
	b.WriteString(theme.DiffClose.Render("✕"))
	// The tab bar must not exceed the pane's INNER content width (no horizontal overflow / tearing);
	// pad the remainder so the underline stays flush.
	if cur := ansi.StringWidth(b.String()); cur < m.paneInnerWidth() {
		b.WriteString(strings.Repeat(" ", m.paneInnerWidth()-cur))
	}
	return b.String()
}

func (m *Model) diffBody() string {
	if len(m.rows) == 0 {
		return theme.HintText.Render("  select a file (tree) to view its diff")
	}
	// The SINGLE renderer the scroll clamps also read: wrapping a row to several physical lines changes
	// the line count, and the viewport must slice exactly what visibleLines counted.
	lines := m.renderedLines()
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
	return m.withScrollbar(lines[start:end], len(lines))
}

func (m *Model) treeBody() string {
	if len(m.groups) == 0 {
		return theme.HintText.Render("  no changed files yet")
	}
	w := m.bodyWidth()
	rows := make([]string, 0, len(m.groups))
	for _, g := range m.groups {
		// A list ROW is an index entry, not diff content (D4): it is ellipsized to the body width rather
		// than wrapped, so group<->row stays 1:1 and the viewport-aware click keeps resolving the row
		// under.
		line := truncate(fmt.Sprintf(" %s +%d −%d", g.Path, g.Adds, g.Dels), w)
		if g.Path == m.SelectedPath {
			rows = append(rows, theme.DiffFileSel.Render(line))
			continue
		}
		// THE ROW MUST CARRY THEME COLOURS.
		rows = append(rows, theme.ListItem.Render(line))
	}
	return m.listViewport(rows)
}

func (m *Model) timelineBody() string {
	if len(m.groups) == 0 {
		return theme.HintText.Render("  no edits yet")
	}
	w := m.bodyWidth()
	rows := make([]string, 0, len(m.groups))
	for _, g := range m.groups {
		line := truncate(fmt.Sprintf(" %s %s (%s)", g.Path, g.Kind, g.LastTool), w)
		if g.Path == m.SelectedPath {
			// Same selected-row treatment as treeBody: without it a Timeline click changed the shared selection
			// invisibly, which is why clicking a Timeline row read as doing nothing.
			rows = append(rows, theme.DiffFileSel.Render(line))
			continue
		}
		// Styled for the same reason as treeBody: a raw row carries no theme colour and renders in
		// whatever the terminal's default foreground happens to be.
		rows = append(rows, theme.ListItem.Render(line))
	}
	return m.listViewport(rows)
}

// listViewport applies the SAME window as diffBody to a list of rendered rows: [scroll :
// scroll+viewHeight], then the scrollbar.
func (m *Model) listViewport(rows []string) string {
	viewH := m.viewHeight()
	start := m.scroll
	if start > len(rows) {
		start = len(rows)
	}
	end := start + viewH
	if end > len(rows) {
		end = len(rows)
	}
	if start > end {
		start = end
	}
	return m.withScrollbar(rows[start:end], len(rows))
}

// FetchDoneMsg / OwnerSetMsg are the pane's internal async completions (exported so the shell can
// forward them to the pane's Update).
type FetchDoneMsg struct {
	Snapshot *Snapshot
	Err      error
}
type OwnerSetMsg struct{}
