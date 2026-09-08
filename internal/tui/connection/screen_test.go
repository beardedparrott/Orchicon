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
	if !strings.Contains(err.Error(), "read scopes") || !strings.Contains(err.Error(), "execution:read") {
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
