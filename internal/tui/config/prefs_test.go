package config

// prefs_test.go — the display preferences that have to survive a relaunch.
//
// The operator: "Conversation categories don't stay collapsed when you leave orch and come back in."
//
// The collapse lived in a session-only map, so a restart forgot it. These pin the file format that fixes
// that — and, more importantly, that adding it cannot DAMAGE the credentials the same file holds. A
// display preference is the least important thing in that document and must never be able to stop the
// operator connecting.

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A CREDENTIAL SURVIVES A PREFERENCE ROUND TRIP. This is the assertion that matters: the same
// Load/Save path writes tokens, so a preference that rendered or parsed badly could take the
// credential with it.
func TestCollapsedGroupsRoundTripWithoutDisturbingCredentials(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")

	in := &Config{
		Active: "default",
		Profiles: map[string]*Profile{
			"default": {URL: "https://orch.example.com", AuthMethod: AuthAPIKey, Token: "oc_supersecret"},
		},
		Theme:           "light",
		CollapsedGroups: []string{"conversations:cat-1", "conversations:cat-2"},
	}
	if err := Save(path, in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !reflect.DeepEqual(out.CollapsedGroups, in.CollapsedGroups) {
		t.Errorf("collapsed groups = %v, want %v", out.CollapsedGroups, in.CollapsedGroups)
	}
	if out.Theme != "light" {
		t.Errorf("theme = %q, want light — the preference beside it must not be disturbed", out.Theme)
	}
	p := out.Profiles["default"]
	if p == nil {
		t.Fatal("the profile was lost")
	}
	if p.Token != "oc_supersecret" {
		t.Errorf("token = %q, want the credential intact", p.Token)
	}
	if p.URL != "https://orch.example.com" {
		t.Errorf("url = %q, want the credential intact", p.URL)
	}
}

// NO COLLAPSED GROUPS WRITES NO KEY, so an operator who has never collapsed anything does not have a
// line appear in their config (and older builds keep reading the file unchanged).
func TestNoCollapsedGroupsOmitsTheKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := Save(path, &Config{Active: "default", Profiles: map[string]*Profile{}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(data), "collapsed_groups") {
		t.Errorf("an empty set still wrote the key:\n%s", data)
	}
}

// THE OUTPUT IS SORTED, so re-saving unchanged state is byte-identical. Without this a map's iteration
// order would churn the file on every toggle, and the diff would look like a change when nothing did.
func TestCollapsedGroupsRenderSorted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	cfg := &Config{
		Profiles:        map[string]*Profile{},
		CollapsedGroups: []string{"conversations:z", "conversations:a", "conversations:m"},
	}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(data)
	za := strings.Index(s, `"conversations:z"`)
	am := strings.Index(s, `"conversations:a"`)
	mm := strings.Index(s, `"conversations:m"`)
	if !(am < mm && mm < za) {
		t.Errorf("the list is not sorted (positions a=%d m=%d z=%d):\n%s", am, mm, za, s)
	}
}

// A MALFORMED LIST IS TOLERATED, not fatal. A display preference must never be able to stop the
// operator connecting, so a bad line drops the entry rather than failing the whole load.
func TestMalformedCollapsedGroupsDoesNotFailTheLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	// A hand-edited file with a trailing comma and an unquoted entry — the two mistakes a person makes.
	body := "active = \"default\"\ncollapsed_groups = [\"conversations:cat-1\", , oops]\n" +
		"\n[profiles.default]\nurl = \"https://orch.example.com\"\ntoken = \"oc_x\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("a malformed preference list must not fail the load: %v", err)
	}
	if cfg.Profiles["default"] == nil || cfg.Profiles["default"].Token != "oc_x" {
		t.Fatalf("the profile did not survive a malformed preference line: %+v", cfg.Profiles)
	}
	if len(cfg.CollapsedGroups) != 1 || cfg.CollapsedGroups[0] != "conversations:cat-1" {
		t.Errorf("collapsed groups = %v, want just the well-formed entry", cfg.CollapsedGroups)
	}
}

// A QUOTED COMMA stays part of one entry, so a category id containing punctuation cannot split in two.
func TestCollapsedGroupsSurvivePunctuation(t *testing.T) {
	list := []string{`conversations:cat, with comma`, `conversations:quote"inside`, `conversations:plain`}
	got := parseList("[" + quoteList(list) + "]")
	if !reflect.DeepEqual(got, list) {
		t.Errorf("round trip = %v, want %v", got, list)
	}
}
