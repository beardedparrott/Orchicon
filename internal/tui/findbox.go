package tui

// findbox.go — THE FIND BOX: the modal transcript search the operator types into.
//
// The operator: "ctrl+f works odd. You have to first type what you want then hit ctrl+f. That's not
// very intuitive. I would prefer a modal pop up that you can type in your search."
//
// They are right about the old shape, and WHY it was that shape is worth recording because it is the
// thing this file removes: a terminal cannot open a search box, so ctrl+f took the COMPOSER's text as
// the query — the chord really meant "search for whatever is in my message box". That made it useless
// with an empty box, which is the state you are in while READING a transcript and the only state a
// find is for; and it silently turned a half-written message into a search term.
//
// THE BOX IS A MODAL of the shape this shell already uses for one-input surfaces (the rename form,
// the /connect form): a solid panel over the frame, owning the keyboard while it is up, so nothing
// typed into the query can reach the composer or a screen behind it. Two things about it are NOT the
// standard form shape, and both are deliberate:
//
//   - IT FILTERS LIVE rather than on submit, so the query is judged by what it finds. Every keystroke
//     goes through the SAME functions /find and n/N use (transcript_search.go: setAskSearch ->
//     transcriptFilterItems / transcriptSearchCount), so the box, the slash command and the chords
//     cannot disagree about what a match is.
//
//   - ESC CANCELS, WHICH MEANS RESTORES. A live filter that could only be accepted would make "let me
//     see what this matches" a commitment, so the box snapshots the search state it opened with and
//     puts it back — including the "no search at all" case, which is the one that matters most,
//     because that is the state on the way IN.
//
// ENTER KEEPS THE SEARCH AND HANDS OVER THE KEYBOARD. Committing moves focus to the TRANSCRIPT, and
// that is the point of committing rather than a flourish: n/N step matches and home/end jump, and
// neither can work from the composer — 'n' is a printable letter, and a bare letter in the composer is
// the operator's message (see composerBypassKeys). A find that left the composer focused would hand
// over a filtered transcript and no way to walk it.

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// findBox is the transcript find modal's state.
type findBox struct {
	input textinput.Model
	// prevQuery and prevMatch are WHAT ESC PUTS BACK — the search the operator had before the box
	// opened, so a box opened to look at something can always be abandoned without losing a search
	// they had already built.
	prevQuery string
	prevMatch int
}

// findBoxPrompt labels the query row. Without it the box reads as a stray line of transcript rather
// than as a field, which is the same mistake the rail's chord list made when it was drawn in the rail.
const findBoxPrompt = "find > "

// findBoxKeysHint is the box's own key line. It names only keys the box actually honours — and it
// deliberately does NOT name n/N, which are the transcript's chords rather than the box's: while the
// box is up those are letters in the operator's search term.
const findBoxKeysHint = "enter: keep searching · ↑/↓: step matches · esc: cancel"

// openFindBox opens the transcript find box (ctrl+f).
//
// THE QUERY STARTS FROM THE ACTIVE SEARCH, NOT FROM THE COMPOSER. Seeding it from the composer is
// exactly the "type the term first, then press ctrl+f" order the operator called unintuitive, and it
// turned a half-written message into a search term. Re-opening with the CURRENT query is the useful
// seed instead: ctrl+f then means "refine this search", and there is nothing to guess when no search
// is active yet.
//
// The input's blink command is STAGED rather than returned, because a KeyRoute.Handle can only report
// a bool — the same constraint pendingStopCmd exists for; drainStaged re-emits it.
func (m *App) openFindBox() {
	if m.chatConvID == "" {
		m.dock.SetError("no conversation open — nothing to search")
		return
	}
	if m.active != TabAsk {
		// The box filters the OPEN CONVERSATION, which is only on screen on Ask. Opening an invisible
		// filter from another tab would look like the chord did nothing while it quietly hid messages,
		// so the notice says where the surface is instead.
		m.dock.SetNotice("ctrl+f searches the open conversation — F1 opens Ask Orchicon")
		return
	}
	in := textinput.New()
	in.Prompt = findBoxPrompt
	in.PromptStyle = theme.DetailKey
	// THE CARET IS THE COMPOSER'S, deliberately: theme.ComposerCursor is written PRE-REVERSED (bubbles
	// reverses the style again before painting), so it is the one caret style in this codebase that is
	// known legible on every palette. A fresh style here would be a second caret with its own bugs.
	in.Cursor.Style = theme.ComposerCursor
	// TextStyle carries the panel's own foreground: the box sits on a solid PANEL, so inheriting the
	// terminal's default foreground would put light-theme text on a light panel — the exact legibility
	// failure theme.PanelBg exists to prevent.
	in.TextStyle = lipgloss.NewStyle().Foreground(theme.Text)
	in.SetValue(m.askSearch)
	in.CursorEnd()
	blink := in.Focus()
	m.findBox = &findBox{input: in, prevQuery: m.askSearch, prevMatch: m.askMatchIdx}
	m.pendingFindCmd = blink
	m.dock.SetError("")
	m.refreshComposerHint()
}

// findBoxKey routes a key while the box is up. handled=false means the key is NOT the box's and must
// continue through the dispatch chain — which is how the shell's hard escapes stay reachable.
func (m *App) findBoxKey(k tea.KeyMsg) (bool, tea.Cmd) {
	if m.findBox == nil {
		return false, nil
	}
	switch k.String() {
	case "esc":
		m.cancelFindBox()
		return true, nil
	case "enter", "ctrl+j":
		// ctrl+j is the second spelling of Enter some terminals send. The dock and the slash palette
		// both accept the pair, so the box does too rather than making Enter dependent on the terminal.
		m.commitFindBox()
		return true, nil
	case "up", "ctrl+p":
		m.askSearchStep(-1)
		return true, nil
	case "down", "ctrl+n":
		m.askSearchStep(1)
		return true, nil
	case "ctrl+c", "ctrl+g":
		// THE HARD ESCAPES ARE NOT THE BOX'S TO EAT. Both are documented as working from ANY state —
		// ctrl+c quits even mid-composition, and ctrl+g returns to the composer and DISMISSES whatever
		// holds the keys. A modal that swallowed them would be the "ctrl+g is spotty and usually
		// doesn't work" class of bug the chord exists to end. So the box closes first (cancel: the
		// operator is leaving), and the key is handed back for the ONE implementation of each chord to
		// run — it must be closed here, or it would still own the keyboard while the composer had the
		// caret, which is the split-keyboard state ctrl+g is written to avoid.
		m.cancelFindBox()
		return false, nil
	}
	// EVERYTHING ELSE IS THE QUERY. This is why the box is claimed AHEAD of the composer, the slash
	// palette and every route: the letters that are chords elsewhere ('q' quits, 'n' steps through
	// matches, '/' opens the palette) are ordinary characters in a search term, and a find box that
	// quits the shell when you search for "query" is not a find box.
	before := m.findBox.input.Value()
	next, cmd := m.findBox.input.Update(k)
	m.findBox.input = next
	if m.findBox.input.Value() != before {
		m.setAskSearch(m.findBox.input.Value())
	}
	return true, cmd
}

// commitFindBox keeps the query and hands the keyboard to the transcript (enter).
func (m *App) commitFindBox() {
	if m.findBox == nil {
		return
	}
	// The box's text becomes the search through the same setter /find and the chords use. An empty box
	// therefore commits to "no filter", which is what the operator is already looking at.
	m.setAskSearch(m.findBox.input.Value())
	m.findBox = nil
	// THE FOCUS MOVE IS THE POINT, not a flourish — see the file header.
	m.askPane = askPaneConversation
	m.setFocus(focusContent)
	m.syncAskPaneFocus()
	m.refreshComposerHint()
	m.refreshStreamStatus()
}

// cancelFindBox puts the search back the way the box found it and closes (esc, ctrl+g, ctrl+c).
func (m *App) cancelFindBox() {
	if m.findBox == nil {
		return
	}
	m.askSearch, m.askMatchIdx = m.findBox.prevQuery, m.findBox.prevMatch
	m.findBox = nil
	m.onChatWake()
	m.refreshStreamStatus()
	m.refreshComposerHint()
}

// findBoxView composes the box over the frame: a SOLID panel, centred, like every other modal in the
// shell (splicing a ragged body raw is what made /connect look broken).
func (m *App) findBoxView(base string, w, h int) string {
	if m.findBox == nil {
		return base
	}
	return m.overlayCentered(base, m.modalPanel(m.findBoxBody(m.modalInnerWidth()), m.modalWidth()))
}

// findBoxBody renders the box's interior: the title, the query with its caret, the live count, and the
// keys.
//
// THE QUERY ROW IS SIZED FROM THE PANEL'S OWN INTERIOR WIDTH, so a long term scrolls INSIDE the input
// instead of the panel clipping the caret off the end — a find field whose caret can be pushed out of
// sight cannot be edited. The two text rows are truncated to the same width for the reason the rail's
// chord list is: a body line is cut to the panel's interior, and a hint cut mid-word advertises a key
// nobody can read.
func (m *App) findBoxBody(inner int) string {
	m.findBox.input.Width = max(inner-lipgloss.Width(findBoxPrompt), 8)
	return strings.Join([]string{
		theme.DetailKey.Render("find in transcript"),
		m.findBox.input.View(),
		theme.HintText.Render(truncateRight(m.findBoxCountLine(), inner)),
		theme.HintText.Render(truncateRight(findBoxKeysHint, inner)),
	}, "\n")
}

// findBoxCountLine is the box's live count: what the query currently matches, or how much there is to
// search when nothing has been typed yet. It is never blank, so the operator can always tell whether
// the filter is doing anything.
func (m *App) findBoxCountLine() string {
	q := strings.TrimSpace(m.findBox.input.Value())
	items := m.chatStore.snapshot(m.chatConvID)
	if q == "" {
		return fmt.Sprintf("%d messages — type a term to filter", len(items))
	}
	matches, total := transcriptSearchCount(items, q)
	if matches == 0 {
		return fmt.Sprintf("no matches in %d messages", total)
	}
	idx := m.askMatchIdx
	if idx >= matches {
		idx = matches - 1
	}
	if idx < 0 {
		idx = 0
	}
	return fmt.Sprintf("%d/%d matches · at match %d", matches, total, idx+1)
}
