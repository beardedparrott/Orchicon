package tui

// palette.go — the slash-command palette that appears on "/" in the
// composer. Fuzzy-filtered, arrow/click navigable, enter-select,
// esc-close. Registry-driven from the slash registry (no second command
// list, so the palette can never drift from /help or behavior).

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// palette is the composer '/' overlay state.
type palette struct {
	open  bool
	query string // the slash prefix typed so far ("" for first '/')
	sel    int // selected index into the filtered candidate list
	scroll int // viewport offset so the palette never overflows a short terminal
	// connect state: /connect opens an in-place re-auth overlay that never
	// tears down alt-screen or exits the process.
	connectOpen bool
	connectMsg  string
	filter      []*SlashCommand
}

// paletteOpen reports whether the '/' palette is showing.
func (p *palette) PaletteOpen() bool { return p != nil && p.open }

// openPalette starts the palette; the query is the text after "lead".
func (m *App) openPalette() {
	// The composer already holds "/<word>"; seed the query from it so the
	// filter starts narrowed to what the user typed.
	m.palette.open = true
	m.palette.query = strings.TrimPrefix(m.dock.Value(), "/")
	m.palette.sel = 0
	m.refreshPalette()
}

// closePalette hides the palette (focus stays in the composer).
func (m *App) closePalette() {
	m.palette.open = false
	m.palette.sel = 0
	m.palette.scroll = 0
	m.palette.filter = nil
}

// refreshPalette recomputes the filtered candidate list from the live
// slash registry (primary names only, no-dup).
func (m *App) refreshPalette() {
	var out []*SlashCommand
	seen := map[string]bool{}
	for _, n := range m.slash.names {
		c := m.slash.byName[n]
		if n != c.Name {
			continue // alias rendered with its primary
		}
		if seen[c.Name] {
			continue
		}
		if strings.Contains(c.Name, m.palette.query) {
			out = append(out, c)
		}
		seen[c.Name] = true
	}
	m.palette.filter = out
	if m.palette.sel >= len(out) {
		m.palette.sel = len(out) - 1
	}
	if m.palette.sel < 0 {
		m.palette.sel = 0
	}
	m.clampPaletteScroll()
}

// paletteSelect moves the selection by delta (arrow keys).
func (m *App) paletteSelect(delta int) {
	if len(m.palette.filter) == 0 {
		return
	}
	m.palette.sel += delta
	if m.palette.sel < 0 {
		m.palette.sel = 0
	}
	if m.palette.sel >= len(m.palette.filter) {
		m.palette.sel = len(m.palette.filter) - 1
	}
	m.ensurePaletteSelVisible()
}

// paletteSelected returns the currently-selected command (or nil).
func (m *App) paletteSelected() *SlashCommand {
	if m.palette.sel >= 0 && m.palette.sel < len(m.palette.filter) {
		return m.palette.filter[m.palette.sel]
	}
	return nil
}

// paletteVisibleRows is how many candidate rows the floating palette can
// render given the terminal height (header + border + padding consume ~6
// rows). Bounding here keeps the overlay from overflowing a short terminal
// (e.g. 80×24, where an unbounded list of ~20 commands would scroll off).
func (m *App) paletteVisibleRows() int {
	if m.height < 10 {
		return 3
	}
	return m.height - 6
}

// paletteContentWidth returns the max content width (cells) for one palette
// line. The HelpOverlay style adds a rounded border (1 each side) and
// horizontal padding (2 each side) = 6 cells, so the content must stop short
// of the terminal width to avoid horizontal overflow (regression: an
// unbounded description rendered 98 cells at 80 cols, clipping off-edge).
func (m *App) paletteContentWidth() int {
	max := m.width - 6
	if max < 24 {
		return 24
	}
	if max > 70 {
		return 70
	}
	return max
}

// clampPaletteScroll keeps the viewport within the candidate list.
func (m *App) clampPaletteScroll() {
	vis := m.paletteVisibleRows()
	max := len(m.palette.filter) - vis
	if max < 0 {
		max = 0
	}
	if m.palette.scroll > max {
		m.palette.scroll = max
	}
	if m.palette.scroll < 0 {
		m.palette.scroll = 0
	}
}

// ensurePaletteSelVisible scrolls so the selected row is in the window
// (keeps arrow navigation useful at the top/bottom of a long list).
func (m *App) ensurePaletteSelVisible() {
	vis := m.paletteVisibleRows()
	if m.palette.sel < m.palette.scroll {
		m.palette.scroll = m.palette.sel
	}
	if m.palette.sel >= m.palette.scroll+vis {
		m.palette.scroll = m.palette.sel - vis + 1
	}
	m.clampPaletteScroll()
}

// paletteView renders the overlay box centered over the content area,
// listing matching commands with name + description. The candidate list is
// windowed (via palette.scroll) so it never overflows the terminal height.
func (m *App) paletteView() string {
	if !m.palette.open {
		return ""
	}
	m.ensurePaletteSelVisible()
	vis := m.paletteVisibleRows()
	max := m.paletteContentWidth()
	var b strings.Builder
	// Header: bound the plain hint text so title+hint fit max cells, then
	// style each part separately (truncate BEFORE styling, since truncateRight
	// is byte/rune-based and would split ANSI escape sequences if styled first).
	title := "  / command palette  "
	hint := "(type to filter · ↑/↓/click · enter select · esc close)"
	if len([]rune(title))+len([]rune(hint)) > max {
		hint = truncateRight(hint, max-len([]rune(title)))
	}
	b.WriteString(theme.DetailKey.Render(title))
	b.WriteString(theme.HintText.Render(hint))
	b.WriteString("\n")
	if len(m.palette.filter) == 0 {
		b.WriteString(theme.HintText.Render(truncateRight("  no matching commands", max)) + "\n")
	} else {
		lo := m.palette.scroll
		hi := lo + vis
		if hi > len(m.palette.filter) {
			hi = len(m.palette.filter)
		}
		for i := lo; i < hi; i++ {
			c := m.palette.filter[i]
			line := "  " + c.Name
			pad := 30 - len(c.Name)
			if pad < 1 {
				pad = 1
			}
			line += strings.Repeat(" ", pad) + c.Desc
			// Bound the PLAIN content to max cells before styling (styling
			// adds ANSI that truncateRight can't safely split).
			line = truncateRight(line, max)
			if i == m.palette.sel {
				b.WriteString(theme.ListItemSelected.Render(line))
			} else {
				b.WriteString(theme.ListItem.Render(line))
			}
			b.WriteString("\n")
		}
	}
	return theme.HelpOverlay.Render(strings.TrimRight(b.String(), "\n"))
}

// paletteHandleKey processes a key while the palette is open. Returns
// (handled, cmd). Composer editing keys (letters) update the filter; esc
// closes; up/down navigate; enter/tab selects.
func (m *App) paletteHandleKey(k tea.KeyMsg) (bool, tea.Cmd) {
	switch k.String() {
	case "up":
		m.paletteSelect(-1)
		return true, nil
	case "down":
		m.paletteSelect(1)
		return true, nil
	case "esc":
		m.closePalette()
		return true, nil
	case "enter", "tab":
		if c := m.paletteSelected(); c != nil {
			m.closePalette()
			// Run the command; insert its usage hint into the composer so
			// arg-taking commands (e.g. /wi <id>) continue from a known text.
			m.dock.SetValue(c.Name)
			return true, c.Run(m, nil)
		}
		return true, nil
	default:
		// Backspace edits the query; a printable rune appends to it.
		if k.String() == "backspace" {
			if len(m.palette.query) > 0 {
				m.palette.query = m.palette.query[:len(m.palette.query)-1]
				m.refreshPalette()
			}
			return true, nil
		}
		if len(k.Runes) > 0 {
			m.palette.query += string(k.Runes)
			m.refreshPalette()
			return true, nil
		}
		return false, nil
	}
}

// placeholder kept to satisfy the lipgloss import guard.
var _ = lipgloss.NewStyle

// ConnectOverlayOpen reports whether the /connect in-place overlay is
// showing (never quits the process — first-run stays in main.go).
func (m *App) ConnectOverlayOpen() bool { return m.palette.connectOpen }

// openConnectOverlay opens the in-place re-auth overlay.
func (m *App) openConnectOverlay() {
	m.palette.connectOpen = true
	m.palette.connectMsg = "Reconnect: open the Orchicon web GUI (Settings → API keys) to (re)create/rotate your key, then re-run orch with the new credential."
}

// closeConnectOverlay closes it (esc).
func (m *App) closeConnectOverlay() { m.palette.connectOpen = false }

// connectOverlayView renders the overlay box. The message is wrapped to
// the overlay's content width so a long re-auth hint never overflows the
// terminal horizontally.
func (m *App) connectOverlayView() string {
	max := m.paletteContentWidth()
	var b strings.Builder
	b.WriteString(theme.ListTitle.Render("  / connect (in place)  "))
	b.WriteString("\n\n")
	b.WriteString(theme.DetailValue.Render(wordWrap(m.palette.connectMsg, max)))
	b.WriteString("\n\n")
	b.WriteString(theme.HintText.Render(wordWrap("  esc: close · q: quit orch · the shell is still running (alt-screen intact)", max)))
	return theme.HelpOverlay.Render(b.String())
}

// wordWrap wraps s to width w cells at spaces (plain text only — no ANSI).
// Used to bound long overlay copy horizontally.
func wordWrap(s string, w int) string {
	if w < 1 || s == "" {
		return s
	}
	var out []string
	cur := ""
	for _, word := range strings.Fields(s) {
		if cur == "" {
			cur = word
			continue
		}
		if len([]rune(cur))+1+len([]rune(word)) <= w {
			cur += " " + word
		} else {
			out = append(out, cur)
			cur = word
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return strings.Join(out, "\n")
}

// connectHandleKey processes keys while the connect overlay is open.
// Returns (handled, cmd).
func (m *App) connectHandleKey(k tea.KeyMsg) (bool, tea.Cmd) {
	switch k.String() {
	case "esc":
		// Cancel the re-auth: the shell stays put (in-place overlay). Clear
		// reconnectRequested so a later normal quit does NOT re-open the
		// connection screen — the loop is first-run fallback only, and the
		// in-place /connect never exits the process.
		m.reconnectRequested = false
		m.closeConnectOverlay()
		return true, nil
	case "q", "ctrl+c":
		m.reconnectRequested = false // quitting for real, not re-auth
		m.quitting = true
		return true, tea.Quit
	default:
		return true, nil // swallow keys while the overlay is up
	}
}

// connectRequestRun runs the /connect command in place: it opens the
// overlay instead of quitting (the old behavior exited the program).
func (m *App) runConnectCommand() tea.Cmd {
	m.openConnectOverlay()
	return nil
}
