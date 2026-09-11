// Package dock implements the always-present bottom chat composer: a real
// bordered box (Composer 2.0) wrapping a bubbles/textarea that grows from
// MinInputRows to MaxInputRows rows, inside which the context chip, the
// notice/error strip, and a persistent affordance row (what Enter does,
// the newline chord, how to open the command palette) are rendered. The
// dock also owns bracketed-paste handling, the configurable newline keys
// (alt+enter default, leading-backslash+enter alternative — bubbletea
// v1.3.10 has no kitty keyboard protocol, so Shift+Enter cannot be enabled
// programmatically; CSI-u shift+enter is accepted when a terminal emits it
// anyway), and the draft buffer (restored after a failed send).
package dock

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// NewlineMode selects how Enter inserts a newline instead of sending.
type NewlineMode string

const (
	NewlineAltEnter       NewlineMode = "alt+enter"
	NewlineBackslashEnter NewlineMode = "backslash-enter"
	NewlineBoth           NewlineMode = "both"
)

// ParseNewlineMode maps a config string to a NewlineMode (default
// alt+enter on anything else).
func ParseNewlineMode(s string) NewlineMode {
	switch NewlineMode(s) {
	case NewlineBackslashEnter:
		return NewlineBackslashEnter
	case NewlineBoth:
		return NewlineBoth
	default:
		return NewlineAltEnter
	}
}

// Composer geometry (Composer 2.0). The old dock was a single unbordered
// row; the box now carries a border, inner padding, a persistent
// affordance row, and grows with the buffer.
const (
	// MinInputRows is the composer's default input height.
	MinInputRows = 3
	// MaxInputRows caps the box; beyond it the textarea scrolls internally.
	MaxInputRows = 8
	// boxChrome is the box's horizontal chrome: 2 border cells + 2 padding
	// cells on each side.
	boxChrome = 6
	// promptWidth is the "❯ " the dock paints on the input's first row.
	promptWidth = 2
)

// Model is the chat dock composer.
type Model struct {
	ta       textarea.Model
	Focused  bool
	Chip     string // context chip (what outgoing messages inject)
	Notice   string // status line (stream state, pointers)
	Err      string // inline error strip (themed red)
	Width    int
	Height   int // allocated rows (set by the shell)
	Newlines NewlineMode

	// sendRequest is a non-nil callback when Enter produced a send; the
	// shell checks+clears it after Update (avoids channel plumbing).
	sendRequest string
	// lastSent is the text of the most recent send: the draft source for
	// RestoreDraft (a failed send puts the message back in the box).
	lastSent string
}

// New builds the dock.
func New() Model {
	ta := textarea.New()
	ta.Placeholder = "ask orchicon…"
	ta.Prompt = ""
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	m := Model{ta: ta, Newlines: NewlineAltEnter}
	_ = m.ta.Focus() // safe: textarea always lets you re-focus
	m.resizeTa()
	return m
}

// SendRequest returns and clears the pending send text ("" = none).
func (m *Model) SendRequest() string {
	s := m.sendRequest
	m.sendRequest = ""
	return s
}

// LastSent returns the text of the most recent send (draft source).
func (m *Model) LastSent() string { return m.lastSent }

// SetValue replaces the composer buffer (draft restore on failures).
func (m *Model) SetValue(s string) {
	m.ta.SetValue(s)
	m.resizeTa()
}

// Value returns the composer buffer.
func (m *Model) Value() string { return m.ta.Value() }

// RestoreDraft puts the last sent text back into the box after a failed
// send. It never clobbers text the operator has typed since, and returns
// true only when a draft was actually restored.
func (m *Model) RestoreDraft() bool {
	if m.lastSent == "" || strings.TrimSpace(m.ta.Value()) != "" {
		return false
	}
	m.SetValue(m.lastSent)
	return true
}

// boxInner is the box's content width: the dock width minus the box chrome
// (border + inner padding). An unset/too-narrow width falls back to a sane
// default — the shell pads/truncates every frame row to the real width, so
// the box can never open a hole in the h×w frame.
func (m *Model) boxInner() int {
	w := m.Width
	if w < 24 {
		w = 80
	}
	inner := w - boxChrome
	if inner < 12 {
		inner = 12
	}
	return inner
}

// taWidth is the textarea width: the box's inner width minus the prompt
// the dock paints on the input's first row.
func (m *Model) taWidth() int {
	w := m.boxInner() - promptWidth
	if w < 4 {
		w = 4
	}
	return w
}

// visualRows estimates the rows the wrapped buffer needs.
func (m *Model) visualRows() int {
	tw := m.taWidth()
	total := 0
	for _, ln := range strings.Split(m.ta.Value(), "\n") {
		rows := 1
		if cells := lipgloss.Width(ln); cells > tw {
			rows = (cells + tw - 1) / tw
		}
		total += rows
	}
	if total < 1 {
		total = 1
	}
	return total
}

// InputRows is the number of input rows the box shows: at least
// MinInputRows, growing with the buffer up to MaxInputRows (the textarea
// scrolls internally beyond that).
func (m *Model) InputRows() int {
	n := m.visualRows()
	if n < MinInputRows {
		n = MinInputRows
	}
	if n > MaxInputRows {
		n = MaxInputRows
	}
	return n
}

// Lines returns the rows the dock needs: the box's top+bottom border, the
// input rows (InputRows), the persistent affordance row, and the chip /
// notice strips when present. The shell reserves exactly this many rows
// (app.contentHeight) and normalizes the block to it (normalizeBlockKeepTail).
func (m *Model) Lines() int {
	h := 2 + m.InputRows() + 1
	if m.Chip != "" {
		h++
	}
	if m.Err != "" || m.Notice != "" {
		h++
	}
	return h
}

// Focus / Blur move keyboard focus into/out of the composer.
func (m *Model) Focus() {
	m.Focused = true
	_ = m.ta.Focus()
}

// Blur releases focus (content pane keeps editing keys).
func (m *Model) Blur() {
	m.Focused = false
	m.ta.Blur()
}

// SetError sets the inline error strip ("" clears).
func (m *Model) SetError(s string) { m.Err = s }

// SetNotice sets the status strip ("" clears).
func (m *Model) SetNotice(s string) { m.Notice = s }

// resizeTa keeps the textarea width+height synced to the box.
func (m *Model) resizeTa() {
	m.ta.SetWidth(m.taWidth())
	m.ta.SetHeight(m.InputRows())
}

// Update handles composer keys. Returns (consumed, cmd): consumed=false
// means the shell should fall through (only when unfocused).
func (m *Model) Update(msg tea.Msg) (bool, tea.Cmd) {
	if !m.Focused {
		// Even unfocused, a paste must not leak to the screen: capture it
		// into the buffer so nothing is lost (focus first).
		if k, ok := msg.(tea.KeyMsg); ok && k.Paste {
			m.Focus()
		} else {
			return false, nil
		}
	}

	switch k := msg.(type) {
	case tea.KeyMsg:
		switch {
		case k.Paste:
			// Bracketed paste: bubbletea collapses the byte sequence into
			// one KeyMsg with embedded newlines — render verbatim, never
			// send. textarea inserts it as-is.
			return true, m.pasteCmd(k)
		case k.Type == tea.KeyEnter && k.Alt:
			if m.Newlines == NewlineAltEnter || m.Newlines == NewlineBoth {
				m.insertNewline()
				return true, nil
			}
			return true, m.requestSend()
		case k.Type == tea.KeyEnter:
			// CSI-u shift+enter arrives as "shift+enter" on v1.3.10 when
			// the terminal emits \x1b[13;2u.
			if k.String() == "shift+enter" {
				m.insertNewline()
				return true, nil
			}
			if m.leadingBackslash() && (m.Newlines == NewlineBackslashEnter || m.Newlines == NewlineBoth) {
				// A trailing lone backslash + Enter = newline (the escape
				// hatch); strip the backslash and wrap.
				v := m.ta.Value()
				m.ta.SetValue(strings.TrimSuffix(v, "\\"))
				m.insertNewline()
				return true, nil
			}
			return true, m.requestSend()
		default:
			ta, cmd := m.ta.Update(msg)
			m.ta = ta
			m.resizeTa()
			return true, cmd
		}
	}
	return true, nil
}

// pasteCmd inserts the pasted block verbatim (multi-line pastes render
// in the composer and only send on explicit Enter).
func (m *Model) pasteCmd(k tea.KeyMsg) tea.Cmd {
	// bubbletea delivers paste runes in k.Runes.
	ta, cmd := m.ta.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: k.Runes, Paste: true})
	m.ta = ta
	m.resizeTa()
	return cmd
}

func (m *Model) insertNewline() {
	ta, _ := m.ta.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: false})
	m.ta = ta
	m.resizeTa()
}

// leadingBackslash reports whether the buffer ends with a lone "\"
// (newline-by-escape). A "\\" before "/" is the slash-escape and never
// counts (the slash framework owns that path).
func (m *Model) leadingBackslash() bool {
	v := m.ta.Value()
	return strings.HasSuffix(v, "\\") && !strings.HasSuffix(v, "\\\\")
}

func (m *Model) requestSend() tea.Cmd {
	v := strings.TrimRight(m.ta.Value(), "\n")
	if strings.TrimSpace(v) == "" {
		return nil // empty enter is a no-op
	}
	m.sendRequest = v
	m.lastSent = v // draft source for RestoreDraft (failed send)
	m.ta.Reset()
	m.resizeTa()
	return nil
}

// Hint is the persistent affordance row's text: what Enter does, the
// newline chord, and how to reach the command palette.
func (m *Model) Hint() string {
	nl := "alt+enter"
	switch m.Newlines {
	case NewlineBackslashEnter:
		nl = "\\+enter"
	case NewlineBoth:
		nl = "alt+enter or \\+enter"
	}
	return "enter send · " + nl + " newline · / commands (↑/↓ pick · esc close)"
}

// View renders the composer as a bordered box: the context chip, the input
// rows (the "❯ " prompt on the first), the notice/error strip, and the
// persistent affordance row — all inside the box's inner padding. The
// rendered block is exactly Lines() rows tall, which is the row budget the
// shell reserves for it (baseView's normalizeBlockKeepTail).
func (m *Model) View() string {
	m.resizeTa()
	inner := m.boxInner()

	rows := make([]string, 0, m.Lines())
	if m.Chip != "" {
		rows = append(rows, theme.ListMeta.Render(fit("["+m.Chip+"]", inner)))
	}
	for i, l := range m.inputLines() {
		if i == 0 {
			// The dock paints the prompt on the first input row (the
			// textarea's own prompt stays empty so wrapped rows are not
			// re-prefixed); continuation rows are indented to match.
			rows = append(rows, theme.ListTitle.Render("❯ ")+l)
			continue
		}
		rows = append(rows, "  "+l)
	}
	if m.Err != "" {
		rows = append(rows, theme.ErrorText.Render(fit(m.Err, inner)))
	} else if m.Notice != "" {
		rows = append(rows, theme.HintText.Render(fit(m.Notice, inner)))
	}
	rows = append(rows, theme.HintText.Render(fit(m.Hint(), inner)))

	return theme.ComposerBox.Render(strings.Join(fitAll(rows, inner), "\n"))
}

// inputLines returns exactly InputRows() rows of the input view (the
// textarea renders its own height, but the box's row budget is never left
// to it).
func (m *Model) inputLines() []string {
	n := m.InputRows()
	src := strings.Split(m.ta.View(), "\n")
	if len(src) > n {
		src = src[:n]
	}
	out := append([]string{}, src...)
	for len(out) < n {
		out = append(out, "")
	}
	return out
}

// fit clips s to w cells (ANSI-aware) and pads it to exactly w cells so
// every box row is the same width (the border stays aligned).
func fit(s string, w int) string {
	if w < 1 {
		return s
	}
	if lipgloss.Width(s) > w {
		s = ansi.Truncate(s, w, "")
	}
	if gap := w - lipgloss.Width(s); gap > 0 {
		s += strings.Repeat(" ", gap)
	}
	return s
}

func fitAll(rows []string, w int) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = fit(r, w)
	}
	return out
}

// PlaceholderText is exported for tests/docs.
func PlaceholderText() string { return fmt.Sprintf("ask orchicon…") }
