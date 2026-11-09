// Package dock implements the always-present bottom chat composer: a real bordered box (Composer
// 2.0) wrapping a bubbles/textarea that grows from MinInputRows to MaxInputRows rows.
package dock

import (
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/beardedparrott/orchicon/internal/chatlimits"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// NewlineMode selects how Enter inserts a newline instead of sending.
type NewlineMode string

const (
	NewlineAltEnter       NewlineMode = "alt+enter"
	NewlineBackslashEnter NewlineMode = "backslash-enter"
	// NewlineCtrlJ is the chord that works where the others cannot, and on Windows it is the default.
	//
	// WHY IT EXISTS. The two "natural" chords are both unavailable on a native Windows console, and neither
	// reason is ours to fix:
	//
	//   - SHIFT+ENTER never arrives as a distinguishable key. Windows Terminal emits no distinct VT sequence
	//     for it by default, and the console decoder (bubbletea's key_windows.go) maps VK_RETURN to KeyEnter
	//     reading ONLY Alt — it never consults Shift. So the platform is told "Enter", and Enter sends.
	//   - ALT+ENTER IS CONSUMED BY THE TERMINAL. Windows Terminal binds alt+enter to toggleFullscreen, which
	//     is why the operator saw the tab bar disappear instead of a newline: the key never reached orch at
	//     all. No application can recover a chord its host has claimed.
	//
	// CTRL+J has neither problem: Ctrl+J produces the control character 0x0A, which both the Windows console
	// decoder and the ANSI byte reader report as KeyCtrlJ, and Windows Terminal leaves it unbound. It also
	// works on Linux and macOS terminals, so it is a universal fallback rather than a Windows special case.
	NewlineCtrlJ NewlineMode = "ctrl+j"
	NewlineBoth  NewlineMode = "both"
)

// ParseNewlineMode maps a config string to a NewlineMode (default
// alt+enter on anything else).
func ParseNewlineMode(s string) NewlineMode {
	switch NewlineMode(strings.ToLower(strings.TrimSpace(s))) {
	case NewlineBackslashEnter, "backslash+enter", "\\+enter":
		return NewlineBackslashEnter
	case NewlineCtrlJ, "ctrl-j", "ctrlj":
		return NewlineCtrlJ
	case NewlineAltEnter:
		return NewlineAltEnter
	case NewlineBoth, "all":
		return NewlineBoth
	default:
		// AN UNRECOGNISED VALUE FALLS BACK TO THE PLATFORM DEFAULT, not to alt+enter.
		return DefaultNewlineMode()
	}
}

// ChordReachesOrch reports whether a newline mode's chord can actually be DELIVERED on this
// platform.
func ChordReachesOrch(mode NewlineMode) bool { return chordReachesOrchFor(mode, runtime.GOOS) }

// chordReachesOrchFor is ChordReachesOrch's decision, with the platform injected so it is testable from any host.
//
// PER CHORD ON WINDOWS, not "everything except alt+enter", because the two chords fail for DIFFERENT reasons and
// only one of them is a terminal binding:
//
//   - alt+enter: Windows Terminal binds it to toggleFullscreen. Claimed by the terminal.
//   - shift+enter: not claimable by the terminal at all. The Windows console decoder maps VK_RETURN to KeyEnter
//     reading ONLY Alt — it never consults Shift — so no terminal binding can make it arrive distinct. (The
//     backslash chord needs Shift too, for its backslash, but that shift produces a RUNE, which is decoded
//     normally, so the chord itself works.)
//   - ctrl+j: delivered on both input paths and unbound in the terminal, so it always reaches the program.
func chordReachesOrchFor(mode NewlineMode, goos string) bool {
	if goos != "windows" {
		return true
	}
	switch mode {
	case NewlineCtrlJ:
		return true
	case NewlineAltEnter, NewlineBoth:
		// NewlineBoth's FIRST named chord is ctrl+j, so it still yields a working chord on Windows; but the mode
		// as a value is not the recommended one there, and mentioning alt+enter in the hint would mislead.
		return mode == NewlineBoth
	default:
		// backslash-enter.
		return true
	}
}

// DefaultNewlineMode is the newline chord a profile gets when it has not chosen one.
func DefaultNewlineMode() NewlineMode { return defaultNewlineModeFor(runtime.GOOS) }

// defaultNewlineModeFor is DefaultNewlineMode's decision, with the platform injected so it is testable from any
// host.
func defaultNewlineModeFor(goos string) NewlineMode {
	if goos == "windows" {
		return NewlineCtrlJ
	}
	return NewlineAltEnter
}

// Composer geometry (Composer 2.0).
const (
	// MinInputRows is the composer's default input height.
	MinInputRows = 3
	// MaxInputRows is the ceiling used when the shell has NOT told the dock how tall the viewport is —
	// a dock built in isolation (a unit test, an embedder).
	MaxInputRows = 8
	// minContentRows is the room the composer must leave for the screen above it: it grows into the
	// space that is going spare, never into the space the transcript or the panes are using.
	minContentRows = 6
	// boxChrome is the box's horizontal chrome: 2 border cells + 2 padding
	// cells on each side.
	boxChrome = 6
	// promptWidth is the "❯ " the dock paints on the input's first row.
	promptWidth = 2
)

// Model is the chat dock composer.
type Model struct {
	ta      textarea.Model
	Focused bool
	Chip    string // context chip (what outgoing messages inject)
	Notice  string // status line (stream state, pointers)
	// charLimit is the message bound this composer feeds (SetCharLimit); 0 disables the near-limit counter.
	charLimit int
	// replyInFlight drives the STOP affordance: while a turn is streaming, the affordance row offers
	// ctrl+y — the operator's "we need a stop button/control key + hint in composer that can interrupt.
	replyInFlight bool
	// stopRunHint, when non-empty, is the ACTIVE SCREEN's stop chord — advertised while the operator is
	// looking at a running item/run, so the composer names the key that stops it.
	stopRunHint string
	// abortRunHint is the RUN-level abort chord (AbortWorkflow), advertised while the active screen
	// holds a live run.
	abortRunHint string
	Err          string // inline error strip (themed red)
	Width        int
	Height       int // allocated rows (set by the shell)
	// viewportRows is the WHOLE terminal height, set by the shell each layout.
	viewportRows int
	Newlines     NewlineMode

	// Context is the ACTIVE screen's shortcut list (the shell derives it from the screen's HintLine),
	// rendered at the head of the affordance row so the guidance always names what the current page
	// can.
	Context string

	// Stats is the session stat strip (ask model · context · tokens · cache · cost), rendered
	// RIGHT-ALIGNED on the box's bottom row — the operator's "bottom right corner of the chat box".
	Stats string
	Mode  string
	// Fullsend is the FULLSEND indicator, rendered immediately to the LEFT of the mode pill (mirroring
	// the GUI, which places its fullsend dropdown left of its mode dropdown).
	Fullsend string
	// Model is the ask model ref, rendered LEFT-aligned on the same stat row.
	Model string

	// sendRequest is a non-nil callback when Enter produced a send; the
	// shell checks+clears it after Update (avoids channel plumbing).
	sendRequest string
	// lastSent is the text of the most recent send: the draft source for
	// RestoreDraft (a failed send puts the message back in the box).
	lastSent string

	// blinkStart is the textarea's focus command, captured when the dock takes focus.
	blinkStart tea.Cmd

	// pastePathHook, when set, is offered every BRACKETED PASTE before it is inserted: it returns a
	// command when the pasted text is a FILE PATH it wants to attach.
	pastePathHook func(text string) tea.Cmd

	// selectAll marks the WHOLE composer as selected, so the operator can copy it all or throw it away
	// in one gesture.
	selectAll bool
}

// SetPastePathHook installs the bracketed-paste file-path hook (see pastePathHook).
func (m *Model) SetPastePathHook(fn func(string) tea.Cmd) { m.pastePathHook = fn }

// InsertText adds text at the cursor — the clipboard-paste path (ctrl+v, when the clipboard holds text
// rather than an image). It goes through the textarea so the caret and the box's height follow.
func (m *Model) InsertText(s string) {
	if s == "" {
		return
	}
	m.ta.InsertString(s)
	m.resizeTa()
}

// SelectAll marks the whole composer selected. The SHELL owns the clipboard, so it copies the buffer on the
// way in; this only puts the composer into the state where the next key replaces what is there.
func (m *Model) SelectAll() {
	if m.ta.Value() == "" {
		return // nothing to select, and an empty "selection" would just swallow the next key
	}
	m.selectAll = true
	m.Notice = fmt.Sprintf("all %d characters selected — type or backspace replaces, esc keeps",
		len([]rune(m.ta.Value())))
}

// SelectAllActive reports whether the whole composer is currently selected (tests and the shell).
func (m *Model) SelectAllActive() bool { return m.selectAll }

// ClearSelectAll drops the selection without touching the text (esc, or any key that is not a replace).
func (m *Model) ClearSelectAll() { m.selectAll = false }

// New builds the dock.
func New() Model {
	ta := textarea.New()
	// No placeholder: the box shows a plain blinking cursor instead of hint text.
	ta.Placeholder = ""
	ta.Prompt = ""
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	// THE PLATFORM DEFAULT, not alt+enter: on Windows that chord belongs to Windows Terminal, so a dock built
	// without an explicit profile setting must not default to one that cannot work (see NewlineCtrlJ).
	m := Model{ta: ta, Newlines: DefaultNewlineMode()}
	// themeStyles pins every textarea cell to the box's surface so no cell can be left unpainted, and
	// makes the caret a visible accent block.
	m.themeStyles()
	_ = m.ta.Focus() // safe: textarea always lets you re-focus
	m.resizeTa()
	return m
}

// ApplyTheme re-pins the composer's captured styles to the ACTIVE palette.
// The shell calls it after /theme so the box never keeps the old theme.
func (m *Model) ApplyTheme() { m.themeStyles() }

// styledTa returns the textarea with this theme's styles applied and the
// ACTIVE-style pointer re-resolved.
//
// Two bubbles details make this necessary, and both produced visible bugs:
//
//   - EndOfBuffer defaults to a FOREGROUND-only style, and bubbles renders the
//     cursor line's TRAILING PADDING with that style and nothing else. With no
//     background on those cells they render on the terminal's own background —
//     the black band that appeared the moment the operator typed.
//   - Style resolution goes through an unexported `style` POINTER that Focus()
//     /Blur() re-point. Assigning the Style structs alone can leave the pointer
//     on a stale copy, so the assignment is followed by Focus()/Blur() to make
//     bubbles re-resolve it against the styles just set.
func (m *Model) styledTa() textarea.Model {
	ta := m.ta
	base := lipgloss.NewStyle().Background(theme.ComposerFill).Foreground(theme.Text)
	dim := lipgloss.NewStyle().Background(theme.ComposerFill).Foreground(theme.TextFaint)

	ta.FocusedStyle.Base = base
	ta.BlurredStyle.Base = base
	ta.FocusedStyle.Text = base
	ta.BlurredStyle.Text = base
	// The cursor's own line, and the padding that follows the text on it.
	ta.FocusedStyle.CursorLine = base
	ta.BlurredStyle.CursorLine = base
	ta.FocusedStyle.EndOfBuffer = base
	ta.BlurredStyle.EndOfBuffer = base
	ta.FocusedStyle.Prompt = base
	ta.BlurredStyle.Prompt = base
	ta.FocusedStyle.Placeholder = dim
	ta.BlurredStyle.Placeholder = dim
	ta.FocusedStyle.CursorLineNumber = dim
	ta.BlurredStyle.CursorLineNumber = dim
	ta.FocusedStyle.LineNumber = dim
	ta.BlurredStyle.LineNumber = dim

	// THE CARET.
	ta.Cursor.Style = theme.ComposerCursor
	ta.Cursor.TextStyle = base

	// THE DOCK OWNS THE CARET'S BLINK LOOP.
	ta.Cursor.SetMode(cursor.CursorStatic)

	// Re-resolve the active style pointer against what we just assigned.
	if m.Focused {
		_ = ta.Focus()
	} else {
		ta.Blur()
	}
	return ta
}

// Placeholder returns the composer's placeholder text ("" — the box shows a
// plain cursor; the hints live on the affordance row).
func (m *Model) Placeholder() string { return m.ta.Placeholder }

// themeStyles re-pins the model's own textarea styles (kept so Focus/Blur and
// any direct render of m.ta stay consistent).
func (m *Model) themeStyles() {
	m.ta = m.styledTa()
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

// RestoreDraft puts the last sent text back into the box after a failed send.
func (m *Model) RestoreDraft() bool {
	if m.lastSent == "" || strings.TrimSpace(m.ta.Value()) != "" {
		return false
	}
	m.SetValue(m.lastSent)
	return true
}

// boxContentCol is the box's left chrome before anything is painted inside it: one border cell plus
// the ComposerBox style's two cells of horizontal padding.
const boxContentCol = 1 + 2

// Caret reports the caret's line and column.
func (m *Model) Caret() (line, col int) {
	return m.ta.Line(), m.ta.LineInfo().ColumnOffset
}

// TextOrigin is the dock row and column at which the input's TEXT begins.
func (m *Model) TextOrigin() (row, col int) {
	row = 1 // the box's top border
	if m.Chip != "" {
		row++ // the chip is the box's first inner row
	}
	return row, boxContentCol + promptWidth
}

// caretRow is one VISUAL row of the composer's input: the logical line it belongs to, and the rune range
// within that line which this row displays.
type caretRow struct{ line, start, end int }

// goToTop walks the cursor to the very FIRST visual row (line 0, row offset 0).
func (m *Model) goToTop() {
	for i := 0; i < walkLimit; i++ {
		li := m.ta.LineInfo()
		if m.ta.Line() == 0 && li.RowOffset == 0 {
			return
		}
		beforeLine, beforeStart, beforeRow := m.ta.Line(), li.StartColumn, li.RowOffset
		m.ta.CursorUp()
		li2 := m.ta.LineInfo()
		if m.ta.Line() == beforeLine && li2.StartColumn == beforeStart && li2.RowOffset == beforeRow {
			return // the cursor cannot move: this is already the top
		}
	}
}

// walkLimit bounds every cursor walk in this file.
const walkLimit = 100000

// caretRows enumerates the input's visual rows by WALKING THE TEXTAREA'S OWN CURSOR, and restores the cursor
// afterwards.
//
// WHY IT DOES NOT COMPUTE THE LAYOUT ITSELF, which is what this used to do and what made the click fail. The
// textarea wraps with `cellbuf.Wrap` — a WORD-WRAPPING algorithm: it breaks at word boundaries and splits a
// word only when it must. So "the number of rows a line takes" is NOT `ceil(cells/width)`; a 200-cell line in
// a 70-cell box may take 3 rows or 4 depending on where its words fall, and nothing outside the widget can
// predict which. (The dock's `visualRows()` still approximates that for SIZING, which is why the composer
// grows a touch less readily than it could — a separate, cosmetic shortfall, left alone here.)
//
// The widget's cursor is the one thing that knows the real layout, and it answers through public API:
// `CursorDown` moves exactly ONE VISUAL ROW and — by its own construction — leaves the cursor at the START of
// the row it moved to (`m.col = nli.StartColumn`). So walking from the top and reading `Line()` plus
// `LineInfo().StartColumn` at each step gives the exact row map, for any content, with no guess about
// wrapping.
//
// THE WALK RESTORES THE CURSOR, which is why it runs only on a CLICK (a rare gesture where moving the caret
// is the whole point) and never per render: it disturbs cursor state, so doing it on every frame would quietly
// break the operator's vertical cursor movement.
// scrollOffset is the first VISIBLE row of the scrolled input area.
//
// IT REPLACES A REFUSAL, and that refusal was the operator's bug:
//
//	"We have tried to fix this two different times but it seems like the composer in the TUI when
//	 clicking with the mouse is not always accurate. You have to click under it to position it
//	 right instead of right on the text. Not sure why this works sometimes and other times it does
//	 not."
//
// ClickAt used to know the offset in exactly ONE state — the caret at the end of the buffer — and
// REFUSED every other click while the content was scrolled. So the first click worked (typing and
// pasting leave the caret at the end, which pins the view to the bottom) and every click after it
// was silently ignored, because the first click moved the caret off the end. "Works sometimes and
// other times it does not" was literally one-click-then-broken. Measured before this: a 40-line
// draft, click one → caret 744, click two → still 744, click three → still 744.
//
// The offset IS knowable, from public API only:
//
//	cursorRow  = (every row of every EARLIER line, soft-wrapped) + LineInfo().RowOffset
//	offset     = cursorRow when the cursor is unclamped, since RepositionView scrolls the minimum
//	             needed to reveal the cursor and therefore only clamps when the cursor is in the
//	             first or last visible row.
//
// The TWO CLAMPED states are the ones the old code special-cased, and each is recovered exactly:
//
//	cursor at the END   → the view is pinned to the bottom, so offset = total - height
//	cursor near the TOP → the view is pinned to the top, offset = 0
//
// A cursor strictly inside the view cannot be clamped (clamping moves the offset only until the
// cursor's row is inside the window), which is what makes "offset = cursorRow" exact rather than a
// guess. There is no residual guessed state, so nothing is refused.
//
// The wrap width is the textarea's own (m.ta.Width()), and memoizedWrap wraps by the textarea's
// width — the same figure — so the row counts agree.
func (m *Model) scrollOffset(totalRows int) int {
	height := m.InputRows()
	if totalRows <= height {
		return 0 // everything fits: the offset is necessarily zero
	}
	lines := strings.Split(m.ta.Value(), "\n")
	li := m.ta.LineInfo()
	// The cursor's own line, as a rune count, decides "at the end of the buffer".
	cursorLine := m.ta.Line()
	if cursorLine < 0 || cursorLine >= len(lines) {
		return totalRows - height
	}
	lastLineLen := len([]rune(lines[len(lines)-1]))
	atEnd := cursorLine == len(lines)-1 && li.StartColumn+li.ColumnOffset == lastLineLen
	if atEnd {
		return totalRows - height // the widget pins the caret to the bottom row
	}

	w := m.ta.Width()
	cursorRow := 0
	for i := 0; i < cursorLine; i++ {
		cursorRow += wrappedRowCount(lines[i], w)
	}
	cursorRow += li.RowOffset

	// THE CLAMPED-AT-THE-TOP STATE: if the cursor is inside the first window the view need not
	// have scrolled at all, and RepositionView never scrolls above 0.
	if cursorRow < height {
		return 0
	}
	if cursorRow > totalRows-height {
		cursorRow = totalRows - height
	}
	return cursorRow
}

// wrappedRowCount is how many visual rows a single logical line occupies at the given
// width: ceil(len/width), and at least one (an empty line is still a row).
func wrappedRowCount(line string, width int) int {
	if width <= 0 {
		return 1
	}
	n := (len([]rune(line)) + width - 1) / width
	if n < 1 {
		n = 1
	}
	return n
}

func (m *Model) caretRows() []caretRow {
	// Remember the caret, so a click that turns out not to be placeable leaves it alone.
	saveLine := m.ta.Line()
	li := m.ta.LineInfo()
	saveCol := li.StartColumn + li.ColumnOffset

	m.goToTop()
	rows := []caretRow{{line: m.ta.Line(), start: 0}}
	for i := 0; i < walkLimit; i++ {
		atLine, atStart := m.ta.Line(), m.ta.LineInfo().StartColumn
		m.ta.CursorDown()
		if m.ta.Line() == atLine && m.ta.LineInfo().StartColumn == atStart {
			break // the cursor could not move: this was the last visual row
		}
		rows = append(rows, caretRow{line: m.ta.Line(), start: m.ta.LineInfo().StartColumn})
	}

	// Close each row's rune range: a row ends where the next row ON THE SAME LINE begins, and the last row of
	// a line ends at the line's own length.
	lineLens := make([]int, 0, 16)
	for _, ln := range strings.Split(m.ta.Value(), "\n") {
		lineLens = append(lineLens, len([]rune(ln)))
	}
	for i := range rows {
		if i+1 < len(rows) && rows[i+1].line == rows[i].line {
			rows[i].end = rows[i+1].start
		} else if rows[i].line < len(lineLens) {
			rows[i].end = lineLens[rows[i].line]
		}
		if rows[i].end < rows[i].start {
			rows[i].end = rows[i].start
		}
	}

	// Restore by ROW INDEX, which the map itself tells us.
	saveRow := 0
	for i, r := range rows {
		if r.line == saveLine && saveCol >= r.start && saveCol <= r.end {
			saveRow = i
		}
	}
	m.goToTop()
	for i := 0; i < saveRow; i++ {
		m.ta.CursorDown()
	}
	m.ta.SetCursor(saveCol)
	return rows
}

// runeAtCell returns the rune index within row r that a click `cell` cells into the row lands on.
func (m *Model) runeAtCell(r caretRow, cell int) int {
	if cell <= 0 {
		return r.start
	}
	lines := strings.Split(m.ta.Value(), "\n")
	if r.line < 0 || r.line >= len(lines) {
		return r.start
	}
	runes := []rune(lines[r.line])
	width, idx := 0, r.start
	for idx < r.end && idx < len(runes) {
		w := lipgloss.Width(string(runes[idx]))
		if width+w > cell {
			break
		}
		width += w
		idx++
	}
	return idx
}

// ClickAt places the caret at a click inside the composer, in DOCK coordinates (the shell converts
// from the frame, since only it knows where the dock starts).
func (m *Model) ClickAt(x, y int) bool {
	originRow, originCol := m.TextOrigin()
	vis := y - originRow
	if vis < 0 || vis >= m.InputRows() {
		return false
	}
	rows := m.caretRows()
	offset := m.scrollOffset(len(rows))
	target := offset + vis

	// A CLICK LEFT OF THE TEXT IS A CLICK AT ITS START, not a refusal: the prompt is two cells of chrome the
	// operator may well have clicked on the way to column one.
	if x < originCol {
		x = originCol
	}

	// BELOW THE TEXT IS THE END OF THE TEXT, which is what a box does: clicking the empty space under a short
	// message puts the caret after the last character rather than refusing or jumping to a line.
	if target >= len(rows) {
		m.goToTop()
		for i := 0; i < len(rows); i++ {
			m.ta.CursorDown()
		}
		m.ta.CursorEnd()
		return true
	}

	// Walk to the clicked row and drop the caret `runeAtCell` into it.
	m.goToTop()
	for i := 0; i < target; i++ {
		m.ta.CursorDown()
	}
	m.ta.SetCursor(m.runeAtCell(rows[target], x-originCol))
	return true
}

// boxInner is the box's content width: the dock width minus the box chrome (border + inner
// padding).
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

// visualRows estimates the rows the wrapped buffer needs, so the box can grow as the operator
// types.
func (m *Model) visualRows() int {
	return m.exactWrappedRows()
}

// exactWrappedRows asks the TEXTAREA for its real wrapped height instead of predicting it.
func (m *Model) exactWrappedRows() int {
	// THE WIDGET KNOWS ITS OWN HEIGHT, and it reports it for the CURSOR's line via LineInfo().Height.
	probe := m.ta
	probe.SetWidth(m.taWidth())

	lines := strings.Split(m.ta.Value(), "\n")
	if len(lines) <= 1 {
		// The single-line case is the common one and the widget answers it exactly.
		if h := m.ta.LineInfo().Height; h > 1 {
			return h
		}
		// An empty draft: one row.
		if h := m.ta.LineInfo().Height; h > 0 {
			return h
		}
		return 1
	}

	// Count the wrapped rows of a single line by handing it to the widget alone and reading its height.
	rowOf := func(line string) int {
		c := m.ta
		c.SetWidth(m.taWidth())
		c.SetHeight(probeRows)
		c.SetValue(line)
		// A fresh value parks the cursor at the end of the line, so Height is that line's wrapped height.
		if h := c.LineInfo().Height; h > 0 {
			return h
		}
		return 1
	}

	total := 0
	for _, line := range lines {
		total += rowOf(line)
	}
	if total < 1 {
		total = 1
	}
	return total
}

// probeRows is the height given to the throwaway textarea copy when measuring the wrapped buffer: large
// enough that the widget never needs to scroll, so every wrapped row is rendered and countable.
const probeRows = 200

// InputRows is the number of input rows the box shows: at least MinInputRows, growing with the
// buffer up to however many rows the viewport can spare (maxInputRows.
func (m *Model) InputRows() int {
	n := m.visualRows()
	if n < MinInputRows {
		n = MinInputRows
	}
	if max := m.maxInputRows(); n > max {
		n = max
	}
	return n
}

// maxInputRows is the tallest the input may grow: whatever the viewport can spare once the shell's
// chrome, the box's own chrome and minContentRows are accounted for.
func (m *Model) maxInputRows() int {
	if m.viewportRows <= 0 {
		return MaxInputRows
	}
	// 4 = the shell's fixed chrome (tab bar + rule, separator + footer);
	// 2 = the box's own top and bottom border.
	overhead := 4 + 2 + m.HintRows() + m.NoticeRows()
	if m.Chip != "" {
		overhead++
	}
	n := m.viewportRows - overhead - minContentRows
	if n < MinInputRows {
		n = MinInputRows
	}
	return n
}

// SetViewportRows tells the dock how tall the terminal is, which is what the growth
// ceiling is derived from. Called by the shell on every layout.
func (m *Model) SetViewportRows(n int) {
	if m.viewportRows == n {
		return
	}
	m.viewportRows = n
	// Re-fit the textarea immediately: a resize that changes how tall the composer may
	// grow must not wait for the next keystroke to take effect.
	m.resizeTa()
}

// maxHintRows caps the wrapped affordance row.
const maxHintRows = 3

// hintLines is the affordance row's final rows: the segment-aware wrap, capped at maxHintRows with
// a trailing ellipsis.
func (m *Model) hintLines() []string {
	lines := kit2.WrapHint(m.Hint(), m.boxInner())
	if len(lines) == 0 {
		return []string{""}
	}
	if len(lines) > maxHintRows {
		lines = lines[:maxHintRows]
		lines[maxHintRows-1] = strings.TrimRight(lines[maxHintRows-1], " ") + " …"
	}
	return lines
}

// HintRows is how many rows the affordance hint occupies at the current width
// (see hintLines).
func (m *Model) HintRows() int { return len(m.hintLines()) }

// NoticeRows is how many rows the notice/error strip occupies at the current width (0 when there is
// none).
func (m *Model) NoticeRows() int {
	n := m.wrapRows(m.Err) + m.wrapRows(m.Notice)
	if m.charCounter() != "" {
		n++
	}
	return n
}

// wrapRows is how many rows one strip message occupies at the current width.
func (m *Model) wrapRows(msg string) int {
	if msg == "" {
		return 0
	}
	n := len(kit2.WrapHint(msg, m.boxInner()))
	if n < 1 {
		n = 1
	}
	return n
}

// Lines returns the rows the dock needs: the box's top+bottom border, the input rows (InputRows),
// the affordance row (HintRows.
func (m *Model) Lines() int {
	h := 2 + m.InputRows() + m.HintRows() + m.NoticeRows()
	if m.Chip != "" {
		h++
	}
	h += m.StatsRows()
	return h
}

// StatsRows is 1 when there is a stat strip / mode pill to draw, else 0. The
// shell reserves exactly this many rows, so the box can never overflow.
func (m *Model) StatsRows() int {
	if m.Stats == "" && m.Mode == "" && m.Model == "" && m.Fullsend == "" {
		return 0
	}
	return 1
}

// statLine renders the bottom stat row: the ask model on the LEFT, and the stats followed by the
// mode pill right-aligned against the box edge.
func (m *Model) statLine(inner int) string {
	mode := ""
	if m.Mode != "" {
		mode = "[" + m.Mode + "]"
	}
	// THE LAYOUT MATHS RUNS ON PLAIN STRINGS, and the styled form is assembled at the very end from the
	// same pieces.
	keep := ""
	if m.Fullsend != "" {
		keep = m.Fullsend
	}
	if mode != "" {
		if keep != "" {
			keep += "  "
		}
		keep += mode
	}
	right := m.Stats
	if keep != "" {
		if right != "" {
			right += "  "
		}
		right += keep
	}
	if right == "" && m.Model == "" {
		return ""
	}
	// Keep the right-hand group whole where the pane allows: trim the STATS first, never the pill or
	// the FULLSEND badge.
	if lipgloss.Width(right) > inner {
		avail := inner - lipgloss.Width(keep) - 2
		if avail < 4 {
			avail = 4
		}
		right = ansi.Truncate(m.Stats, avail, "…")
		if keep != "" {
			if right != "" {
				right += "  "
			}
			right += keep
		}
	}
	// Then the model ref: truncate it, and drop it entirely if even that will not
	// fit — the numbers must never be pushed off the edge to make room for a name.
	left := m.Model
	if room := inner - lipgloss.Width(right) - 2; lipgloss.Width(left) > room {
		if room >= 10 {
			left = ansi.Truncate(left, room, "…")
		} else {
			left = ""
		}
	}
	if lipgloss.Width(left)+lipgloss.Width(right) > inner {
		left = ""
	}
	// The row must never exceed `inner`: Pad TRUNCATES, so an overlong line loses its tail — which is
	// how the pill's closing "] " disappeared.
	if lipgloss.Width(right) > inner {
		right = ansi.Truncate(right, inner, "…")
	}
	pad := inner - lipgloss.Width(left) - lipgloss.Width(right)
	if pad < 0 {
		pad = 0
	}
	// THE ASSEMBLY.
	if m.Fullsend == "" {
		return theme.ListMeta.Render(left + strings.Repeat(" ", pad) + right)
	}
	headed := left + strings.Repeat(" ", pad)
	if m.Stats != "" {
		headed += m.Stats + "  "
	}
	line := theme.ListMeta.Render(headed) + theme.FullsendBadge.Render(m.Fullsend)
	if mode != "" {
		line += theme.ListMeta.Render("  " + mode)
	}
	return line
}

// Focus / Blur move keyboard focus into/out of the composer.
func (m *Model) Focus() {
	m.Focused = true
	// Capture — do not discard — the commands that start the caret's loop and its own tick.
	m.blinkStart = tea.Batch(m.ta.Focus(), dockBlinkTick())
}

// TakeBlinkStart hands out (once) the command that starts the caret's blink loop, captured when the
// dock took focus.
func (m *Model) TakeBlinkStart() tea.Cmd {
	c := m.blinkStart
	m.blinkStart = nil
	return c
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

// SetCharLimit installs the message bound this composer feeds, so it can show a counter as the bound is
// APPROACHED rather than only after a send is refused. 0 disables the counter.
func (m *Model) SetCharLimit(n int) { m.charLimit = n }

// charCounter is the near-limit counter's text, or "" when there is nothing to say.
func (m *Model) charCounter() string {
	if m.charLimit <= 0 {
		return ""
	}
	n := len([]rune(m.ta.Value()))
	if n < chatlimits.WarnAt {
		return ""
	}
	s := groupDigits(n) + " / " + groupDigits(m.charLimit) + " characters"
	if n > m.charLimit {
		s += " — over the limit; trim before sending"
	}
	return s
}

// groupDigits renders 100000 as 100,000 so a six-figure count is readable at a glance.
func groupDigits(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// SendingAck is the transient ack the composer writes the instant Enter fires, so a send that
// produces no visible reaction cannot be mistaken for a dead key (see the enter case in Update).
const SendingAck = "sending …"

// SettleSendingAck clears the strip IF it still holds the send ack, reporting whether it did.
func (m *Model) SettleSendingAck() bool {
	if m.Notice != SendingAck {
		return false
	}
	m.Notice = ""
	return true
}

// SetReplyInFlight toggles the STOP affordance on the affordance row (see Hint).
func (m *Model) SetReplyInFlight(on bool) { m.replyInFlight = on }

// resizeTa keeps the textarea width+height synced to the box.
func (m *Model) resizeTa() {
	m.ta.SetWidth(m.taWidth())
	m.ta.SetHeight(m.InputRows())
}

// Update handles composer keys. Returns (consumed, cmd): consumed=false
// means the shell should fall through (only when unfocused).
func (m *Model) Update(msg tea.Msg) (handled bool, cmd tea.Cmd) {
	// The caret's blink loop starts from the command captured when focus was taken, and it has to be
	// dispatched with WHATEVER this message produces.
	if start := m.blinkStart; start != nil {
		m.blinkStart = nil
		defer func() { cmd = tea.Batch(start, cmd) }()
	}
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
	case dockBlinkMsg:
		// Our own tick: toggle the caret and re-arm. ONE loop, and nothing an edit does
		// can cancel it (see dockBlinkTick).
		m.ta.Cursor.Blink = !m.ta.Cursor.Blink
		return true, dockBlinkTick()
	case tea.KeyMsg:
		// ANY key makes the caret SOLID, which is what bubbles did on a cursor move (Blink=false) and what
		// keeps it from blinking out mid-word.
		m.ta.Cursor.Blink = false
		// THE ALL-SELECTED STATE GETS FIRST REFUSAL.
		if m.selectAll {
			if handled, cmd := m.selectAllKey(k); handled {
				return true, cmd
			}
		}
		switch {
		case k.Paste:
			// Bracketed paste: bubbletea collapses the byte sequence into one KeyMsg with embedded newlines —
			// render verbatim, never send.
			if m.pastePathHook != nil {
				if cmd := m.pastePathHook(string(k.Runes)); cmd != nil {
					return true, cmd
				}
			}
			return true, m.pasteCmd(k)
		case k.Type == tea.KeyCtrlJ && (m.Newlines == NewlineCtrlJ || m.Newlines == NewlineBoth):
			// NOTE: this branch is reached only when the key still HAS its ctrl+j identity here. THIS BRANCH
			// MUST COME BEFORE THE enterKey ARMS.
			m.insertNewline("ctrl+j")
			return true, nil
		case enterKey(k) && k.Alt:
			if m.Newlines == NewlineAltEnter || m.Newlines == NewlineBoth || m.Newlines == NewlineCtrlJ {
				// alt+enter still newlines wherever it is DELIVERED (Linux/macOS, or a Windows terminal whose
				// bindings leave it alone).
				m.insertNewline("alt+enter")
				return true, nil
			}
			return true, m.requestSend()
		case isCtrlJRune(k) && (m.Newlines == NewlineCtrlJ || m.Newlines == NewlineBoth):
			// CTRL+J AS A CHARACTER, which is HOW WINDOWS DELIVERS IT.
			m.insertNewline("ctrl+j")
			return true, nil
		case enterKey(k):
			// NOTHING here may silently swallow a send. The composer's own hint
			// documents the contract as "enter send · alt+enter newline", so any
			// Enter variant that is not an explicitly configured newline chord
			// SENDS.
			//
			// Three real cases made Enter appear dead, and two are fixed:
			//  1. a "shift+enter inserts a newline" convenience the hint never
			//     documented (removed here). It is BACK, but not as a branch in this
			//     switch: the terminal's Shift+Enter sequence is rewritten to the
			//     alt+enter chord as input is read (internal/tui/input), so it
			//     arrives as the branch above;
			//  2. Enter arriving as LF rather than CR — a different KeyType that
			//     matched no branch here and no textarea keymap (handled by
			//     enterKey).
			// The notice below is the third: it makes "did the send branch run at
			// all" observable from the UI. A key that produces neither a send nor
			// a visible change is indistinguishable from a dead key, which is
			// exactly how this took several rounds to pin down.
			if m.leadingBackslash() && (m.Newlines == NewlineBackslashEnter || m.Newlines == NewlineBoth) {
				// A trailing lone backslash + Enter = newline (the escape
				// hatch); strip the backslash and wrap.
				v := m.ta.Value()
				m.ta.SetValue(strings.TrimSuffix(v, "\\"))
				m.insertNewline("\\+enter")
				return true, nil
			}
			// Ack the send BEFORE it happens: sendFromComposer replaces this with the outcome (context
			// injected, an RPC error, or the conversation opening), so it can never linger as a false promise.
			m.Notice = SendingAck
			return true, m.requestSend()
		default:
			ta, cmd := m.ta.Update(msg)
			m.ta = ta
			m.resizeTa()
			return true, cmd
		}
	}
	// A NON-key message (notably the textarea's cursor BlinkMsg) must still reach the textarea.
	ta, cmd := m.ta.Update(msg)
	m.ta = ta
	m.resizeTa()
	return true, cmd
}

// selectAllKey handles a key while the whole composer is selected. handled=false means the key is not a
// replacing one: the selection is dropped and the caller handles the key normally.
func (m *Model) selectAllKey(k tea.KeyMsg) (handled bool, cmd tea.Cmd) {
	switch {
	case k.String() == "esc":
		m.ClearSelectAll()
		m.Notice = "selection cancelled"
		return true, nil
	case k.String() == "backspace", k.String() == "delete", k.String() == "ctrl+u":
		m.ta.Reset()
		m.ClearSelectAll()
		m.resizeTa()
		m.Notice = "composer cleared"
		return true, nil
	case k.Paste:
		// A paste replaces the selection, like typing does.
		m.ta.Reset()
		m.ClearSelectAll()
		return true, m.pasteCmd(k)
	case len(k.Runes) > 0 && !k.Alt:
		// A printable key overwrites the selection.
		m.ta.Reset()
		m.ClearSelectAll()
		m.ta.InsertString(string(k.Runes))
		m.resizeTa()
		m.Notice = ""
		return true, nil
	}
	// Not a replacing key: drop the selection and let it through.
	m.ClearSelectAll()
	return false, nil
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

// enterKey reports whether a key IS the Enter key, whichever byte the terminal
// sends for it.
//
// Enter does not always arrive as KeyEnter. Most terminals send CR (0x0D), which
// bubbletea reports as KeyEnter — but some terminals and PTY configurations send
// LF (0x0A) instead, and bubbletea reports THAT as KeyCtrlJ (keyLF), which is a
// DIFFERENT KeyType. Nothing in this composer handled ctrl+j and nothing in the
// textarea bound it either, so on such a terminal pressing Enter did literally
// NOTHING: no send, no newline, no reaction at all — the operator's "enter is not
// doing anything at all".
//
// The documented contract is "enter send", so both spellings of the key send.
// Treating ctrl+j as Enter costs nothing real: it is unbound and undocumented here,
// and "my Enter key does nothing" is a far worse failure than losing a chord
// nobody can invoke through the UI. (KeyCtrlM is the same value as KeyEnter, so the
// first case already covers it.)
// isCtrlJRune reports whether a key message IS Ctrl+J arriving as a CHARACTER rather than a key code.
//
// WHY BOTH FORMS EXIST. The control character for Ctrl+J is LF (0x0A), and how that reaches bubbletea depends
// entirely on the input path:
//
//   - the ANSI BYTE reader (Linux, macOS, and any terminal streaming bytes) decodes 0x0A to KeyCtrlJ;
//   - the WINDOWS CONSOLE reader — which is the path in use whenever CONIN$ is opened — reads the key record and
//     reports the character it produced, so the same chord arrives as KeyRunes("\n"). It is not reported as
//     KeyEnter either, because the console's VK_RETURN arm is what maps to KeyEnter and Ctrl+J is not VK_RETURN.
//
// A composer that only knew the first form therefore had a newline chord that worked on Linux and macOS and did
// nothing on Windows — while the hint told the operator to press it.
//
// IT IS DELIBERATELY NARROW. Only a rune message whose text is EXACTLY "\n" qualifies: a plain Enter arrives as
// KeyEnter (a different Type), so this cannot swallow a send, and a multi-character paste is not a chord.
func isCtrlJRune(k tea.KeyMsg) bool {
	return k.Type == tea.KeyRunes && len(k.Runes) == 1 && k.Runes[0] == '\n'
}

func enterKey(k tea.KeyMsg) bool {
	switch k.Type {
	case tea.KeyEnter, tea.KeyCtrlJ:
		return true
	}
	return false
}

// insertNewline appends a newline instead of sending, and SAYS SO.
func (m *Model) insertNewline(chord string) {
	ta, _ := m.ta.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: false})
	m.ta = ta
	m.resizeTa()
	if chord == "" {
		chord = "newline chord"
	}
	m.Notice = "newline inserted (" + chord + ") — enter sends"
}

// leadingBackslash reports whether the buffer ends with a lone "\" (newline-by-escape).
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

// Hint is the persistent affordance row: the ctrl+g focus chord FIRST (so it survives truncation on
// a narrow pane), then the ACTIVE screen's shortcut list (Context.
func (m *Model) Hint() string {
	// THE HINT NAMES THE CHORD THIS PROFILE ACTUALLY USES, and on Windows that is ctrl+j — because
	// naming alt+enter there is naming a key the terminal has already claimed (toggleFullscreen in
	// Windows.
	nl := "alt+enter"
	switch m.Newlines {
	case NewlineBackslashEnter:
		nl = "\\+enter"
	case NewlineCtrlJ:
		nl = "ctrl+j"
	case NewlineBoth:
		nl = "ctrl+j or alt+enter"
	}
	// ctrl+d is named here because it is the ONLY way to open the diff sidebar from the keyboard, and
	// it was invisible: the operator's "the shortcut advice in the composer is missing for ctrl+d.
	parts := []string{"ctrl+g text"}
	if m.replyInFlight {
		parts = append(parts, "ctrl+y stop")
	}
	// THE RUN'S STOP COMES BEFORE THE REPLY'S, because when both are live the RUN is what the
	// operator is trying to interrupt — and `ctrl+y` would only abort the chat turn.
	if s := strings.TrimSpace(m.stopRunHint); s != "" {
		parts = append(parts, s)
	}
	if s := strings.TrimSpace(m.abortRunHint); s != "" {
		parts = append(parts, s)
	}
	parts = append(parts, "ctrl+d diff")
	if s := strings.TrimSpace(m.Context); s != "" {
		parts = append(parts, s)
	}
	parts = append(parts, "enter send · "+nl+" newline · / commands")
	return strings.Join(parts, " · ")
}

// SetContext sets the active screen's shortcut list for the affordance row.
func (m *Model) SetContext(s string) { m.Context = s }

// SetStopRunHint advertises the active screen's "stop the running item" chord on the affordance
// row.
func (m *Model) SetStopRunHint(s string) { m.stopRunHint = strings.TrimSpace(s) }

// SetAbortRunHint advertises the active screen's run-abort chord (AbortWorkflow). Empty
// clears it. Set by the shell from the focused row, so it cannot outlive the live run.
func (m *Model) SetAbortRunHint(s string) { m.abortRunHint = strings.TrimSpace(s) }

// View renders the composer as a bordered box: the context chip, the input rows (the "❯ " prompt on
// the first), the notice/error strip, and the persistent affordance row.
func (m *Model) View() string {
	m.resizeTa()
	inner := m.boxInner()

	rows := make([]string, 0, m.Lines())
	if m.Chip != "" {
		rows = append(rows, theme.ListMeta.Render(fit("["+m.Chip+"]", inner)))
	}
	// WHILE THE WHOLE BUFFER IS SELECTED THE ROWS ARE PAINTED ON THE SELECTION FILL.
	selecting := m.selectAll
	input := m.inputLines()
	if selecting {
		input = m.selectionLines()
	}
	for i, l := range input {
		if selecting {
			// ONE UNIFORM FILL ACROSS THE ROW, prompt included and padded to the box's inner width.
			prefix := "  "
			if i == 0 {
				prefix = "❯ "
			}
			rows = append(rows, theme.ComposerSelect.Render(fit(prefix+l, inner)))
			continue
		}
		if i == 0 {
			// The dock paints the prompt on the first input row (the textarea's own prompt stays empty so
			// wrapped rows are not re-prefixed); continuation rows are indented to match.
			rows = append(rows, theme.ListTitle.Render("❯ ")+l)
			continue
		}
		rows = append(rows, "  "+l)
	}
	if m.Err != "" {
		for _, l := range kit2.WrapHint(m.Err, inner) {
			rows = append(rows, theme.ErrorText.Render(l))
		}
	}
	// BOTH STRIPS ARE DRAWN — see NoticeRows. The notice used to be an `else if`, so it vanished whenever
	// an unrelated error was on screen.
	if m.Notice != "" {
		for _, l := range kit2.WrapHint(m.Notice, inner) {
			rows = append(rows, theme.HintText.Render(l))
		}
	}
	// THE COUNTER IS ONE ROW, and NoticeRows counts it, so the shell reserves it and the block stays
	// exactly Lines() tall.
	if counter := m.charCounter(); counter != "" {
		rows = append(rows, m.counterRow(counter, inner))
	}
	// The affordance row WRAPS (capped) rather than truncating, so the important
	// shortcuts stay readable on a narrow composer.
	for _, l := range m.hintLines() {
		rows = append(rows, theme.HintText.Render(fit(l, inner)))
	}
	// The stat row sits BELOW the affordance row, flush against the box's
	// bottom edge and right-aligned: the stats then the mode pill.
	if sl := m.statLine(inner); sl != "" {
		rows = append(rows, sl)
	}

	// The repair takes the composer's BACKGROUND-ONLY fill: ComposerBox itself has a border + padding,
	// and re-asserting through THAT injected border glyphs into the row.
	return theme.RepairAfterResets(
		theme.ComposerBox.Render(strings.Join(fitAll(rows, inner), "\n")),
		theme.ComposerBg)
}

// inputLines returns exactly InputRows() rows of the input view (the textarea renders its own
// height, but the box's row budget is never left to it).
func (m *Model) inputLines() []string {
	n := m.InputRows()
	// Read the textarea DIRECTLY — do NOT re-style per render.
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

// selectionLines returns the composer's input rows as PLAIN text with NO caret, for the SELECTED
// state.
func (m *Model) selectionLines() []string {
	ta := m.ta
	ta.Blur()
	n := m.InputRows()
	src := strings.Split(ta.View(), "\n")
	if len(src) > n {
		src = src[:n]
	}
	out := make([]string, 0, n)
	for _, l := range src {
		out = append(out, ansi.Strip(l))
	}
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

// --- the dock's own caret blink ------------------------------------------
//
// THE DOCK OWNS THE CARET'S BLINK LOOP, and that is a performance fix with a
// correctness-adjacent reason for existing.
//
// What bubbles does on its own: every time the CURSOR MOVES it re-arms the blink —
//
//	if (newRow != oldRow || newCol != oldCol) && m.Cursor.Mode() == cursor.CursorBlink {
//	    m.Cursor.Blink = false
//	    cmd = m.Cursor.BlinkCmd()
//	}
//
// — and BlinkCmd CANCELS the previous timer before starting a new one. A cancelled
// blink command's goroutine unblocks immediately and returns `blinkCanceled`, which the
// runtime still DELIVERS as a message. So every keystroke, backspace and arrow key (all of
// which move the cursor) produced a SECOND message and therefore a SECOND full-screen
// repaint, for nothing: the wasted frame's only job was to be rejected by the cursor's
// tag check.
//
// Measured, before this: 20 keystrokes scheduled 20 blink timers, and a frame costs
// ~0.6ms at 120x40 rising to ~1.5ms at 320x100 (the frame paints every cell with the
// theme background, so its cost scales with the terminal's AREA). Typing therefore paid
// roughly double the frames it needed — the operator's "when typing into the composer,
// moving the cursor with the arrow keys, backspacing, etc. it seems a little laggy".
//
// So the loop is ours now: ONE timer, started on focus, re-armed by its own tick, and
// never cancelled by an edit. The library cannot mint a rival because its move handler
// requires the cursor mode to be CursorBlink (see New, which sets CursorStatic); the
// caret itself is unaffected, since cursor.View() renders from Blink alone.
const dockBlinkSpeed = 530 * time.Millisecond // bubbles' cursor.BlinkSpeed default

// dockBlinkMsg is our blink tick.
type dockBlinkMsg struct{}

// dockBlinkTick returns the one command that keeps the caret blinking: it fires after the
// blink interval, and delivering dockBlinkMsg re-arms the next one.
func dockBlinkTick() tea.Cmd {
	return tea.Tick(dockBlinkSpeed, func(time.Time) tea.Msg { return dockBlinkMsg{} })
}

// overLimit reports whether the buffer is past the bound, i.e. a send would be refused.
func (m *Model) overLimit() bool {
	return m.charLimit > 0 && len([]rune(m.ta.Value())) > m.charLimit
}

// counterRow pads the counter to the box's inner width so it sits against the right edge — the conventional
// place for a count — and keeps the row exactly as tall as the shell reserved for it.
func (m *Model) counterRow(text string, inner int) string {
	style := theme.HintText
	if m.overLimit() {
		style = theme.ErrorText
	}
	pad := inner - lipgloss.Width(text)
	if pad < 0 {
		pad = 0
	}
	return style.Render(strings.Repeat(" ", pad) + text)
}
