package tui

// ask_search_highlight_test.go — THE SEARCH HIGHLIGHT REACHES THE PAINTED TRANSCRIPT.
//
// The md package proves the band is drawn (internal/tui/md/search_highlight_test.go). This proves the pane
// actually ASKS for it: the shell owns the query (m.askSearch) and the transcript is rendered by the shell,
// so a highlight armed only in md's tests could be dead code in the app and every md test would still pass.

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/md"
)

// A search marks the hits in what the Ask pane renders.
func TestSearchHighlightsThePaintedTranscript(t *testing.T) {
	m := askRelaunched(t, "c1")
	// The tone of the transcript is the model's band; use its own surface so the band can restore.
	m.chatStore.append("c1", chat.ChatItem{
		Kind: chat.KindText, Text: "alpha NEEDLE beta needle gamma", Key: "t1", At: 1,
	})

	// Arm a match band. The theme does this in production; a test must do it explicitly because no theme is
	// installed in the harness.
	armSearchBand(t)

	// WITHOUT A QUERY: no band in the painted output, which is the state before a search starts.
	noQuery, _ := m.renderCache.Render("c1", chat.GroupByPhase(m.chatStore.snapshot("c1")), 80, chat.CopyGlyph, m.foldedReasoning, "")
	if strings.Contains(noQuery, "\x1b[48;") {
		t.Fatalf("a background band was drawn with no search active:\n%q", noQuery)
	}

	// WITH THE SHELL'S QUERY SET: the pane renders the hits banded, and the text is intact.
	m.askSearch = "needle"
	rendered, _ := m.renderCache.Render("c1", chat.GroupByPhase(m.chatStore.snapshot("c1")), 80, chat.CopyGlyph, m.foldedReasoning, m.askSearch)
	if !strings.Contains(rendered, "\x1b[48;") {
		t.Errorf("the painted transcript carries no match band with a search active:\n%q", rendered)
	}
	plain := stripSGRFor(t, rendered)
	if !strings.Contains(plain, "NEEDLE") || !strings.Contains(plain, "needle") {
		t.Errorf("the text was altered by marking: %q", plain)
	}
}

// AND THE CACHE DOES NOT SERVE A MARKED SEGMENT TO AN UNSEARCHED RENDER. This is the failure the query in the
// cache key exists to prevent: the first paint would otherwise keep its bands after the search was cleared.
func TestSearchHighlightDoesNotLeakThroughTheRenderCache(t *testing.T) {
	m := askRelaunched(t, "c1")
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindText, Text: "a needle here", Key: "t1", At: 1})
	armSearchBand(t)

	items := chat.GroupByPhase(m.chatStore.snapshot("c1"))
	// Search first, so the marked segment is the one in the cache.
	m.askSearch = "needle"
	searched, _ := m.renderCache.Render("c1", items, 80, chat.CopyGlyph, m.foldedReasoning, m.askSearch)
	if !strings.Contains(searched, "\x1b[48;") {
		t.Fatal("fixture: the searched render carried no band")
	}
	// Now clear the search. The cache holds the marked segment under a DIFFERENT key, so this must come back
	// clean rather than reusing it.
	cleared, _ := m.renderCache.Render("c1", items, 80, chat.CopyGlyph, m.foldedReasoning, "")
	if strings.Contains(cleared, "\x1b[48;") {
		t.Errorf("the cleared search still painted bands — the cache served a marked segment:\n%q", cleared)
	}
}

// armSearchBand installs the match band the theme would install.
func armSearchBand(t *testing.T) {
	t.Helper()
	md.SetSearchHighlight("#e5e5e5", "#fbbf24", "#e5e5e5", "#181818")
	t.Cleanup(func() { md.SetSearchHighlight("", "", "", "") })
}

// stripSGRFor removes escape sequences so a test can assert on the TEXT.
func stripSGRFor(t *testing.T, s string) string {
	t.Helper()
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			i++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
