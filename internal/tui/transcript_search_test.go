package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// Acceptance 3/4: transcript search (case-insensitive filter + count + no-match
// state + Esc clear) and jump-to-top/bottom on the Ask transcript, without
// hijacking the composer.

func searchFixture(m *App) {
	m.chatConvID = "c1"
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindUser, Text: "Hello WORLD", Key: "u1", At: 1})
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindText, Text: "the world is big", Key: "t1", At: 2})
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindText, Text: "unrelated", Key: "t2", At: 3})
}

func TestTranscriptSearchFilterIsCaseInsensitiveSubstring(t *testing.T) {
	items := []chat.ChatItem{
		{Kind: chat.KindUser, Text: "Hello WORLD", Key: "u1"},
		{Kind: chat.KindText, Text: "the world is big", Key: "t1"},
		{Kind: chat.KindText, Text: "unrelated", Key: "t2"},
	}
	if got := transcriptFilterItems(items, ""); len(got) != 3 {
		t.Fatalf("an empty query must not filter: got %d", len(got))
	}
	if got := transcriptFilterItems(items, "   "); len(got) != 3 {
		t.Fatalf("a whitespace query must not filter: got %d", len(got))
	}
	got := transcriptFilterItems(items, "WoRLd")
	if len(got) != 2 || got[0].Key != "u1" || got[1].Key != "t1" {
		t.Fatalf("case-insensitive substring filter wrong: %+v", got)
	}
	m, total := transcriptSearchCount(items, "world")
	if m != 2 || total != 3 {
		t.Fatalf("count = %d/%d, want 2/3", m, total)
	}
}

func TestTranscriptSearchStatusShowsCountAndEmptyState(t *testing.T) {
	if s := transcriptSearchStatus("zzz", 0, 5); !strings.Contains(s, "no matches") {
		t.Fatalf("no-match status must say so: %q", s)
	}
	if s := transcriptSearchStatus("e", 3, 7); !strings.Contains(s, "3/7") {
		t.Fatalf("status must carry the n/m count: %q", s)
	}
}

func TestSearchVisibleItemsKeepsConsentCards(t *testing.T) {
	m := newTestApp()
	items := []chat.ChatItem{
		{Kind: chat.KindText, Text: "alpha", Key: "a"},
		{Kind: chat.KindConsent, Key: "card"},
		{Kind: chat.KindText, Text: "beta", Key: "b"},
	}
	m.askSearch = "alpha"
	got := m.searchVisibleItems(items)
	if len(got) != 2 {
		t.Fatalf("want the match + the card, got %d", len(got))
	}
	keys := []string{got[0].Key, got[1].Key}
	if !(keys[0] == "a" && keys[1] == "card") {
		t.Fatalf("consent card must survive the filter alongside the match: %v", keys)
	}
	m.askSearch = ""
	if got := m.searchVisibleItems(items); len(got) != 3 {
		t.Fatalf("no search must render every item: got %d", len(got))
	}
}

func TestTranscriptStatusLineReportsSearch(t *testing.T) {
	m := newTestApp()
	searchFixture(m)
	if line := m.transcriptStatusLine(m.chatStore.snapshot("c1")); line != "" {
		t.Fatalf("no search → no search footer, got %q", line)
	}
	m.askSearch = "world"
	if line := m.transcriptStatusLine(m.chatStore.snapshot("c1")); !strings.Contains(line, "2/3") {
		t.Fatalf("footer must report 2/3 matches, got %q", line)
	}
	m.askSearch = "zzz"
	if line := m.transcriptStatusLine(m.chatStore.snapshot("c1")); !strings.Contains(line, "no matches") {
		t.Fatalf("footer must report no matches, got %q", line)
	}
}

func TestSetAndClearAskSearch(t *testing.T) {
	m := newTestApp()
	searchFixture(m)
	m.setAskSearch("  WORLD  ")
	if m.askSearch != "WORLD" {
		t.Fatalf("setAskSearch must store the trimmed query, got %q", m.askSearch)
	}
	if !m.clearAskSearch() {
		t.Fatal("clearAskSearch must report an active search was cleared")
	}
	if m.askSearch != "" || m.askMatchIdx != 0 {
		t.Fatalf("clear must reset the query and match index, got %q/%d", m.askSearch, m.askMatchIdx)
	}
	if m.clearAskSearch() {
		t.Fatal("clearAskSearch must be a no-op when no search is active")
	}
}

func TestAskSearchStepWrapsAndAdvances(t *testing.T) {
	m := newTestApp()
	searchFixture(m)
	m.setAskSearch("world") // 2 matches
	m.askSearchStep(1)
	if m.askMatchIdx != 1 {
		t.Fatalf("step forward → idx 1, got %d", m.askMatchIdx)
	}
	m.askSearchStep(1)
	if m.askMatchIdx != 0 {
		t.Fatalf("step past the last match must wrap to 0, got %d", m.askMatchIdx)
	}
	m.askSearchStep(-1)
	if m.askMatchIdx != 1 {
		t.Fatalf("step back from 0 must wrap to the last match, got %d", m.askMatchIdx)
	}
}

func TestTranscriptJumpTopAndBottom(t *testing.T) {
	m := newTestApp()
	m.chatConvID = "c1"
	str := kit2.NewStream("transcript", 40, 4)
	str.SetLines([]string{"1", "2", "3", "4", "5", "6", "7", "8"})
	m.chatStreams = map[string]*kit2.Stream{"c1": str}
	if !str.AtBottom() || !str.Following() {
		t.Fatal("precondition: a fresh long transcript follows the tail")
	}
	m.jumpTranscriptTop()
	if str.Offset != 0 || str.Following() {
		t.Fatalf("jump-to-top must land on line 0 and stop following: offset=%d following=%v", str.Offset, str.Following())
	}
	m.jumpTranscriptBottom()
	if !str.AtBottom() || !str.Following() {
		t.Fatalf("jump-to-bottom must pin to the tail and follow: offset=%d following=%v", str.Offset, str.Following())
	}
}

func TestTranscriptSearchKeysAreRegistered(t *testing.T) {
	want := map[string]bool{"n": false, "N": false, "home": false, "end": false}
	for _, r := range GlobalKeyRoutes(Tabs) {
		if _, ok := want[r.Keys]; ok {
			want[r.Keys] = true
		}
	}
	for k, found := range want {
		if !found {
			t.Errorf("no route registered for the transcript search/jump key %q", k)
		}
	}
}

func TestTranscriptKeysGateOnAskContentFocus(t *testing.T) {
	m := newTestApp()
	searchFixture(m)
	m.chatFocus = focusContent
	m.active = TabAsk
	if !m.askTranscriptContentFocus() {
		t.Fatal("Ask + content focus + open conversation must be the active transcript")
	}
	m.chatFocus = focusComposer
	if m.askTranscriptContentFocus() {
		t.Fatal("composer focus must NOT expose the transcript search/jump keys")
	}
}

// The composer keeps plain letters: typing "n" while composing must never run the
// search step (acceptance 4: no hijacking of composer input).
func TestSearchKeyNeverHijacksTheComposer(t *testing.T) {
	m := newTestApp()
	searchFixture(m)
	m.setAskSearch("world")
	m.chatFocus = focusComposer
	m.active = TabAsk
	idx := m.askMatchIdx
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	got := m2.(*App)
	if got.askMatchIdx != idx {
		t.Fatal("'n' while composing must go to the composer, not step the search")
	}
	if !strings.Contains(got.dock.Value(), "n") {
		t.Fatalf("'n' must reach the composer buffer, got %q", got.dock.Value())
	}
}

// Esc clears an active search from content focus (acceptance 3).
func TestEscClearsTranscriptSearch(t *testing.T) {
	m := newTestApp()
	searchFixture(m)
	m.setAskSearch("world")
	m.chatFocus = focusContent
	m.active = TabAsk
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if got := m2.(*App); got.askSearch != "" {
		t.Fatalf("Esc must clear the transcript search, got %q", got.askSearch)
	}
}

// CTRL+F IS FIND (acceptance 1). The universal chord must ACTIVATE the transcript
// search on the composer's text, through the REAL key dispatch, and it must do so
// from EITHER focus: from the content pane AND while the composer holds focus (the
// route is global AND a composer bypass). It delegates to the same /find path — no
// duplicate search UI.
func TestCtrlFActivatesTranscriptSearchFromEitherFocus(t *testing.T) {
	for _, focus := range []focusMode{focusContent, focusComposer} {
		focus := focus
		t.Run(map[focusMode]string{focusContent: "content-focus", focusComposer: "composer-focus"}[focus], func(t *testing.T) {
			m := askRelaunched(t, "c1")
			m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindText, Text: "the needle is here", Key: "t1", At: 1})
			m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindText, Text: "unrelated", Key: "t2", At: 2})
			m.dock.SetValue("needle")
			m.active = TabAsk
			m.chatFocus = focus

			m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlF})
			got := m2.(*App)
			if got.askSearch != "needle" {
				t.Fatalf("ctrl+f from %v must activate the transcript search on the composer's text, got %q",
					focus, got.askSearch)
			}
			matches, total := transcriptSearchCount(got.chatStore.snapshot("c1"), "needle")
			if matches != 1 || total != 2 {
				t.Fatalf("the ctrl+f search must filter the transcript: got %d/%d, want 1/2", matches, total)
			}
		})
	}
}
