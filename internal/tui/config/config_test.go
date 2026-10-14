package config

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// Regression for the operator's "themes are not saving when you exit orch and
// re-enter". The palette lived only inside [profiles.<active>], so it was lost
// whenever no profile was written — env-driven sessions (which resolve to a
// synthetic "env" profile that is deliberately never persisted) and first runs
// with no config file. A display preference must not depend on a credential
// being saved.
func TestThemePersistsWithoutAProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)

	// Simulates an env-driven session: a config with NO profiles at all.
	cfg := &Config{Profiles: map[string]*Profile{}, Theme: "gruvbox-dark"}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Theme != "gruvbox-dark" {
		t.Fatalf("theme = %q after a round trip with no profiles, want gruvbox-dark", got.Theme)
	}
	if len(got.Profiles) != 0 {
		t.Fatalf("round trip invented %d profiles", len(got.Profiles))
	}

	// And it still parses when profiles ARE present (the top-level key must not
	// be confused with the legacy per-profile one).
	cfg2 := &Config{
		Active: "default",
		Profiles: map[string]*Profile{
			"default": {Name: "default", URL: "http://x", Token: "t", Theme: "legacy"},
		},
		Theme: "forest",
	}
	if err := Save(path, cfg2); err != nil {
		t.Fatalf("save 2: %v", err)
	}
	got2, err := Load(path)
	if err != nil {
		t.Fatalf("load 2: %v", err)
	}
	if got2.Theme != "forest" {
		t.Fatalf("top-level theme = %q, want forest", got2.Theme)
	}
	if p := got2.Profiles["default"]; p == nil || p.Theme != "legacy" {
		t.Fatalf("legacy per-profile theme lost: %+v", p)
	}
}

// ORCHICON_CONFIG_DIR relocates the config, so a machine whose $HOME is not
// writable can still persist anything at all.
//
// The default path is $HOME/.orchicon/config. In the dev container $HOME is
// ROOT-OWNED, so the directory cannot be created and EVERY save fails — profile,
// token, theme, newline mode — which is why themes stopped surviving rebuilds.
// The TUI already reports the failure ("this run only — cannot write <path>"),
// but reporting is not fixing: this variable is the way to say where the config
// may live. It must be a directory the user owns AND that survives a container
// restart (/tmp is a tmpfs the restart wipes).
func TestDefaultPathHonoursEnvConfigDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvConfigDir, dir)
	got, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	if want := filepath.Join(dir, FileName); got != want {
		t.Fatalf("DefaultPath() = %q, want %q", got, want)
	}
	// And a save round-trip through that path actually persists.
	cfg, err := Load(got)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]*Profile{}
	}
	cfg.Active = "default"
	cfg.Profiles["default"] = &Profile{Name: "default", Theme: "gruvbox-dark"}
	if err := Save(got, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	back, err := Load(got)
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}
	if p := back.Profiles["default"]; p == nil || p.Theme != "gruvbox-dark" {
		t.Fatalf("theme did not round-trip: %+v", p)
	}
}

// Without the override the historical $HOME-derived path is unchanged, so this
// is a no-op everywhere it is not needed.
func TestDefaultPathFallsBackToHome(t *testing.T) {
	t.Setenv(EnvConfigDir, "")
	got, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	if want := filepath.Join(home, DirName, FileName); got != want {
		t.Fatalf("DefaultPath() = %q, want %q", got, want)
	}
}

// The diff rail's width is a TOP-LEVEL display preference beside theme/collapsed_groups, so it must
// round-trip through the same file — render writes it, parse reads it — and 0 (auto) must write NOTHING.
func TestDiffRailWidthRoundTripsWithoutAProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)

	// Simulates an env-driven session: a config with NO profiles at all.
	cfg := &Config{Profiles: map[string]*Profile{}, DiffRailWidth: 72}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.DiffRailWidth != 72 {
		t.Fatalf("DiffRailWidth = %d after a round trip, want 72", got.DiffRailWidth)
	}
	if len(got.Profiles) != 0 {
		t.Fatalf("round trip invented %d profiles", len(got.Profiles))
	}

	// AUTO (0) OMITS the key, the same contract collapsed_groups uses: a width the operator never chose
	// must not be pinned in the file.
	if err := Save(path, &Config{Profiles: map[string]*Profile{}}); err != nil {
		t.Fatalf("save auto: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "diff_rail_width") {
		t.Errorf("auto mode wrote a diff_rail_width key:\n%s", data)
	}
	auto, err := Load(path)
	if err != nil {
		t.Fatalf("load auto: %v", err)
	}
	if auto.DiffRailWidth != 0 {
		t.Errorf("auto reloaded as %d, want 0", auto.DiffRailWidth)
	}
}

// cfgWithToken builds a one-profile config whose token is the given string, so a test
// can tell two documents apart by reading the file.
func cfgWithToken(tok string) *Config {
	return &Config{
		Active:   "default",
		Profiles: map[string]*Profile{"default": {Name: "default", URL: "http://localhost:8080", AuthMethod: AuthAPIKey, Token: tok}},
	}
}

// THE WRITE REPLACES THE FILE; IT DOES NOT TRUNCATE IT IN PLACE.
//
// This is what makes a second orch process safe to run, and it is observable WITHOUT the
// platform's inode API — which matters, because this repo builds for Windows too and
// syscall.Stat_t.Ino does not exist there. A handle opened BEFORE a save keeps pointing
// at the OLD file, so after the save it must still read the OLD complete document. Under
// os.WriteFile the same handle would see the NEW bytes, and a reader that chanced on the
// window between truncate and write would see a PARTIAL document — which the launch path
// treats as corruption, starts from an empty config, and then SAVES over the other
// instance's profile.
func TestSaveReplacesTheFileInsteadOfTruncatingIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := Save(path, cfgWithToken("first")); err != nil {
		t.Fatalf("save: %v", err)
	}

	held, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer held.Close()
	before, err := io.ReadAll(held)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(before), "first") {
		t.Fatalf("fixture: the held handle did not read the first document:\n%s", before)
	}

	if err := Save(path, cfgWithToken("second")); err != nil {
		t.Fatalf("second save: %v", err)
	}

	if _, err := held.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	after, err := io.ReadAll(held)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if !strings.Contains(string(after), "first") {
		t.Fatalf("the handle opened before the save now reads the NEW document — the save truncated "+
			"the file in place instead of replacing it, which is exactly the window in which a "+
			"second orch process can read a half-written config:\n%s", after)
	}

	// The path itself names the new document, and nothing extra was left behind.
	got, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Profiles["default"] == nil || got.Profiles["default"].Token != "second" {
		t.Fatalf("the path does not name the new document: %+v", got.Profiles)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("a temp file was left behind: %s (it holds credentials)", e.Name())
		}
	}
}

// PreserveUnreadable must MOVE the file, so the save that follows cannot destroy it, and
// must not overwrite a backup an earlier failure already made.
func TestPreserveUnreadableMovesTheFileAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	const contents = "active = \"default\"\n\n[profiles.default]\nurl = \"http://localhost:8091\"\ntoken = \"keep-me\"\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	backup, err := PreserveUnreadable(path)
	if err != nil {
		t.Fatalf("preserve: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the original is still at %s — the next save would overwrite it", path)
	}
	kept, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("the preserved copy is not readable: %v", err)
	}
	if string(kept) != contents {
		t.Errorf("the preserved copy differs from what was there:\n%s", kept)
	}

	// A second unreadable config must not land on the first backup.
	if err := os.WriteFile(path, []byte("active = \"other\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := PreserveUnreadable(path)
	if err != nil {
		t.Fatalf("second preserve: %v", err)
	}
	if second == backup {
		t.Fatalf("the second backup reused the first one's name (%s), destroying it", backup)
	}
	if got, err := os.ReadFile(backup); err != nil || string(got) != contents {
		t.Errorf("the first backup was damaged by the second: %q (%v)", got, err)
	}
}

// The parser still REJECTS a genuinely unknown key: the new case must not have loosened the default.
//
// That strictness is deliberate and is NOT relaxed by the multi-instance work: the
// per-key cases above are what make a new field a compile-visible obligation, and a
// tolerated typo would silently drop a credential. What multi-instance needed instead is
// that an unreadable file is PRESERVED rather than overwritten — see
// config.PreserveUnreadable and the launch path's use of it — so the strictness costs an
// operator a re-login and never an unexplained wipe of the other instance's profile.
func TestUnknownKeyStillRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte("active = \"x\"\nsome_future_key = \"v\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted an unknown key — a typo'd or foreign key must be a hard error, not a silent " +
			"default (the property that makes the parser case mandatory for every new key)")
	}
}
