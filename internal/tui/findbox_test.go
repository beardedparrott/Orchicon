package tui

// findbox_test.go — THE FIND BOX (ctrl+f), and the transcript's navigation keys from the composer.
//
// ONE REPORT, TWO HALVES, ONE CAUSE. The operator: "In the TUI the new home/end keys are not jumping to
// the top or bottom of a conversation. Also, no note in the composer about search or jumping to beginning
// to end is there so no one would know they exist. Also ctrl+f works odd. You have to first type what you
// want then hit ctrl+f. That's not very intuitive. I would prefer a modal pop up that you can type in your
// search."
//
// The cause is shared, which is why these are tested together: the Ask pane's keyboard navigation was
// reachable only from CONTENT focus, and the operator sits in the COMPOSER — NewApp makes it the launch
// focus, because that is where you type. There, Home/End/arrows are pure no-ops in the textarea YET the
// textarea consumes every key it is handed, so the shell's routes never saw them; and ctrl+f had no box to
// open, so it took the composer's text as the query — useless from an empty composer, which is the state
// you are in while READING a transcript and the only state a find is for.
//
// SO EVERY TEST BELOW STARTS FROM THE COMPOSER, the seat the bug lived in. A test that started from the
// content would have passed all along and proved nothing.

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// findFixture opens a conversation with a searchable transcript, from the operator's seat: an empty
// composer with the conversation selected (which is what OpenAskConversation leaves behind).
func findFixture(t *testing.T) *App {
	t.Helper()
	m := askRelaunched(t, "c1")
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindUser, Text: "the needle is here", Key: "u1", At: 1})
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindText, Text: "unrelated chatter", Key: "t1", At: 2})
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindText, Text: "another needle", Key: "t2", At: 3})
	m.active = TabAsk
	m.setFocus(focusComposer)
	return m
}

// longTranscript parks a LONG transcript mid-scroll, so a jump has somewhere to go in BOTH directions and
// neither assertion below can pass by accident. It returns the stream the assertions read (the app's own
// map holds the same pointer).
func longTranscript(t *testing.T, m *App) *kit2.Stream {
	t.Helper()
	str := kit2.NewStream("transcript", 120, 10)
	lines := make([]string, 80)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i)
	}
	str.SetLines(lines)
	m.chatStreams[m.chatConvID] = str
	str.Offset = 35
	str.Wheel(0) // 35 is above the tail, so this is a deliberate scroll: following goes false
	if str.AtBottom() || str.Following() {
		t.Fatal("fixture: the transcript must be parked away from the tail")
	}
	return str
}

var ctrlFKey = tea.KeyMsg{Type: tea.KeyCtrlF}
var escKey = tea.KeyMsg{Type: tea.KeyEsc}
var homeKey = tea.KeyMsg{Type: tea.KeyHome}
var endKey = tea.KeyMsg{Type: tea.KeyEnd}
var pgdownKey = tea.KeyMsg{Type: tea.KeyPgDown}

// ---------------------------------------------------------------- the find box

// CTRL+F OPENS A BOX YOU TYPE INTO — from either focus, with an empty box, and without touching the draft.
//
// The operator's complaint is about the ORDER ("you have to first type what you want then hit ctrl+f"), so
// "the box opens EMPTY" is the assertion that the order is gone. It is also the assertion that a
// half-written message can no longer become a search term.
func TestCtrlFOpensAnEmptyFindBoxAndLeavesTheDraftAlone(t *testing.T) {
	for _, focus := range []focusMode{focusContent, focusComposer} {
		focus := focus
		t.Run(map[focusMode]string{focusContent: "content-focus", focusComposer: "composer-focus"}[focus], func(t *testing.T) {
			m := findFixture(t)
			m.dock.SetValue("a half-written message")
			m.setFocus(focus)

			m = press(m, ctrlFKey)
			if m.findBox == nil {
				t.Fatal("ctrl+f must open the find box — opening it is the chord's whole job")
			}
			if got := m.findBox.input.Value(); got != "" {
				t.Errorf("the box must open EMPTY, got %q. Seeding it from the composer is exactly the "+
					"type-it-first order the operator called unintuitive, and it silently turns a message "+
					"into a search term", got)
			}
			if got := m.dock.Value(); got != "a half-written message" {
				t.Errorf("the box must not touch the composer's draft, got %q", got)
			}
			if m.askSearch != "" {
				t.Errorf("opening the box must not start a search by itself, got %q", m.askSearch)
			}
		})
	}
}

// THE BOX FILTERS LIVE, through the same functions /find and n/N use — so what the operator reads in the
// box is the count of a search that is already applied to the transcript behind it.
func TestFindBoxFiltersTheTranscriptAsYouType(t *testing.T) {
	m := findFixture(t)
	m = press(m, ctrlFKey)
	m = typeInto(t, m, "needle")

	if m.askSearch != "needle" {
		t.Fatalf("typing must apply the search as you go, got %q", m.askSearch)
	}
	matches, total := transcriptSearchCount(m.chatStore.snapshot("c1"), "needle")
	if matches != 2 || total != 3 {
		t.Fatalf("fixture: want 2 matches of 3, got %d/%d", matches, total)
	}
	if got := m.searchVisibleItems(m.chatStore.snapshot("c1")); len(got) != 2 {
		t.Errorf("the transcript behind the box must be filtered to the matches, got %d items", len(got))
	}
	if line := m.findBoxCountLine(); !strings.Contains(line, "2/3") {
		t.Errorf("the box's own count must report 2/3, got %q", line)
	}
}

// ENTER KEEPS THE SEARCH AND HANDS OVER THE KEYBOARD — and the focus move is asserted by USING it, because
// the move only means something if the chords it unlocks actually work from where it lands.
func TestFindBoxEnterKeepsTheSearchAndHandsOverTheKeyboard(t *testing.T) {
	m := findFixture(t)
	m = press(m, ctrlFKey)
	m = typeInto(t, m, "needle")
	m = press(m, endKey) // stepping is the box's own key: it must move the current match, not the transcript

	m = press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.findBox != nil {
		t.Fatal("enter must close the box")
	}
	if m.askSearch != "needle" {
		t.Fatalf("enter must KEEP the search, got %q — esc is the key that abandons it", m.askSearch)
	}
	if m.chatFocus != focusContent {
		t.Fatal("enter must hand the keyboard to the transcript. n/N step matches and home/end jump, and " +
			"none of them can work from the composer — a bare letter in the composer is the operator's message")
	}
	if m.askPane != askPaneConversation {
		t.Fatal("enter must select the conversation pane, or the arrows would still belong to the rail")
	}

	// THE FOCUS MOVE IS WHAT THIS BUYS, measured: the step chord now works.
	before := m.askMatchIdx
	m = press(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if m.askMatchIdx == before {
		t.Fatalf("after committing, 'n' must step to the next match (still at %d)", before)
	}
}

// ESC CANCELS, WHICH MEANS RESTORES — both when there was no search before the box opened (the common
// case, and the one a "cancel" that kept the filter would get wrong) and when there WAS one.
func TestFindBoxEscRestoresTheSearchItFound(t *testing.T) {
	t.Run("no search on the way in", func(t *testing.T) {
		m := findFixture(t)
		m = press(m, ctrlFKey)
		m = typeInto(t, m, "needle")
		if m.askSearch != "needle" {
			t.Fatal("fixture: the box must have applied its query")
		}

		m = press(m, escKey)
		if m.findBox != nil {
			t.Fatal("esc must close the box")
		}
		if m.askSearch != "" {
			t.Fatalf("cancelling a box that found NO search must leave the transcript unfiltered, got %q — "+
				"a live filter that could only be accepted would make looking a commitment", m.askSearch)
		}
		if got := m.searchVisibleItems(m.chatStore.snapshot("c1")); len(got) != 3 {
			t.Errorf("every message must be back after the cancel, got %d", len(got))
		}
	})

	t.Run("a search already running", func(t *testing.T) {
		m := findFixture(t)
		m.setAskSearch("chatter")

		m = press(m, ctrlFKey)
		if got := m.findBox.input.Value(); got != "chatter" {
			t.Fatalf("re-opening the box must seed it with the ACTIVE search — that is what makes ctrl+f a "+
				"refinement gesture instead of a retype — got %q", got)
		}
		m = typeInto(t, m, "x") // really change it, so the restore has something to undo
		if m.askSearch == "chatter" {
			t.Fatal("fixture: typing must have changed the live search")
		}

		m = press(m, escKey)
		if m.askSearch != "chatter" {
			t.Fatalf("esc must put back the search the box found, got %q", m.askSearch)
		}
	})
}

// THE BOX TAKES LETTERS THAT ARE CHORDS EVERYWHERE ELSE. This is why its key claim sits AHEAD of every
// route and of the composer: a find box that quits the shell when you search for "query" is not a find box.
func TestFindBoxTakesLettersThatAreChordsElsewhere(t *testing.T) {
	m := findFixture(t)
	m = press(m, ctrlFKey)

	m = typeInto(t, m, "q")
	if m.quitting {
		t.Fatal("'q' in a find term is a CHARACTER — it must not quit the shell")
	}
	if m.findBox == nil {
		t.Fatal("the box must still be open")
	}
	if got := m.findBox.input.Value(); got != "q" {
		t.Fatalf("the letter must reach the query, got %q", got)
	}

	m = typeInto(t, m, "/")
	if m.palette.PaletteOpen() {
		t.Error("'/' typed into the box is a character, not the slash palette — the box is text, and a " +
			"palette over a query would swallow the term being typed")
	}
}

// A MODAL MUST HIDE A SURFACE, NOT SUSPEND THE SHELL. A turn can be streaming behind the box, and the box
// filters LIVE, so the transcript must keep updating under it — if the box ate the waiter's messages the
// transcript (and the box's own count) would freeze for the rest of the session.
func TestFindBoxDoesNotSuspendTheSession(t *testing.T) {
	m := findFixture(t)
	m = press(m, ctrlFKey)
	m.chatStore.append("c1", chat.ChatItem{
		Kind: chat.KindText, Text: "LIVE-CHUNK-BEHIND-THE-BOX", Key: "t9", Live: true, At: 9,
	})

	nm, _ := m.Update(refreshTickMsg{gen: m.refreshGen})
	m = nm.(*App)

	if m.findBox == nil {
		t.Fatal("a non-key message must not close the box")
	}
	str := m.TranscriptStream("c1")
	if str == nil || !strings.Contains(str.View(), "LIVE-CHUNK-BEHIND-THE-BOX") {
		t.Error("the transcript behind the box did not repaint: a modal that swallows async messages leaves " +
			"the surface frozen — see the conversation-scope modal's note")
	}
}

// CTRL+G IS A HARD ESCAPE AND MUST STILL WORK with the box up: it dismisses whatever holds the keys and
// returns to the composer. It leaves the box (cancel), so it must not also keep the query it was previewing.
func TestFindBoxCtrlGClosesItAndReturnsToTheComposer(t *testing.T) {
	m := findFixture(t)
	m.setFocus(focusContent)
	m = press(m, ctrlFKey)
	m = typeInto(t, m, "needle")

	m = press(m, tea.KeyMsg{Type: tea.KeyCtrlG})
	if m.findBox != nil {
		t.Fatal("ctrl+g must dismiss the box — it is documented as leaving whatever holds the keys")
	}
	if m.askSearch != "" {
		t.Error("ctrl+g leaves WITHOUT committing, so the previewed query must be put back (see findBoxKey)")
	}
	if m.chatFocus != focusComposer {
		t.Fatal("ctrl+g must end with the composer focused: a box still holding the keyboard while the " +
			"composer has the caret is the split-keyboard state the chord exists to end")
	}
}

// NOTHING TO SEARCH IS SAID, NOT SILENTLY OPENED. A box filtering an empty conversation would filter
// nothing and look broken.
func TestCtrlFWithoutAConversationSaysSoInsteadOfOpeningABox(t *testing.T) {
	m := findFixture(t)
	m.chatConvID = ""

	m = press(m, ctrlFKey)
	if m.findBox != nil {
		t.Fatal("there is no open conversation, so there is nothing a find box could search")
	}
	if !strings.Contains(m.dock.Err, "nothing to search") {
		t.Errorf("the chord must name its own reason, got %q — a chord that silently does nothing is "+
			"indistinguishable from one that is not bound", m.dock.Err)
	}
}

// ---------------------------------------------------------------- the navigation keys

// HOME/END JUMP FROM THE COMPOSER — the operator's report, and the whole point of the change.
//
// It starts where they were: the composer, empty, with the conversation selected. Before the fix every one
// of these keys left the offset at 35 (measured), because an empty textarea still consumes Home/End/arrows.
func TestHomeEndJumpTheTranscriptFromTheComposer(t *testing.T) {
	m := findFixture(t)
	str := longTranscript(t, m)
	if m.chatFocus != focusComposer || !m.dockEmpty() || m.askPane != askPaneConversation {
		t.Fatalf("fixture: the operator's seat is an EMPTY composer with the conversation selected "+
			"(focus=%v empty=%v pane=%v)", m.chatFocus, m.dockEmpty(), m.askPane)
	}

	m = press(m, homeKey)
	if str.Offset != 0 || str.Following() {
		t.Errorf("home from the composer must jump to the FIRST message: offset=%d following=%v",
			str.Offset, str.Following())
	}
	m = press(m, endKey)
	if !str.AtBottom() || !str.Following() {
		t.Errorf("end from the composer must jump to the LAST message and resume following the tail: "+
			"offset=%d following=%v", str.Offset, str.Following())
	}
}

// THE DRAFT KEEPS HOME/END, and this is the contrast that stops the fix from being "make them global".
// With text in the box they are the textarea's line-start/line-end — and home is the only line-start chord
// a draft has left, because the shell took ctrl+a for select-all.
func TestHomeEndStayWithTheDraftOnceThereIsOne(t *testing.T) {
	m := findFixture(t)
	str := longTranscript(t, m)
	m.dock.SetValue("a draft I am editing")
	before := str.Offset

	m = press(m, homeKey)
	if str.Offset != before {
		t.Errorf("home must NOT move the transcript while a draft is being written (offset %d -> %d): there "+
			"it is the textarea's line-start", before, str.Offset)
	}
	if got := m.dock.Value(); got != "a draft I am editing" {
		t.Errorf("the draft must be intact, got %q", got)
	}
}

// THE ARROWS COME WITH IT, because the rule is one rule: an empty composer has nothing to edit, so the
// pane the operator selected owns the navigation keys. The branch already applied it to the RAIL ("empty
// composer: the vertical keys drive the conversation list ... otherwise they scroll the active pane's
// detail"); it had simply never been finished for the conversation, so an empty composer with the
// conversation selected was a dead keyboard for every navigation key.
func TestTranscriptScrollKeysReachTheConversationFromTheComposer(t *testing.T) {
	m := findFixture(t)
	str := longTranscript(t, m)

	m = press(m, downKey)
	if str.Offset != 38 {
		t.Errorf("down from the composer must scroll the conversation (offset=%d, want 38)", str.Offset)
	}
	m = press(m, pgdownKey)
	if str.Offset != 50 {
		t.Errorf("pgdown from the composer must page the conversation (offset=%d, want 50)", str.Offset)
	}
}

// AND THE RAIL KEEPS ITS CLAIM — the negative control for the rule above, so "the transcript gets the
// arrows" cannot quietly become "the transcript gets the arrows even when the rail is selected".
func TestTheRailStillOwnsTheArrowsWhenItIsSelected(t *testing.T) {
	m := findFixture(t)
	str := longTranscript(t, m)
	m.askPane = askPaneRail

	m = press(m, downKey)
	if str.Offset != 35 {
		t.Errorf("with the RAIL selected the arrows belong to the rail, so the transcript must not move "+
			"(offset=%d, want 35)", str.Offset)
	}
}

// ---------------------------------------------------------------- the advertisement

// THE COMPOSER SAYS THE KEYS EXIST — the operator's second ask: "no note in the composer about search or
// jumping to beginning to end is there so no one would know they exist."
func TestTheComposerAdvertisesTheTranscriptFindAndJumpKeys(t *testing.T) {
	m := findFixture(t)
	m.refreshComposerHint()
	hint := m.dock.Hint()
	for _, want := range []string{"ctrl+f", "home/end"} {
		if !strings.Contains(hint, want) {
			t.Errorf("the composer's affordance row must advertise %q, got %q", want, hint)
		}
	}

	// AND THE BOX DESCRIBES ITSELF while it owns the keyboard: the row must not keep advertising the
	// transcript's keys (which are inert) instead of the box's.
	m = press(m, ctrlFKey)
	hint = m.dock.Hint()
	if !strings.Contains(hint, "enter: keep searching") {
		t.Errorf("while the box is up the row must describe the BOX's keys, got %q", hint)
	}
	if strings.Contains(hint, "home/end: first·latest") {
		t.Errorf("the transcript's keys are inert while the box holds the keyboard, so the row must not "+
			"advertise them, got %q", hint)
	}
}
