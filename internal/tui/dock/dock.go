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

	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
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
	// MaxInputRows is the ceiling used when the shell has NOT told the dock how tall
	// the viewport is — a dock built in isolation (a unit test, an embedder). The real
	// ceiling is DERIVED from the viewport (see maxInputRows), because a fixed one is
	// wrong in both directions: eight rows is a lot on a short terminal and far too
	// few for a long prompt or a pasted block on a tall one.
	//
	// The operator, on a long prompt: "Remove the cap." Removing it outright is not
	// possible — the composer shares the screen with the transcript and the panes, and
	// a buffer of a thousand lines cannot have a thousand rows of composer. So the
	// fixed cap is replaced by the space the viewport can spare, which leaves the
	// screen above it a usable minimum and grows without limit on a bigger terminal.
	MaxInputRows = 8
	// minContentRows is the room the composer must leave for the screen above it: it
	// grows into the space that is going spare, never into the space the transcript or
	// the panes are using.
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
	// replyInFlight drives the STOP affordance: while a turn is streaming, the affordance row offers
	// ctrl+y — the operator's "we need a stop button/control key + hint in composer that can interrupt
	// the agent mid flight". It is DERIVED from the chat controller's own streaming state by the shell
	// (SetReplyInFlight), so the row cannot advertise a dead key after the reply ends.
	replyInFlight bool
	Err           string // inline error strip (themed red)
	Width         int
	Height        int // allocated rows (set by the shell)
	// viewportRows is the WHOLE terminal height, set by the shell each layout. The
	// composer's growth ceiling is derived from it (maxInputRows) rather than being a
	// constant, because how tall the composer may get depends on how much screen there
	// is. 0 means "not told", and MaxInputRows is then the fallback.
	viewportRows int
	Newlines     NewlineMode

	// Context is the ACTIVE screen's shortcut list (the shell derives it from the
	// screen's HintLine), rendered at the head of the affordance row so the
	// guidance always names what the current page can do.
	Context string

	// Stats is the session stat strip (ask model · context · tokens · cache ·
	// cost), rendered RIGHT-ALIGNED on the box's bottom row — the operator's
	// "bottom right corner of the chat box". Mode is the persona pill rendered
	// immediately to its right, mirroring where the GUI puts its mode dropdown
	// (the stats sit BEFORE it). Both are set by the shell.
	Stats string
	Mode  string
	// Model is the ask model ref, rendered LEFT-aligned on the same stat row.
	// It is deliberately NOT part of Stats: prefixing the ref to the right-aligned
	// numbers made the row longer than the pane, so the tail (cost, and part of
	// the cache figure) was clipped off the edge. Keeping the ref at the left
	// mirrors the GUI, where the model is its own chip beside the stats.
	Model string

	// sendRequest is a non-nil callback when Enter produced a send; the
	// shell checks+clears it after Update (avoids channel plumbing).
	sendRequest string
	// lastSent is the text of the most recent send: the draft source for
	// RestoreDraft (a failed send puts the message back in the box).
	lastSent string

	// blinkStart is the textarea's focus command, captured when the dock takes
	// focus. It STARTS the caret's blink loop, so it must reach the runtime —
	// discarding it (as `_ = m.ta.Focus()` did) left the caret frozen solid and
	// the operator unable to tell the composer held focus. It is handed out
	// with the next Update (see the defer there) rather than returned from
	// Focus, so the focus path needs no command plumbing.
	blinkStart tea.Cmd

	// pastePathHook, when set, is offered every BRACKETED PASTE before it is inserted: it returns a command
	// when the pasted text is a FILE PATH it wants to attach, and nil to let the paste insert as text.
	//
	// A hook rather than dock logic because the SHELL owns the attachment set and does the I/O — the dock is
	// a textarea and knows nothing about files. See the k.Paste case for the gesture this serves.
	pastePathHook func(text string) tea.Cmd

	// selectAll marks the WHOLE composer as selected, so the operator can copy it all or throw it away in
	// one gesture. bubbles' textarea has NO selection model (v1.0.0: no Select/SelectAll, no selection
	// state at all), so this is the composer's own: the shell copies the buffer on the way in, and the next
	// replacing key clears it on the way out. See selectAllKey.
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
	// No placeholder: the box shows a plain blinking cursor instead of hint
	// text. The hints live on the affordance row below, so the placeholder was
	// redundant — and bubbles takes a SEPARATE view path for an empty value
	// with a placeholder, which rendered differently from the typed state.
	ta.Placeholder = ""
	ta.Prompt = ""
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	m := Model{ta: ta, Newlines: NewlineAltEnter}
	// themeStyles pins every textarea cell to the box's surface so no cell can
	// be left unpainted, and makes the caret a visible accent block. Called at
	// construction and again after a theme switch (theme.Use re-derives the
	// package styles, but these are captured per-render).
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
	base := lipgloss.NewStyle().Background(theme.Surface).Foreground(theme.Text)
	dim := lipgloss.NewStyle().Background(theme.Surface).Foreground(theme.TextFaint)

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

	// THE CARET. theme.ComposerCursor is written PRE-REVERSED because bubbles reverses it again before
	// painting (`m.Style.Inline(true).Reverse(true).Render(char)`), so the style's FOREGROUND becomes the
	// block and its BACKGROUND becomes the character. Read that assignment before changing this line: the
	// obvious-looking `Background(accent).Foreground(Bg)` is what made the cursor a black block on every dark
	// theme, which is the operator's report.
	//
	// TextStyle is what bubbles uses while the caret is BLINKING — i.e. the frames where it is hidden — so it
	// is the ordinary line style: the character must not vanish when the blink is off.
	ta.Cursor.Style = theme.ComposerCursor
	ta.Cursor.TextStyle = base

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

// boxContentCol is the box's left chrome before anything is painted inside it: one border cell plus the
// ComposerBox style's two cells of horizontal padding. The prompt (`❯ `) then sits before the text on EVERY
// input row — continuation rows are indented to match — so the text always starts promptWidth further right.
const boxContentCol = 1 + 2

// Caret reports the caret's line and column.
//
// Exported for the SHELL's tests: the caret is placed from geometry the shell computes (where the dock starts
// in the frame), so asserting that a click landed correctly needs to read it back from outside this package.
func (m *Model) Caret() (line, col int) {
	return m.ta.Line(), m.ta.LineInfo().ColumnOffset
}

// TextOrigin is the dock row and column at which the input's TEXT begins.
//
// Both halves are derived from the render rather than guessed: the box paints its top border first, the chip
// row (when there is one) sits above the input inside the box, and the prompt precedes the text.
func (m *Model) TextOrigin() (row, col int) {
	row = 1 // the box's top border
	if m.Chip != "" {
		row++ // the chip is the box's first inner row
	}
	return row, boxContentCol + promptWidth
}

// ClickAt places the caret at a click inside the composer, in DOCK coordinates (the shell converts from the
// frame, since only it knows where the dock starts). It reports whether the click was one it could place.
//
// The operator: "I also just realized you can't use your mouse in the composer to change the position of your
// cursor." The gesture cannot come from the terminal: orch runs with tea.WithMouseCellMotion(), so the
// terminal hands every press to the program instead of placing the caret itself. Selecting text and placing a
// caret are the same gesture at that level, so the app has to do both — the selection lives in the shell
// (clipboard.go) and the caret belongs here, next to the widget that owns it.
//
// EXACT FOR UNWRAPPED CONTENT. Every input row is the prompt followed by the textarea's row, so visual row r
// is logical line r whenever no line soft-wraps; the caret then lands on the line clicked and at the column
// clicked.
//
// A CLICK ON A SOFT-WRAPPED LINE IS DELIBERATELY NOT RESOLVED. The textarea's wrap grid is unexported, and a
// caret placed by GUESSING at it would land somewhere plausible and wrong — worse than doing nothing, because
// the operator cannot tell it was a guess. Reporting false leaves the caret where the keyboard put it. The
// same applies to a click that misses the box's input rows.
func (m *Model) ClickAt(x, y int) bool {
	originRow, originCol := m.TextOrigin()
	vis := y - originRow
	if vis < 0 || vis >= m.InputRows() {
		return false
	}
	tw := m.taWidth()
	lines := strings.Split(m.ta.Value(), "\n")
	for _, ln := range lines {
		if lipgloss.Width(ln) > tw {
			return false // soft-wrapped — see above
		}
	}
	// A CLICK LEFT OF THE TEXT IS A CLICK AT ITS START, not a refusal: the prompt is two cells of chrome the
	// operator may well have clicked on the way to column one.
	if x < originCol {
		x = originCol
	}
	// BELOW THE TEXT IS THE END OF THE TEXT, which is what a box does: clicking the empty space under a short
	// message puts the caret after the last character rather than refusing or jumping to a line.
	atEnd := vis >= len(lines)
	target := vis
	if atEnd {
		target = len(lines) - 1
	}
	for m.ta.Line() < target {
		m.ta.CursorDown()
	}
	for m.ta.Line() > target {
		m.ta.CursorUp()
	}
	if atEnd {
		m.ta.CursorEnd()
		return true
	}
	m.ta.SetCursor(x - originCol)
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

// InputRows is the number of input rows the box shows: at least MinInputRows,
// growing with the buffer up to however many rows the viewport can spare
// (maxInputRows; the textarea scrolls internally beyond that).
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

// maxInputRows is the tallest the input may grow: whatever the viewport can spare
// once the shell's chrome, the box's own chrome and minContentRows are accounted for.
//
// THE ARITHMETIC MIRRORS app.contentHeight, which computes the content region as
// `m.height - 4 - dock.Lines()`, where dock.Lines() is 2 border rows plus the input,
// hint and notice rows (plus the chip when present). Solving that for a content region
// of at least minContentRows gives the bound below, so the composer can grow freely and
// still cannot starve the screen it sits under.
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

// maxHintRows caps the wrapped affordance row. The hint is context-driven (it
// carries the active screen's shortcuts), so it can be very long — without a
// cap the composer would grow a row at a time and eat the content region, which
// is exactly what the layout test caught. Three rows is what the composer can
// afford out of the screen's budget; anything beyond is marked with an ellipsis
// rather than silently dropped, and "?" opens the full key list.
const maxHintRows = 3

// hintLines is the affordance row's final rows: the segment-aware wrap, capped
// at maxHintRows with a trailing ellipsis. HintRows and View both use it so the
// reserved row count and the drawn rows can never disagree.
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

// NoticeRows is how many rows the notice/error strip occupies at the current
// width (0 when there is none).
//
// THE ERROR AND THE NOTICE BOTH GET ROWS, because they are DIFFERENT information: the error strip is a
// sticky global failure (a subscription that cannot connect), the notice is the outcome of what the
// operator just did. The strip used to render `Err` INSTEAD OF `Notice`, so ANY unrelated failure made the
// acknowledgement of the operator's own action invisible.
//
// Measured while building the stop control (ctrl+y): the byte arrived, the route fired, the notice was
// set — and nothing was painted, because an unrelated stream error (from a test plane that does not
// implement every RPC) had taken the row. A control whose feedback depends on nothing else being wrong
// is a control that appears broken at exactly the moment the operator needs it.
func (m *Model) NoticeRows() int {
	return m.wrapRows(m.Err) + m.wrapRows(m.Notice)
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

// Lines returns the rows the dock needs: the box's top+bottom border, the input
// rows (InputRows), the affordance row (HintRows — more than one when it wraps)
// and the chip / notice strips when present. The shell reserves exactly this many
// rows (app.contentHeight) and normalizes the block to it
// (normalizeBlockKeepTail).
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
	if m.Stats == "" && m.Mode == "" && m.Model == "" {
		return 0
	}
	return 1
}

// statLine renders the bottom stat row: the ask model on the LEFT, and the stats
// followed by the mode pill right-aligned against the box edge. The pill is the
// CONTROL and the numbers are what the operator watches, so when the row is tight
// the MODEL ref is sacrificed first.
func (m *Model) statLine(inner int) string {
	mode := ""
	if m.Mode != "" {
		mode = "[" + m.Mode + "]"
	}
	right := m.Stats
	if mode != "" {
		if right != "" {
			right += "  "
		}
		right += mode
	}
	if right == "" && m.Model == "" {
		return ""
	}
	// Keep the right-hand group whole where the pane allows: trim the STATS first,
	// never the pill.
	if lipgloss.Width(right) > inner {
		avail := inner - lipgloss.Width(mode) - 2
		if avail < 4 {
			avail = 4
		}
		right = ansi.Truncate(m.Stats, avail, "…")
		if mode != "" {
			right += "  " + mode
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
	// The row must never exceed `inner`: Pad TRUNCATES, so an overlong line loses
	// its tail — which is how the pill's closing "] " disappeared. Guarantee the
	// sum fits, then pad the gap (0 is a valid pad for an exact fit).
	if lipgloss.Width(right) > inner {
		right = ansi.Truncate(right, inner, "…")
	}
	pad := inner - lipgloss.Width(left) - lipgloss.Width(right)
	if pad < 0 {
		pad = 0
	}
	return theme.ListMeta.Render(left + strings.Repeat(" ", pad) + right)
}

// Focus / Blur move keyboard focus into/out of the composer.
func (m *Model) Focus() {
	m.Focused = true
	// Capture — do not discard — the command that starts the caret's blink
	// loop. Update hands it to the runtime with the next message.
	m.blinkStart = m.ta.Focus()
}

// TakeBlinkStart hands out (once) the command that starts the caret's blink loop,
// captured when the dock took focus. Focus paths call this so the caret animates
// from the moment the composer holds focus, rather than waiting for the operator's
// first keystroke; Update also drains it (see the defer there) as a safety net for
// any path that forgets.
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

// SetReplyInFlight toggles the STOP affordance on the affordance row (see Hint). The shell sets it from
// the chat controller's live streaming state, so the advertised chord and the running turn can never
// disagree.
func (m *Model) SetReplyInFlight(on bool) { m.replyInFlight = on }

// resizeTa keeps the textarea width+height synced to the box.
func (m *Model) resizeTa() {
	m.ta.SetWidth(m.taWidth())
	m.ta.SetHeight(m.InputRows())
}

// Update handles composer keys. Returns (consumed, cmd): consumed=false
// means the shell should fall through (only when unfocused).
func (m *Model) Update(msg tea.Msg) (handled bool, cmd tea.Cmd) {
	// The caret's blink loop starts from the command captured when focus was
	// taken, and it has to be dispatched with WHATEVER this message produces.
	// A deferred batch guarantees that no early return below can drop it.
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
	case tea.KeyMsg:
		// THE ALL-SELECTED STATE GETS FIRST REFUSAL. While the whole composer is selected, the keys that
		// would normally edit it REPLACE it — which is what "selected" means, and what makes the gesture
		// useful (the operator asked to "easily copy or delete it all").
		//
		// Any OTHER key simply DROPS the selection and then behaves normally, so the state can never trap
		// the operator: an arrow, a tab or a ctrl chord exits it and does what it always did.
		if m.selectAll {
			if handled, cmd := m.selectAllKey(k); handled {
				return true, cmd
			}
		}
		switch {
		case k.Paste:
			// Bracketed paste: bubbletea collapses the byte sequence into
			// one KeyMsg with embedded newlines — render verbatim, never
			// send. textarea inserts it as-is.
			//
			// EXCEPT A PASTE THAT IS A FILE PATH. An operator who copies a file in their file manager
			// and pastes it into this box produces exactly that text, and attaching the file is what they
			// meant — inserting "/home/me/shot.png" as prose is not. The shell decides (it owns the
			// attachment set and the I/O), so the paste is offered to it first and only inserted when it
			// is not a path.
			// A hook that is NOT INSTALLED must not be called: this is a nil func field, and docks are
			// built in several places (including tests) that have no shell to install one — so the
			// ungurded call panicked on the first bracketed paste of every such dock.
			if m.pastePathHook != nil {
				if cmd := m.pastePathHook(string(k.Runes)); cmd != nil {
					return true, cmd
				}
			}
			return true, m.pasteCmd(k)
		case enterKey(k) && k.Alt:
			if m.Newlines == NewlineAltEnter || m.Newlines == NewlineBoth {
				m.insertNewline("alt+enter")
				return true, nil
			}
			return true, m.requestSend()
		case enterKey(k):
			// NOTHING here may silently swallow a send. The composer's own hint
			// documents the contract as "enter send · alt+enter newline", so any
			// Enter variant that is not an explicitly configured newline chord
			// SENDS.
			//
			// Three real cases made Enter appear dead, and two are fixed:
			//  1. a "shift+enter inserts a newline" convenience the hint never
			//     documented (removed);
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
			// Ack the send BEFORE it happens: sendFromComposer replaces this with
			// the outcome (context injected, an RPC error, or the conversation
			// opening), so it can never linger as a false promise.
			//
			// An EMPTY buffer never reaches here by design: the shell claims
			// Enter-with-an-empty-composer to open the active tab's menu
			// (menuActivationKey, shell.go), so this branch only ever runs with
			// something to send.
			m.Notice = "sending …"
			return true, m.requestSend()
		default:
			ta, cmd := m.ta.Update(msg)
			m.ta = ta
			m.resizeTa()
			return true, cmd
		}
	}
	// A NON-key message (notably the textarea's cursor BlinkMsg) must still
	// reach the textarea. Swallowing it froze the caret solid while the box
	// held focus, so the operator had no way to tell where their keystrokes
	// would land — "the cursor should blink when in the composer and focus is
	// active so people know they truly have focus there."
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
func enterKey(k tea.KeyMsg) bool {
	switch k.Type {
	case tea.KeyEnter, tea.KeyCtrlJ:
		return true
	}
	return false
}

// insertNewline appends a newline instead of sending, and SAYS SO.
//
// The notice is not decoration: an Enter that produces neither a send nor a
// visible change is indistinguishable from a dead key, which is exactly how a
// terminal reporting its Enter as a modified key hid this for several rounds. The
// chord that fired is named, so the next occurrence is diagnosable from the UI.
func (m *Model) insertNewline(chord string) {
	ta, _ := m.ta.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: false})
	m.ta = ta
	m.resizeTa()
	if chord == "" {
		chord = "newline chord"
	}
	m.Notice = "newline inserted (" + chord + ") — enter sends"
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

// Hint is the persistent affordance row: the ctrl+g focus chord FIRST (so it
// survives truncation on a narrow pane), then the ACTIVE screen's shortcut
// list (Context, set by the shell from that screen's HintLine), then the
// composer's own chords.
func (m *Model) Hint() string {
	nl := "alt+enter"
	switch m.Newlines {
	case NewlineBackslashEnter:
		nl = "\\+enter"
	case NewlineBoth:
		nl = "alt+enter or \\+enter"
	}
	// ctrl+d is named here because it is the ONLY way to open the diff sidebar from
	// the keyboard, and it was invisible: the operator's "the shortcut advice in the
	// composer is missing for ctrl+d to open/close the diff box".
	//
	// "text box" is shortened to "text" to make room, and that is not cosmetic: the
	// affordance row is ONE LINE by contract (TestComposerBoxShowsHintAndChip asserts
	// the box renders the whole hint unbroken), and at an 80-column terminal boxInner
	// is 74 — the full wording with ctrl+d added measured 79 and wrapped, which pushed
	// the affordance row out of the box entirely at 80x24. Measured: 75 with "text",
	// which fits with headroom.
	// STOP LEADS, immediately after the focus chord: while a reply is streaming it is the action the
	// operator is looking for, and the row is truncated from the RIGHT on a narrow terminal (which is
	// why ctrl+g is first). Derived from the live turn, never assumed.
	parts := []string{"ctrl+g text"}
	if m.replyInFlight {
		parts = append(parts, "ctrl+y stop")
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
	// WHILE THE WHOLE BUFFER IS SELECTED THE ROWS ARE PAINTED ON THE SELECTION FILL.
	//
	// The operator: "it DOES select all but it doesn't actually show the cursor highlight over all of the text,
	// it just gives you a little message that the text is highlighted." The message was ALL there was: this
	// loop rendered the buffer exactly as it does when nothing is selected, so the only evidence of ctrl+a was
	// the notice strip. A selection you cannot see is a selection you cannot trust — and the next keystroke
	// silently REPLACES the whole draft, which is a data-loss trap if the state was not visible.
	selecting := m.selectAll
	input := m.inputLines()
	if selecting {
		input = m.selectionLines()
	}
	for i, l := range input {
		if selecting {
			// ONE UNIFORM FILL ACROSS THE ROW, prompt included and padded to the box's inner width. Nothing
			// inside the selection competes for attention (no prompt colour, no caret): the fill IS the answer
			// to "did ctrl+a do anything".
			prefix := "  "
			if i == 0 {
				prefix = "❯ "
			}
			rows = append(rows, theme.ComposerSelect.Render(fit(prefix+l, inner)))
			continue
		}
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

	// The box's own background must survive the inner rows' resets: the rows
	// carry styled spans (prompt, hint, the textarea's own cursor styling),
	// and each one's reset would otherwise switch the background off for the
	// rest of the row — the "hole" the operator saw as soon as they typed.
	// The repair takes the BACKGROUND-ONLY surface style: ComposerBox itself
	// has a border + padding, and re-asserting through THAT injected border
	// glyphs into the row.
	return theme.RepairAfterResets(
		theme.ComposerBox.Render(strings.Join(fitAll(rows, inner), "\n")),
		theme.SurfaceBg)
}

// inputLines returns exactly InputRows() rows of the input view (the
// textarea renders its own height, but the box's row budget is never left
// to it).
func (m *Model) inputLines() []string {
	n := m.InputRows()
	// Read the textarea DIRECTLY — do NOT re-style per render. styledTa()
	// re-points bubbles' unexported style pointer by calling Focus()/Blur(), and
	// each of those returns a cursor.BlinkCmd() that CANCELS the in-flight blink
	// and bumps its tag. Called from here it ran on every render, so it killed
	// each pending tick and the caret never animated ("no blinking cursor").
	// m.ta's style pointer is already correct between renders — themeStyles/
	// ApplyTheme and Focus/Blur each re-point it — so the render path only reads.
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

// selectionLines returns the composer's input rows as PLAIN text with NO caret, for the SELECTED state.
//
// IT RENDERS A BLURRED COPY OF THE TEXTAREA rather than the live one, because bubbles' caret is a
// REVERSE-VIDEO BLOCK: left in place it punches a hole in the selection fill at the caret's cell and reads as
// an artefact rather than as part of the selection. Blur() has NO return value, so unlike styledTa's Focus/Blur
// there is no BlinkCmd for this to accidentally execute and cancel the caret's blink with (see inputLines for
// the bug that caused) — and it is called on a COPY, so the dock's own textarea keeps its focus.
//
// The rows are ANSI-STRIPPED because the textarea paints every cell with its own base style (Foreground Text on
// Background Surface). An outer fill would never show through that: the inner background wins cell by cell. What
// survives the strip is exactly the characters the operator typed, which is what the caller then fills.
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
