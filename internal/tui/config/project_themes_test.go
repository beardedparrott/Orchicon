package config

// project_themes_test.go — the project->palette bindings, same contract as the other display preferences
// in prefs_test.go: a round trip that does not disturb credentials, a sorted rewrite, an omitted key when
// empty, and a malformed/unknown entry that degrades rather than failing the whole load.

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestProjectThemesRoundTripWithoutDisturbingCredentials(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")

	in := &Config{
		Active: "default",
		Profiles: map[string]*Profile{
			"default": {URL: "https://orch.example.com", AuthMethod: AuthAPIKey, Token: "oc_supersecret"},
		},
		Theme:         "light",
		ProjectThemes: map[string]string{"proj-a": "tokyo-night", "proj-b": "catppuccin-latte"},
	}
	if err := Save(path, in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !reflect.DeepEqual(out.ProjectThemes, in.ProjectThemes) {
		t.Errorf("project themes = %v, want %v", out.ProjectThemes, in.ProjectThemes)
	}
	if out.Theme != "light" {
		t.Errorf("theme = %q, want light — the preference beside it must not be disturbed", out.Theme)
	}
	p := out.Profiles["default"]
	if p == nil || p.Token != "oc_supersecret" {
		t.Fatalf("the credential did not survive: %+v", p)
	}
}

// NO BINDINGS WRITES NO SECTION, so an operator who has never bound a project does not get a line in
// their config.
func TestNoProjectThemesOmitsTheSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := Save(path, &Config{Active: "default", Profiles: map[string]*Profile{}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(data), "project_themes") {
		t.Errorf("an empty map still wrote the section:\n%s", data)
	}
}

// THE SECTION IS SORTED BY PROJECT ID, so re-saving unchanged state is byte-identical — the same
// reason collapsed_groups sorts.
func TestProjectThemesRenderSortedAndByteIdentical(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	cfg := &Config{
		Profiles:      map[string]*Profile{},
		ProjectThemes: map[string]string{"zz": "dark", "aa": "light", "mm": "teal"},
	}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(first)
	az := strings.Index(s, "zz =")
	aa := strings.Index(s, "aa =")
	am := strings.Index(s, "mm =")
	if !(aa < am && am < az) {
		t.Errorf("the section is not sorted (positions aa=%d mm=%d zz=%d):\n%s", aa, am, az, s)
	}

	// Load and re-save with no change: the bytes must match exactly.
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := Save(path, loaded); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after resave: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("rewriting unchanged bindings was not byte-identical:\n--- first ---\n%s\n--- second ---\n%s",
			first, second)
	}
}

// A MALFORMED LINE IN THE SECTION IS TOLERATED. A display preference must never be able to stop the
// operator connecting, so a bad line drops that one binding rather than failing the whole load.
func TestMalformedProjectThemeEntryDoesNotFailTheLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	body := "active = \"default\"\n" +
		"\n[project_themes]\nproj-a = \"dark\"\nnot-a-valid-line-at-all\nproj-b = \"light\"\n" +
		"\n[profiles.default]\nurl = \"https://orch.example.com\"\ntoken = \"oc_x\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("a malformed binding must not fail the load: %v", err)
	}
	if cfg.Profiles["default"] == nil || cfg.Profiles["default"].Token != "oc_x" {
		t.Fatalf("the profile did not survive a malformed binding line: %+v", cfg.Profiles)
	}
	want := map[string]string{"proj-a": "dark", "proj-b": "light"}
	if !reflect.DeepEqual(cfg.ProjectThemes, want) {
		t.Errorf("project themes = %v, want %v", cfg.ProjectThemes, want)
	}
}

// AN UNKNOWN PALETTE NAME IS STORED VERBATIM — config.parse does not validate it (see config.go's note on
// Theme); degrading an unknown palette to the default happens at Use() time, in the TUI, not here.
func TestProjectThemeStoresUnknownPaletteVerbatim(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	body := "active = \"default\"\n\n[project_themes]\nproj-a = \"not-a-real-palette\"\n" +
		"\n[profiles.default]\nurl = \"https://orch.example.com\"\ntoken = \"oc_x\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ProjectThemes["proj-a"] != "not-a-real-palette" {
		t.Errorf("project theme = %q, want the verbatim unknown name", cfg.ProjectThemes["proj-a"])
	}
}
