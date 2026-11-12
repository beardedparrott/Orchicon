package ask

// cardinput_test.go — PASTING INTO A CARD'S FREE-TEXT ROW.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// questionCardWithOtherRow opens a question card and enters its free-text row through the REAL keyboard
// path (down to Other, Enter), so the fixture cannot disagree with how the row actually opens.
func questionCardWithOtherRow(t *testing.T) (*Model, []chat.ChatItem) {
	t.Helper()
	m, _ := newTestModel(t)
	items := []chat.ChatItem{chat.ConsentItem(chat.PermissionAsk{
		ID: "q1", Kind: chat.AskQuestion, Question: "Which file?",
		Options: []string{"main.go", "util.go"}, AllowOther: true}, 1000)}
	m.RenderTranscript(items, chat.Conversation{}, false)
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !items[0].Consent.OtherMode {
		t.Fatal("fixture: enter on Other must open the free-text row")
	}
	return m, items
}

// NO ROW, NO PASTE TARGET: with no free-text row up, the screen must not claim to be one — otherwise the
// shell would read the clipboard on every ctrl+v and route it here instead of to the composer.
func TestNoCardRowMeansNoPasteTarget(t *testing.T) {
	m, _ := newTestModel(t)
	if m.CardInputOpen() {
		t.Fatal("a screen with no card row claims a paste target — ctrl+v would stop reaching the composer")
	}
	if landed, _ := m.PasteIntoCardInput("text"); landed {
		t.Fatal("text landed in no row")
	}
}

// THE FIX: with the row open, the screen reports a paste target and the text lands IN THE ROW.
func TestPasteLandsInTheConsentOtherRow(t *testing.T) {
	m, items := questionCardWithOtherRow(t)
	host := m.Shell().(*stubHost)

	if !m.CardInputOpen() {
		t.Fatal("the open free-text row is not reported as a paste target — this is the reported bug")
	}
	landed, cmd := m.PasteIntoCardInput("seed.sql")
	if !landed {
		t.Fatal("the paste did not land in the open row")
	}
	if got := items[0].Consent.OtherInput; got != "seed.sql" {
		t.Fatalf("the row holds %q, want the pasted text", got)
	}
	// THE REPAINT IS NOT OPTIONAL: the row is drawn from the transcript, and a paste that mutates the
	// state without repainting shows the operator nothing — indistinguishable from the bug.
	if cmd == nil || host.repaints == 0 {
		t.Fatal("the paste did not ask for a repaint, so the pasted text would not appear until some " +
			"other wake — which reads to the operator as the paste having done nothing")
	}
}

// PASTE IS NOT RETYPING: newlines survive, because the row's content becomes an answer (a message
// or a tool result) where line breaks are the operator's own formatting.
func TestPastedNewlinesSurviveInTheRow(t *testing.T) {
	m, items := questionCardWithOtherRow(t)
	const pasted = "error: build failed\n  at main.go:42\n"
	if landed, _ := m.PasteIntoCardInput(pasted); !landed {
		t.Fatal("the paste did not land")
	}
	if got := items[0].Consent.OtherInput; got != pasted {
		t.Fatalf("the row holds %q, want the pasted text verbatim (%q) — a paste of code or an error "+
			"trace must not be silently reflowed", got, pasted)
	}
	// AND IT SUBMITS AS THE ANSWER, line breaks intact — the reason fidelity here matters.
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got, want := items[0].Consent.Choice, strings.TrimSpace(pasted); got != want {
		t.Fatalf("the submitted answer is %q, want %q", got, want)
	}
	if !strings.Contains(items[0].Consent.Choice, "\n") {
		t.Fatal("the internal line break was lost on submit — the pasted structure is the operator's " +
			"formatting and it is what the model needs to read the error")
	}
}

// THE RECORDED CARD'S DRAFT ROW IS THE SAME CAPABILITY, and it is asserted separately because the two
// rows are different state on different types: a fix for one is not a fix for the other.
func TestPasteLandsInTheRecordedCardDraftRow(t *testing.T) {
	m, host := newTestModel(t)
	item := chat.ChatItem{
		Kind: chat.KindAsk,
		Key:  "m-1-ask",
		Ask:  &chat.ParsedAsk{Question: "Which file?", AllowOther: true, Drafting: true},
	}
	m.RenderTranscript([]chat.ChatItem{item}, chat.Conversation{}, false)

	if !m.CardInputOpen() {
		t.Fatal("the open draft row is not reported as a paste target")
	}
	if landed, cmd := m.PasteIntoCardInput("util/parse.go"); !landed || cmd == nil || host.repaints == 0 {
		t.Fatalf("paste into the draft row: landed=%v cmd=%v repaints=%d", landed, cmd != nil, host.repaints)
	}
	if got := item.Ask.Draft; got != "util/parse.go" {
		t.Fatalf("the draft row holds %q, want the pasted text", got)
	}
}

// A DEFERRED ROW DOES NOT TAKE THE PASTE.
func TestADeferredRowDoesNotTakeThePaste(t *testing.T) {
	m, items := questionCardWithOtherRow(t)
	// ctrl+g defers the card for THIS ask id.
	m.DropKeyClaim()
	if m.CardInputOpen() {
		t.Fatal("a row the operator deferred with ctrl+g still claims the paste — the text would land " +
			"in a row their typing is not going to")
	}
	if landed, _ := m.PasteIntoCardInput("ignored"); landed {
		t.Fatal("the paste landed in a deferred row")
	}
	if got := items[0].Consent.OtherInput; got != "" {
		t.Fatalf("the deferred row collected %q", got)
	}
}
