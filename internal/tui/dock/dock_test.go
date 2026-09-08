package dock

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func paste(runes string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(runes), Paste: true}
}

func TestPasteRendersWithoutSending(t *testing.T) {
	m := New()
	m.Focus()
	m.Update(paste("line one\nline two\nline three"))
	if got := m.Value(); got != "line one\nline two\nline three" {
		t.Fatalf("value = %q", got)
	}
	if s := m.SendRequest(); s != "" {
		t.Fatalf("paste must not send, got %q", s)
	}
	if m.ta.LineCount() != 3 {
		t.Fatalf("line count = %d, want 3", m.ta.LineCount())
	}
	// explicit Enter after paste sends the whole block
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if s := m.SendRequest(); s != "line one\nline two\nline three" {
		t.Fatalf("enter after paste must send the block, got %q", s)
	}
}

func TestEnterSendsAndClears(t *testing.T) {
	m := New()
	m.Focus()
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello")})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if s := m.SendRequest(); s != "hello" {
		t.Fatalf("send = %q", s)
	}
	if m.Value() != "" {
		t.Fatalf("buffer must clear after send, got %q", m.Value())
	}
	// SendRequest is consume-once
	if s := m.SendRequest(); s != "" {
		t.Fatalf("second read = %q", s)
	}
}

func TestAltEnterInsertsNewline(t *testing.T) {
	m := New()
	m.Focus()
	m.Newlines = NewlineAltEnter
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("one")})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("two")})
	if got := m.Value(); got != "one\ntwo" {
		t.Fatalf("value = %q", got)
	}
	if s := m.SendRequest(); s != "" {
		t.Fatalf("alt+enter must not send, got %q", s)
	}
}

func TestBackslashEnterNewline(t *testing.T) {
	m := New()
	m.Focus()
	m.Newlines = NewlineBackslashEnter
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("one\\")})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := m.Value(); got != "one\n" {
		t.Fatalf("value = %q, want one\\n", got)
	}
	if s := m.SendRequest(); s != "" {
		t.Fatalf("backslash+enter must not send, got %q", s)
	}
	// plain enter (no backslash) still sends
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("two")})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if s := m.SendRequest(); s != "one\ntwo" {
		t.Fatalf("send = %q", s)
	}
}

func TestShiftEnterCSIU(t *testing.T) {
	m := New()
	m.Focus()
	k := tea.KeyMsg{Type: tea.KeyEnter}
	// bubbletea renders CSI-u shift+enter as "shift+enter"
	if got := k.String(); got != "enter" {
		t.Fatalf("sanity: enter = %q", got)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: false}) // plain
	if s := m.SendRequest(); s != "" {
		t.Fatalf("empty enter must not send, got %q", s)
	}
}

func TestLinesGrowsAndCaps(t *testing.T) {
	m := New()
	if m.Lines() < 2 {
		t.Fatalf("lines = %d, want >= 2 (input + chip)", m.Lines())
	}
	m.Focus()
	var sb strings.Builder
	for i := 0; i < 10; i++ {
		sb.WriteString("line\n")
	}
	m.Update(paste(sb.String()))
	if m.Lines() != 5 { // 4 input rows + chip strip (notice only when set)
		t.Fatalf("lines = %d, want 5", m.Lines())
	}
}

func TestUnfocusedIgnoresPlainKeys(t *testing.T) {
	m := New()
	m.Blur()
	consumed, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if consumed {
		t.Fatal("unfocused dock must fall through")
	}
	// but paste is captured even unfocused (never leak to the screen)
	consumed, _ = m.Update(paste("accidental"))
	if !consumed {
		t.Fatal("paste must be captured even unfocused")
	}
}

func TestErrorStrip(t *testing.T) {
	m := New()
	m.SetError("auth expired — run /connect")
	if m.Lines() < 3 {
		t.Fatalf("error strip must add a line, lines = %d", m.Lines())
	}
	v := m.View()
	if !strings.Contains(v, "auth expired") {
		t.Fatalf("view missing error: %q", v)
	}
	m.SetError("")
	if strings.Contains(m.View(), "auth expired") {
		t.Fatal("error must clear")
	}
}

func TestChipRenders(t *testing.T) {
	m := New()
	m.Chip = "Executions: exec-123 (running)"
	v := m.View()
	if !strings.Contains(v, "[Executions: exec-123 (running)]") {
		t.Fatalf("chip missing: %q", v)
	}
}

func TestParseNewlineMode(t *testing.T) {
	if ParseNewlineMode("backslash-enter") != NewlineBackslashEnter {
		t.Fatal("backslash-enter")
	}
	if ParseNewlineMode("both") != NewlineBoth {
		t.Fatal("both")
	}
	if ParseNewlineMode("alt+enter") != NewlineAltEnter {
		t.Fatal("alt+enter")
	}
	if ParseNewlineMode("garbage") != NewlineAltEnter {
		t.Fatal("default must be alt+enter")
	}
}
