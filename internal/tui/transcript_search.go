package tui

import (
	"fmt"
	"strings"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// transcript_search.go — in-transcript search for the Ask pane, plus the
// jump-to-top / jump-to-bottom transcript controls.
//
// WHY IT LIVES HERE. The Ask transcript is painted by the shell (onChatWake ->
// syncTranscript -> chat.RenderCache.Render), and the shell owns the stream's
// scroll offset and the pane's fixed footer. So the search query, the match
// navigation and the jump controls are shell state, beside askMode/chatConvID —
// the screen stays a pure renderer of what the shell hands it.
//
// THE SHAPE: /find <text> sets the query; the transcript is FILTERED to the items
// whose searchable text contains it (case-insensitive substring — no regex, by
// scope); the pane footer reports the match count or "no matches"; n/N step the
// current match and scroll it into view; Esc clears back to the full transcript;
// home/end jump to the first/latest message. The key bindings are registered in
// the KeyRoute registry (router.go) so the '?' overlay cannot drift from them.
//
// FOCUS RULE: every entry point here is reached only from CONTENT focus. The
// composer consumes plain letters and Home/End as editing keys, so a search key
// can never steal a keystroke from typing (see askTranscriptContentFocus).

// transcriptSearchSep is the field separator the searchable text is built with.
// It is a newline, spelled as a rune so this file carries no escape sequences.
var transcriptSearchSep = string(rune(10))

// transcriptSearchText is the text a transcript item is searched over: the
// operator-visible text plus the tool/ask vocabulary, so a search for a tool name
// ("bash") or a question's wording finds the row that carries it.
func transcriptSearchText(it chat.ChatItem) string {
	parts := []string{it.Text, it.Content, it.Name}
	if it.Tool != nil {
		parts = append(parts, it.Tool.ToolName, it.Tool.Input)
	}
	if it.Ask != nil {
		parts = append(parts, it.Ask.Question)
	}
	return strings.Join(parts, transcriptSearchSep)
}

// transcriptItemMatches reports whether an item's searchable text contains q
// (case-insensitive). An empty/whitespace query matches EVERYTHING — the
// "no filter" state.
func transcriptItemMatches(it chat.ChatItem, q string) bool {
	q = strings.TrimSpace(q)
	if q == "" {
		return true
	}
	return strings.Contains(strings.ToLower(transcriptSearchText(it)), strings.ToLower(q))
}

// transcriptFilterItems returns the items matching q (every item when q is empty).
// It is the pure core of the search, so a test can assert the filter without a
// screen or a stream.
func transcriptFilterItems(items []chat.ChatItem, q string) []chat.ChatItem {
	if strings.TrimSpace(q) == "" {
		return items
	}
	out := make([]chat.ChatItem, 0, len(items))
	for _, it := range items {
		if transcriptItemMatches(it, q) {
			out = append(out, it)
		}
	}
	return out
}

// transcriptSearchCount returns (matches, total) for the status line.
func transcriptSearchCount(items []chat.ChatItem, q string) (int, int) {
	total := len(items)
	if strings.TrimSpace(q) == "" {
		return total, total
	}
	n := 0
	for _, it := range items {
		if transcriptItemMatches(it, q) {
			n++
		}
	}
	return n, total
}

// transcriptSearchStatus is the pane footer while a search is active: the count,
// or an explicit no-match line — never empty, so the operator always sees that a
// search is live.
func transcriptSearchStatus(q string, matches, total int) string {
	q = strings.TrimSpace(q)
	if matches == 0 {
		return fmt.Sprintf("search %q · no matches · esc clears", q)
	}
	return fmt.Sprintf("search %q · %d/%d matches · n/N next·prev · esc clears", q, matches, total)
}

// searchVisibleItems is the item set the transcript RENDERS: the full set when no
// search is active, else only the matching items — EXCEPT consent/ask cards,
// which are always kept. A card is not a message, and hiding a pending approval
// behind a text filter would be a trap.
func (m *App) searchVisibleItems(items []chat.ChatItem) []chat.ChatItem {
	if strings.TrimSpace(m.askSearch) == "" {
		return items
	}
	out := make([]chat.ChatItem, 0, len(items))
	for _, it := range items {
		if it.Kind == chat.KindConsent || it.Kind == chat.KindAsk || transcriptItemMatches(it, m.askSearch) {
			out = append(out, it)
		}
	}
	return out
}

// setAskSearch starts (or replaces) the transcript search and repaints.
func (m *App) setAskSearch(q string) {
	m.askSearch = strings.TrimSpace(q)
	m.askMatchIdx = 0
	m.onChatWake()
	m.refreshStreamStatus()
	m.askSearchScrollToMatch()
}

// clearAskSearch drops the query, restoring the unfiltered transcript. It returns
// whether a search was active, so Esc can fall through to its other meanings when
// there was nothing to clear.
func (m *App) clearAskSearch() bool {
	if strings.TrimSpace(m.askSearch) == "" {
		return false
	}
	m.askSearch = ""
	m.askMatchIdx = 0
	m.onChatWake()
	m.refreshStreamStatus()
	return true
}

// askSearchStep moves the current match by delta (wrapping) and scrolls it into
// view, with a notice so stepping is visible even when the match is already on
// screen.
func (m *App) askSearchStep(delta int) {
	if m.chatConvID == "" || strings.TrimSpace(m.askSearch) == "" {
		return
	}
	matches := transcriptFilterItems(m.chatStore.snapshot(m.chatConvID), m.askSearch)
	n := len(matches)
	if n == 0 {
		return
	}
	m.askMatchIdx = ((m.askMatchIdx+delta)%n + n) % n
	m.askSearchScrollToMatch()
	m.dock.SetNotice(fmt.Sprintf("search match %d/%d", m.askMatchIdx+1, n))
}

// askSearchScrollToMatch wheels the transcript so the current match's first line
// sits at the top of the window. It resolves the match's KEY against the click
// geometry the last render recorded (transcriptSpans), which is the only place
// the rendered layout and the items are known to agree.
func (m *App) askSearchScrollToMatch() {
	if m.chatConvID == "" || strings.TrimSpace(m.askSearch) == "" {
		return
	}
	matches := transcriptFilterItems(m.chatStore.snapshot(m.chatConvID), m.askSearch)
	if len(matches) == 0 {
		return
	}
	if m.askMatchIdx >= len(matches) {
		m.askMatchIdx = len(matches) - 1
	}
	if m.askMatchIdx < 0 {
		m.askMatchIdx = 0
	}
	str := m.chatStreams[m.chatConvID]
	if str == nil {
		return
	}
	key := matches[m.askMatchIdx].Key
	for _, sp := range m.transcriptSpans[m.chatConvID] {
		if sp.Key == key {
			str.Wheel(sp.Line - str.Offset)
			return
		}
	}
}

// jumpTranscriptTop pins the Ask transcript to its FIRST message.
func (m *App) jumpTranscriptTop() {
	if str := m.chatStreams[m.chatConvID]; str != nil {
		str.ScrollToTop()
	}
}

// jumpTranscriptBottom pins the Ask transcript to its LATEST message and resumes
// the live follow (the existing stick-to-bottom behavior).
func (m *App) jumpTranscriptBottom() {
	if str := m.chatStreams[m.chatConvID]; str != nil {
		str.ScrollToBottom()
	}
}

// askTranscriptContentFocus reports whether the Ask transcript holds the keyboard
// from the CONTENT side — the only place the search/jump keys act. Composer focus
// is deliberately excluded: there every letter is the operator's message, and a
// search key must never steal it.
func (m *App) askTranscriptContentFocus() bool {
	return m.active == TabAsk && m.chatFocus == focusContent && m.chatConvID != ""
}
