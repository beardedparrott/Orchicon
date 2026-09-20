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

	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// TabMenuEntry is one dropdown row: a sub-screen of the parent tab,
// sourced from the screen inventory (nav config) — no hand-maintained
// menu list to drift.
type TabMenuEntry struct {
	Cmd    string // nav command word ("work-items")
	Label  string // human title ("Work Items")
	Source string // screenkit source name ("" = tab-level)
	// Action is carried for verb rows (Ask's New / Conversations); nil for
	// source rows, which focus their source instead.
	Action func(m *App) tea.Cmd
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
//
// This is the DATA half (what the menu contains), deliberately separate from where the keyboard
// goes: the composer's Enter/Space opens a dropdown with an empty buffer, and THAT path must not
// move the keyboard — the operator is mid-compose, and moving focus would send their next letter to
// the screen instead of the box. Taking focus is the job of openTabWithMenu, the tab-SELECTION
// gesture.
func (m *App) openTabMenu(id TabID) {
	tm := &TabMenu{}
	for _, e := range m.navEntries(id) {
		tm.Entries = append(tm.Entries, TabMenuEntry{Cmd: e.Cmd, Label: e.Label, Source: e.Source, Action: e.Action})
	}
	tm.Sel = 0
	m.menus[id] = tm
	m.menuOpen = id
}

// openTabWithMenu is the ONE meaning of selecting a tab by chord or by click: GO TO IT, DROP ITS
// SUBMENU DOWN, and PUT THE KEYBOARD IN IT.
//
// This existed as THREE copies and they had already drifted: the tab CLICK did SwitchTo + openTabMenu,
// menuHandleKey did SwitchTo + openTabMenu, and the global CHORD route did SwitchTo ALONE. So the same
// key meant different things depending on whether a dropdown happened to be open — the chord switched
// tabs and left the menu shut, which is what the operator hit.
//
// WITH THE MENU SHUT, ENTER WENT WHEREVER LAY UNDERNEATH, and that is why the report was screen
// dependent: "when hitting the F#, it automatically gains focus on the first submenu in the list and
// hitting enter doesn't bring down the submenu". On Ask the conversations rail claims Enter while a
// rail is up, a screen claiming keys (an open form, a latched search) claims it everywhere, and a
// non-empty composer takes it as send — none of which is the submenu. With the keyboard on the bar
// the menu owns the arrows and Enter outright, because menuHandleKey runs ahead of every screen
// claim, so the key after a chord means one thing on every screen.
func (m *App) openTabWithMenu(id TabID) {
	// Re-selecting the tab whose menu is already down CLOSES it — the toggle the click route,
	// SwitchTo's same-tab path and menuHandleKey all already honour. A re-press still re-arms the
	// streams (SwitchTo's same-tab path deliberately does not, since it cannot tell a re-press from
	// a re-selection).
	if m.active == id && m.menuOpen == id {
		m.closeTabMenu()
		m.EnsureSubscriptions(id)
		return
	}
	wasActive := m.active
	m.SwitchTo(id)
	// Arm the streams ONCE. SwitchTo arms internally only on its same-tab path (above, where it is
	// skipped); for a NEW active tab it deliberately does not, so the arm happens here.
	if m.active != wasActive {
		m.EnsureSubscriptions(id)
	}
	m.openTabMenu(id)
	m.setFocus(focusTabs)
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
	// Selecting an entry HANDS THE ARROWS TO THE SCREEN.
	//
	// This is step 4 of the operator's model — "then down/arrow keys move through
	// the pane items" — and without it the keys stayed wherever they were (the
	// composer), because selecting a source is a screen-side change that never
	// moved shell focus. Content focus is what makes the next arrow key reach the
	// list the operator just chose.
	m.setFocus(focusContent)
	// A verb row runs its action (Ask's New / Conversations).
	if entry.Action != nil {
		entry.Action(m)
		return
	}
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

// menuActivationKey reports whether k opens the active tab's dropdown.
//
// The TAB BAR opens it, on Enter — and that is the ONLY way a submenu appears
// ("submenus should only pop up if you enter on them"). Tab used to pop it open
// as it advanced, which ALSO left the menu up eating the arrows.
//
// From the COMPOSER, Enter/Space still open it with an empty buffer, so a real
// message keeps Enter as send (the original Phase-3.5 gesture).
//
// Nothing else opens it, and no VERTICAL key does from anywhere: "no menus should
// grab up/down until you actually select it".
func (m *App) menuActivationKey(k tea.KeyMsg) bool {
	if m.chatFocus == focusTabs {
		return k.String() == "enter"
	}
	if m.chatFocus != focusComposer {
		return false
	}
	switch k.String() {
	case "enter":
		return strings.TrimSpace(m.dock.Value()) == ""
	case " ", "space":
		return m.dock.Value() == ""
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
	for i, br := range strings.Split(box, "\n") {
		r := top + i
		if r < 0 {
			continue
		}
		if r >= len(rows) {
			break
		}
		rows[r] = overlayRow(rows[r], br, left)
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

// modalInnerWidth is the width a modal's CONTENT has to lay itself out in — the panel's interior,
// i.e. the modal width minus its two border cells. It exists so a form can be told the width it
// actually has instead of the outer width it does not: laying a field out to `modalWidth` overflows the
// panel by exactly the two border cells, and Panel.Pad then truncates the field's last two columns.
func (m *App) modalInnerWidth() int {
	w := m.modalWidth() - 2
	if w < 8 {
		w = 8
	}
	return w
}

// modalPanel wraps a modal's body in a SOLID, uniformly-sized bordered panel.
//
// THE OPERATOR'S ASK, VERBATIM: "Weird visual bug when renaming a conversation. Probably should just
// be its own modal with a solid background. This also happens when applying to a category."
//
// The modals used to splice the form's raw View() straight over the frame, and a kit2 form's View is
// NOT a rectangle — its title and hint rows are written at their natural width while its field rows
// are padded to the form width. Two consequences, both measured: the base showed through the gaps
// inside the modal, and the ragged rows tripped the splice defect in overlayRow and shifted everything
// after the modal sideways.
//
// A kit2.Panel is a rectangle BY CONSTRUCTION: every interior row is padded (and truncated) to the
// inner width and painted with the opaque screen background, and the whole thing is normalized to
// exactly w×h. The conversations rail has used one since it was built; the modals now do too, so a
// modal cannot be anything but solid.
//
// The panel is sized to its CONTENT, not to the viewport: a viewport-tall panel would paint a wall of
// background over the screen behind it, which is a worse artefact than the one being fixed.
func (m *App) modalPanel(body string, w int) string {
	if w < 10 {
		w = 10
	}
	lines := strings.Split(body, "\n")
	// Trailing blank lines would only add empty interior rows.
	for len(lines) > 0 && strings.TrimSpace(ansi.Strip(lines[len(lines)-1])) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		lines = []string{""}
	}
	// No panel TITLE: the form already renders its own title as its first line, and putting it on the
	// border as well would print it twice.
	p := kit2.NewPanel("", w, len(lines)+2)
	p.SetContent(strings.Join(lines, "\n"))
	return p.View()
}

// composeView renders the shell with the open dropdown overlaid on the
// opaque full-screen base.
func (m *App) composeView(base string) string {
	menu := m.menuView()
	if menu == "" {
		return base
	}
	top, left := m.menuGeometry()
	rows := strings.Split(base, "\n")
	menuRows := strings.Split(menu, "\n")
	for i, mr := range menuRows {
		r := top + i
		if r >= len(rows) {
			break
		}
		rows[r] = overlayRow(rows[r], mr, left)
	}
	return strings.Join(rows, "\n")
}

// overlayRow splices overlay into row starting at column left (both ANSI-aware). Cells right of the
// overlay keep the row's background.
//
// THE OVERLAY'S WIDTH IS MEASURED HERE, NOT PASSED IN, and that is the fix for a real visual defect.
// The caller used to pass the BOX's MAXIMUM line width and it was then used as the width of EVERY row.
// A box whose lines are not all the same width — which the kit2 forms are BY DESIGN, since their title
// and hint rows are written at their natural width while their field rows are padded — therefore
// spliced a SHORT row as if it were a wide one, and `keep` landed far to the right of where the glyphs
// actually ended. That DELETED the base cells in between and returned a row shorter than the frame,
// which the final fillView then padded at the FAR RIGHT: everything after the modal shifted sideways,
// and base content stayed visible inside the modal's own rectangle.
//
// Measured before the fix: a 5-cell row inside a 25-cell box turned a 40-cell row into a 25-cell one,
// and 18 base cells survived within the box's rectangle — the operator's "weird visual bug".
func overlayRow(row, overlay string, left int) string {
	cols := lipgloss.Width(row)
	ow := lipgloss.Width(overlay)
	var prefix, suffix string
	if left > 0 {
		if left >= cols {
			prefix = row + strings.Repeat(" ", left-cols)
		} else {
			prefix = ansi.Truncate(row, left, "")
		}
	}
	// Keep everything from the cell AFTER the overlay's own last cell. Using the overlay's measured
	// width rather than a box-wide constant is what makes a ragged overlay safe.
	keep := lipgloss.Width(prefix) + ow
	if rest := cols - keep; rest > 0 {
		suffix = ansi.TruncateLeft(row, keep, "")
	}
	return prefix + overlay + suffix
}

// tabSeparator joins a tab's KEY to its title in the key-labelled chrome — "F1·Ask Orchicon".
const tabSeparator = "·"

// tabFullLabel is the key-labelled form of a tab's label — "F1·Ask Orchicon". Shared by the layout
// and the renderer so the underline can never be applied to a label that is not actually the
// key-labelled form.
//
// The printed key double-serves as the mockup's numbered chrome (the ordinal prefix), which is why
// the prefix is the key rather than a decorative number.
func tabFullLabel(t Tab) string { return t.Ordinal + tabSeparator + t.Title }

// underlineTabKey wraps a run in the underline attribute (SGR 4 / 24), leaving the surrounding
// styling alone. The tab's key label is underlined because it IS a key, not decoration — the
// operator asked for exactly that: "We should probably underline the numbers", and then, when the
// chords became function keys, "underline the F1-F7 individually". Each tab's own label is a separate
// run, so each is underlined on its own.
func underlineTabKey(s string) string { return "\x1b[4m" + s + "\x1b[24m" }

// tabBarLeftPad and tabBarPillPad measure the STYLES' own padding instead of assuming a cell count,
// so a theme change cannot silently shift every click target. tabLabelStarts computes columns from
// these, so they must agree with what the styles actually draw.
func tabBarLeftPad() int { return lipgloss.Width(theme.TabBar.Render("x")) - 1 }
func tabBarPillPad() int { return (lipgloss.Width(theme.TabInactive.Render("x")) - 1) / 2 }

// tabBarDisplay is a label as DRAWN: the key-labelled chrome gets its key underlined. The plain tier
// (titles only) is drawn as-is, so the underline can never appear on a label with no key in it.
func (m App) tabBarDisplay(i int, label string) string {
	if i >= len(Tabs) {
		return label
	}
	if label != tabFullLabel(Tabs[i]) {
		return label
	}
	return underlineTabKey(Tabs[i].Ordinal) + label[len(Tabs[i].Ordinal):]
}

// tabBarLayout picks the label form and inter-tab gap for the current
// viewport: the widest tier that fits wins. Narrow widths drop the
// inter-tab gaps first, then the key prefixes, so all SEVEN tabs stay
// inside the viewport (the pills still separate visually via their own
// padding). m.width <= 0 (unsized — e.g. registry introspection) always
// uses the full key-labelled form, so the labelled chrome is the default.
//
// NOTE the key prefixes are only LABELS: the chords are global routes and work at every width, so a
// narrow terminal loses the reminder, not the binding.
func (m App) tabBarLayout() (labels []string, gap string) {
	full := make([]string, len(Tabs))
	plain := make([]string, len(Tabs))
	for i, t := range Tabs {
		full[i] = tabFullLabel(t)
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

// tabBarBuilt renders the tab bar AND reports the visible column where each label's text begins.
//
// The bar is now ONLY the tabs: the modifier label that used to sit at its left ("alt+ 1·Ask
// Orchicon") is gone, because the label printed at each tab IS the whole key ("F1") and a modifier
// prefix would explain a modifier that no longer exists.
//
// Both come from ONE pass, which is the point: the columns used to be recovered afterwards by
// searching the STYLED render for the plain label (strings.Index), which works only while the label
// appears in the bar byte-for-byte. Underlining the number breaks that (the render now interleaves
// escape codes inside the label), and a search that fails returns -1, i.e. a silently dead click
// target. Computing the columns as the bar is BUILT cannot drift from what was drawn.
func (m App) tabBarBuilt(labels []string, gap string) (string, []int) {
	leftPad := tabBarLeftPad()
	pillPad := tabBarPillPad()

	parts := make([]string, len(labels))
	starts := make([]int, len(labels))
	col := leftPad
	for i, label := range labels {
		disp := m.tabBarDisplay(i, label)
		if i < len(Tabs) && Tabs[i].ID == m.active {
			parts[i] = theme.TabActive.Render(disp)
		} else {
			parts[i] = theme.TabInactive.Render(disp)
		}
		// The label's TEXT starts after the pill's own left padding.
		starts[i] = col + pillPad
		col += pillPad + lipgloss.Width(disp) + pillPad + lipgloss.Width(gap)
	}
	return theme.TabBar.Render(strings.Join(parts, gap)), starts
}

// tabBarRender renders the tab bar from explicit labels + gap (the active
// tab styled, the rest inactive), wrapped in the TabBar container style.
func (m App) tabBarRender(labels []string, gap string) string {
	bar, _ := m.tabBarBuilt(labels, gap)
	return bar
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
// label TEXT begins in the centered tab bar (Tabs order), using the SAME layout tier the bar was
// rendered with — so key navigation, the dropdown geometry, and the mouse hit-test can never disagree
// with what is on screen. It includes the bar's centering offset.
//
// The columns come from tabBarBuilt, which records them AS IT DRAWS. They used to be recovered by
// searching the styled render for each label (strings.Index), which silently returned -1 — a dead
// click target — the moment the label stopped appearing in the bar byte-for-byte, which is exactly
// what underlining the number does.
func (m App) tabLabelStarts() []int {
	labels, gap := m.tabBarLayout()
	bar, starts := m.tabBarBuilt(labels, gap)
	barW := lipgloss.Width(bar)
	if barW > 0 && barW < m.width {
		offset := (m.width - barW) / 2
		for i := range starts {
			starts[i] += offset
		}
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
			// ONE meaning, shared with the click and the global chord route. This copy already got
			// the switch-and-drop-the-menu half right, which is exactly how the routing came to be
			// inconsistent: whether the chord opened a menu depended on whether one was already up.
			m.openTabWithMenu(t.ID)
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
