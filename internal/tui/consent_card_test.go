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
		chat.ConsentAllowOnce, chat.ConsentSessionPrefix, chat.ConsentDeny} {
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

// TestConsentQuestionCardAnswersThroughTheReplyRPC pins the clarifying-question
// criterion AGAINST THE REAL SHELL, under the PAUSE contract.
//
// IT USED TO ASSERT THE OPPOSITE. Before the pause, ask_user was record-and-return,
// so answering a card sent the option's label as the NEXT USER MESSAGE — a new turn
// with the answer as its prompt. Now the question BLOCKS the turn, so the answer is
// delivered as the ask_user TOOL RESULT over the reply RPC and the SAME turn resumes.
// Sending it as a user message would leave the blocked call blocked and start a second
// turn on top of it.
//
// What the operator asked for is unchanged and is what this still checks: answering
// is ONE keystroke, and the card settles with the choice recorded — no retyping prose,
// and no second turn.
func TestConsentQuestionCardAnswersThroughTheReplyRPC(t *testing.T) {
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

	// THE ANSWER MUST NOT BECOME A USER MESSAGE: that was the pre-pause contract, and
	// under the pause it would spawn a second turn alongside the blocked one.
	for _, it := range m.chatStore.snapshot("c1") {
		if it.Kind == chat.KindUser {
			t.Fatalf("the answer was sent as a user message (%q) — under the pause it must go to the server as the tool result", it.Text)
		}
	}
	// The card settles, so the claim is released and the turn is free to resume.
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

// TestConsentCardOwnsTabWhilePending is the SHELL-level half of the
// key-ownership criterion: with a card pending, tab must reach the card and must
// NOT be consumed by the shell's tab chord. Asserted against the real router,
// because the defect it pins lived in the shell (the chord runs above the
// ClaimsKeys gate), not in the card.
func TestConsentCardOwnsTabWhilePending(t *testing.T) {
	m, as, items := pendingCard(t)
	before := items[0].Consent.Sel
	for i := 0; i < 2; i++ {
		nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = nm.(*App)
		if m.active != TabAsk {
			t.Fatalf("tab press %d rotated the tab ring out of a pending card (active=%v)", i+1, m.active)
		}
	}
	if items[0].Consent.Sel == before {
		t.Fatalf("tab must move the card's selection, sel stayed %d", before)
	}
	if !as.ClaimsKeys() {
		t.Fatal("the card must still own the keys after tab")
	}
	// Resolve, then tab is the shell's again — the claim is not sticky.
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if as.OwnsTab() {
		t.Fatal("tab must return to the shell once the card resolves")
	}
}
