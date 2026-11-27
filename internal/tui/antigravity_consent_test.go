package tui

// antigravity_consent_test.go — the CONSENT CARD an antigravity `run_command` permission event draws
// (child 8, AC2's second half).
//
// WHY IT IS ASSERTED IN THIS PACKAGE AND NOT ONLY IN `chat`. The claude consent suite proves the card's
// shape for `write`; what is specific to agy is the ROUTE a `run_command` ask takes and the words that
// survive it:
//
//   1. THE EVENT NAMES THE AGY SPELLING. internal/antigravity/ask_permission.go emits the permission bus
//      event with Tool = agy's own name (`run_command`), NOT the canonical `bash`. The card must show
//      what the model actually called, or the transcript disagrees with itself (the same rule
//      ask_mapper.go states for tool_use parts).
//   2. THE ASK NAMES THE COMMAND, not a path. A `run_command {CommandLine:"ls"}` carries no file
//      target, so the card's subject has to come from the Command field — a card that fell back to
//      Targets would render an approval prompt with nothing to approve.
//   3. IT CLAIMS THE KEYS WHILE PENDING. A card that renders but does not take the keyboard is the
//      Phase-2c finding (a footer promising an interaction that does nothing).
//
// The ask is built from the WIRE shape (apiv1.PermissionAsk) rather than a hand-made struct, because
// the converter — PermissionAskFromProto — is part of the route a real event takes.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// agyRunCommandAsk is the wire ask the plane sends for the recorded agy event
// `session/request_permission {run_command, CommandLine:"ls"}`: agy's own tool name, the command in
// Command, no path targets, and the conversation's project as the grant directory.
func agyRunCommandAsk() *apiv1.PermissionAsk {
	return &apiv1.PermissionAsk{
		AskId:          "agy-ask-1",
		ConversationId: "c1",
		SessionId:      "s-agy",
		Tool:           "run_command",
		Command:        "ls",
		Directory:      "/home/ops/project",
		Summary:        "run_command ls · /home/ops/project · inside this conversation's project",
	}
}

// TestAntigravityRunCommandCardNamesTheAgyToolAndTheCommand is the AC2 consent half: a `run_command`
// permission event becomes a card that shows the agy tool name, the command, and the three actions.
func TestAntigravityRunCommandCardNamesTheAgyToolAndTheCommand(t *testing.T) {
	ask := chat.PermissionAskFromProto(agyRunCommandAsk())

	// The conversion keeps agy's spelling and takes the command as the subject.
	if ask.Tool != "run_command" {
		t.Fatalf("Tool = %q, want agy's own name %q — canonicalising it to bash here would make the card "+
			"site disagree with the transcript it sits in", ask.Tool, "run_command")
	}
	if ask.Target != "ls" {
		t.Fatalf("Target = %q, want the command %q (a run_command ask carries no path target)", ask.Target, "ls")
	}
	if ask.Kind != chat.AskTool {
		t.Fatalf("Kind = %q, want %q — this is a decision, not a clarifying question", ask.Kind, chat.AskTool)
	}

	card := chat.ConsentCardText(chat.ConsentItem(ask, 1000), 90)
	for _, want := range []string{"run_command", "ls", chat.ConsentAllowOnce, ask.SessionLabel(), chat.ConsentDeny} {
		if !strings.Contains(card, want) {
			t.Fatalf("the antigravity consent card must name %q:\n%s", want, card)
		}
	}
}

// TestAntigravityRunCommandCardOwnsTheKeysAndResolvesAllowOnce drives the ask through the SHELL, so the
// claim is made against the real model rather than a stub: the card lands in the open conversation,
// takes the keyboard, and "allow once" reaches the server as that decision.
func TestAntigravityRunCommandCardOwnsTheKeysAndResolvesAllowOnce(t *testing.T) {
	m, as := consentApp(t)
	m.ShowConsentAsk(chat.PermissionAskFromProto(agyRunCommandAsk()))

	items := m.chatStore.snapshot("c1")
	if len(items) != 1 || items[0].AskID != "agy-ask-1" {
		t.Fatalf("the antigravity ask must land in the open conversation as a card, got %+v", items)
	}
	as.RenderTranscript(items, chat.Conversation{}, false)
	if !as.ClaimsKeys() {
		t.Fatal("a pending antigravity card must claim the keyboard — a card that renders but cannot be " +
			"answered is the Phase-2c finding")
	}

	// The rendered transcript names the agy tool and the command, so the operator can see what they
	// are about to approve before pressing anything.
	out := chat.RenderItems(items, 90)
	for _, want := range []string{"run_command", "ls", chat.ConsentAllowOnce} {
		if !strings.Contains(out, want) {
			t.Fatalf("the transcript must show %q:\n%s", want, out)
		}
	}

	// Enter on the default row is "allow once" (row 0), which the shell forwards as ALLOW_ONCE.
	m.ConsentResolve("agy-ask-1", chat.DecisionAllowOnce, "")
	st := m.chatStore.consentState("c1", "agy-ask-1")
	if st == nil || st.Pending() {
		t.Fatalf("the card must settle after an answer, got %+v", st)
	}

	// And the card no longer swallows the keyboard, which is the half a rendered string cannot see.
	items = m.chatStore.snapshot("c1")
	as.RenderTranscript(items, chat.Conversation{}, false)
	if as.ClaimsKeys() {
		t.Fatal("after the card settles it must hand the keyboard back — a settled card that still claims " +
			"the keys makes the composer unreachable")
	}
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	m = nm.(*App)
	if m.quitting {
		t.Fatal("a settled card must not consume/act on a bare letter")
	}
}
