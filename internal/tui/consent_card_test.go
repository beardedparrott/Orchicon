package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
	"github.com/beardedparrott/orchicon/internal/tui/screens/ask"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
)

// consentApp builds a shell with the REAL Ask screen registered and an open
// conversation, so the key claim is asserted against the actual model rather
// than a stub that merely reports `true`.
func consentApp(t *testing.T) (*App, *ask.Model) {
	t.Helper()
	m := NewApp(&client.Clients{}, &config.Profile{Name: "default", URL: "https://x.example.com"}, "v9.9.9")
	m.width, m.height = 120, 40
	as := ask.New(m.clients, subs.NewRegistry())
	m.RegisterScreen(TabAsk, as)
	m.RegisterScreen(TabWork, &claimStub{id: TabWork})
	m.SwitchTo(TabAsk)
	m.chatConvID = "c1"
	return m, as
}

func pendingCard(t *testing.T) (*App, *ask.Model, []chat.ChatItem) {
	t.Helper()
	m, as := consentApp(t)
	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "ask-1", Kind: chat.AskTool, Tool: "write",
		Target: "/home/ops/project/main.go", Directory: "/home/ops/project",
	})
	items := m.chatStore.snapshot("c1")
	// The shell's wake repaints the transcript through the screen, which is what
	// reconciles the card. Drive that one hop explicitly.
	as.RenderTranscript(items, chat.Conversation{}, false)
	if !as.ClaimsKeys() {
		t.Fatal("precondition: a pending ask must claim the keys")
	}
	return m, as, items
}

// TestConsentCardRendersTheToolAndTargetInTheTranscript pins the acceptance
// criterion against the REAL transcript renderer: the card names the tool and
// the target, with the three actions.
func TestConsentCardRendersTheToolAndTargetInTheTranscript(t *testing.T) {
	m, _, items := pendingCard(t)
	out := chat.RenderItems(items, 90)
	for _, want := range []string{"write", "/home/ops/project/main.go",
		chat.ConsentAllowOnce, chat.ConsentAllowSession, chat.ConsentDeny} {
		if !strings.Contains(out, want) {
			t.Fatalf("the transcript must show %q:\n%s", want, out)
		}
	}
	_ = m
}

// TestConsentCardOwnsTheKeysWhilePending is THE key-ownership assertion: with a
// card pending, a bare letter is not a shell route and does not reach the
// composer.
func TestConsentCardOwnsTheKeysWhilePending(t *testing.T) {
	m, as, _ := pendingCard(t)
	keys := []tea.KeyMsg{
		{Type: tea.KeySpace},
		{Type: tea.KeyRunes, Runes: []rune("/")},
		{Type: tea.KeyRunes, Runes: []rune("d")},
		{Type: tea.KeyRunes, Runes: []rune("a")},
		{Type: tea.KeyRunes, Runes: []rune("w")},
	}
	for _, k := range keys {
		nm, _ := m.Update(k)
		m = nm.(*App)
		if m.quitting {
			t.Fatalf("a claimed %q must not quit", k.String())
		}
		if m.TabMenu() != nil {
			t.Fatalf("a claimed %q must not open the tab menu", k.String())
		}
		if m.palette.PaletteOpen() {
			t.Fatalf("a claimed %q must not open the palette", k.String())
		}
		if strings.TrimSpace(m.dock.Value()) != "" {
			t.Fatalf("a claimed %q leaked into the composer: %q", k.String(), m.dock.Value())
		}
	}
	if !as.ClaimsKeys() {
		t.Fatal("the card must still own the keys after stray letters")
	}
}

// TestConsentEscapeDeniesAndRecordsTheRefusal pins the pipeline end to end:
// Escape denies, the claim is released (the turn stays usable), and the refusal
// is recorded in the transcript.
func TestConsentEscapeDeniesAndRecordsTheRefusal(t *testing.T) {
	m, as, _ := pendingCard(t)
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = nm.(*App)
	if as.ClaimsKeys() {
		t.Fatal("esc must release the claim so the turn stays usable")
	}
	items := m.chatStore.snapshot("c1")
	out := chat.RenderItems(items, 90)
	if !strings.Contains(out, chat.ConsentDeny) || !strings.Contains(out, "main.go") {
		t.Fatalf("the refusal must be recorded in the transcript:\n%s", out)
	}
	if strings.Contains(out, "┌") {
		t.Fatalf("a resolved ask must no longer draw the card box:\n%s", out)
	}
}

// TestConsentSessionGrantSuppressesTheNextCardForTheDirectory pins "ask once per
// directory per session" AND that the grants are visible afterwards.
func TestConsentSessionGrantSuppressesTheNextCardForTheDirectory(t *testing.T) {
	m, as, _ := pendingCard(t)

	// Move to "Allow for this session" and confirm.
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = nm.(*App)
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm.(*App)

	grants, ok := m.ConsentGrants("c1")
	if !ok || len(grants) != 1 || grants[0].Directory != "/home/ops/project" {
		t.Fatalf("the grant must be recorded for the directory, got %+v (ok=%v)", grants, ok)
	}
	if _, v := as.RenderTranscript(m.chatStore.snapshot("c1"), chat.Conversation{}, true); !fieldValueContains(v, "grants", "/grants") {
		t.Fatalf("the grants roll-up must be visible in the header, got %+v", v)
	}

	// A second ask for the SAME directory must not produce a card.
	before := len(m.chatStore.snapshot("c1"))
	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "ask-2", Kind: chat.AskTool, Tool: "write",
		Target: "/home/ops/project/other.go", Directory: "/home/ops/project",
	})
	if after := len(m.chatStore.snapshot("c1")); after != before {
		t.Fatalf("a granted directory must not ask again: %d -> %d items", before, after)
	}

	// A DIFFERENT directory still asks.
	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "ask-3", Kind: chat.AskTool, Tool: "bash",
		Target: "make ci", Directory: "/home/ops/other",
	})
	if after := len(m.chatStore.snapshot("c1")); after != before+1 {
		t.Fatalf("an ungranted directory must still ask: %d -> %d items", before, after)
	}
}

// TestConsentQuestionCardSendsTheChoiceThroughTheComposerFunnel pins the
// clarifying-question criterion AGAINST THE REAL SHELL: choosing an option
// sends the choice as the next user message — the operator must not have to
// retype prose. The screen-level test proves the decision reaches the host;
// this one proves the host turns it into a user message on the send path the
// composer itself uses (SendUserMessage -> the optimistic echo in the store).
func TestConsentQuestionCardSendsTheChoiceThroughTheComposerFunnel(t *testing.T) {
	m, _ := newAskApp(t)
	as := ask.New(m.clients, m.reg)
	m.RegisterScreen(TabAsk, as)
	m.SwitchTo(TabAsk)
	m.chatConvID = "c1"
	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "q1", Kind: chat.AskQuestion, Tool: "ask",
		Question: "Which file did you mean?",
		Options:  []string{"main.go", "util.go"}, AllowOther: true,
	})
	as.RenderTranscript(m.chatStore.snapshot("c1"), chat.Conversation{}, false)
	if !as.ClaimsKeys() {
		t.Fatal("precondition: a pending question must claim the keys")
	}

	// options: main.go(0) -> down -> util.go(1) -> enter.
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = nm.(*App)
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm.(*App)

	sent := false
	for _, it := range m.chatStore.snapshot("c1") {
		if it.Kind == chat.KindUser && it.Text == "util.go" {
			sent = true
		}
	}
	if !sent {
		t.Fatal("choosing an option must send it as the next user message")
	}
	if as.ClaimsKeys() {
		t.Fatal("the claim must be released once the question is answered")
	}
}

func fieldValueContains(fields []screenkit.Field, key, want string) bool {
	for _, f := range fields {
		if f.Key == key && strings.Contains(f.Value, want) {
			return true
		}
	}
	return false
}
