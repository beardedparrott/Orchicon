package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// asksWithRail builds an Ask shell with a loaded conversations rail, which is the state the operator
// is in when they want to rename something they can see.
func asksWithRail(t *testing.T, titles ...string) (*App, *stubAskParity) {
	t.Helper()
	m, stub := newAskApp(t)
	m.active = TabAsk
	m.askMode = askConversations
	m.chatConvID = "c1"
	m.convRailOpen = true
	m.width, m.height = 120, 40
	m.conversations = nil
	for i, title := range titles {
		m.conversations = append(m.conversations, chat.Conversation{ID: "c" + string(rune('1'+i)), Title: title})
	}
	m.convSel = 0
	return m, stub
}

// TestRenameOpensAPrefilledForm is the operator's report — "We can't rename conversations in the TUI."
//
// A rename box that opened EMPTY would make the operator retype a title they cannot see, which is why
// the GUI prefills (startRenameConv selects the existing title). The RPC itself was already wired and
// reachable by `/rename <title>`, but a command you have to know, which cannot show you the current
// title, is not a rename UI.
func TestRenameOpensAPrefilledForm(t *testing.T) {
	m, _ := asksWithRail(t, "TUI FAILED COMPACT", "another test")
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlN})
	m = nm.(*App)

	if m.renameConv == nil {
		t.Fatal("ctrl+n on the rail must open the rename form")
	}
	if got := m.renameConv.Values["title"]; got != "TUI FAILED COMPACT" {
		t.Fatalf("the form must be PREFILLED with the current title, got %q", got)
	}
	if m.renameConvID != "c1" {
		t.Fatalf("the form must target the SELECTED conversation, got %q", m.renameConvID)
	}
	// The modal owns the keyboard, so the composer must say what saves.
	if !strings.Contains(m.dock.View(), "ctrl+s") {
		t.Fatalf("the composer must advertise the form's save chord, got %q", m.dock.View())
	}
}

// TestRenameSavesThroughTheRealKeys presses the actual keys — edit, then ctrl+s — and runs the command
// that comes back, rather than driving the form's own methods. The distinction matters: a test that
// sets a value and calls Submit proves the form works and proves NOTHING about whether ctrl+s reaches
// it, which is the class of gap this session has already been bitten by.
func TestRenameSavesThroughTheRealKeys(t *testing.T) {
	m, stub := asksWithRail(t, "old title")
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlN})
	m = nm.(*App)

	// Clear the prefilled value (ctrl+u clears a field) and type a new one.
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	m = nm.(*App)
	nm, _ = m.Update(keyRunes("renamed by hand"))
	m = nm.(*App)

	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = nm.(*App)

	if m.renameConv != nil {
		t.Fatal("a successful save must close the form")
	}
	if cmd == nil {
		t.Fatal("ctrl+s must produce the rename command")
	}
	if m.renameConvID != "" {
		t.Fatalf("the modal's target must be cleared on close, got %q", m.renameConvID)
	}
	// The command carries the RPC, and RUNNING it is what proves the call and its payload — asserting
	// on the message shape would pass even if the title never left the process.
	msg := cmd()
	mut, ok := msg.(chat.ConversationMutatedMsg)
	if !ok {
		t.Fatalf("expected a conversation mutation, got %T", msg)
	}
	if mut.Op != "rename" || mut.ID != "c1" || mut.Err != "" {
		t.Fatalf("wrong mutation: %+v", mut)
	}
	if stub.renamedID != "c1" || stub.renamedTitle != "renamed by hand" {
		t.Fatalf("UpdateConversationTitle got (%q, %q)", stub.renamedID, stub.renamedTitle)
	}
}

// TestRenameRefusesAnEmptyTitleWithoutClosing: a save that closes the form and writes nothing is the
// silent-rejection class. The trimmed value is required, and the reason must be SHOWN.
func TestRenameRefusesAnEmptyTitleWithoutClosing(t *testing.T) {
	m, _ := asksWithRail(t, "keep me")
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlN})
	m = nm.(*App)
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlU}) // clear
	m = nm.(*App)

	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = nm.(*App)

	if m.renameConv == nil {
		t.Fatal("an invalid save must NOT close the form — the operator would see nothing happen")
	}
	if cmd != nil {
		t.Fatal("an invalid save must not issue a write")
	}
	// The reason is the FIELD's own error, which the form draws beside the title — not SubmitErr,
	// which exists for a rejected OnSubmit (a server refusal) rather than a validation failure.
	if got := m.renameConv.Errors["title"]; got == "" {
		t.Fatal("the refusal must state a reason next to the field")
	}
}

// TestRenameBlankTitleIsRejectedEvenWhenPaddedBySpaces: the check is on the TRIMMED value, so "   "
// cannot become a title of three spaces — which the rail would then render as a blank row.
func TestRenameBlankTitleIsRejectedEvenWhenPaddedBySpaces(t *testing.T) {
	m, _ := asksWithRail(t, "keep me")
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlN})
	m = nm.(*App)
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	m = nm.(*App)
	nm, _ = m.Update(keyRunes("   "))
	m = nm.(*App)
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = nm.(*App)

	if m.renameConv == nil || cmd != nil {
		t.Fatal("a spaces-only title must be refused, with the form left open")
	}
}

// TestRenameUnchangedIsANoOp: the GUI skips the write when the title is unchanged. A save that looks
// like it worked while writing nothing is fine; a write on every save is noise in the audit trail.
func TestRenameUnchangedIsANoOp(t *testing.T) {
	m, _ := asksWithRail(t, "same as before")
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlN})
	m = nm.(*App)
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = nm.(*App)

	if m.renameConv != nil {
		t.Fatal("saving an unchanged title must still close the form")
	}
	if cmd != nil {
		t.Fatalf("an unchanged title must not issue a write, got %v", cmd)
	}
}

// TestRenameEscCancels: esc closes without writing, and clears the target so a later ctrl+s cannot
// write to a conversation the operator has stopped looking at.
func TestRenameEscCancels(t *testing.T) {
	m, _ := asksWithRail(t, "untouched")
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlN})
	m = nm.(*App)
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = nm.(*App)

	if m.renameConv != nil || m.renameConvID != "" {
		t.Fatal("esc must close the form and forget its target")
	}
	if cmd != nil {
		t.Fatal("esc must not write")
	}
	// And the composer's hint goes back to the screen's own.
	if strings.Contains(m.dock.View(), "ctrl+s: save") {
		t.Fatalf("the form's hint outlived the form: %q", m.dock.View())
	}
}

// TestRenameModalOwnsItsKeys: while the form is up, a keystroke aimed at it must not reach the composer
// behind it — otherwise typing a title could also be composing a message, and a save chord could fire
// into a screen.
func TestRenameModalOwnsItsKeys(t *testing.T) {
	m, _ := asksWithRail(t, "target")
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlN})
	m = nm.(*App)
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	m = nm.(*App)
	nm, _ = m.Update(keyRunes("xyz"))
	m = nm.(*App)

	if got := m.dock.Value(); got != "" {
		t.Fatalf("typing in the rename form leaked into the composer: %q", got)
	}
	if got := m.renameConv.Values["title"]; got != "xyz" {
		t.Fatalf("the form did not receive the typing: %q", got)
	}
}

// TestSlashRenameWithNoTitleOpensTheForm: the palette is the documented command surface, so `/rename`
// must be a real route to the box rather than a command that silently needs an argument.
func TestSlashRenameWithNoTitleOpensTheForm(t *testing.T) {
	m, _ := asksWithRail(t, "from the palette")
	handled, cmd := m.dispatchSlash("/rename")
	if !handled {
		t.Fatal("/rename must be handled")
	}
	if cmd != nil {
		t.Fatalf("opening the form writes nothing, got %v", cmd)
	}
	if m.renameConv == nil {
		t.Fatal("/rename with no title must open the prefilled form")
	}
	if got := m.renameConv.Values["title"]; got != "from the palette" {
		t.Fatalf("the form must prefill from the rail selection, got %q", got)
	}
}

// TestSlashRenameWithATitleStillWritesDirectly keeps the delivered behaviour: the one-shot form was
// tested and is useful for scripts and muscle memory.
func TestSlashRenameWithATitleStillWritesDirectly(t *testing.T) {
	m, _ := asksWithRail(t, "old")
	handled, cmd := m.dispatchSlash("/rename a new title")
	if !handled || cmd == nil {
		t.Fatalf("/rename <title> must still write directly (handled=%v cmd=%v)", handled, cmd)
	}
	if m.renameConv != nil {
		t.Fatal("/rename <title> must NOT open a form")
	}
	msg := cmd()
	if mut, ok := msg.(chat.ConversationMutatedMsg); !ok || mut.ID != "c1" {
		t.Fatalf("wrong mutation: %#v", msg)
	}
}

// TestRenameChordNeedsTheRail: ctrl+n is the RAIL's action, so it must do nothing when the rail is not
// on screen — otherwise it would be a hidden global chord with no visible target.
func TestRenameChordNeedsTheRail(t *testing.T) {
	m := newDockTestApp()
	m.convRailOpen = false
	m.active = TabWork
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlN})
	m = nm.(*App)
	if m.renameConv != nil {
		t.Fatal("ctrl+n must not open a rename form with no rail on screen")
	}
}

// TestRailAdvertisesTheRenameChord: a chord nobody can see is the same as no chord, and the rail is
// exactly where the operator is looking when they want this.
func TestRailAdvertisesTheRenameChord(t *testing.T) {
	m, _ := asksWithRail(t, "one", "two")
	if !strings.Contains(m.rightRailView(), "ctrl+n") {
		t.Fatalf("the rail must advertise its own rename chord:\n%s", m.rightRailView())
	}
}

// TestHelpOverlayNamesTheRenameChord keeps the overlay honest — it lists what exists.
func TestHelpOverlayNamesTheRenameChord(t *testing.T) {
	m, _ := asksWithRail(t, "one")
	joined := strings.Join(helpLines(m.routes), "\n")
	if !strings.Contains(joined, "ctrl+n") {
		t.Fatalf("the help overlay must name the rename chord:\n%s", joined)
	}
}
