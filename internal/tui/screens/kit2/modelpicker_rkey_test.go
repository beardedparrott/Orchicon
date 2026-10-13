package kit2

// modelpicker_rkey_test.go — 'r' IS TEXT FIRST, AND A RETRY KEY ONLY WHEN THERE IS NOTHING TO TYPE.
//
// Reported: "when searching models in the TUI model modal, the 'r' key is being swallowed. Most likely
// by that 'r' refresh key."
//
// It was. The model tier is a TYPE-TO-SEARCH box, and the old `case "r"` either retried (when the load
// had failed) or returned false — and returning early from HandleKey means the typing branch at the
// bottom is never reached EITHER WAY. So `r` could never be typed: it retried, or it disappeared.
//
// The fix moves the check outside the switch and requires BOTH conditions: the load failed AND the
// query is empty. The second is what keeps the key working as text, and it is also exactly when the
// on-screen hint offers it ("models unavailable — press r to retry" only renders when there are no
// options and an error is set).

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// rKey is the message Bubble Tea delivers for a printable 'r'.
func rKey() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}} }

func TestRTypesIntoTheSearchBoxWhenThereIsNoError(t *testing.T) {
	// THE REPORTED CASE: models are listed, the operator searches, and 'r' must be a letter.
	mp := NewModelPicker("m")
	mp.tier = TierModel
	mp.query = "big"

	handled, _ := mp.HandleKey(rKey())
	if !handled {
		t.Error("'r' must be handled — as TEXT. Returning unhandled sends it to the host shell, " +
			"which is the 'swallowed' behaviour reported")
	}
	if mp.query != "bigr" {
		t.Errorf("query = %q, want %q — 'r' must be appended to the search like any other letter",
			mp.query, "bigr")
	}
}

func TestRTypesFromAnEmptyQueryWithNoError(t *testing.T) {
	mp := NewModelPicker("m")
	mp.tier = TierModel

	if _, _ = mp.HandleKey(rKey()); mp.query != "r" {
		t.Errorf("query = %q, want %q — an empty search box must accept 'r'", mp.query, "r")
	}
}

func TestRTypesEvenWhenTheLoadFailedAndTheOperatorIsTyping(t *testing.T) {
	// The subtle one: an error is set, but the operator has already typed. 'r' must be TEXT, because a
	// search that silently retries instead of typing is the same bug in a narrower disguise.
	mp := NewModelPicker("m")
	mp.tier = TierModel
	mp.err = "models unavailable"
	mp.query = "son"

	if _, _ = mp.HandleKey(rKey()); mp.query != "sonr" {
		t.Errorf("query = %q, want %q — with a query in progress 'r' is text, not a command",
			mp.query, "sonr")
	}
}

func TestRRetriesOnlyWhenTheLoadFailedAndNothingIsTyped(t *testing.T) {
	// And the retry still exists, in the one state where it is unambiguous — which is also the state
	// the on-screen hint describes.
	mp := NewModelPicker("m")
	mp.tier = TierModel
	mp.err = "models unavailable"
	mp.query = ""
	// A LOADER MUST BE WIRED, or begin() legitimately returns nil and the test would be asserting
	// about the fixture rather than about the key. (Measured: the first version of this test failed
	// for exactly that reason.)
	mp.LoadAdapters = func() tea.Cmd { return func() tea.Msg { return nil } }

	handled, cmd := mp.HandleKey(rKey())
	if !handled {
		t.Fatal("'r' must be handled as a retry when the load failed and nothing is typed")
	}
	if cmd == nil {
		t.Error("retrying must return a command — otherwise 'press r to retry' does nothing")
	}
	if mp.err != "" {
		t.Error("Retry must clear the displayed error, or the modal keeps showing a stale failure")
	}
	if mp.inFlight == "" || !mp.loading {
		t.Errorf("the retry must actually START a load (inFlight=%q loading=%v) — clearing the error "+
			"alone would look like a retry and do nothing", mp.inFlight, mp.loading)
	}
}

func TestTheRetryKeyIsOnlyOfferedWhenItWorks(t *testing.T) {
	// The hint and the behaviour must agree: the modal advertises 'r' only when there are no options
	// and an error is set, which is precisely the state where the key retries.
	mp := NewModelPicker("m")
	mp.tier = TierModel
	mp.err = ""

	_, cmd := mp.HandleKey(rKey())
	if cmd != nil {
		t.Error("with no error, 'r' must not run a retry command — it is a letter in the search box")
	}
}
