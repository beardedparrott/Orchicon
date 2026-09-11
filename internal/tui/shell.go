// shell.go — full-screen chrome (Phase 2a): the shell paints EVERY cell
// of the terminal viewport on every screen (opaque theme background, zero
// bleed-through), a horizontally CENTERED tab bar, and per-tab dropdown
// submenus (the mockup pattern: a tab opens a menu of its sub-screens).
//
// Rendering contract: View() produces exactly m.height lines, every line
// padded/truncated to m.width cells, every cell carrying the theme's
// solid background.
package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// TabMenuEntry is one dropdown row: a sub-screen of the parent tab,
// sourced from the screen inventory (nav config) — no hand-maintained
// menu list to drift.
type TabMenuEntry struct {
	Cmd    string // nav command word ("work-items")
	Label  string // human title ("Work Items")
	Source string // screenkit source name ("" = tab-level)
}

// TabMenu is the dropdown for one tab.
type TabMenu struct {
	Entries []TabMenuEntry
	Sel     int // selected row index
}

// MenuOpenID returns the tab whose dropdown is open ("" = none).
func (m *App) MenuOpenID() TabID { return m.menuOpen }

// TabMenu returns the open dropdown's model (nil when closed).
func (m *App) TabMenu() *TabMenu {
	if m.menuOpen == "" {
		return nil
	}
	tm, ok := m.menus[m.menuOpen]
	if !ok || tm == nil {
		return nil
	}
	return tm
}

// openTabMenu populates + opens tab's dropdown (entries from nav config).
func (m *App) openTabMenu(id TabID) {
	tm := &TabMenu{}
	for _, e := range m.navEntries(id) {
		tm.Entries = append(tm.Entries, TabMenuEntry{Cmd: e.Cmd, Label: e.Label, Source: e.Source})
	}
	tm.Sel = 0
	m.menus[id] = tm
	m.menuOpen = id
}

// closeTabMenu closes the open dropdown.
func (m *App) closeTabMenu() { m.menuOpen = "" }

// selectMenu moves the dropdown selection by delta (arrow keys).
func (m *App) selectMenu(delta int) {
	tm := m.TabMenu()
	if tm == nil {
		return
	}
	tm.Sel += delta
	if tm.Sel < 0 {
		tm.Sel = 0
	}
	if tm.Sel >= len(tm.Entries) {
		tm.Sel = len(tm.Entries) - 1
	}
}

// MenuSelect activates the dropdown's selected entry.
func (m *App) MenuSelect() {
	tm := m.TabMenu()
	if tm == nil {
		return
	}
	open := m.menuOpen
	if tm.Sel < 0 || tm.Sel >= len(tm.Entries) {
		m.closeTabMenu()
		return
	}
	entry := tm.Entries[tm.Sel]
	m.closeTabMenu()
	if entry.Source == "" {
		return // tab-level row: the tab is already active
	}
	m.selectScreenSource(open, entry.Source)
}

// menuEntryRow maps an absolute terminal row to the dropdown entry index:
// the panel is a rounded border (1 row) + the header row (tab title) + the
// entries, so entry i sits at top+2+i. Returns -1 when the row is not an
// entry (border, header, or outside the panel).
func (m *App) menuEntryRow(tm *TabMenu, y int) int {
	top, _ := m.menuGeometry()
	row := y - top - 2
	if row < 0 || row >= len(tm.Entries) {
		return -1
	}
	return row
}

// MenuClick activates the dropdown row under absolute terminal (x, y) —
// the mouse path for submenu entries (the geometry is derived from the
// rendered panel, so key and mouse can never disagree).
func (m *App) MenuClick(x, y int) bool {
	tm := m.TabMenu()
	if tm == nil {
		return false
	}
	row := m.menuEntryRow(tm, y)
	if row < 0 {
		return false
	}
	tm.Sel = row
	m.MenuSelect()
	return true
}

// menuActivationKey reports whether k is the submenu-open key: Enter on
// the active tab (always from content focus; from the composer only when
// the buffer is empty, so a real message keeps Enter as send), or Space
// under the same rule.
func (m *App) menuActivationKey(k tea.KeyMsg) bool {
	switch k.String() {
	case "enter":
		return m.chatFocus == focusContent || strings.TrimSpace(m.dock.Value()) == ""
	case " ", "space":
		return m.chatFocus == focusContent || m.dock.Value() == ""
	}
	return false
}

// menuHit reports whether (x, y) is inside the open dropdown panel.
func (m *App) menuHit(x, y int) bool {
	tm := m.TabMenu()
	if tm == nil {
		return false
	}
	top, left := m.menuGeometry()
	w, h := m.menuSize(tm)
	return y >= top && y < top+h && x >= left && x < left+w
}

// menuGeometry returns the panel's absolute top/left. It hangs BELOW its
// tab's span in the centered tab bar.
func (m *App) menuGeometry() (top, left int) {
	if m.menuOpen == "" {
		return 0, 0
	}
	for _, t := range Tabs {
		if t.ID == m.menuOpen {
			return tabBarRows, m.tabStartCol(t)
		}
	}
	return tabBarRows, 0
}

// menuSize computes the dropdown's width (longest entry + gutter) and
// height (header + rows + border), bounded to the terminal.
func (m *App) menuSize(tm *TabMenu) (int, int) {
	w := len(" ▸ ") + len("sub-screens")
	for _, e := range tm.Entries {
		if n := len(e.Label) + 4; n > w {
			w = n
		}
	}
	w += 4                   // panel border + inner gutter
	h := len(tm.Entries) + 4 // border + header + rows + border
	if h > m.height-tabBarRows {
		h = m.height - tabBarRows
	}
	if h < 3 {
		h = 3
	}
	if w > m.width {
		w = m.width
	}
	if w < 12 {
		w = 12
	}
	return w, h
}

// menuView renders the dropdown panel (header + rows), pre-padded to its
// full size so the overlay composes row-by-row.
func (m *App) menuView() string {
	tm := m.TabMenu()
	if tm == nil {
		return ""
	}
	w, h := m.menuSize(tm)
	var b strings.Builder
	b.WriteString(" " + m.tabTitle(m.menuOpen) + " ▾")
	b.WriteString("\n")
	if len(tm.Entries) == 0 {
		b.WriteString(" (no sub-screens)")
		b.WriteString("\n")
	}
	for i, e := range tm.Entries {
		marker := "  "
		if i == tm.Sel {
			marker = "▸ "
		}
		b.WriteString(marker + e.Label)
		b.WriteString("\n")
	}
	// Pad every line to the panel width (w-2: rounded borders consume
	// one cell each side), truncate to the same budget, then frame.
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	for i, l := range lines {
		l = " " + l
		plain := len([]rune(l))
		if plain > w-2 {
			l = string([]rune(l)[:w-2])
			plain = w - 2
		}
		if plain < w-2 {
			l += strings.Repeat(" ", w-2-plain)
		}
		if i == 0 {
			lines[i] = theme.MenuTitle.Render(l)
		} else if i-1 == tm.Sel && len(tm.Entries) > 0 {
			lines[i] = theme.MenuRowSel.Render(l)
		} else {
			lines[i] = theme.MenuRow.Render(l)
		}
	}
	for len(lines) < h-2 {
		lines = append(lines, theme.MenuRow.Render(strings.Repeat(" ", w-2)))
	}
	if len(lines) > h-2 {
		lines = lines[:h-2]
	}
	return theme.MenuPanel.Render(strings.Join(lines, "\n"))
}

// overlayBoxAt splices a multi-line box over base starting at absolute
// (top, left) — row-by-row, ANSI-aware. Used by the tab dropdown, the
// slash palette, and the centered overlays.
func (m *App) overlayBoxAt(base, box string, top, left int) string {
	rows := strings.Split(base, "\n")
	bw := lipgloss.Width(box)
	for i, br := range strings.Split(box, "\n") {
		r := top + i
		if r < 0 {
			continue
		}
		if r >= len(rows) {
			break
		}
		rows[r] = overlayRow(rows[r], br, left, bw)
	}
	return strings.Join(rows, "\n")
}

// overlayCentered composes box centered over the base view.
func (m *App) overlayCentered(base, box string) string {
	rows := strings.Split(base, "\n")
	bh := len(strings.Split(box, "\n"))
	top := (len(rows) - bh) / 2
	if top < 0 {
		top = 0
	}
	left := (m.width - lipgloss.Width(box)) / 2
	if left < 0 {
		left = 0
	}
	return m.overlayBoxAt(base, box, top, left)
}

// composeView renders the shell with the open dropdown overlaid on the
// opaque full-screen base.
func (m *App) composeView(base string) string {
	menu := m.menuView()
	if menu == "" {
		return base
	}
	top, left := m.menuGeometry()
	w, _ := m.menuSize(m.TabMenu())
	rows := strings.Split(base, "\n")
	menuRows := strings.Split(menu, "\n")
	for i, mr := range menuRows {
		r := top + i
		if r >= len(rows) {
			break
		}
		rows[r] = overlayRow(rows[r], mr, left, w)
	}
	return strings.Join(rows, "\n")
}

// overlayRow splices overlay into row starting at column left (both
// ANSI-aware). Cells right of the overlay keep the row's background.
func overlayRow(row, overlay string, left, width int) string {
	cols := lipgloss.Width(row)
	var prefix, suffix string
	if left > 0 {
		if left >= cols {
			prefix = row + strings.Repeat(" ", left-cols)
		} else {
			prefix = ansi.Truncate(row, left, "")
		}
	}
	total := lipgloss.Width(prefix) + width
	if rest := cols - total; rest > 0 {
		keep := lipgloss.Width(prefix) + width
		suffix = ansi.Truncate(ansi.Truncate(row, keep+rest, ""), cols, "")
		// Truncate(row, keep+rest) keeps exactly keep+rest visible cells;
		// drop the first keep of them to isolate the tail.
		if keep > 0 {
			suffix = ansi.TruncateLeft(suffix, keep, "")
		}
	}
	return prefix + overlay + suffix
}

// tabBarLayout picks the label form and inter-tab gap for the current
// viewport: the widest tier that fits wins. Narrow widths drop the
// inter-tab gaps first, then the ordinal prefixes, so all SEVEN tabs stay
// inside the viewport (the pills still separate visually via their own
// padding). m.width <= 0 (unsized — e.g. registry introspection) always
// uses the full numbered form, so the numbered chrome is the default.
func (m App) tabBarLayout() (labels []string, gap string) {
	full := make([]string, len(Tabs))
	plain := make([]string, len(Tabs))
	for i, t := range Tabs {
		full[i] = t.Ordinal + "·" + t.Title
		plain[i] = t.Title
	}
	if m.width <= 0 {
		return full, " "
	}
	for _, tier := range []struct {
		labels []string
		gap    string
	}{{full, " "}, {full, ""}, {plain, " "}, {plain, ""}} {
		if lipgloss.Width(m.tabBarRender(tier.labels, tier.gap)) <= m.width {
			return tier.labels, tier.gap
		}
	}
	return plain, ""
}

// tabBarRender renders the tab bar from explicit labels + gap (the active
// tab styled, the rest inactive), wrapped in the TabBar container style.
func (m App) tabBarRender(labels []string, gap string) string {
	parts := make([]string, len(labels))
	for i, label := range labels {
		if Tabs[i].ID == m.active {
			parts[i] = theme.TabActive.Render(label)
		} else {
			parts[i] = theme.TabInactive.Render(label)
		}
	}
	return theme.TabBar.Render(strings.Join(parts, gap))
}

// tabBarView renders the centered top tab bar at the widest layout tier
// that fits the viewport.
func (m App) tabBarView() string {
	labels, gap := m.tabBarLayout()
	return m.tabBarRender(labels, gap)
}

// centeredTabBarView centers the tab bar row across m.width columns and
// paints the full row with the theme background (the mockup's chrome).
func (m App) centeredTabBarView() string {
	bar := m.tabBarView()
	w := lipgloss.Width(bar)
	if w >= m.width {
		return theme.ScreenBg.Render(ansi.Truncate(bar, m.width, ""))
	}
	pad := (m.width - w) / 2
	left := strings.Repeat(" ", pad)
	right := strings.Repeat(" ", m.width-w-pad)
	return theme.ScreenBg.Render(left + bar + right)
}

// tabTitle resolves a tab's GUI label.
func (m App) tabTitle(id TabID) string {
	for _, t := range Tabs {
		if t.ID == id {
			return t.Title
		}
	}
	return string(id)
}

// tabLabelStarts returns the visible terminal column where each tab's
// rendered label begins in the centered tab bar (Tabs order, -1 when the
// label is not found), using the SAME layout tier the bar was rendered
// with — so key navigation, the dropdown geometry, and the mouse hit-test
// can never disagree with what is on screen.
//
// Mouse X is a terminal COLUMN (0-based) but strings.Index returns a BYTE
// offset into the ANSI-styled render — measure the visible width of the
// styled prefix with lipgloss.Width (ANSI-aware) to get the label's true
// starting column. Labels are located sequentially so a label that repeats
// a substring earlier in the styled render cannot shift the result.
func (m App) tabLabelStarts() []int {
	labels, gap := m.tabBarLayout()
	bar := m.tabBarRender(labels, gap)
	barW := lipgloss.Width(bar)
	offset := 0
	if barW > 0 && barW < m.width {
		offset = (m.width - barW) / 2
	}
	starts := make([]int, len(Tabs))
	from := 0
	for i := range Tabs {
		idx := strings.Index(bar[from:], labels[i])
		if idx < 0 {
			starts[i] = -1
			continue
		}
		idx += from
		starts[i] = offset + lipgloss.Width(bar[:idx])
		from = idx + len(labels[i])
	}
	return starts
}

// tabStartCol returns the visible terminal column where tab's label begins
// in the centered tab bar (ANSI-aware) — used by the tab mouse hit-test,
// the dropdown geometry, and their tests.
func (m App) tabStartCol(t Tab) int {
	starts := m.tabLabelStarts()
	for i, tt := range Tabs {
		if tt.ID == t.ID {
			if starts[i] < 0 {
				return 0
			}
			return starts[i]
		}
	}
	return 0
}

// TabClick maps a mouse click on the tab bar (row 0) to the tab whose
// rendered span contains column x. Spans TILE the bar (a tab owns the cells
// from its label's start up to the next label's start, so the pills' own
// padding and the inter-tab gaps stay clickable) and the last tab keeps
// the historical +3-cell trailing tolerance — so the hit-test can never
// overlap two tabs on a narrow (gap-dropping) layout. Returns (tabID, true)
// when a tab was hit.
func (m App) TabClick(x int) (TabID, bool) {
	starts := m.tabLabelStarts()
	labels, _ := m.tabBarLayout()
	for i, t := range Tabs {
		start := starts[i]
		if start < 0 {
			continue
		}
		end := start + lipgloss.Width(labels[i]) + 3
		if i+1 < len(starts) && starts[i+1] >= 0 {
			end = starts[i+1]
		}
		if x >= start && x < end {
			return t.ID, true
		}
	}
	return "", false
}

// menuHandleKey processes a key while a dropdown is open. Returns
// (handled, cmd). Left/esc close; up/down navigate; enter selects; a
// different tab's chord switches the menu (or closes it when already on
// that tab).
func (m *App) menuHandleKey(k tea.KeyMsg) (bool, tea.Cmd) {
	switch k.String() {
	case "esc", "left", "shift+tab":
		m.closeTabMenu()
		return true, nil
	case "up", "k":
		m.selectMenu(-1)
		return true, nil
	case "down", "j":
		m.selectMenu(1)
		return true, nil
	case "enter", " ", "space":
		m.MenuSelect()
		return true, nil
	case "ctrl+c":
		return false, nil // quit route handles it
	}
	for _, t := range Tabs {
		if t.Chord == k.String() {
			if t.ID == m.menuOpen {
				m.closeTabMenu()
			} else {
				m.SwitchTo(t.ID)
				m.EnsureSubscriptions(t.ID)
				m.openTabMenu(t.ID)
			}
			return true, nil
		}
	}
	return false, nil
}

// tabBarRows is the tab chrome's height: the centered tab row + the
// full-width underline rule.
const tabBarRows = 2

// tabBarUnderlineView renders the one-row full-width rule under the
// centered tab bar.
func (m App) tabBarUnderlineView() string {
	if m.width < 1 {
		return ""
	}
	return theme.ScreenBg.Render(strings.Repeat("─", m.width))
}
