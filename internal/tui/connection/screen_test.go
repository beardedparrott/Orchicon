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

// --- Phase 2b: the embedded (in-place /connect) overlay contract ---

// TestEmbeddedOverlayNeverQuits pins the overlay contract: with
// SetEmbedded, the FULL screen form (URL + auth-method toggle + credential
// field) completes the whole submit→probe→connected cycle without ever
// emitting tea.Quit, and the shell reads the result via Result()/Connected().
func TestEmbeddedOverlayNeverQuits(t *testing.T) {
	m := New(nil, fakeProbes(nil, nil, nil, "v9.0.1"))
	m.SetEmbedded()
	m.inputs[fieldURL].SetValue("https://orch.example.com")
	m.inputs[fieldCredential].SetValue("oc_k")

	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm.(Model)
	if cmd == nil {
		t.Fatal("submit must return a cmd")
	}
	if _, ok := cmd().(probeStartMsg); !ok {
		t.Fatal("submit cmd must produce probeStartMsg")
	}
	nm, cmd = m.Update(probeStartMsg{})
	m = nm.(Model)
	done := cmd()
	pd, ok := done.(probeDoneMsg)
	if !ok || pd.err != nil {
		t.Fatalf("probe failed: %#v", done)
	}
	nm, cmd = m.Update(pd)
	m = nm.(Model)
	cm, ok := cmd().(connectedMsg)
	if !ok {
		t.Fatalf("expected connectedMsg, got %T", cmd())
	}
	nm, _ = m.Update(cm)
	m = nm.(Model)
	// Embedded: success does NOT quit the process — the shell consumes it.
	if !m.Connected() {
		t.Fatal("embedded model must report Connected() after a successful probe")
	}
	res := m.Result()
	if res == nil || res.Profile.Token != "oc_k" || res.ServerVersion != "v9.0.1" {
		t.Fatalf("result mismatch: %+v", res)
	}
}

// TestEmbeddedCtrlCDoesNotQuit pins ctrl+c inertness inside the shell's
// overlay (the shell's global quit chord owns process exit; ctrl+c inside
// the embedded form is swallowed).
func TestEmbeddedCtrlCDoesNotQuit(t *testing.T) {
	m := New(nil, fakeProbes(nil, nil, nil, "v1"))
	m.SetEmbedded()
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = nm.(Model)
	if cmd != nil {
		if _, isQuit := cmd().(tea.QuitMsg); isQuit {
			t.Fatal("embedded ctrl+c must never quit the process")
		}
	}
	if m.embedded == false {
		t.Fatal("embedded flag lost")
	}
	// Non-embedded behavior is unchanged: ctrl+c still quits.
	m2 := New(nil, fakeProbes(nil, nil, nil, "v1"))
	nm2, cmd2 := m2.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	_ = nm2
	if cmd2 == nil {
		t.Fatal("standalone ctrl+c must still quit")
	}
	if _, ok := cmd2().(tea.QuitMsg); !ok {
		t.Fatal("standalone ctrl+c must produce tea.Quit")
	}
}

// TestPasswordModeStoresRefreshToken pins the auto-refresh plumbing on the
// login path: the probe stores the login's refresh token (the HttpOnly
// orchicon_refresh Set-Cookie value surfaced through LoginResponse) in the
// profile so the client can refresh the 900s access token for 24h.
func TestPasswordModeStoresRefreshToken(t *testing.T) {
	m := New(nil, ProbeFuncs{
		Versionz: func(ctx context.Context, baseURL string, insecure bool) (*client.VersionzResponse, error) {
			return &client.VersionzResponse{Version: "v9"}, nil
		},
		ListProjects: func(ctx context.Context, baseURL, token string, insecure bool) error { return errors.New("unused") },
		LocalLogin: func(ctx context.Context, baseURL, username, password string, insecure bool) (*client.LoginResponse, error) {
			return &client.LoginResponse{AccessToken: "acc-xyz", ExpiresIn: 900, RefreshToken: "refresh-tok-24h"}, nil
		},
	})
	m.SetEmbedded()
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
	if res.Profile.RefreshToken != "refresh-tok-24h" {
		t.Fatalf("refresh token must be stored for auto-refresh, got %q", res.Profile.RefreshToken)
	}
	if res.Profile.Token != "acc-xyz" {
		t.Fatalf("token = %q", res.Profile.Token)
	}
	// An API-key switch clears the stored refresh token (it belongs to the
	// password-mode session).
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlA})
	m2 := nm.(Model)
	if !m2.authAPI {
		t.Fatal("ctrl+a must toggle to api-key mode")
	}
	probe, _ := m2.probe(context.Background())
	if probe != nil && probe.Profile.RefreshToken != "" {
		t.Fatalf("api-key profile must not carry a password-mode refresh token: %q", probe.Profile.RefreshToken)
	}
}

// TestStoredRefreshCarriedThroughEdit pins: a re-connect of an existing
// password profile (no re-login needed if fields unchanged) keeps the
// stored refresh token; an API-key switch drops it.
func TestStoredRefreshCarriedThroughEdit(t *testing.T) {
	m := New(&config.Profile{Name: "default", URL: "https://orch.example.com", AuthMethod: config.AuthPassword, Token: "acc", RefreshToken: "r-24h"}, fakeProbes(nil, nil, nil, "v1"))
	if m.storedRefresh != "r-24h" {
		t.Fatalf("storedRefresh = %q, want r-24h carried from the profile", m.storedRefresh)
	}
	// Switch to api-key mode → refresh token dropped.
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlA})
	m = nm.(Model)
	if m.storedRefresh != "" {
		t.Fatalf("api-key switch must drop the stored refresh token, got %q", m.storedRefresh)
	}
}

// TestViewNamesInPlaceContract pins the operator-facing wording: the
// password-mode hint must promise the in-place auto-refresh (never
// "exit and re-run orch").
func TestViewNamesInPlaceContract(t *testing.T) {
	m := New(nil, fakeProbes(nil, nil, nil, "v1"))
	m.authAPI = false // the password-mode hint carries the in-place promise
	view := m.View()
	if strings.Contains(view, "re-run") || strings.Contains(view, "re-run orch") {
		t.Fatalf("the screen must never tell the operator to exit and re-run: %s", view)
	}
	if !strings.Contains(view, "auto-refresh in place") {
		t.Fatalf("password hint must name the in-place auto-refresh: %s", view)
	}
}
