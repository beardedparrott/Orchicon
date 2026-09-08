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
	sel   int    // selected index into the filtered candidate list
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
}

// paletteSelected returns the currently-selected command (or nil).
func (m *App) paletteSelected() *SlashCommand {
	if m.palette.sel >= 0 && m.palette.sel < len(m.palette.filter) {
		return m.palette.filter[m.palette.sel]
	}
	return nil
}

// paletteView renders the overlay box centered over the content area,
// listing matching commands with name + description.
func (m *App) paletteView() string {
	if !m.palette.open {
		return ""
	}
	var b strings.Builder
	b.WriteString(theme.DetailKey.Render("  / command palette  "))
	b.WriteString(theme.HintText.Render("(type to filter · ↑/↓/click · enter select · esc close)"))
	b.WriteString("\n")
	if len(m.palette.filter) == 0 {
		b.WriteString(theme.HintText.Render("  no matching commands") + "\n")
	} else {
		for i, c := range m.palette.filter {
			line := "  " + c.Name
			pad := 30 - len(c.Name)
			if pad < 1 {
				pad = 1
			}
			line += strings.Repeat(" ", pad) + c.Desc
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

// connectOverlayView renders the overlay box.
func (m *App) connectOverlayView() string {
	var b strings.Builder
	b.WriteString(theme.ListTitle.Render("  / connect (in place)  "))
	b.WriteString("\n\n")
	b.WriteString(theme.DetailValue.Render(m.palette.connectMsg))
	b.WriteString("\n\n")
	b.WriteString(theme.HintText.Render("  esc: close · q: quit orch · the shell is still running (alt-screen intact)"))
	return theme.HelpOverlay.Render(b.String())
}

// connectHandleKey processes keys while the connect overlay is open.
// Returns (handled, cmd).
func (m *App) connectHandleKey(k tea.KeyMsg) (bool, tea.Cmd) {
	switch k.String() {
	case "esc":
		m.closeConnectOverlay()
		return true, nil
	case "q", "ctrl+c":
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
