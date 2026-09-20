package tui

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// ctxStubScreen is a screen that exposes a selected item, so the shell's
// context resolver has something to resolve.
type ctxStubScreen struct {
	stubScreen
	src   string
	title string
}

func (s *ctxStubScreen) ActiveSourceName() string { return s.src }
func (s *ctxStubScreen) ActiveItem() (screenkit.Item, bool) {
	return screenkit.Item{ID: "sel-1", Title: s.title}, true
}

// Regression for the operator's report: a message sent from Ask Orchicon
// carried "[context: Conversations: <the conversation's own title>]". The Ask
// tab's conversations list is the chat's own conversation, so its title is
// self-referential — it must not be injected as context (and the dock chip
// must not claim it will be).
func TestAskConversationContextIsNotInjected(t *testing.T) {
	m := newTestApp()
	m.RegisterScreen(TabAsk, &ctxStubScreen{
		stubScreen: stubScreen{id: "ask"},
		src:        "conversations",
		title:      "More TUI goodness",
	})
	m.SwitchTo(TabAsk)

	if got := m.contextPreamble(); got != "" {
		t.Fatalf("contextPreamble on Ask = %q, want empty (self-referential context)", got)
	}
	if got := m.activeContextLabel(); got != "" {
		t.Fatalf("context chip on Ask = %q, want empty", got)
	}
	// A pinned override still wins (the escape hatch is preserved).
	m.contextOverride = "Workers: Quick Software Engineer"
	if got, want := m.contextPreamble(), "[context: Workers: Quick Software Engineer]"; got != want {
		t.Fatalf("pinned context = %q, want %q", got, want)
	}
}

// Off the Ask tab, a selected entity IS real context and must still be
// injected — the guard is specific to the chat's own conversation list.
func TestSelectedEntityContextIsStillInjected(t *testing.T) {
	m := newTestApp()
	m.RegisterScreen(TabWork, &ctxStubScreen{
		stubScreen: stubScreen{id: "work"},
		src:        "workitems",
		title:      "Ship TUI 2.0",
	})
	m.SwitchTo(TabWork)

	if got := m.contextPreamble(); got == "" {
		t.Fatal("a selected work item must still be injected as context")
	}
}
