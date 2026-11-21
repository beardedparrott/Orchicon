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
