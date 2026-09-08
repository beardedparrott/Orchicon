package connection

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
)

func fakeProbes(vErr, keyErr, loginErr error, version string) ProbeFuncs {
	return ProbeFuncs{
		Versionz: func(ctx context.Context, baseURL string, insecure bool) (*client.VersionzResponse, error) {
			if vErr != nil {
				return nil, vErr
			}
			return &client.VersionzResponse{Version: version}, nil
		},
		ListProjects: func(ctx context.Context, baseURL, token string, insecure bool) error {
			return keyErr
		},
		LocalLogin: func(ctx context.Context, baseURL, username, password string, insecure bool) (*client.LoginResponse, error) {
			if loginErr != nil {
				return nil, loginErr
			}
			return &client.LoginResponse{AccessToken: "acc-123", ExpiresIn: 900}, nil
		},
	}
}

func typeInto(m Model, s string) Model {
	for _, r := range s {
		nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = nm.(Model)
	}
	return m
}

func submit(t *testing.T, m Model) (Model, []tea.Msg) {
	t.Helper()
	var msgs []tea.Msg
	for i := 0; i < 10; i++ {
		nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		m = nm.(Model)
		if cmd == nil {
			break
		}
		msg := cmd()
		msgs = append(msgs, msg)
		if _, ok := msg.(probeStartMsg); ok {
			return m, msgs
		}
	}
	return m, msgs
}

func TestValidateURLAndCredential(t *testing.T) {
	m := New(nil, fakeProbes(nil, nil, nil, "v1"))
	if msg := m.validate(); msg != "server URL is required" {
		t.Fatalf("validate = %q", msg)
	}
	m = typeInto(m, "notaurl")
	if msg := m.validate(); msg == "" {
		t.Fatal("bare host without scheme must fail")
	}
	m2 := New(nil, fakeProbes(nil, nil, nil, "v1"))
	m2 = typeInto(m2, "https://orch.example.com")
	if msg := m2.validate(); msg == "" {
		t.Fatal("missing API key must fail")
	}
}

func TestProbeSuccessAPIKey(t *testing.T) {
	m := New(nil, fakeProbes(nil, nil, nil, "v9.0.1"))
	m.inputs[fieldURL].SetValue("https://orch.example.com/")
	m.inputs[fieldCredential].SetValue("oc_k")
	res, err := m.probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.ServerVersion != "v9.0.1" {
		t.Fatalf("server version = %q", res.ServerVersion)
	}
	if res.Profile.AuthMethod != config.AuthAPIKey || res.Profile.Token != "oc_k" {
		t.Fatalf("profile mismatch: %+v", res.Profile)
	}
	// URL normalized (trailing slash trimmed).
	if res.Profile.URL != "https://orch.example.com" {
		t.Fatalf("url = %q", res.Profile.URL)
	}
}

func TestProbeBadKeySurfacesScopeGuidance(t *testing.T) {
	m := New(nil, fakeProbes(nil, errors.New("insufficient entitlement: execution:read"), nil, "v1"))
	m.inputs[fieldURL].SetValue("https://orch.example.com")
	m.inputs[fieldCredential].SetValue("oc_bad")
	_, err := m.probe(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "Read scopes") || !strings.Contains(err.Error(), "write scopes") || !strings.Contains(err.Error(), "execution:read") {
		t.Fatalf("error must carry re-create guidance + missing scope: %v", err)
	}
}

func TestProbeUnreachableVersionz(t *testing.T) {
	m := New(nil, fakeProbes(errors.New("connection refused"), nil, nil, ""))
	m.inputs[fieldURL].SetValue("https://orch.example.com")
	m.inputs[fieldCredential].SetValue("oc_k")
	_, err := m.probe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "versionz") {
		t.Fatalf("expected versionz error, got %v", err)
	}
}

func TestProbePasswordModeMintsTokenNotPassword(t *testing.T) {
	m := New(nil, fakeProbes(nil, nil, nil, "v1"))
	m.authAPI = false
	m.inputs[fieldURL].SetValue("https://orch.example.com")
	m.inputs[fieldUsername].SetValue("me")
	m.inputs[fieldCredential].SetValue("hunter2")
	res, err := m.probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.Profile.AuthMethod != config.AuthPassword {
		t.Fatalf("auth method = %q", res.Profile.AuthMethod)
	}
	if res.Profile.Token != "acc-123" {
		t.Fatalf("token must be the minted access token, got %q", res.Profile.Token)
	}
	if res.Profile.Username != "me" || strings.Contains(res.Profile.Token, "hunter2") {
		t.Fatalf("password must never be persisted: %+v", res.Profile)
	}
}

func TestSubmitFlowEmitsProbeMessages(t *testing.T) {
	m := New(nil, fakeProbes(nil, nil, nil, "v1"))
	m.inputs[fieldURL].SetValue("https://orch.example.com")
	m.inputs[fieldCredential].SetValue("oc_k")
	m, msgs := submit(t, m)
	if len(msgs) == 0 {
		t.Fatal("expected probeStartMsg")
	}
	if _, ok := msgs[0].(probeStartMsg); !ok {
		t.Fatalf("first msg = %T, want probeStartMsg", msgs[0])
	}
}

// TestCredentialFieldIsModeAware verifies the credential field's prompt +
// placeholder derive from the active auth mode: API-key mode shows
// "API key >" with the oc_… placeholder; username+password mode shows
// "Password >" with a password placeholder — never "API key" in password
// mode (regression for the bug where password mode rendered "API key >").
func TestCredentialFieldIsModeAware(t *testing.T) {
	m := New(nil, fakeProbes(nil, nil, nil, "v1"))
	if !m.authAPI {
		t.Fatal("default auth mode must be API key")
	}
	if got := m.inputs[fieldCredential].Prompt; got != "API key > " {
		t.Fatalf("API-key prompt = %q, want %q", got, "API key > ")
	}
	if got := m.inputs[fieldCredential].Placeholder; !strings.Contains(got, "oc_") {
		t.Fatalf("API-key placeholder must mention oc_…, got %q", got)
	}

	// Toggle to username+password.
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlA})
	m = nm.(Model)
	if m.authAPI {
		t.Fatal("ctrl+a must toggle to username+password")
	}
	if got := m.inputs[fieldCredential].Prompt; got != "Password > " {
		t.Fatalf("password prompt = %q, want %q", got, "Password > ")
	}
	if got := m.inputs[fieldCredential].Placeholder; got != "password" {
		t.Fatalf("password placeholder = %q, want %q", got, "password")
	}
	if strings.Contains(m.inputs[fieldCredential].Prompt, "API key") {
		t.Fatal("password mode must never show 'API key' prompt")
	}

	// Toggle back to API key.
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlA})
	m = nm.(Model)
	if got := m.inputs[fieldCredential].Prompt; got != "API key > " {
		t.Fatalf("back to API-key prompt = %q, want %q", got, "API key > ")
	}
}

// TestToggleClearsStaleError verifies that toggling the auth method (ctrl+a)
// or the insecure flag (ctrl+s) clears any stale validation/probe error so
// an "API key is required" message does not persist after switching to
// username+password (regression from the operator screenshot).
func TestToggleClearsStaleError(t *testing.T) {
	m := New(nil, fakeProbes(nil, nil, nil, "v1"))
	// Raise a validation error in API-key mode (missing key).
	m.inputs[fieldURL].SetValue("https://orch.example.com")
	m.errMsg = "API key is required (create one in the GUI: Settings → API keys)"

	// ctrl+a to password mode must clear the stale error.
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlA})
	m = nm.(Model)
	if m.errMsg != "" {
		t.Fatalf("ctrl+a must clear stale errMsg, got %q", m.errMsg)
	}

	// Re-raise an error, then ctrl+s must clear it too.
	m.errMsg = "some error"
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = nm.(Model)
	if m.errMsg != "" {
		t.Fatalf("ctrl+s must clear stale errMsg, got %q", m.errMsg)
	}
}

// TestViewShowsAuthToggleHint verifies the ctrl+a guidance line naming both
// modes is visible in the rendered view.
func TestViewShowsAuthToggleHint(t *testing.T) {
	m := New(nil, fakeProbes(nil, nil, nil, "v1"))
	view := m.View()
	if !strings.Contains(view, "ctrl+a: switch between API key and username + password login") {
		t.Fatalf("view must show the ctrl+a toggle hint naming both modes")
	}
}

// TestViewGuidanceNamesReadAndWrite verifies the API-key hint and the
// rejection error name BOTH read and write scopes (browsing needs read;
// sending chat messages / interjecting into live executions needs write),
// pointing at Settings → API keys.
func TestViewGuidanceNamesReadAndWrite(t *testing.T) {
	m := New(nil, fakeProbes(nil, nil, nil, "v1"))
	view := m.View()
	if !strings.Contains(view, "Read scopes cover browsing") {
		t.Fatalf("hint must say read scopes cover browsing")
	}
	if !strings.Contains(view, "write scopes") || !strings.Contains(view, "interjecting") {
		t.Fatalf("hint must name write scopes for chat/interjections")
	}
	if !strings.Contains(view, "Settings → API keys") {
		t.Fatalf("hint must point at Settings → API keys")
	}

	// The rejection error must carry the same read+write guidance.
	m2 := New(nil, fakeProbes(nil, errors.New("insufficient entitlement: execution:read"), nil, "v1"))
	m2.inputs[fieldURL].SetValue("https://orch.example.com")
	m2.inputs[fieldCredential].SetValue("oc_bad")
	_, err := m2.probe(context.Background())
	if err == nil {
		t.Fatal("expected probe error")
	}
	if !strings.Contains(err.Error(), "Read scopes") || !strings.Contains(err.Error(), "write scopes") {
		t.Fatalf("rejection error must name read AND write scopes: %v", err)
	}
	if !strings.Contains(err.Error(), "Settings → API keys") {
		t.Fatalf("rejection error must point at Settings → API keys: %v", err)
	}
}

func TestSaveProfilePersists0600AndActivates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg")
	res := &Result{Profile: &config.Profile{
		Name: "default", URL: "https://orch.example.com", AuthMethod: config.AuthAPIKey, Token: "oc_k",
	}, ServerVersion: "v1"}
	if err := SaveProfile(path, res); err != nil {
		t.Fatalf("save: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("perms = %o, want 600", st.Mode().Perm())
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Active != "default" || cfg.Profiles["default"].Token != "oc_k" {
		t.Fatalf("config mismatch: %+v", cfg)
	}
}

// TestConnectedMsgCompletesFlow drives the full message cycle: submit →
// probeStartMsg → probeDoneMsg → connectedMsg → tea.Quit with a non-nil
// Result(). Guards the regression where connectedMsg had no handler and
// the first-run flow could never complete (cmd/orch always exited with
// "no connection established").
func TestConnectedMsgCompletesFlow(t *testing.T) {
	m := New(nil, fakeProbes(nil, nil, nil, "v9.0.1"))
	m.inputs[fieldURL].SetValue("https://orch.example.com")
	m.inputs[fieldCredential].SetValue("oc_k")

	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // submit
	m = nm.(Model)
	if cmd == nil {
		t.Fatal("submit must return a cmd")
	}
	if _, ok := cmd().(probeStartMsg); !ok {
		t.Fatal("submit cmd must produce probeStartMsg")
	}
	nm, cmd = m.Update(probeStartMsg{}) // runs the probe
	m = nm.(Model)
	if cmd == nil {
		t.Fatal("probeStartMsg must start the probe")
	}
	done := cmd()
	pd, ok := done.(probeDoneMsg)
	if !ok {
		t.Fatalf("probe cmd produced %T, want probeDoneMsg", done)
	}
	if pd.err != nil {
		t.Fatalf("probe err: %v", pd.err)
	}
	nm, cmd = m.Update(pd) // success → connectedMsg cmd
	m = nm.(Model)
	if cmd == nil {
		t.Fatal("successful probeDoneMsg must emit connectedMsg")
	}
	cm, ok := cmd().(connectedMsg)
	if !ok {
		t.Fatalf("expected connectedMsg, got %T", cm)
	}
	nm, cmd = m.Update(cm) // the handler under test
	final := nm.(Model)
	if cmd == nil {
		t.Fatal("connectedMsg must quit the program")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("connectedMsg must produce tea.Quit, got %T", cmd())
	}
	if final.Result() == nil {
		t.Fatal("connectedMsg must record the result for cmd/orch")
	}
	if final.Result().Profile.Token != "oc_k" || final.Result().ServerVersion != "v9.0.1" {
		t.Fatalf("result mismatch: %+v", final.Result())
	}
}
