package tui

// composer2_test.go — the Composer 2.0 acceptance gate: a bordered, padded,
// growing composer box with a persistent affordance row and the context
// chip; the slash palette floating ABOVE the box (live filter, up/down
// navigation, usage + description per row, enter executes); multi-line
// sends; drafts across screen switches and failed sends; and the exactly
// h×w frame contract with a taller composer (the highest regression risk).

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/dock"
)

func composer2Sizes() [][2]int { return [][2]int{{80, 24}, {120, 40}} }

// assertFrameExact pins the render contract: the view is EXACTLY h rows of
// EXACTLY w cells, and the composer box's top border lands on the first row
// of the dock's row budget (no sliver above it).
func assertFrameExact(t *testing.T, m *App, w, h int) {
	t.Helper()
	lines := strings.Split(m.View(), "\n")
	if len(lines) != h {
		t.Fatalf("%dx%d: frame renders %d rows, want exactly %d", w, h, len(lines), h)
	}
	for i, l := range lines {
		if got := lipgloss.Width(l); got != w {
			t.Fatalf("%dx%d: row %d width %d, want %d (%q)", w, h, i, got, w, lipglossStrip(l))
		}
	}
	comTop := h - 1 - m.dock.Lines()
	if comTop < 0 || comTop >= h {
		t.Fatalf("%dx%d: composer box top row %d outside the frame", w, h, comTop)
	}
	if !strings.Contains(lipglossStrip(lines[comTop]), "╭") {
		t.Fatalf("%dx%d: composer box top border not on the dock's first row %d: %q", w, h, comTop, lipglossStrip(lines[comTop]))
	}
	if got := m.contentHeight() + m.dock.Lines() + 4; got != h {
		t.Fatalf("%dx%d: contentHeight()+dock.Lines()+4 = %d, want exactly %d", w, h, got, h)
	}
}

// TestComposerBoxedAtBothSizes: the composer is a real bordered box with
// inner padding and at least dock.MinInputRows input rows by default —
// strictly larger than the old single-line field — at 80×24 and 120×40.
func TestComposerBoxedAtBothSizes(t *testing.T) {
	for _, size := range composer2Sizes() {
		w, h := size[0], size[1]
		m := phase3App(w, h)
		m.setFocus(focusComposer)

		if got := m.dock.InputRows(); got < dock.MinInputRows {
			t.Fatalf("%dx%d: input rows = %d, want >= %d", w, h, got, dock.MinInputRows)
		}
		// The old composer was 1 input row + a chip/notice strip = 2 rows.
		if m.dock.Lines() <= 2 {
			t.Fatalf("%dx%d: composer block = %d rows, not larger than the old single-line dock", w, h, m.dock.Lines())
		}
		assertFrameExact(t, m, w, h)

		box := lipglossStrip(m.dock.View())
		rows := strings.Split(box, "\n")
		if len(rows) != m.dock.Lines() {
			t.Fatalf("%dx%d: box renders %d rows, want Lines()=%d", w, h, len(rows), m.dock.Lines())
		}
		top := []rune(rows[0])
		bottom := []rune(rows[len(rows)-1])
		if top[0] != '╭' || top[len(top)-1] != '╮' {
			t.Fatalf("%dx%d: box top border missing: %q", w, h, rows[0])
		}
		if bottom[0] != '╰' || bottom[len(bottom)-1] != '╯' {
			t.Fatalf("%dx%d: box bottom border missing: %q", w, h, rows[len(rows)-1])
		}
		// Inner padding: the input row is bordered and the prompt is inset.
		input := ""
		for i := 1; i < len(rows)-1; i++ {
			if strings.Contains(rows[i], "❯") {
				input = rows[i]
				break
			}
		}
		if input == "" {
			t.Fatalf("%dx%d: no input row inside the box: %q", w, h, box)
		}
		r := []rune(input)
		if r[0] != '│' || r[len(r)-1] != '│' {
			t.Fatalf("%dx%d: input row must be bordered: %q", w, h, input)
		}
		pos := strings.Index(input, "❯")
		if pos < 0 {
			t.Fatalf("%dx%d: input row missing the prompt: %q", w, h, input)
		}
		if inset := len([]rune(input[:pos])); inset < 3 {
			t.Fatalf("%dx%d: prompt inset = %d cells, want >= 3 (border + padding): %q", w, h, inset, input)
		}
	}
}

// TestComposerBoxGrowsAndFrameStaysExact: a full buffer grows the box to
// dock.MaxInputRows (>= 8) and the frame still measures exactly h×w.
func TestComposerBoxGrowsAndFrameStaysExact(t *testing.T) {
	for _, size := range composer2Sizes() {
		w, h := size[0], size[1]
		m := phase3App(w, h)
		m.setFocus(focusComposer)
		m.dock.SetValue("one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\neleven")
		if got := m.dock.InputRows(); got != dock.MaxInputRows {
			t.Fatalf("%dx%d: grown input rows = %d, want %d", w, h, got, dock.MaxInputRows)
		}
		if dock.MaxInputRows < 8 {
			t.Fatalf("the box must grow to at least 8 input rows, max = %d", dock.MaxInputRows)
		}
		assertFrameExact(t, m, w, h)

		// Worst case for the budget: the box at max height, PLUS the chip and
		// an error strip inside it.
		m.dock.Chip = "Executions: exec-12345 (running)"
		m.dock.SetError("send: connection closed — retry with enter")
		assertFrameExact(t, m, w, h)
	}
}

// TestComposerBoxShowsHintAndChip: the box carries the persistent key
// affordance row and the active context chip.
func TestComposerBoxShowsHintAndChip(t *testing.T) {
	for _, size := range composer2Sizes() {
		w, h := size[0], size[1]
		m := phase3App(w, h)
		m.dock.Chip = "Executions: exec-123 (running)"
		box := lipglossStrip(m.dock.View())
		if !strings.Contains(box, "[Executions: exec-123 (running)]") {
			t.Fatalf("%dx%d: context chip missing from the box: %q", w, h, box)
		}
		hint := m.dock.Hint()
		for _, want := range []string{"enter", "newline", "commands"} {
			if !strings.Contains(hint, want) {
				t.Fatalf("%dx%d: affordance row %q missing %q", w, h, hint, want)
			}
		}
		if !strings.Contains(box, hint) {
			t.Fatalf("%dx%d: affordance row not rendered in the box", w, h)
		}
		assertFrameExact(t, m, w, h)
	}
}

// TestPaletteOpensAboveComposerBox: the palette floats ABOVE the boxed
// composer, filters live on the composer text (which stays visible),
// navigates with up/down (never typing into the box), and renders usage +
// description per row.
func TestPaletteOpensAboveComposerBox(t *testing.T) {
	for _, size := range composer2Sizes() {
		w, h := size[0], size[1]
		m := phase3App(w, h)
		m.setFocus(focusComposer)
		for _, r := range "/pro" {
			nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
			m = nm
		}
		if !m.palette.PaletteOpen() {
			t.Fatalf("%dx%d: '/' must open the palette", w, h)
		}
		if got := m.dock.Value(); got != "/pro" {
			t.Fatalf("%dx%d: composer value = %q, want /pro (the box owns the typing)", w, h, got)
		}
		if len(m.palette.filter) == 0 {
			t.Fatalf("%dx%d: palette must filter live on the composer text", w, h)
		}
		for _, c := range m.palette.filter {
			if !strings.Contains(c.Name, "pro") {
				t.Fatalf("%dx%d: candidate %q does not match the typed \"pro\"", w, h, c.Name)
			}
		}
		// up/down navigate the palette and are consumed (never typed).
		before := m.palette.sel
		nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyDown})
		m = nm
		if m.palette.sel != before+1 && len(m.palette.filter) > before+1 {
			t.Fatalf("%dx%d: down must move the selection (%d -> %d)", w, h, before, m.palette.sel)
		}
		nm, _ = m.dispatch(tea.KeyMsg{Type: tea.KeyUp})
		m = nm
		if m.palette.sel != before {
			t.Fatalf("%dx%d: up must move the selection back (%d, want %d)", w, h, m.palette.sel, before)
		}
		if got := m.dock.Value(); got != "/pro" {
			t.Fatalf("%dx%d: arrows must never be typed into the box: %q", w, h, got)
		}

		// Geometry: the palette is fully above the composer box (whose top
		// border is the dock block's first row), with no overlap.
		lines := strings.Split(lipglossStrip(m.View()), "\n")
		comTop := h - 1 - m.dock.Lines()
		if !strings.Contains(lines[comTop], "╭") {
			t.Fatalf("%dx%d: composer box top border missing from row %d", w, h, comTop)
		}
		paletteRow, paletteBottom := -1, -1
		for i, l := range lines {
			if paletteRow < 0 && strings.Contains(l, "command palette") {
				paletteRow = i
			}
			if strings.Contains(l, "╰") && i < comTop {
				paletteBottom = i
			}
		}
		if paletteRow < 0 {
			t.Fatalf("%dx%d: palette never rendered above the composer", w, h)
		}
		if paletteRow >= comTop {
			t.Fatalf("%dx%d: palette row %d must be above the composer box top row %d", w, h, paletteRow, comTop)
		}
		if paletteBottom != comTop-1 {
			t.Fatalf("%dx%d: palette bottom edge row %d, want %d (directly above the composer box)", w, h, paletteBottom, comTop-1)
		}
		// The typed text stays visible in the composer box inside the frame.
		typed := -1
		for i, l := range lines {
			if strings.Contains(l, "❯ /pro") {
				typed = i
			}
		}
		if typed < 0 {
			t.Fatalf("%dx%d: the typed /pro must stay visible in the composer box", w, h)
		}
		if typed <= paletteRow {
			t.Fatalf("%dx%d: composer row %d must be below the palette row %d", w, h, typed, paletteRow)
		}
		// Usage + description per candidate row.
		sel := m.paletteSelected()
		if sel == nil {
			t.Fatalf("%dx%d: palette has no selection", w, h)
		}
		usage := sel.Usage
		if usage == "" {
			usage = sel.Name
		}
		rowHit := false
		for i := paletteRow; i < comTop; i++ {
			if strings.Contains(lines[i], usage) && strings.Contains(lines[i], sel.Desc) {
				rowHit = true
			}
		}
		if !rowHit {
			t.Fatalf("%dx%d: no palette row shows usage %q + description %q", w, h, usage, sel.Desc)
		}
		assertFrameExact(t, m, w, h)
	}
}

// TestSlashPartialExecutesAndUnknownGivesUsage: "partial command + enter"
// runs the selected command; an unknown slash gives usage feedback and is
// never sent as a chat message.
func TestSlashPartialExecutesAndUnknownGivesUsage(t *testing.T) {
	m := phase3App(120, 40)
	m.setFocus(focusComposer)
	for _, r := range "/he" {
		nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = nm
	}
	sel := m.paletteSelected()
	if sel == nil || sel.Name != "/help" {
		t.Fatalf("first match for /he = %v, want /help", sel)
	}
	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm
	if m.palette.PaletteOpen() {
		t.Fatal("enter on a selection must close the palette")
	}
	if !strings.Contains(m.dock.Notice, "commands") {
		t.Fatalf("enter must run the selected command (/help notice), got notice %q", m.dock.Notice)
	}
	if m.dock.Value() != "/help" {
		t.Fatalf("the selected command must be seeded back into the box, got %q", m.dock.Value())
	}

	// Unknown command: usage feedback, never a send.
	u := phase3App(120, 40)
	u.setFocus(focusComposer)
	for _, r := range "/not-a-command" {
		nm, _ := u.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		u = nm
	}
	nm, cmd := u.dispatch(tea.KeyMsg{Type: tea.KeyEnter})
	u = nm
	if !strings.Contains(u.dock.Err, "unknown command") {
		t.Fatalf("unknown slash must yield usage feedback, got err %q", u.dock.Err)
	}
	if cmd != nil {
		t.Fatal("unknown slash must never be sent as a chat message")
	}
	if u.chatConvID != "" {
		t.Fatalf("unknown slash must not create a conversation, got %q", u.chatConvID)
	}
}

// TestMultiLineMessageSendsWithNewlines: the newline chord inserts a
// newline, Enter sends, and the payload arrives as ONE message with its
// newlines intact.
func TestMultiLineMessageSendsWithNewlines(t *testing.T) {
	m := phase3App(120, 40)
	m.setFocus(focusComposer)
	m.chatConvID = "conv-1"

	nm, _ := m.dispatch(keyRunes("line one"))
	m = nm
	nm, _ = m.dispatch(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	m = nm
	nm, _ = m.dispatch(keyRunes("line two"))
	m = nm
	if got := m.dock.Value(); got != "line one\nline two" {
		t.Fatalf("newline chord must insert a newline, got %q", got)
	}
	if got := m.dock.InputRows(); got < 3 {
		t.Fatalf("input rows = %d, want >= 3", got)
	}

	nm, cmd := m.dispatch(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm
	if cmd == nil {
		t.Fatal("enter must send the multi-line message")
	}
	if m.dock.Value() != "" {
		t.Fatalf("the box must clear after a send, got %q", m.dock.Value())
	}
	items := m.chatStore.snapshot("conv-1")
	if len(items) != 1 {
		t.Fatalf("sent %d messages, want exactly 1 multi-line message", len(items))
	}
	if items[0].Text != "line one\nline two" {
		t.Fatalf("newlines must survive the send path, got %q", items[0].Text)
	}
}

// TestComposerDraftSurvivesScreenSwitchAndFailedSend: the buffer survives
// a screen switch and is restored after a failed send.
func TestComposerDraftSurvivesScreenSwitchAndFailedSend(t *testing.T) {
	// Stub screens (not the real factories): switching tabs must not arm
	// live streams from a test harness that has no client set.
	m := newDockTestApp()
	m.dispatch(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.setFocus(focusComposer)

	nm, _ := m.dispatch(keyRunes("half-written thought"))
	m = nm
	if m.dock.Value() != "half-written thought" {
		t.Fatalf("precondition: %q", m.dock.Value())
	}
	// Screen switch (ctrl+w is a structural chord: it switches tabs while
	// composing) must not touch the draft.
	nm, _ = m.dispatch(tea.KeyMsg{Type: tea.KeyCtrlW})
	m = nm
	if m.ActiveTab() != TabWork {
		t.Fatalf("precondition: ctrl+w must switch tabs, got %q", m.ActiveTab())
	}
	if got := m.dock.Value(); got != "half-written thought" {
		t.Fatalf("the draft must survive a screen switch, got %q", got)
	}

	// Send, then fail: the draft comes back into the box.
	m.chatConvID = "conv-9"
	nm, _ = m.dispatch(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm
	if m.dock.Value() != "" {
		t.Fatalf("the box must clear on send, got %q", m.dock.Value())
	}
	nm, _ = m.dispatch(chat.ErrMsg{Where: "send", Err: errors.New("stream closed")})
	m = nm
	if got := m.dock.Value(); got != "half-written thought" {
		t.Fatalf("a failed send must restore the draft, got %q", got)
	}
	if !strings.Contains(m.dock.Err, "stream closed") {
		t.Fatalf("the failure must still surface in the box, got %q", m.dock.Err)
	}
	// The restored draft can be sent again, and the box is still framed.
	nm, cmd := m.dispatch(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm
	if cmd == nil {
		t.Fatal("the restored draft must be sendable")
	}
	assertFrameExact(t, m, 120, 40)
}
