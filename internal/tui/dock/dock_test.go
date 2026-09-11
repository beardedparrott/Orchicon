package dock

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
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
	// Composer 2.0: box border (2) + MinInputRows + the affordance row.
	if want := 2 + MinInputRows + 1; m.Lines() != want {
		t.Fatalf("lines = %d, want %d", m.Lines(), want)
	}
	m.Focus()
	var sb strings.Builder
	for i := 0; i < 10; i++ {
		sb.WriteString("line\n")
	}
	m.Update(paste(sb.String()))
	if want := 2 + MaxInputRows + 1; m.Lines() != want {
		t.Fatalf("lines = %d, want %d", m.Lines(), want)
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

func TestBoxRendersExactRowsAndWidth(t *testing.T) {
	m := New()
	m.Width = 80
	m.Focus()
	rows := strings.Split(m.View(), "\n")
	if len(rows) != m.Lines() {
		t.Fatalf("box rows = %d, want Lines() = %d", len(rows), m.Lines())
	}
	for i, r := range rows {
		if got := lipgloss.Width(r); got != 80 {
			t.Fatalf("row %d width = %d, want 80 (%q)", i, got, r)
		}
	}
	plain := ansi.Strip(m.View())
	lines := strings.Split(plain, "\n")
	if !strings.HasPrefix(lines[0], "╭") || !strings.HasSuffix(lines[0], "╮") {
		t.Fatalf("top border missing: %q", lines[0])
	}
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "╰") || !strings.HasSuffix(last, "╯") {
		t.Fatalf("bottom border missing: %q", last)
	}
	// Inner padding: the input row is inset from both borders by >= 2 cells
	// (right after the left border sits a cell of padding).
	input := ""
	for i := 1; i < len(lines)-1; i++ {
		if strings.Contains(lines[i], "❯") {
			input = lines[i]
			break
		}
	}
	if input == "" {
		t.Fatalf("no input row inside the box: %q", plain)
	}
	r := []rune(input)
	if r[0] != '│' || r[len(r)-1] != '│' {
		t.Fatalf("input row must be bordered: %q", input)
	}
	pos := strings.Index(input, "❯")
	if pos < 0 {
		t.Fatalf("input row missing the prompt: %q", input)
	}
	if got := len([]rune(input[:pos])); got < 3 {
		t.Fatalf("input row inset = %d cells, want >= 3 (border + 2 padding): %q", got, input)
	}
}

func TestInputRowsGrowToMax(t *testing.T) {
	m := New()
	m.Width = 120
	if m.InputRows() != MinInputRows {
		t.Fatalf("default input rows = %d, want %d", m.InputRows(), MinInputRows)
	}
	if m.Lines() != 2+MinInputRows+1 {
		t.Fatalf("default lines = %d, want %d", m.Lines(), 2+MinInputRows+1)
	}
	m.Focus()
	m.Update(paste(strings.Repeat("line\n", 20)))
	if m.InputRows() != MaxInputRows {
		t.Fatalf("grown input rows = %d, want %d", m.InputRows(), MaxInputRows)
	}
	if m.Lines() != 2+MaxInputRows+1 {
		t.Fatalf("grown lines = %d, want %d", m.Lines(), 2+MaxInputRows+1)
	}
}

func TestHintRowPersistent(t *testing.T) {
	m := New()
	m.Width = 100
	hint := m.Hint()
	for _, want := range []string{"enter", "newline", "commands"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("affordance row %q missing %q", hint, want)
		}
	}
	if !strings.Contains(ansi.Strip(m.View()), "enter send") {
		t.Fatal("the affordance row must render inside the box")
	}
}

func TestDraftRestoredAfterFailedSend(t *testing.T) {
	m := New()
	m.Width = 80
	m.Focus()
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello")})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := m.SendRequest(); got != "hello" {
		t.Fatalf("send = %q", got)
	}
	if m.Value() != "" {
		t.Fatalf("buffer must clear after send, got %q", m.Value())
	}
	if !m.RestoreDraft() {
		t.Fatal("a failed send must restore the draft")
	}
	if m.Value() != "hello" {
		t.Fatalf("restored draft = %q, want hello", m.Value())
	}
	// Never clobber text typed since the failed send (SetValue leaves the
	// cursor at the end of the restored draft, so the new rune appends).
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if m.RestoreDraft() {
		t.Fatal("must not restore over text typed since")
	}
	if !strings.Contains(m.Value(), "x") {
		t.Fatalf("typed text must stay in the box, got %q", m.Value())
	}
}

func TestMultiLineSendPreservesNewlines(t *testing.T) {
	m := New()
	m.Width = 100
	m.Focus()
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := m.SendRequest(); got != "a\nb" {
		t.Fatalf("multi-line send = %q, want %q", got, "a\nb")
	}
}
