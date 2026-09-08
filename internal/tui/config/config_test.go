package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingFileIsEmptyConfig(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope", "config"))
	if err != nil {
		t.Fatalf("missing file must not error: %v", err)
	}
	if cfg.Active != "" || len(cfg.Profiles) != 0 {
		t.Fatalf("expected empty config, got %+v", cfg)
	}
}

func TestSavePermsAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DirName, FileName)
	cfg := &Config{
		Active: "prod",
		Profiles: map[string]*Profile{
			"prod": {Name: "prod", URL: "https://orch.example.com/", AuthMethod: AuthAPIKey, Token: "oc_secret", Username: ""},
			"dev":  {Name: "dev", URL: "http://localhost:8080", AuthMethod: AuthPassword, Token: "tok", Username: "me", InsecureSkipVerify: true},
		},
	}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config file perms = %o, want 600 (holds credentials)", perm)
	}
	dst, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dst.Mode().Perm(); perm != 0o700 {
		t.Fatalf("config dir perms = %o, want 700", perm)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Active != "prod" || len(got.Profiles) != 2 {
		t.Fatalf("round-trip mismatch: active=%q profiles=%d", got.Active, len(got.Profiles))
	}
	p := got.Profiles["dev"]
	if p.URL != "http://localhost:8080" || p.AuthMethod != AuthPassword || p.Username != "me" || !p.InsecureSkipVerify {
		t.Fatalf("dev profile mismatch: %+v", p)
	}
	if got.Profiles["prod"].Token != "oc_secret" {
		t.Fatalf("token round-trip failed")
	}
}

func TestResolveEnvWinsAndNeverPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	fileCfg := &Config{
		Active:   "file",
		Profiles: map[string]*Profile{"file": {Name: "file", URL: "https://file.example.com", AuthMethod: AuthAPIKey, Token: "file-token"}},
	}
	if err := Save(path, fileCfg); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	beforeBytes, _ := os.ReadFile(path)

	t.Setenv(EnvURL, "https://env.example.com/")
	t.Setenv(EnvToken, "env-token")
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := Resolve(got)
	if p == nil || p.URL != "https://env.example.com" || p.Token != "env-token" {
		t.Fatalf("env override lost: %+v", p)
	}
	if p.Name != "env" {
		t.Fatalf("env profile must be ephemeral, got name %q", p.Name)
	}
	// URL trailing slash must be trimmed.
	if p.URL != "https://env.example.com" {
		t.Fatalf("env URL not normalized: %q", p.URL)
	}
	afterBytes, _ := os.ReadFile(path)
	if string(beforeBytes) != string(afterBytes) {
		t.Fatal("resolve must not mutate the config file when env is set")
	}
	_ = before
}

func TestResolveFileProfile(t *testing.T) {
	cfg := &Config{
		Active:   "prod",
		Profiles: map[string]*Profile{"prod": {Name: "prod", URL: "https://p.example.com/", AuthMethod: AuthAPIKey, Token: "t1"}},
	}
	p := Resolve(cfg)
	if p == nil || p.URL != "https://p.example.com" || p.Token != "t1" || p.Name != "prod" {
		t.Fatalf("file profile resolve mismatch: %+v", p)
	}
}

func TestResolveEnvURLWithTokenFromProfile(t *testing.T) {
	// ORCHICON_URL alone + active file profile: env URL wins, file token is
	// NOT applied to the ephemeral env profile (different instance).
	t.Setenv(EnvURL, "https://other.example.com")
	t.Setenv(EnvToken, "")
	cfg := &Config{
		Active:   "prod",
		Profiles: map[string]*Profile{"prod": {Name: "prod", URL: "https://p.example.com", AuthMethod: AuthAPIKey, Token: "t1"}},
	}
	p := Resolve(cfg)
	if p == nil || p.URL != "https://other.example.com" || p.Token != "" {
		t.Fatalf("env-with-token-profile resolve mismatch: %+v", p)
	}
	if p.AuthMethod != "" {
		t.Fatalf("env profile without token must leave auth method unset for the connection screen, got %q", p.AuthMethod)
	}
}

func TestResolveNothingConfigured(t *testing.T) {
	t.Setenv(EnvURL, "")
	t.Setenv(EnvToken, "")
	if p := Resolve(&Config{Profiles: map[string]*Profile{}}); p != nil {
		t.Fatalf("expected nil profile for empty config, got %+v", p)
	}
	if p := Resolve(nil); p != nil {
		t.Fatalf("expected nil profile for nil config, got %+v", p)
	}
}
