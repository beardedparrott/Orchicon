package tui

// form_paste_test.go — THE SHELL'S HALF OF PASTING INTO A FORM FIELD.
//
// The operator: "The modal does not allow me to paste anything in any of the fields from my clipboard. ctrl+v
// should allow me to paste a secret name, token, etc."
//
// The bug was structural, not a missing binding: ctrl+v arrives as tea.KeyCtrlV (no runes), so no form
// inserted it, and the clipboard read has to be a COMMAND because it shells out. So the shell has to
// intercept it above the forms, and route the text to whichever form has the focus.
//
// The clipboard reader is not driven from here (it execs a helper); what is pinned is the DECISION — pasteKey
// — and the destination, which is the part that can be wrong: the same message type the composer uses would
// have pasted a token into a chat message instead of the field.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// openCredentialFormOnARow opens the MCP scope modal and presses `k`, which opens the credential form for the
// focused row — the exact surface the report is about.
func openCredentialFormOnARow(t *testing.T) *App {
	t.Helper()
	m, _, _ := newScopeApp(t)
	openScopeFrom(t, m, "/scope")
	putCursor(m, rowIndexOf(t, m, "github"))
	pressScope(t, m, "k")
	if m.convScopeForm == nil {
		t.Fatal("`k` opened no credential form — the paste assertions below would prove nothing")
	}
	return m
}

func TestCtrlVPastesIntoTheOpenFormField(t *testing.T) {
	m := openCredentialFormOnARow(t)

	cmd, handled := m.pasteKey(tea.KeyMsg{Type: tea.KeyCtrlV})
	if !handled {
		t.Fatal("ctrl+v was not handled while a form field has the focus — the operator's report exactly")
	}
	if cmd == nil {
		t.Fatal("ctrl+v produced no clipboard read: the key is swallowed and nothing is pasted")
	}
	// The cursor is parked on the credential form's KEY field, so this is where a paste must land.
	m.pasteIntoForms("MCP_GITHUB_TOKEN")
	if got := m.convScopeForm.Values["key"]; got != "MCP_GITHUB_TOKEN" {
		t.Errorf("the pasted text landed at key=%q, want the focused field to hold the token", got)
	}
}

func TestCtrlVIsTheComposersWhenNoFormIsOpen(t *testing.T) {
	// THE GUARD IS THE CONTRACT. ctrl+v in the shell is the composer's attachment chord (paste an image, or a
	// file path); a form only changes that while a form is up. Without this, the interception would have
	// stolen the composer's paste for every keystroke in the app.
	m, _, _ := newScopeApp(t)
	if _, handled := m.pasteKey(tea.KeyMsg{Type: tea.KeyCtrlV}); handled {
		t.Fatal("ctrl+v was claimed with no form open — the composer's own paste would never run")
	}
}

func TestBracketedPasteIntoAFormFieldIsSanitised(t *testing.T) {
	// The TERMINAL's own paste (middle-click, or a terminal that sends bracketed paste) arrives as runes, so
	// a form would insert it by its ordinary rune path — which happily accepts the trailing newline a copied
	// token carries. It has to go through PasteText instead.
	m := openCredentialFormOnARow(t)

	cmd, handled := m.pasteKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("MCP_TOKEN\n"), Paste: true})
	if !handled {
		t.Fatal("a bracketed paste into an open form was not handled")
	}
	if cmd != nil {
		t.Error("a bracketed paste asked for a clipboard read — the text is already in the message")
	}
	got := m.convScopeForm.Values["key"]
	if got != "MCP_TOKEN" {
		t.Errorf("the pasted token = %q, want it inserted without the trailing newline", got)
	}
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("a single-line field received a line break from a paste: %q", got)
	}
}

func TestAClipboardReadForAFormPastesIntoTheFormNotTheComposer(t *testing.T) {
	// THE DESTINATION IS THE POINT. The shell already has a clipboardTextMsg, and its handler inserts into the
	// COMPOSER — so a paste meant for a field must not be that message, or a token would be typed into a chat
	// message instead.
	m := openCredentialFormOnARow(t)
	m.dock.SetValue("")

	m.onClipboardText(formClipboardTextMsg{text: "SLACK_BOT_TOKEN"})
	if got := m.convScopeForm.Values["key"]; got != "SLACK_BOT_TOKEN" {
		t.Errorf("the field holds %q, want the pasted token", got)
	}
	if got := m.dock.Value(); got != "" {
		t.Errorf("the paste reached the composer (%q) instead of the form field", got)
	}
}

func TestAFailedClipboardReadSaysSo(t *testing.T) {
	// A paste that silently does nothing is the report being fixed, so a read that fails — no helper
	// installed, or no text on the clipboard — has to be visible where the operator is looking.
	m := openCredentialFormOnARow(t)
	m.dock.SetError("")

	m.onClipboardText(formClipboardTextMsg{err: "the clipboard holds no text"})
	if m.dock.Err == "" {
		t.Fatal("a failed paste said nothing — the key would read as broken")
	}
	if !strings.Contains(m.dock.Err, "paste") || !strings.Contains(m.dock.Err, "no text") {
		t.Errorf("the message does not name the paste or the reason: %q", m.dock.Err)
	}
}

// pasteScreenStub embeds kit2.Base exactly as every screen Model does, which is the whole point: the shell
// asks the SCREEN for its form instead of being handed a list of screens to remember.
type pasteScreenStub struct{ kit2.Base }

func (s *pasteScreenStub) Init() tea.Cmd                              { return nil }
func (s *pasteScreenStub) Update(tea.Msg) (screenkit.Screen, tea.Cmd) { return s, nil }
func (s *pasteScreenStub) Name() string                               { return "paste-stub" }
func (s *pasteScreenStub) Close()                                     {}

// A SCREEN-HOSTED FORM IS PASTED INTO TOO — the half that was MISSING.
//
// The first version of this support enumerated the forms the SHELL owns, so a screen's forms were invisible
// to it and the operator could not paste into the Control screen's secret form. The capability now lives on
// kit2.Base, which every screen embeds, so the shell reaches any screen's form by one assertion.
func TestCtrlVPastesIntoScreenHostedForm(t *testing.T) {
	m, _, _ := newScopeApp(t)
	st := &pasteScreenStub{}
	st.BeginDetailEdit("New secret", kit2.NewForm("New secret",
		kit2.FieldSpec{Name: "name", Label: "Name", Kind: kit2.KText},
	))
	m.screens[m.active] = st

	if !m.hasPasteTarget() {
		t.Fatal("the shell sees no paste target while a screen has a form open — this is the report: a " +
			"secret form you cannot paste into")
	}
	cmd, handled := m.pasteKey(tea.KeyMsg{Type: tea.KeyCtrlV})
	if !handled {
		t.Fatal("ctrl+v was not handled with a screen-hosted form open")
	}
	if cmd == nil {
		t.Fatal("ctrl+v produced no clipboard read for a screen-hosted form")
	}
	if !m.pasteIntoForms("MCP_GITHUB_TOKEN") {
		t.Fatal("the paste did not land in the screen's form")
	}
	if got := st.DetailForm().Values["name"]; got != "MCP_GITHUB_TOKEN" {
		t.Errorf("the screen's form holds %q, want the pasted token", got)
	}
}

// THE GUARD IS PER-SCREEN, and it has to be: every screen can paste by virtue of embedding Base, so asking
// "can you paste?" would claim ctrl+v on EVERY tab and the composer's own paste would never run again. The
// question is "is a form actually up?" — EditingDetail.
func TestAScreenWithNoFormDoesNotClaimCtrlV(t *testing.T) {
	m, _, _ := newScopeApp(t)
	m.screens[m.active] = &pasteScreenStub{} // embeds Base, but no form is open
	if m.hasPasteTarget() {
		t.Fatal("a screen with no form open claimed a paste target — ctrl+v would never reach the composer")
	}
	if _, handled := m.pasteKey(tea.KeyMsg{Type: tea.KeyCtrlV}); handled {
		t.Fatal("ctrl+v was claimed with no form open ANYWHERE")
	}
}
