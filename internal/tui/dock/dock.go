// Package dock implements the always-present bottom chat composer: a
// bubbles/textarea wrapper (single-line growing to 4) with the context
// chip, notice/error strip, bracketed-paste handling, and the
// configurable newline keys (alt+enter default, leading-backslash+enter
// alternative — bubbletea v1.3.10 has no kitty keyboard protocol, so
// Shift+Enter cannot be enabled programmatically; CSI-u shift+enter is
// accepted when a terminal emits it anyway).
package dock

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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
}

// New builds the dock.
func New() Model {
	ta := textarea.New()
	ta.Placeholder = "ask orchicon… (ctrl+g focus · enter send · alt+enter newline · /help commands)"
	ta.Prompt = ""
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	ta.SetHeight(1)
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	m := Model{ta: ta, Newlines: NewlineAltEnter}
	_ = m.ta.Focus() // safe: textarea always lets you re-focus
	return m
}

// SendRequest returns and clears the pending send text ("" = none).
func (m *Model) SendRequest() string {
	s := m.sendRequest
	m.sendRequest = ""
	return s
}

// SetValue replaces the composer buffer (draft restore on failures).
func (m *Model) SetValue(s string) { m.ta.SetValue(s) }

// Value returns the composer buffer.
func (m *Model) Value() string { return m.ta.Value() }

// Lines returns the rows the dock needs: input lines (1..4) + one
// chip/notice line. At the 80×24 floor the notice merges into the chip
// line (plan §8 risk 3).
func (m *Model) Lines() int {
	lines := m.ta.LineCount()
	if lines < 1 {
		lines = 1
	}
	if lines > 4 {
		lines = 4
	}
	h := lines + 1 // + chip/notice strip
	if m.Err != "" || m.Notice != "" {
		h++ // error/notice strip on its own line when present
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

// resizeTa keeps the textarea height synced to its content.
func (m *Model) resizeTa() {
	lines := m.ta.LineCount()
	if lines < 1 {
		lines = 1
	}
	if lines > 4 {
		lines = 4
	}
	m.ta.SetHeight(lines)
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
	m.ta.Reset()
	m.resizeTa()
	return nil
}

// View renders the dock: chip line, input, notice/error strip.
func (m *Model) View() string {
	var b strings.Builder

	// chip / notice line (merged at any width; one row)
	chip := ""
	if m.Chip != "" {
		chip = theme.ListMeta.Render("[" + m.Chip + "]")
	}
	b.WriteString(chip + "\n")

	b.WriteString(theme.ListTitle.Render("❯ ") + m.ta.View())
	b.WriteString("\n")

	notice := m.Notice
	if m.Err != "" {
		notice = theme.ErrorText.Render(truncate(m.Err, max(20, m.Width-4)))
	} else if notice != "" {
		notice = theme.HintText.Render(truncate(notice, max(20, m.Width-4)))
	}
	if m.Err != "" || m.Notice != "" {
		b.WriteString(notice)
		b.WriteString("\n")
	}
	return b.String()
}

func truncate(s string, w int) string {
	if w <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	return string(r[:w-1]) + "…"
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// PlaceholderText is exported for tests/docs.
func PlaceholderText() string { return fmt.Sprintf("ask orchicon…") }
