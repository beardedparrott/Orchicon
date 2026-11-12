package control

// secret_provider_save_test.go — SAVING THROUGH THE REAL KEY PATH.

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/providers"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// pressKey delivers one key to the screen's real Update, the way the shell does.
func pressKey(t *testing.T, m *Model, key string) tea.Cmd {
	t.Helper()
	var msg tea.KeyMsg
	switch key {
	case "ctrl+s":
		msg = tea.KeyMsg{Type: tea.KeyCtrlS}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "tab":
		msg = tea.KeyMsg{Type: tea.KeyTab}
	case "down":
		msg = tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		msg = tea.KeyMsg{Type: tea.KeyUp}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
	_, cmd := m.Update(msg)
	return cmd
}

// typeInto types a whole string, one rune at a time (as a keyboard would).
func typeInto(t *testing.T, m *Model, s string) tea.Cmd {
	t.Helper()
	var last tea.Cmd
	for _, r := range s {
		last = pressKey(t, m, string(r))
	}
	return last
}

// drainCmds runs a command tree to completion, returning the mutate.Result the RPC produced (if any).
func drainCmds(t *testing.T, cmd tea.Cmd) (mutateResult, bool) {
	t.Helper()
	if cmd == nil {
		return mutateResult{}, false
	}
	msg := cmd()
	if msg == nil {
		return mutateResult{}, false
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			if res, ok := drainCmds(t, c); ok {
				return res, true
			}
		}
		return mutateResult{}, false
	}
	if res, ok := msg.(mutateResult); ok {
		return res, true
	}
	// A non-mutation message (a fetch landing, say) carries no result of its own.
	return mutateResult{}, false
}

// A SECRET CREATED THROUGH THE KEYS IS ACTUALLY CREATED — the operator's report, as an assertion.
func TestNewSecretThroughTheKeysIsPersisted(t *testing.T) {
	m, _ := newWriteModel(t)
	var created *apiv1.CreateSecretRequest
	m.rpcCreateSecret = func(_ context.Context, r *apiv1.CreateSecretRequest) error {
		created = r
		return nil
	}
	m.LoadItems("secrets", []kit2.Item{{ID: "s1", Title: "GITHUB_TOKEN", Meta: "value hidden"}}, "")
	m.SelectSource("secrets")

	// "n" opens the create form IN THE DETAILS PANE (not a modal).
	pressKey(t, m, "n")
	if !m.Base.EditingDetail() {
		t.Fatalf("n did not open the secret form in the detail pane (EditingDetail=%v, modal=%v)",
			m.Base.EditingDetail(), m.form != nil)
	}
	f := m.Base.DetailForm()
	if f == nil {
		t.Fatal("the pane has no form")
	}

	// TYPE the values — through key events, so the form's own cursor/insert path is exercised too.
	typeInto(t, m, "NEW_TOKEN")
	pressKey(t, m, "tab") // to the value field
	typeInto(t, m, "top-secret")

	// SAVE with ctrl+s, through the screen's dispatch.
	cmd := pressKey(t, m, "ctrl+s")

	if created == nil {
		// The RPC rides in the returned cmd; run it before concluding.
		if res, ok := drainCmds(t, cmd); ok && res.Err != nil {
			t.Fatalf("the save's RPC failed: %v", res.Err)
		}
	}
	if created == nil {
		t.Fatalf("ctrl+s did not create the secret — the operator's report. form submitted=%v errors=%v",
			f.Submitted, f.Errors)
	}
	if created.GetName() != "NEW_TOKEN" {
		t.Errorf("created name = %q, want NEW_TOKEN (the typed value)", created.GetName())
	}
	if created.GetValue() != "top-secret" {
		t.Errorf("created value = %q, want the typed value", created.GetValue())
	}
	// The editor closes on a successful save, so the operator is returned to the list.
	if m.Base.EditingDetail() {
		t.Error("the form is still open after ctrl+s — the save did not finish")
	}
}

// A PROVIDER created through the keys is actually created.
func TestNewProviderThroughTheKeysIsPersisted(t *testing.T) {
	m, _ := newWriteModel(t)
	var created *apiv1.ProviderCreateCustomRequest
	m.rpcCreateProvider = func(_ context.Context, r *apiv1.ProviderCreateCustomRequest) error {
		created = r
		return nil
	}
	m.LoadItems("providers", []kit2.Item{{ID: "p1", Title: "OpenAI", Meta: "openai enabled"}}, "")
	m.SelectSource("providers")

	pressKey(t, m, "n")
	if !m.Base.EditingDetail() {
		t.Fatalf("n did not open the provider form in the detail pane (modal=%v)", m.form != nil)
	}
	f := m.Base.DetailForm()
	if f == nil {
		t.Fatal("the pane has no form")
	}
	// Every REQUIRED field, filled by TYPING through real key events.
	for _, name := range []string{"display_name", "ref_id", "base_url"} {
		if !f.FocusName(name) {
			t.Fatalf("the provider form has no %q field", name)
		}
		switch name {
		case "base_url":
			typeInto(t, m, "http://127.0.0.1:11434") // must pass the URL validator
		default:
			typeInto(t, m, "val-"+name)
		}
	}

	cmd := pressKey(t, m, "ctrl+s")
	if created == nil {
		if res, ok := drainCmds(t, cmd); ok && res.Err != nil {
			t.Fatalf("the save's RPC failed: %v", res.Err)
		}
	}
	if created == nil {
		t.Fatalf("ctrl+s did not create the provider — the operator's report. errors=%v", f.Errors)
	}
	if created.GetDisplayName() != "val-display_name" {
		t.Errorf("display name = %q, want the typed value", created.GetDisplayName())
	}
}

// This is the operator's report: "I tried adding a secret and a provider in the TUI and saved it,
// yet they were never actually created." The store's rule is `^[A-Z][A-Z0-9_]+$` — UPPERCASE ONLY.
func TestLowercaseSecretNameIsRefusedBeforeSubmit(t *testing.T) {
	m, _ := newWriteModel(t)
	var created *apiv1.CreateSecretRequest
	m.rpcCreateSecret = func(_ context.Context, r *apiv1.CreateSecretRequest) error {
		created = r
		return nil
	}
	m.LoadItems("secrets", []kit2.Item{{ID: "s1", Title: "GITHUB_TOKEN", Meta: "value hidden"}}, "")
	m.SelectSource("secrets")

	pressKey(t, m, "n")
	f := m.Base.DetailForm()
	if f == nil {
		t.Fatal("n did not open the create form")
	}
	f.FocusName("name")
	typeInto(t, m, "my_token") // lowercase: what a person actually types
	f.FocusName("value")
	typeInto(t, m, "secret-value")

	cmd := pressKey(t, m, "ctrl+s")

	if created != nil {
		t.Error("a lowercase name was SENT to the server — the form must refuse it first")
	}
	if !m.Base.EditingDetail() {
		t.Error("the form CLOSED on an invalid name — that is the defect: the operator sees a close " +
			"and no row, instead of the reason")
	}
	if len(f.Errors) == 0 {
		t.Error("the form shows no error for a name the store will refuse")
	}
	// The message must name the rule in human terms, not just quote the regex.
	msg := strings.Join(errorStrings(f.Errors), " ")
	if !strings.Contains(strings.ToUpper(msg), "UPPERCASE") {
		t.Errorf("the error does not say what is wrong: %q", msg)
	}
	_ = cmd
}

// And a VALID name still saves — the validator must not be a wall.
func TestUppercaseSecretNameStillSaves(t *testing.T) {
	m, _ := newWriteModel(t)
	var created *apiv1.CreateSecretRequest
	m.rpcCreateSecret = func(_ context.Context, r *apiv1.CreateSecretRequest) error { created = r; return nil }
	m.LoadItems("secrets", []kit2.Item{{ID: "s1", Title: "GITHUB_TOKEN", Meta: "value hidden"}}, "")
	m.SelectSource("secrets")

	pressKey(t, m, "n")
	f := m.Base.DetailForm()
	f.FocusName("name")
	typeInto(t, m, "MY_TOKEN")
	f.FocusName("value")
	typeInto(t, m, "v")

	cmd := pressKey(t, m, "ctrl+s")
	if created == nil {
		if res, ok := drainCmds(t, cmd); ok && res.Err != nil {
			t.Fatalf("the save failed: %v (errors=%v)", res.Err, f.Errors)
		}
	}
	if created == nil {
		t.Fatalf("a VALID uppercase name did not save (errors=%v)", f.Errors)
	}
}

// errorStrings flattens a form's field errors for an assertion.
func errorStrings(errs map[string]string) []string {
	out := make([]string, 0, len(errs))
	for _, e := range errs {
		out = append(out, e)
	}
	return out
}

// EVERY AUTH MODE THE FORM OFFERS IS ONE THE SERVER ACCEPTS.
func TestProviderAuthModesAreAllServerAccepted(t *testing.T) {
	opts := authModeOptions()
	if len(opts) == 0 {
		t.Fatal("the form offers no auth modes — a provider could not be created")
	}
	seen := map[string]bool{}
	for _, o := range opts {
		if err := providers.ValidateAuthMode(o.Value); err != nil {
			t.Errorf("the form offers auth mode %q, which the SERVER REFUSES: %v", o.Value, err)
		}
		if seen[o.Value] {
			t.Errorf("auth mode %q is offered twice", o.Value)
		}
		seen[o.Value] = true
	}
	// And the value the operator actually needs is offered.
	if !seen[providers.AuthModeToken] {
		t.Error("`token` is NOT offered — the value the GUI offers and every existing provider uses")
	}
	if !seen[providers.AuthModeNone] {
		t.Error("`none` is not offered")
	}
	// The two invented modes must be GONE, not merely accompanied by the right ones.
	for _, bogus := range []string{"bearer", "api_key"} {
		if seen[bogus] {
			t.Errorf("the form still offers %q, which the server does not accept", bogus)
		}
	}
}

// The form's Initial matches one of its own options, so the select is never in a state the operator
// cannot see.
func TestProviderFormAuthModeInitialIsAnOption(t *testing.T) {
	m, _ := newWriteModel(t)
	m.LoadItems("providers", []kit2.Item{{ID: "p1", Title: "OpenAI", Meta: "openai enabled"}}, "")
	m.SelectSource("providers")
	pressKey(t, m, "n")
	f := m.Base.DetailForm()
	if f == nil {
		t.Fatal("n did not open the provider form")
	}
	// The create form's auth_mode field must start on a valid option.
	f.FocusName("auth_mode")
	if got := f.Values["auth_mode"]; providers.ValidateAuthMode(got) != nil {
		t.Errorf("the create form starts on auth mode %q, which the server refuses", got)
	}
}
