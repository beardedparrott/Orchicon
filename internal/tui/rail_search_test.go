package tui

// rail_search_test.go — the conversations rail's free-text search.
//
// The rail could always be SCOPED by project, and the TUI could always mark rows and bulk-delete or
// bulk-categorize them. What neither client had was a way to narrow the list to a STRING — so "delete
// every conversation matching this name" was unreachable: you could only mark what you could scroll to.
//
// Mirrored in the GUI as conversationMatchesSearch; these tests pin the same semantics on this side so
// the two clients cannot drift into searching differently.

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

func conv(id, title, preview string) chat.Conversation {
	return chat.Conversation{ID: id, Title: title, LastMessagePreview: preview}
}

func TestConvMatchesSearchEmptyQueryMatchesEverything(t *testing.T) {
	// The "no filter" state. Returning nothing here would empty the rail the moment the box is focused.
	for _, q := range []string{"", "   ", "\t"} {
		if !convMatchesSearch(conv("c1", "Anything", ""), q) {
			t.Errorf("query %q must match everything", q)
		}
	}
}

func TestConvMatchesSearchTitleSubstringCaseInsensitive(t *testing.T) {
	c := conv("c1", "Fix MCP tokens", "")
	for _, q := range []string{"mcp", "MCP", "tokens", "ok"} {
		if !convMatchesSearch(c, q) {
			t.Errorf("query %q did not match the title", q)
		}
	}
}

func TestConvMatchesSearchReachesThePreview(t *testing.T) {
	// Most conversations carry an auto-generated title, so a title-only search misses the chat whose
	// opening line the operator actually remembers.
	c := conv("c1", "New conversation", "let's talk about the weather app")
	if !convMatchesSearch(c, "weather") {
		t.Error("the last-message preview was not searched")
	}
}

func TestConvMatchesSearchANDsEveryTerm(t *testing.T) {
	c := conv("c1", "MCP work", "the token store is broken")
	if !convMatchesSearch(c, "mcp token") {
		t.Error("two terms split across title and preview must match")
	}
	if convMatchesSearch(c, "mcp kubernetes") {
		t.Error("a term present in neither must not match")
	}
}

func TestFilterConversationsBySearch(t *testing.T) {
	list := []chat.Conversation{
		conv("c1", "Weatherapp ideas", ""),
		conv("c2", "MCP token fix", ""),
		conv("c3", "Weatherapp bug", ""),
	}
	got := filterConversationsBySearch(list, "weatherapp")
	if len(got) != 2 {
		t.Fatalf("matched %d conversations, want 2: %v", len(got), got)
	}
	for _, c := range got {
		if c.ID == "c2" {
			t.Error("a non-matching conversation survived the filter")
		}
	}
	// An empty query returns the list untouched.
	if len(filterConversationsBySearch(list, "")) != 3 {
		t.Error("an empty query must not filter")
	}
	// An honest empty when nothing matches.
	if len(filterConversationsBySearch(list, "nonexistent")) != 0 {
		t.Error("a non-matching query must return nothing, not the whole list")
	}
}

// TestSearchComposesAfterScope pins the ordering rule: search narrows what you are ALREADY looking at,
// so it can never surface a conversation from a project you have scoped away.
func TestSearchComposesAfterScope(t *testing.T) {
	all := []chat.Conversation{
		{ID: "c1", Title: "weather one", ProjectID: "p1"},
		{ID: "c2", Title: "weather two", ProjectID: "p2"},
	}
	scoped := filterConversationsByScope(all, "p1")
	found := filterConversationsBySearch(scoped, "weather")
	if len(found) != 1 || found[0].ID != "c1" {
		t.Fatalf("search escaped the project scope: %v", found)
	}
}

// --- select-all-of-visible -------------------------------------------------------------------

// TestSelectAllVisibleMarksTheFilteredSet is the gesture the operator asked for: search, then select
// all of WHAT MATCHED, then delete.
func TestSelectAllVisibleMarksTheFilteredSet(t *testing.T) {
	m := newTestApp()
	m.active = TabAsk
	m.projectScope = projectScopeAll
	m.conversations = []chat.Conversation{
		conv("c1", "Weatherapp ideas", ""),
		conv("c2", "MCP token fix", ""),
		conv("c3", "Weatherapp bug", ""),
	}
	m.convSearch = "weatherapp"
	m.selectAllVisibleConversations()
	if len(m.convMarked) != 2 {
		t.Fatalf("marked %d, want the 2 matching conversations: %v", len(m.convMarked), m.convMarked)
	}
	if !m.convMarked["c1"] || !m.convMarked["c3"] {
		t.Errorf("the WRONG conversations were marked: %v", m.convMarked)
	}
	if m.convMarked["c2"] {
		t.Error("a filtered-out conversation was marked — a bulk delete would have taken it with them")
	}
}

// TestSelectAllIsAToggle: pressing it again clears, so it cannot trap the operator in an
// ever-growing selection.
func TestSelectAllIsAToggle(t *testing.T) {
	m := newTestApp()
	m.active = TabAsk
	m.projectScope = projectScopeAll
	m.conversations = []chat.Conversation{
		conv("c1", "one", ""),
		conv("c2", "two", ""),
	}
	m.selectAllVisibleConversations()
	if len(m.convMarked) != 2 {
		t.Fatalf("first press marked %d, want 2", len(m.convMarked))
	}
	m.selectAllVisibleConversations()
	if len(m.convMarked) != 0 {
		t.Fatalf("second press left %d marked, want 0 (a toggle)", len(m.convMarked))
	}
}

// TestSearchNarrowsWhatSelectAllReaches ties the two halves together: the reason search exists is to
// make a bulk action act on a named subset.
func TestSearchNarrowsWhatSelectAllReaches(t *testing.T) {
	m := newTestApp()
	m.active = TabAsk
	m.projectScope = projectScopeAll
	m.conversations = []chat.Conversation{
		{ID: "c1", Title: "junk scratch", ProjectID: "p1"},
		{ID: "c2", Title: "junk test", ProjectID: "p1"},
		{ID: "c3", Title: "keep this", ProjectID: "p1"},
	}
	m.convSearch = "junk"
	visible := m.scopedConversations()
	if len(visible) != 2 {
		t.Fatalf("search showed %d conversations, want 2", len(visible))
	}
	m.selectAllVisibleConversations()
	if len(m.convMarked) != 2 || m.convMarked["c3"] {
		t.Fatalf("select-all reached beyond the search: %v", m.convMarked)
	}
}

// TestTypingFiltersLive is the specific defect: the first attempt used a SUBMIT FORM, so the rail only
// narrowed when the operator pressed enter, and the dialog told them "hit ctrl+s to save" — which is not
// what a search box does. This drives the box keystroke by keystroke and asserts the list narrows on
// EVERY key, without any commit.
func TestTypingFiltersLive(t *testing.T) {
	m := newTestApp()
	m.active = TabAsk
	m.projectScope = projectScopeAll
	m.conversations = []chat.Conversation{
		conv("c1", "Weatherapp ideas", ""),
		conv("c2", "MCP token fix", ""),
		conv("c3", "Weatherapp bug", ""),
	}
	m.openConvSearch()
	if m.convSearchBox == nil {
		t.Fatal("ctrl+s did not open the search box")
	}

	// Type "wea" one key at a time — no enter, no submit.
	for _, r := range "wea" {
		handled, _ := m.convSearchKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		if !handled {
			t.Fatalf("the box did not consume %q", r)
		}
		if got := len(m.scopedConversations()); got != 2 {
			t.Fatalf("after typing %q the rail shows %d conversations, want 2 (filtering must be LIVE)",
				string(r), got)
		}
	}
	// The box is still open — nothing was committed.
	if m.convSearchBox == nil {
		t.Error("typing closed the box; a search box stays open while the query is being built")
	}

	// Narrow further: "weatherapp" leaves the two, and "mcp" would leave one.
	handled, _ := m.convSearchKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("therapp")})
	if !handled {
		t.Fatal("multi-char input was not consumed")
	}
	if got := len(m.scopedConversations()); got != 2 {
		t.Fatalf("rail shows %d, want 2", got)
	}

	// Enter keeps the filter and closes the box.
	m.convSearchKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.convSearchBox != nil {
		t.Error("enter must close the box")
	}
	if m.convSearch != "weatherapp" {
		t.Errorf("filter = %q, want weatherapp to survive the commit", m.convSearch)
	}
	if got := len(m.scopedConversations()); got != 2 {
		t.Error("committing must leave the filter applied")
	}
}

// TestEscRestoresThePreviousFilter: an accidental ctrl+s must not lose the query in force.
func TestEscRestoresThePreviousFilter(t *testing.T) {
	m := newTestApp()
	m.active = TabAsk
	m.projectScope = projectScopeAll
	m.conversations = []chat.Conversation{
		conv("c1", "alpha", ""), conv("c2", "beta", ""),
	}
	m.convSearch = "alpha"
	m.openConvSearch()
	m.convSearchKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("zzz")})
	m.convSearchKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.convSearch != "alpha" {
		t.Fatalf("esc left filter = %q, want the original alpha", m.convSearch)
	}
}

// TestEmptyTitlesMatchThePlaceholder is the TUI half of the reported bug: a conversation with an empty
// stored title draws as "New conversation", so a search for that visible text must find it.
func TestEmptyTitlesMatchThePlaceholder(t *testing.T) {
	list := []chat.Conversation{
		conv("u1", "", ""),
		conv("u2", "   ", ""),
		conv("n1", "MCP token fix", ""),
	}
	got := filterConversationsBySearch(list, "new conversation")
	if len(got) != 2 {
		t.Fatalf("matched %d, want the 2 untitled conversations", len(got))
	}
	for _, c := range got {
		if c.ID == "n1" {
			t.Error("a titled conversation was matched by the placeholder")
		}
	}
	if d := convDisplayTitle(conv("u1", "", "")); d != "New conversation" {
		t.Errorf("display title = %q, want the rail's placeholder", d)
	}
}
