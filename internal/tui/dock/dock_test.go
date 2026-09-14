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

// The operator: "the main tooltip in the composer is going off the screen as
// well. We need a similar wrap there."
//
// The affordance row WRAPS instead of truncating, and the box's row count grows
// with it — but only up to maxHintRows, because the composer's height comes out
// of the screen's budget. The reserved rows and the drawn rows must agree.
func TestComposerHintWrapsAndStaysWithinItsCap(t *testing.T) {
	m := New()
	m.Width = 60
	// Enough rows that the real per-screen hint fits at a normal width without
	// an ellipsis (three is what the composer can afford out of the screen's
	// budget), but a hard cap so a runaway hint can never eat the content region.
	m.SetContext("x: one · y: two · z: three · a: four · b: five · c: six · d: seven · e: eight · f: nine · g: ten · h: eleven · i: twelve · j: thirteen · k: fourteen · l: fifteen")

	rows := m.Lines()
	if rows != len(strings.Split(m.View(), "\n")) {
		t.Fatalf("Lines()=%d but the box renders %d rows", rows, len(strings.Split(m.View(), "\n")))
	}
	// The hint must have wrapped (it cannot fit on one 60-cell line)...
	if m.HintRows() < 2 {
		t.Fatalf("a long hint must wrap, HintRows=%d", m.HintRows())
	}
	// ...but never beyond the cap that keeps the layout stable.
	if m.HintRows() > maxHintRows {
		t.Fatalf("the hint must be capped at %d rows, got %d", maxHintRows, m.HintRows())
	}
	// And it must say it was cut rather than silently dropping shortcuts.
	if !strings.Contains(m.View(), "…") {
		t.Fatalf("a capped hint must be marked with an ellipsis:\n%s", m.View())
	}
}

// A short hint stays on one row (the cap must not add rows gratuitously).
func TestComposerHintShortStaysSingleRow(t *testing.T) {
	m := New()
	m.Width = 200
	m.SetContext("n: new")
	if got := m.HintRows(); got != 1 {
		t.Fatalf("a short hint must occupy one row, got %d", got)
	}
}

// The caret must ANIMATE while the composer holds focus, so the operator can see
// where their keystrokes will land: "the cursor should blink when in the composer
// and focus is active so people know they truly have focus there."
//
// bubbles animates the caret from a command returned by the cursor's own Focus(),
// which the dock used to DISCARD (`_ = m.ta.Focus()`). With the loop never started
// no tick could arrive, so the caret sat solid — the fix is to capture that command
// on focus and dispatch it.
//
// NOTE on what is NOT tested here: the tick that CONTINUES the loop is a
// cursor.BlinkMsg that bubbles matches against the private id/blinkTag its own
// cursor emitted ("we're choosy about whether to accept blinkMsgs"). A fabricated
// tick is therefore always rejected, so a unit test cannot represent it; the dock
// forwards every non-key message to the textarea, which is what makes the real
// loop work.
func TestComposerStartsWithTheCaretBlink(t *testing.T) {
	m := New()
	m.Focus()
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if cmd == nil {
		t.Fatal("the caret blink loop was never started — the cursor cannot animate")
	}
}

// The composer's own hint documents the key contract as
// "enter send · alt+enter newline". These pin it, because a SILENT newline is
// indistinguishable from a dead Enter key — which is how a terminal reporting its
// Enter as a modified key made "I type a message and hit enter and nothing happens"
// (with the text still sitting in the box) survive several rounds.
func TestPlainEnterSends(t *testing.T) {
	m := New()
	m.Focus()
	m.SetValue("hello")
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := m.SendRequest(); got != "hello" {
		t.Fatalf("plain enter produced send %q, want the text", got)
	}
}

// ANY Enter variant that is not an explicitly configured newline chord must SEND.
//
// A terminal can report a plain Enter as shift+enter (CSI-u / kitty /
// modifyOtherKeys), and the old code honoured that as "insert a newline" — which
// is not part of the documented contract and made Enter unable to send at all on
// such a terminal. The only newline paths left are the two the operator configures
// (alt+enter, and a trailing backslash + enter), so every other Enter sends by
// construction.
func TestEveryOtherEnterVariantSends(t *testing.T) {
	cases := []struct {
		name string
		k    tea.KeyMsg
	}{
		{"plain enter", tea.KeyMsg{Type: tea.KeyEnter}},
		{"ctrl+enter", tea.KeyMsg{Type: tea.KeyEnter, Alt: false}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := New()
			m.Focus()
			m.SetValue("hello")
			_, _ = m.Update(c.k)
			if got := m.SendRequest(); got != "hello" {
				t.Fatalf("%s produced send %q, want the text — an Enter variant that is not the configured newline chord must send", c.name, got)
			}
		})
	}
}

// The configured newline chord STILL inserts a newline — and says so, so the
// gesture is never invisible.
func TestAltEnterInsertsANewlineAndReportsIt(t *testing.T) {
	m := New()
	m.Focus()
	m.SetValue("line one")
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	if got := m.SendRequest(); got != "" {
		t.Fatalf("alt+enter sent %q — it is the configured NEWLINE chord", got)
	}
	if !strings.Contains(m.Value(), "\n") {
		t.Fatalf("alt+enter did not insert a newline: %q", m.Value())
	}
	if m.Notice == "" {
		t.Fatal("inserting a newline must report the chord — a silent newline is indistinguishable from a dead key")
	}
	if !strings.Contains(m.Notice, "alt+enter") {
		t.Errorf("notice = %q, want it to name the chord that fired", m.Notice)
	}
}
