package tui

// composer_typing_never_quits_test.go — A KEY THAT IS TEXT MUST NEVER BE A COMMAND.
//
// The operator: "The last two times I was typing a message into the chat, orch just exited and dropped
// to the command line. Are you able to see the reason why? Either an error or some weird key
// combination I hit?"
//
// It was a key combination, and not a weird one: THE LETTER q.
//
// composerBypassKeys exists because the textarea consumes EVERY key it is handed — unknown chords are
// silent no-ops that still report consumed — so a global chord the composer is allowed to eat becomes
// unreachable. That is the map's whole purpose, and every entry belongs there EXCEPT one that was:
//
//	"q": true
//
// A bare `q` is in the GLOBAL routes as `quit` ("q / ctrl+c"). So while the composer had the focus,
// pressing q skipped the composer entirely and fell through to that route: the character was never
// inserted, `quitting` was set, and orch exited to the command line. No message containing a q could be
// typed — question, request, quote, quite — and it had been that way since Phase 2a with nothing
// asserting otherwise.
//
// The tests here pin the GENERAL rule rather than the instance: while the composer holds the focus,
// every printable character is TEXT, and only chords that cannot be text (ctrl, tab) reach the shell.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// typingQ inserts q, and does not quit.
func TestTypingQInsertsItRatherThanQuitting(t *testing.T) {
	m := newTestApp()
	m.dock.Focus()
	m.chatFocus = focusComposer

	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	app := nm.(*App)

	if app.quitting {
		t.Fatal("typing `q` in the composer QUIT orch. `q` is the global quit route, but while the composer " +
			"holds the focus it is a character in the operator's message — the composer must consume it")
	}
	if got := app.dock.Value(); got != "q" {
		t.Errorf("composer holds %q after typing q, want \"q\" — the character was swallowed on the way to "+
			"the quit route", got)
	}
}

// A WHOLE MESSAGE containing q types in full — the operator's actual situation, since nobody types a
// lone q.
func TestAMessageContainingQCanBeTypedInFull(t *testing.T) {
	const message = "a question about the queue"
	m := newTestApp()
	m.dock.Focus()
	m.chatFocus = focusComposer

	for _, r := range message {
		nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = nm.(*App)
		if m.quitting {
			t.Fatalf("orch quit while typing %q — the value so far was %q", string(r), m.dock.Value())
		}
	}
	if got := m.dock.Value(); got != message {
		t.Errorf("composer holds %q, want %q", got, message)
	}
}

// EVERY PRINTABLE LETTER IS TEXT. Asserted across the alphabet rather than for q alone, because the bug
// was a bypass-map entry and the map is edited by hand: a future entry that happens to be a letter
// should fail here rather than in the operator's session.
func TestNoPrintableLetterQuitsTheComposer(t *testing.T) {
	for _, r := range "abcdefghijklmnopqrstuvwxyz" {
		m := newTestApp()
		m.dock.Focus()
		m.chatFocus = focusComposer
		nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		app := nm.(*App)
		if app.quitting {
			t.Errorf("typing %q in the composer quit orch — every printable character must be text here", string(r))
			continue
		}
		if got := app.dock.Value(); got != string(r) {
			t.Errorf("typing %q left the composer holding %q", string(r), got)
		}
	}
	// The BYPASS MAP is the thing that decides this, so assert it directly too: no printable key in it.
	for k := range composerBypassKeys {
		if len([]rune(k)) == 1 && !strings.HasPrefix(k, "ctrl+") {
			t.Errorf("composerBypassKeys contains the printable key %q. A bypassed key skips the composer "+
				"and reaches the global routes — one of which quits on `q` — so a printable entry makes that "+
				"character untypeable and can exit orch mid-message. Only ctrl chords and tab belong here.", k)
		}
	}
}

// AND CTRL+C STILL QUITS — the documented hard escape, so removing the q bypass did not trap the
// operator in a session they cannot leave from the composer.
func TestCtrlCStillQuitsFromTheComposer(t *testing.T) {
	m := newTestApp()
	m.dock.Focus()
	m.chatFocus = focusComposer

	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !nm.(*App).quitting {
		t.Fatal("ctrl+c no longer quits from the composer — the router documents it as the hard escape that " +
			"\"always works, even mid-composition\", and it is now the only way out from here")
	}
}

// Q STILL QUITS WHEN THE COMPOSER DOES NOT HOLD THE FOCUS, which is the conventional binding and is
// unchanged: the composer branch is simply not reached from content focus.
func TestQStillQuitsOutsideTheComposer(t *testing.T) {
	m := newTestApp()
	m.dock.Focus()
	m.setFocus(focusContent)
	m.chatFocus = focusContent

	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if !nm.(*App).quitting {
		t.Error("`q` no longer quits when the composer is unfocused — the global quit route should still " +
			"answer it there, since no text input is claiming the key")
	}
}
