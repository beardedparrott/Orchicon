package tui

import (
	"fmt"
	"strings"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// transcript_search.go — in-transcript search for the Ask pane, plus the jump-to-top /
// jump-to-bottom transcript controls.

// transcriptSearchSep is the field separator the searchable text is built with.
// It is a newline, spelled as a rune so this file carries no escape sequences.
var transcriptSearchSep = string(rune(10))

// transcriptSearchText is the text a transcript item is searched over: the operator-visible text
// plus the tool/ask vocabulary.
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

// transcriptItemMatches reports whether an item's searchable text contains q (case-insensitive).
func transcriptItemMatches(it chat.ChatItem, q string) bool {
	q = strings.TrimSpace(q)
	if q == "" {
		return true
	}
	return strings.Contains(strings.ToLower(transcriptSearchText(it)), strings.ToLower(q))
}

// transcriptFilterItems returns the items matching q (every item when q is empty).
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

// transcriptSearchStatus is the pane footer while a search is active: the count, or an explicit
// no-match line — never empty, so the operator always sees that a search is live.
func transcriptSearchStatus(q string, matches, total int) string {
	q = strings.TrimSpace(q)
	if matches == 0 {
		return fmt.Sprintf("search %q · no matches · esc clears", q)
	}
	return fmt.Sprintf("search %q · %d/%d matches · n/N next·prev · esc clears", q, matches, total)
}

// searchVisibleItems is the item set the transcript RENDERS: the full set when no search is active,
// else only the matching items — EXCEPT consent/ask cards, which are always kept.
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

// clearAskSearch drops the query, restoring the unfiltered transcript.
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

// askSearchStep moves the current match by delta (wrapping) and scrolls it into view, with a notice
// so stepping is visible even when the match is already on screen.
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

// askSearchScrollToMatch wheels the transcript so the current match's first line sits at the top of
// the window.
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

// askTranscriptContentFocus reports whether the Ask transcript holds the keyboard from the CONTENT
// side — the only place the search/jump keys act.
func (m *App) askTranscriptContentFocus() bool {
	return m.active == TabAsk && m.chatFocus == focusContent && m.chatConvID != ""
}

// dockEmpty reports whether the composer holds no text. Whitespace-only counts as empty — the rule the
// slash palette, the rail's chords and the transcript's navigation keys all share.
func (m *App) dockEmpty() bool { return strings.TrimSpace(m.dock.Value()) == "" }

// transcriptJumpKeysActive reports whether the transcript's JUMP keys (home/end) own the keystroke
// right now.
func (m *App) transcriptJumpKeysActive() bool {
	if m.active != TabAsk || m.chatConvID == "" {
		return false
	}
	return m.chatFocus == focusContent || (m.chatFocus == focusComposer && m.dockEmpty())
}

// transcriptScrollKeysActive reports whether the transcript owns the VERTICAL SCROLL keys.
func (m *App) transcriptScrollKeysActive() bool {
	if m.active != TabAsk || m.chatConvID == "" || m.askPane != askPaneConversation {
		return false
	}
	return m.chatFocus == focusContent || (m.chatFocus == focusComposer && m.dockEmpty())
}

// scrollTranscriptKey applies a vertical SCROLL key to the Ask transcript, reporting whether it
// owned the key.
func (m *App) scrollTranscriptKey(key string) bool {
	d := scrollKeyDelta(key)
	if d == 0 || !m.transcriptScrollKeysActive() {
		return false
	}
	m.ScrollTranscript(d)
	m.onChatWake() // keep the scroll indicator honest (the content path always did this)
	return true
}

// jumpTranscriptKey applies home/end to the Ask transcript, reporting whether it owned the key.
func (m *App) jumpTranscriptKey(key string) bool {
	if !m.transcriptJumpKeysActive() {
		return false
	}
	switch key {
	case "home":
		m.jumpTranscriptTop()
	case "end":
		m.jumpTranscriptBottom()
	default:
		return false
	}
	m.onChatWake() // the pane's own status line is derived at repaint
	return true
}

// transcriptNavKey applies EITHER kind of navigation key — the vertical scroll keys or home/end —
// to the Ask transcript when it is the surface the operator means.
func (m *App) transcriptNavKey(key string) bool {
	return m.scrollTranscriptKey(key) || m.jumpTranscriptKey(key)
}
