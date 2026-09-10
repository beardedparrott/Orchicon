package tui

// palette.go — the slash-command palette that appears on "/" in the
// composer. Fuzzy-filtered, arrow/click navigable, enter-select,
// esc-close. Registry-driven from the slash registry (no second command
// list, so the palette can never drift from /help or behavior).

import (
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
	"github.com/beardedparrott/orchicon/internal/tui/connection"
	"github.com/beardedparrott/orchicon/internal/tui/diffs"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// palette is the composer '/' overlay state.
type palette struct {
	open  bool
	query string // the slash prefix typed so far ("" for first '/')
	sel    int // selected index into the filtered candidate list
	scroll int // viewport offset so the palette never overflows a short terminal
	// connect state: /connect opens an in-place re-auth overlay — the FULL
	// first-run connection screen (URL + auth-method toggle + credential
	// field) hosted inside the running shell. It never tears down
	// alt-screen or exits the process.
	connectOpen bool
	connectMsg  string
	// connectModel is the embedded connection form (connection.Model).
	// The shell routes keys into it while the overlay is open and applies
	// the profile + reconnects when it reports a successful probe.
	connectForm *connection.Model
	connectBusy bool
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
// render given the terminal height. The palette floats ABOVE the composer
// (Phase 2a): its box must fit between the tab chrome and the composer
// line without ever covering the composer row (header + border + padding
// consume ~6 rows). Bounding keeps the overlay from overflowing a short
// terminal (e.g. 80×24, where an unbounded list of ~20 commands would
// scroll off).
func (m *App) paletteVisibleRows() int {
	vis := m.height - m.dock.Lines() - 6
	if vis < 3 {
		return 3
	}
	return vis
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

// paletteComposerView composes the palette box floating ABOVE the
// composer: the box's bottom edge sits on the composer line, so the
// composer line + the user's typed text stay fully visible while the
// palette filters (operator complaint: the centered popup hid the input).
func (m *App) paletteComposerView(base string) string {
	box := m.paletteView()
	if box == "" {
		return base
	}
	rows := strings.Split(base, "\n")
	bh := len(strings.Split(box, "\n"))
	// The composer input line is the second line of the dock block (chip
	// first); its absolute row is h - dock.Lines(). The box ends there.
	top := len(rows) - m.dock.Lines() - bh
	if top < tabBarRows+1 {
		top = tabBarRows + 1
	}
	bw := lipgloss.Width(box)
	left := (m.width - bw) / 2
	if left < 0 {
		left = 0
	}
	return m.overlayBoxAt(base, box, top, left)
}

// paletteView renders the overlay box listing matching commands with
// name + description. The candidate list is windowed (via palette.scroll)
// so it never overflows the terminal height.
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
		// Close the palette. The composer keeps focus AND the typed text
		// (the operator's text is never lost or hidden) — esc owns the
		// palette only, so there is no focus trap to escape.
		m.closePalette()
		return true, nil
	case "enter", "tab":
		c := m.paletteSelected()
		if c == nil {
			// No selection (empty filter): fall through so Enter
			// parses/sends the composer text exactly as today.
			return false, nil
		}
		m.closePalette()
		// Run the command; seed the composer with its name so arg-taking
		// commands (e.g. /wi <id>) continue from a known text.
		m.dock.SetValue(c.Name)
		return true, c.Run(m, nil)
	default:
		// Editing keys flow through to the composer buffer FIRST so the
		// user sees their input live (the palette floats above the composer
		// line — operator requirement), then the filter re-seeds from the
		// buffer (no second source of truth for the query).
		consumed, cmd := m.dock.Update(tea.KeyMsg(k))
		m.palette.query = strings.TrimPrefix(m.dock.Value(), "/")
		m.refreshPalette()
		return consumed, cmd
	}
}

// placeholder kept to satisfy the lipgloss import guard.
var _ = lipgloss.NewStyle

// ConnectOverlayOpen reports whether the /connect in-place overlay is
// showing (never quits the process — first-run stays in main.go).
func (m *App) ConnectOverlayOpen() bool { return m.palette.connectOpen }

// openConnectOverlay opens the in-place re-auth overlay hosting the FULL
// connection form (the first-run screen: URL + auth-method toggle +
// credential field). Pre-filled with the active profile. Never exits the
// process or prints "exit and re-run orch" (operator finding #6).
func (m *App) openConnectOverlay() {
	if m.palette.connectOpen {
		return
	}
	// Re-auth pre-fills URL + username (stable, useful) but NEVER the
	// credential: the stored token is by definition expired/invalid (that
	// is why the overlay is open), and a pre-filled 300-char access token
	// renders as a full-width echo row the operator cannot reason about —
	// the "cannot type a password" trap from the operator's Phase 3
	// screenshot. An empty credential field with a blinking cursor is the
	// correct starting state.
	prefill := *m.profile
	prefill.Token = ""
	form := connection.New(&prefill, connection.DefaultProbes())
	form.SetEmbedded()
	if m.width > 0 {
		// Pre-size the form's text inputs via a WindowSizeMsg (it has no
		// SetSize — the form sizes itself from the shell's dimensions).
		next, _ := form.Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
		if fm, ok := next.(connection.Model); ok {
			form = fm
		}
	}
	m.palette.connectOpen = true
	m.palette.connectForm = &form
	m.palette.connectMsg = ""
}

// closeConnectOverlay closes it (esc).
func (m *App) closeConnectOverlay() {
	m.palette.connectOpen = false
	m.palette.connectForm = nil
	m.palette.connectMsg = ""
}

// connectOverlayView renders the full connection form centered over the
// shell (the same form the first-run screen renders — no drift).
func (m *App) connectOverlayView() string {
	if m.palette.connectForm == nil {
		return ""
	}
	return m.palette.connectForm.View()
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
// Returns (handled, cmd). Keys route into the embedded connection form
// (typing, tab between fields, ctrl+a auth toggle, ctrl+s TLS toggle,
// enter submit); esc cancels back to the shell; ctrl+c quits for real.
// Returns (handled=false) once a successful probe has landed so the
// router stops routing keys here.
func (m *App) connectHandleKey(k tea.KeyMsg) (bool, tea.Cmd) {
	if m.palette.connectForm == nil {
		return false, nil
	}
	switch k.String() {
	case "esc":
		// Cancel the re-auth: the shell stays put (in-place overlay). Clear
		// reconnectRequested so a later normal quit does NOT re-open the
		// connection screen — the loop is first-run fallback only, and the
		// in-place /connect never exits the process.
		m.reconnectRequested = false
		m.closeConnectOverlay()
		return true, nil
	case "q":
		// q types a literal 'q' into the focused field (URLs/keys contain
		// no q constraint) — only ctrl+c quits from here.
	}
	if k.String() == "ctrl+c" {
		m.reconnectRequested = false // quitting for real, not re-auth
		m.quitting = true
		return true, tea.Quit
	}
	// Everything else drives the connection form.
	next, cmd := m.palette.connectForm.Update(k)
	if fm, ok := next.(connection.Model); ok {
		m.palette.connectForm = &fm
	}
	if m.palette.connectForm.Connected() {
		return true, m.applyConnectResult()
	}
	return true, cmd
}

// connectTick drives the embedded form's non-key messages (probe start,
// probe done) so the async probe runs inside the shell's tea program.
// The shell routes unhandled messages here while the overlay is open.
func (m *App) connectTick(msg tea.Msg) tea.Cmd {
	if m.palette.connectForm == nil {
		return nil
	}
	next, cmd := m.palette.connectForm.Update(msg)
	if fm, ok := next.(connection.Model); ok {
		m.palette.connectForm = &fm
	}
	if m.palette.connectForm.Connected() {
		return m.applyConnectResult()
	}
	return cmd
}

// applyConnectResult consumes the form's successful probe: persist the
// profile, swap the shell onto the new client set, redial streams, and
// close the overlay — all in place, without exiting the process.
func (m *App) applyConnectResult() tea.Cmd {
	res := m.palette.connectForm.Result()
	if res == nil || res.Profile == nil {
		return nil
	}
	profile := res.Profile
	// Persist (env-driven profiles are still used in memory; a save error
	// surfaces but does not block the reconnect).
	if path, err := config.DefaultPath(); err == nil {
		if os.Getenv(config.EnvURL) == "" {
			if err := connection.SaveProfile(path, res); err != nil {
				m.dock.SetError("connect: save profile: " + err.Error())
			}
		}
	}
	// Rebuild the client set with the (possibly refreshed) credential and
	// the stored refresh token; redial every live stream + the chat rail.
	opts := client.Options{
		BaseURL:            profile.URL,
		Token:              profile.Token,
		InsecureSkipVerify: profile.InsecureSkipVerify,
		Timeout:            30 * time.Second,
		RefreshToken:       profile.RefreshToken,
	}
	cl := client.New(opts)
	m.profile = profile
	m.clients = cl
	m.diffPane = diffs.NewModel(cl, m.reg)
	m.chat = chat.NewController(cl)
	m.chat.Bind(&appEventStore{m: m}, m.chatCmds)
	// Screens hold the old client set: drop the active screen so it
	// reconstructs lazily through its factory (fresh client set).
	if s, ok := m.screens[m.active]; ok && s != nil {
		s.Close()
	}
	delete(m.screens, m.active)
	if m.width > 0 {
		m.refreshLayout()
	}
	m.reconnectRequested = false
	m.closeConnectOverlay()
	m.reconnectStreams()
	m.dock.SetError("")
	m.dock.SetNotice("✓ connected to " + profile.URL + " — shell reconnected in place")
	// Reload the ask rail + restart the chat waiter on the new clients.
	return tea.Batch(m.chat.LoadConversations(), m.waitChat())
}

// connectRequestRun runs the /connect command in place: it opens the
// overlay instead of quitting (the old behavior exited the program).
func (m *App) runConnectCommand() tea.Cmd {
	m.openConnectOverlay()
	return nil
}
