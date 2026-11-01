package tui

// msgmenu_test.go — THE MENU'S DECISION PATH, INCLUDING THE TWO CONFIRMATIONS.
//
// The operator asked for "right click for the TUI to bring up a popup menu to choose your option", with the two
// verbs they named. This is the only DESTRUCTIVE gesture in the transcript, so what matters is not that the menu
// renders but that:
//
//   - the two entries send the two DIFFERENT delete modes (an inverted mapping would delete a conversation's
//     tail when the operator asked to forget one message);
//   - nothing happens on the first Enter (a right-click plus a stray Enter must not destroy history);
//   - Escape backs out of an armed action instead of closing the menu, and only closes once unarmed;
//   - the keys are swallowed, so a character aimed at the menu never reaches the composer.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/dock"
)

func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// m0 is a minimal App for building the slash registry. buildSlashRegistry reads screens and routes off the App,
// so it cannot be handed nil.
func m0() *App { return &App{} }

func openMenuForTest() *App {
	m := &App{}
	m.openMsgMenu("conv1", "01MSG", "the question")
	return m
}

func TestTheMenuOffersBothVerbsWithTheirRealScopes(t *testing.T) {
	if len(msgMenuEntries) != 2 {
		t.Fatalf("menu has %d entries, want 2 (undo, revert)", len(msgMenuEntries))
	}
	if msgMenuEntries[0].Mode != chat.MessageDeleteOnly {
		t.Errorf("entry 0 is %v, want the SINGLE-message mode — undo must not truncate", msgMenuEntries[0].Mode)
	}
	if msgMenuEntries[1].Mode != chat.MessageDeleteAndFollowing {
		t.Errorf("entry 1 is %v, want the TRUNCATING mode", msgMenuEntries[1].Mode)
	}
	// The scope is on the row, because the choice IS the scope.
	for _, e := range msgMenuEntries {
		if strings.TrimSpace(e.Detail) == "" {
			t.Errorf("entry %q does not state what it removes", e.Label)
		}
	}
}

// TestTheFirstEnterOnlyArms — the guard against a right-click plus a stray Enter.
func TestTheFirstEnterOnlyArms(t *testing.T) {
	m := openMenuForTest()
	cmd := m.handleMsgMenuKey(key("enter"))
	if m.msgMenu == nil {
		t.Fatal("the first Enter closed the menu — it must ARM the action and wait for confirmation")
	}
	if m.msgMenu.armed == nil {
		t.Fatal("the first Enter did not arm anything")
	}
	if cmd != nil {
		t.Error("the first Enter produced a command — nothing may be deleted before the second Enter")
	}
	// Nothing was dispatched: no conversation client is configured, and a command here would have called one.
}

func TestTheSecondEnterDispatchesTheArmedMode(t *testing.T) {
	m := openMenuForTest()
	// Choose REVERT: down, enter (arm), enter (confirm).
	m.handleMsgMenuKey(key("down"))
	m.handleMsgMenuKey(key("enter"))
	if m.msgMenu == nil || m.msgMenu.armed == nil {
		t.Fatal("the entry was not armed")
	}
	if got := msgMenuEntries[*m.msgMenu.armed].Mode; got != chat.MessageDeleteAndFollowing {
		t.Fatalf("armed %v, want the truncating mode", got)
	}
	// The dispatch itself needs a controller; what is asserted here is that the SECOND Enter closes the menu
	// and produces a command only when a controller exists. With none, it must not panic.
	if cmd := m.handleMsgMenuKey(key("enter")); cmd != nil && m.chat == nil {
		t.Error("a command was produced with no controller")
	}
	if m.msgMenu != nil {
		t.Error("the menu stayed open after the confirming Enter")
	}
}

func TestEscapeBacksOutOfTheArmedStepBeforeClosing(t *testing.T) {
	m := openMenuForTest()
	m.handleMsgMenuKey(key("enter"))
	if m.msgMenu == nil || m.msgMenu.armed == nil {
		t.Fatal("not armed")
	}
	m.handleMsgMenuKey(key("esc"))
	if m.msgMenu == nil {
		t.Fatal("Escape closed the menu while an action was armed — the operator loses the menu and has to " +
			"re-open it to cancel properly")
	}
	if m.msgMenu.armed != nil {
		t.Error("Escape did not disarm")
	}
	m.handleMsgMenuKey(key("esc"))
	if m.msgMenu != nil {
		t.Error("a second Escape did not close the menu")
	}
}

// TestTheMenuSwallowsKeys — a key aimed at the menu must never reach the composer.
func TestTheMenuSwallowsKeys(t *testing.T) {
	m := openMenuForTest()
	if !m.msgMenuBlocksKeys() {
		t.Fatal("the menu does not claim the keyboard, so a character would be typed into the composer while a " +
			"destructive action is armed")
	}
	before := m.msgMenu.sel
	m.handleMsgMenuKey(key("x"))
	if m.msgMenu == nil {
		t.Error("an ordinary rune closed the menu")
	}
	if m.msgMenu.sel != before {
		t.Error("an ordinary rune changed the selection")
	}
}

// TestTheSelectionWraps — two entries, and the arrows must not fall off the end.
func TestTheSelectionWraps(t *testing.T) {
	m := openMenuForTest()
	m.handleMsgMenuKey(key("up"))
	if m.msgMenu.sel != 1 {
		t.Errorf("up from 0 gave %d, want 1 (wrap)", m.msgMenu.sel)
	}
	m.handleMsgMenuKey(key("down"))
	if m.msgMenu.sel != 0 {
		t.Errorf("down from 1 gave %d, want 0 (wrap)", m.msgMenu.sel)
	}
}

// TestOpeningRequiresAnID — a span with no durable id cannot be edited, and the guard lives in the opener so a
// caller cannot build a menu that removes "some message".
func TestOpeningRequiresAnID(t *testing.T) {
	m := &App{}
	m.openMsgMenu("conv1", "", "text")
	if m.msgMenu != nil {
		t.Error("a menu opened with no message id")
	}
	m.openMsgMenu("", "01MSG", "text")
	if m.msgMenu != nil {
		t.Error("a menu opened with no conversation id")
	}
}

var _ = apiv1.MessageDeleteMode_MESSAGE_DELETE_MODE_ONLY

// TestTheSlashCommandsArmTheSameMenu — one destructive path, reached two ways.
//
// The operator asked for "a slash command or two" as well as the mouse gesture. A command that executed its
// deletion on Enter would be a SECOND path to an irreversible act, and the second path is the one that forgets
// the confirmation — so /undo and /revert open the menu with the matching entry armed instead.
func TestTheSlashCommandsArmTheSameMenu(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		mode apiv1.MessageDeleteMode
	}{
		{"/undo", chat.MessageDeleteOnly},
		{"/revert", chat.MessageDeleteAndFollowing},
	} {
		reg := buildSlashRegistry(m0())
		c := reg.resolve(tc.cmd)
		if c == nil {
			t.Fatalf("%s is not registered, so the palette would tell the operator it does not exist", tc.cmd)
		}
		m := &App{}
		m.chatConvID = "conv1"
		// A transcript with one of the operator's own messages in it.
		m.transcriptSpans = map[string][]chat.ItemSpan{
			"conv1": {
				{Kind: chat.KindUser, Text: "the question", MessageID: "01MSG", Line: 0, Lines: 1},
				{Kind: chat.KindText, Text: "the reply", Line: 1, Lines: 1},
			},
		}
		if cmd := c.Run(m, nil); cmd != nil {
			t.Errorf("%s produced a command on invocation — it must only OPEN the menu", tc.cmd)
		}
		if m.msgMenu == nil {
			t.Fatalf("%s did not open the menu", tc.cmd)
		}
		if m.msgMenu.messageID != "01MSG" {
			t.Errorf("%s targeted message %q, want the operator's own 01MSG", tc.cmd, m.msgMenu.messageID)
		}
		if m.msgMenu.armed == nil {
			t.Fatalf("%s opened the menu UNARMED — the operator gets no pre-selection, and the command "+
				"added nothing over right-clicking", tc.cmd)
		}
		if got := msgMenuEntries[*m.msgMenu.armed].Mode; got != tc.mode {
			t.Errorf("%s armed %v, want %v", tc.cmd, got, tc.mode)
		}
	}
}

// TestASlashCommandWithNoMessageSaysSo — the empty case must not open an empty menu.
func TestASlashCommandWithNoMessageSaysSo(t *testing.T) {
	reg := buildSlashRegistry(m0())
	c := reg.resolve("/undo")
	if c == nil {
		t.Fatal("/undo is not registered")
	}
	m := &App{}
	m.chatConvID = "conv1"
	m.transcriptSpans = map[string][]chat.ItemSpan{
		"conv1": {{Kind: chat.KindText, Text: "only a reply", Line: 0, Lines: 1}},
	}
	if cmd := c.Run(m, nil); cmd != nil {
		t.Error("a command with nothing to act on produced a command")
	}
	if m.msgMenu != nil {
		t.Error("the menu opened with no message to edit")
	}
}

// TestRevertHandsTheMessageBackToTheComposer — the operator's addition to the feature.
//
// "a revert should also put the message you selected back into the composer". Revert REMOVES the message, so
// without this its text is gone from the interface and the operator has to retype a question they had already
// written — the one cost that removing it should not carry.
//
// The command is NOT executed here: it would call a nil client. What is asserted is the observable effect that
// matters — the composer holds the message by the time the delete is dispatched.
func TestRevertHandsTheMessageBackToTheComposer(t *testing.T) {
	m := &App{}
	m.chatConvID = "conv1"
	m.chat = chat.NewController(nil)
	// A REAL dock: a zero dock.Model panics on SetValue, and the composer is the thing under test here.
	m.dock = dock.New()
	m.openMsgMenu("conv1", "01MSG", "what is the weather in Paris?")
	// Choose Revert: down, then arm, then confirm.
	m.handleMsgMenuKey(key("down"))
	m.handleMsgMenuKey(key("enter"))
	if m.msgMenu == nil || m.msgMenu.armed == nil {
		t.Fatal("revert was not armed")
	}
	if got := msgMenuEntries[*m.msgMenu.armed].Mode; got != chat.MessageDeleteAndFollowing {
		t.Fatalf("armed %v, want the truncating mode", got)
	}
	_ = m.handleMsgMenuKey(key("enter"))
	if got := m.dock.Value(); got != "what is the weather in Paris?" {
		t.Errorf("the composer holds %q, want the reverted message — a revert must hand the text back so it can "+
			"be re-sent without retyping it", got)
	}
}

// TestUndoLeavesTheComposerAlone — the other half of the rule.
//
// Undo keeps the message's REPLY, so restoring the question would leave the transcript reading as though it had
// been asked and answered while the composer offered it again. The asymmetry is deliberate.
func TestUndoLeavesTheComposerAlone(t *testing.T) {
	m := &App{}
	m.chatConvID = "conv1"
	m.chat = chat.NewController(nil)
	m.dock = dock.New()
	m.dock.SetValue("something I was typing")
	m.openMsgMenu("conv1", "01MSG", "an older question")
	// Undo is entry 0, so no arrow keys.
	m.handleMsgMenuKey(key("enter"))
	_ = m.handleMsgMenuKey(key("enter"))
	if got := m.dock.Value(); got != "something I was typing" {
		t.Errorf("undo changed the composer to %q — it must not touch it, and must not clobber a draft the "+
			"operator is part-way through", got)
	}
}
