package control

// secret_paste_test.go — PASTING INTO A SECRET FORM.
//
// The operator: "I can't copy and paste a value into a secret in the TUI under the secrets section when
// creating a new or editing a secret."
//
// WHY IT WAS MISSED, and why this test lives at THIS level rather than in the shell. The shell's paste
// support (tui.form_paste.go) enumerated the forms THE SHELL owns — the scope modal, the launch prompt, the
// grouping forms. Every screen's own forms were missed, and screen forms are most of them, because they open
// in the details pane via kit2.Base.BeginDetailEdit. The fix is a method on that shared host, so the
// capability travels with the screen instead of sitting in a list somebody has to remember to extend.
//
// So what is pinned here is the REAL surface the report came from: the Control screen's secret form, opened
// by `n` on the secrets pane, taking a paste through the assertion the shell itself makes.

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// pastableSecretScreen opens the New-secret form on the secrets pane and returns the screen plus the
// assertion the shell uses to route a paste to it.
func pastableSecretScreen(t *testing.T) (*Model, interface{ PasteIntoForm(string) bool }) {
	t.Helper()
	m := New(nil, nil)
	m.SetShell(&fakeDock{})
	m.SetSize(100, 30)
	if !m.SelectSource("secrets") {
		t.Fatal("no secrets source")
	}
	// The shell's GUARD, before anything is open: nothing to paste into, so ctrl+v must stay the composer's.
	if m.EditingDetail() {
		t.Fatal("the secrets pane reports an open editor before one was opened — the shell's guard would " +
			"claim ctrl+v on a pane that has no form")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if !m.formOpen() {
		t.Fatal("`n` did not open the New-secret form")
	}
	p, ok := interface{}(m).(interface{ PasteIntoForm(string) bool })
	if !ok {
		t.Fatal("the control screen does not accept a paste — the shell would never route one to its form, " +
			"which is exactly the report")
	}
	return m, p
}

func TestPastingIntoANewSecretFormReachesTheField(t *testing.T) {
	m, pastable := pastableSecretScreen(t)

	if !m.EditingDetail() {
		t.Fatal("the form is open but the pane does not report it — the shell's guard would refuse the paste")
	}
	// A TOKEN, with the trailing newline a copied value usually carries.
	if !pastable.PasteIntoForm("MCP_GITHUB_TOKEN\n") {
		t.Fatal("the paste did not land in the open secret form")
	}
	if got := m.activeForm().Values["name"]; got != "MCP_GITHUB_TOKEN" {
		t.Errorf("the pasted value = %q, want the token in the focused field", got)
	}
}

func TestPastingIntoARotateSecretFormReachesTheValueField(t *testing.T) {
	// THE FIELD THE REPORT NAMES — "paste a value into a secret" — is the KSecret value field of the ROTATE
	// form (the edit path, which is the one you reach by pressing `e` on an existing secret).
	m := New(nil, nil)
	m.SetShell(&fakeDock{})
	m.SetSize(100, 30)
	if !m.SelectSource("secrets") {
		t.Fatal("no secrets source")
	}
	m.LoadItems("secrets", []kit2.Item{{ID: "s1", Title: "GITHUB_TOKEN", Meta: "value hidden"}}, "")
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if !m.formOpen() {
		t.Fatal("`e` did not open the rotate form for the selected secret")
	}
	p, ok := interface{}(m).(interface{ PasteIntoForm(string) bool })
	if !ok {
		t.Fatal("the control screen does not accept a paste")
	}
	if !p.PasteIntoForm("ghp_pasted_token") {
		t.Fatal("the paste did not land in the rotate form")
	}
	f := m.activeForm()
	if got := f.Values["value"]; got != "ghp_pasted_token" {
		t.Errorf("value = %q, want the pasted token (the field the report names)", got)
	}
}
