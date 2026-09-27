package tui

// fullsend_test.go — the TUI's half of FULLSEND: the `/fullsend` toggle and the composer
// indicator.
//
// The mode itself is enforced server-side (see internal/askorchicon/fullsend.go); what is
// pinned here is the part the operator relies on to KNOW it is on. A permission bypass whose
// indicator is wrong is worse than one with no indicator at all, because the operator reads
// "the gate is up" from it and proceeds on that basis.

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// TestFullsendSlashTogglesAndSendsTheValue — the operator's "if you type /fullsend again,
// it will set it to off".
//
// The WIRE CARRIES A VALUE, not a toggle request, and the value is derived from the state
// the shell can SEE. That is what stops two taps from flipping against a stale local belief.
func TestFullsendSlashTogglesAndSendsTheValue(t *testing.T) {
	m, stub := newAskApp(t)
	m.chatConvID = "c1"
	// The rail row is what fullsendOn() reads, so the shell must actually hold the
	// conversation — a test that stubbed the read would not exercise the real path.
	m.conversations = []chat.Conversation{{ID: "c1"}}

	handled, cmd := m.dispatchSlash("/fullsend")
	if !handled || cmd == nil {
		t.Fatal("/fullsend must dispatch a write when a conversation is open")
	}
	if mm, ok := cmd().(chat.ConversationMutatedMsg); !ok || mm.Err != "" {
		t.Fatalf("fullsend result = %#v", cmd())
	}
	if stub.fullsendID != "c1" || !stub.fullsendSet {
		t.Fatalf("turning it ON sent (%q, %v), want (c1, true)", stub.fullsendID, stub.fullsendSet)
	}
	if !strings.Contains(m.dock.View(), "FULLSEND ON") {
		t.Fatalf("turning it on must SAY so in the dock:\n%s", m.dock.View())
	}

	// Now the conversation row reports it ON (what the reload after the write produces), and
	// the second tap must send the OPPOSITE value rather than toggling a local flag.
	m.conversations = []chat.Conversation{{ID: "c1", Fullsend: true}}
	handled, cmd = m.dispatchSlash("/fullsend")
	if !handled || cmd == nil {
		t.Fatal("/fullsend must dispatch the off-write too")
	}
	if mm, ok := cmd().(chat.ConversationMutatedMsg); !ok || mm.Err != "" {
		t.Fatalf("fullsend result = %#v", cmd())
	}
	if stub.fullsendID != "c1" || stub.fullsendSet {
		t.Fatalf("turning it OFF sent (%q, %v), want (c1, false)", stub.fullsendID, stub.fullsendSet)
	}
	if !strings.Contains(m.dock.View(), "FULLSEND off") {
		t.Fatalf("turning it off must SAY so:\n%s", m.dock.View())
	}
}

// TestFullsendSlashRefusesWithNoConversation — there is deliberately no PENDING form.
//
// The conversation MODE had one, and the leak it caused (changing an open chat also changed
// what the NEXT one would be created with) is the reason this refuses instead. A permission
// bypass armed for a chat the operator has not opened is one they did not knowingly turn on.
func TestFullsendSlashRefusesWithNoConversation(t *testing.T) {
	m, stub := newAskApp(t)
	m.chatConvID = ""

	handled, cmd := m.dispatchSlash("/fullsend")
	if !handled {
		t.Fatal("/fullsend must be handled (refused, not sent as a chat message)")
	}
	if cmd != nil {
		t.Fatal("/fullsend with no conversation must NOT write anything")
	}
	if stub.fullsendID != "" {
		t.Fatalf("a write was issued for %q with no conversation open", stub.fullsendID)
	}
	if !strings.Contains(m.dock.View(), "no conversation open") {
		t.Fatalf("the refusal must say why:\n%s", m.dock.View())
	}
}

// TestFullsendBadgeIsSilentWhenOffAndLoudWhenOn — the indicator is the safety feature.
//
// OFF shows NOTHING (the mode is the ABSENCE of asking, so there is nothing to draw while
// the gate does its job), and ON shows a token the operator cannot miss. A permanent
// "FULLSEND OFF" would train the eye to ignore the field that must never be ignored.
func TestFullsendBadgeIsSilentWhenOffAndLoudWhenOn(t *testing.T) {
	m, _ := newAskApp(t)

	// No conversation open: off.
	if got := m.fullsendBadge(); got != "" {
		t.Fatalf("badge with no conversation = %q, want empty", got)
	}
	m.chatConvID = "c1"
	m.conversations = []chat.Conversation{{ID: "c1"}}
	m.syncComposerStats()
	if got := m.fullsendBadge(); got != "" {
		t.Fatalf("badge with fullsend OFF = %q, want empty", got)
	}
	if strings.Contains(m.dock.View(), "FULLSEND") {
		t.Fatalf("the composer must not mention FULLSEND while it is off:\n%s", m.dock.View())
	}

	// Now the server says it is on.
	m.conversations = []chat.Conversation{{ID: "c1", Fullsend: true}}
	m.syncComposerStats()
	if got := m.fullsendBadge(); got != "FULLSEND" {
		t.Fatalf("badge with fullsend ON = %q, want FULLSEND", got)
	}
	view := m.dock.View()
	if !strings.Contains(view, "FULLSEND") {
		t.Fatalf("the ON indicator must be rendered in the composer:\n%s", view)
	}
	// AND THE INDICATOR ALONE RESERVES THE ROW: fullsend is the only field set here, so if
	// StatsRows did not count it the badge would be drawn on a line the shell never
	// allocated — clipped away exactly when it matters most.
	if rows := m.dock.StatsRows(); rows != 1 {
		t.Fatalf("StatsRows = %d with only the badge set, want 1 — the row must be reserved", rows)
	}
}

// TestFullsendBadgeReadsTheServersStateNotTheClientsMemory — the freshness property.
//
// The indicator is read from the conversation row the SERVER computes, which is what lets a
// toggle made in the GUI show up here on the next rail reload. A client that remembered its
// own last write would render the gate as open after the GUI closed it.
func TestFullsendBadgeReadsTheServersStateNotTheClientsMemory(t *testing.T) {
	m, _ := newAskApp(t)
	m.chatConvID = "c1"
	m.conversations = []chat.Conversation{{ID: "c1", Fullsend: true}}
	if !m.fullsendOn() {
		t.Fatal("the badge did not follow the server's row")
	}
	// The other client turned it off; the rail reloads.
	m.conversations = []chat.Conversation{{ID: "c1", Fullsend: false}}
	if m.fullsendOn() {
		t.Fatal("the badge still reports ON after the server said off — the operator would read the gate as open")
	}
	// And a DIFFERENT conversation's mode does not leak in.
	m.chatConvID = "c2"
	if m.fullsendOn() {
		t.Fatal("fullsend leaked from another conversation's row")
	}
}

// TestFullsendIsInTheCommandSurface — discoverable, because a mode nobody can find is a mode
// nobody uses, and the palette is the documented way to find one.
func TestFullsendIsInTheCommandSurface(t *testing.T) {
	m := newTestApp()
	if m.slash == nil {
		t.Fatal("no slash registry")
	}
	cmd := m.slash.resolve("/fullsend")
	if cmd == nil {
		t.Fatal("/fullsend is not registered")
	}
	if cmd.Desc == "" {
		t.Fatal("/fullsend has no description for /help")
	}
	// The description must name the boundary: an operator who reads "allow everything" and
	// then finds their deny list still refusing has been misled about their own policy.
	if !strings.Contains(cmd.Desc, "deny") || !strings.Contains(cmd.Desc, "never-allow") {
		t.Fatalf("/fullsend's help must state what still refuses, got %q", cmd.Desc)
	}
	listed := false
	for _, line := range m.slash.helpLines() {
		if strings.Contains(line, "/fullsend") {
			listed = true
		}
	}
	if !listed {
		t.Fatal("/help does not list /fullsend")
	}
}

// TestFullsendIsToggleableWhileATurnIsBlockedOnACard — the operator's own report:
//
//	"I noticed when mid turn, it is greyed out in the GUI. Is that by design? Can it be
//	 enabled mid turn as well for TUI and GUI?"
//
// A PENDING CARD *IS* A TURN IN FLIGHT — the turn is blocked on the operator's answer — so
// this is the mid-turn case, and it is the case that matters: nobody decides to turn fullsend
// on before they start. You reach for it when you are already mid-task and being asked too
// often, which is precisely when the GUI's `disabled={isStreaming}` refused it.
//
// The backend has no objection (the consent layer reads the flag at EACH DECISION and the bash
// guard re-reads it per invocation, so a change lands on the next ask with no restart), and the
// TUI never had a guard — the disabled attribute was mine, copied by reflex from the MODEL
// picker beside it, which really cannot change mid-turn because the running session belongs to
// the model that opened it.
func TestFullsendIsToggleableWhileATurnIsBlockedOnACard(t *testing.T) {
	m, stub := newAskApp(t)
	m.chatConvID = "c1"
	m.conversations = []chat.Conversation{{ID: "c1"}}
	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "ask-1", Kind: chat.AskTool, Tool: "bash",
		Target: "make ci", Directory: "/p/proj",
	})
	if !m.chatStore.hasPendingConsent("c1") {
		t.Fatal("precondition: the card must be pending")
	}

	handled, cmd := m.dispatchSlash("/fullsend")
	if !handled || cmd == nil {
		t.Fatal("/fullsend must dispatch while a turn is blocked on a card")
	}
	if mm, ok := cmd().(chat.ConversationMutatedMsg); !ok || mm.Err != "" {
		t.Fatalf("fullsend result = %#v", cmd())
	}
	if stub.fullsendID != "c1" || !stub.fullsendSet {
		t.Fatalf("sent (%q, %v), want (c1, true)", stub.fullsendID, stub.fullsendSet)
	}

	// AND THE OPERATOR IS TOLD THE CARD IS STILL THEIRS. A mode that appears to do nothing
	// reads as broken, and auto-answering a card they have already been shown would be a
	// consent decision the toggle did not make — the silent escalation this codebase refuses
	// everywhere else. One tap clears the card; every ask after it is skipped.
	if !strings.Contains(m.dock.View(), "still yours to answer") {
		t.Fatalf("the notice must say the pending card is still open:\n%s", m.dock.View())
	}
}

// TestFullsendOnNoticeWithNothingPending is the other branch: the plain ON notice, which must
// still name what the mode does NOT cover (the deny list and the never-allow class).
func TestFullsendOnNoticeWithNothingPending(t *testing.T) {
	m, _ := newAskApp(t)
	m.chatConvID = "c1"
	m.conversations = []chat.Conversation{{ID: "c1"}}

	handled, cmd := m.dispatchSlash("/fullsend")
	if !handled || cmd == nil {
		t.Fatal("/fullsend must dispatch")
	}
	notice := m.dock.View()
	if !strings.Contains(notice, "FULLSEND ON") {
		t.Fatalf("the ON notice is missing:\n%s", notice)
	}
	if strings.Contains(notice, "still yours to answer") {
		t.Fatalf("with no card pending there is nothing to answer:\n%s", notice)
	}
	if !strings.Contains(notice, "deny") || !strings.Contains(notice, "sudo") {
		t.Fatalf("the notice must state what still refuses:\n%s", notice)
	}
}
