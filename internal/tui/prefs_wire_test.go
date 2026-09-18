package tui

// prefs_wire_test.go — the fold state REACHES THE FILE and comes BACK.
//
// The operator: "Conversation categories don't stay collapsed when you leave orch and come back in."
//
// The file format is pinned in the config package; what is pinned HERE is the wiring, which is the layer
// that fails silently: a toggle that updates the in-memory map and never writes, or a write that is never
// read back, both look exactly like a working collapse until the next launch.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// useTempConfigDir points the config at a temp dir for the duration of the test, so a test never reads or
// writes the developer's real ~/.orchicon/config. It returns the path.
//
// IT ALSO ENABLES PREFS PERSISTENCE, which the test-binary default disables (see collapsedPrefsPath): the
// package-wide sandbox that contains OTHER config writers must not double as an opt-in to writing fold
// state, or every test in the package would share one file. A test that genuinely wants to exercise
// persistence says so here — and says it back afterwards.
func useTempConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ORCHICON_CONFIG_DIR", dir)
	prev := collapsePrefsDisabled
	collapsePrefsDisabled = false
	t.Cleanup(func() { collapsePrefsDisabled = prev })
	return filepath.Join(dir, "config")
}

// COLLAPSING WRITES THE FILE, and a fresh App reads it back. This is the operator's report, end to end:
// toggle, then "leave orch and come back in" (a new App over the same config).
func TestCollapsedFolderSurvivesARelaunch(t *testing.T) {
	path := useTempConfigDir(t)

	// Session one: the operator collapses a folder.
	m := newTestApp()
	if m.convCollapsed == nil {
		m.convCollapsed = map[string]bool{}
	}
	m.dock.SetNotice("")
	m.toggleConvFolder("cat-1")
	if !m.convCollapsed["cat-1"] {
		t.Fatal("the toggle did not close the folder in memory")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("collapsing wrote no config file (%s): %v — the state cannot come back", path, err)
	}

	// Session two: a NEW App, constructed the way a relaunch is.
	m2 := newTestApp()
	if !m2.convCollapsed["cat-1"] {
		t.Fatalf("a fresh session did not restore the collapsed folder (got %v) — this is the "+
			"operator's \"don't stay collapsed when you leave orch and come back in\"", m2.convCollapsed)
	}
}

// EXPANDING IS REMEMBERED TOO. A collapse that persists but an EXPAND that does not is worse than neither:
// the folder would come back closed every launch and the operator could not keep it open.
func TestExpandedFolderStaysExpanded(t *testing.T) {
	useTempConfigDir(t)

	m := newTestApp()
	if m.convCollapsed == nil {
		m.convCollapsed = map[string]bool{}
	}
	m.toggleConvFolder("cat-1") // close
	m.toggleConvFolder("cat-1") // re-open
	if m.convCollapsed["cat-1"] {
		t.Fatal("the second toggle did not re-open the folder")
	}

	m2 := newTestApp()
	if m2.convCollapsed["cat-1"] {
		t.Error("a folder the operator RE-OPENED came back closed — the expanded state is not being " +
			"written, so the operator can never keep a folder open")
	}
}

// A FOLDER THAT WAS NEVER TOUCHED IS EXPANDED. The file stores what is CLOSED, so a grouping created
// after the preference was written must not inherit a stale fold.
func TestUntouchedFolderIsExpanded(t *testing.T) {
	useTempConfigDir(t)
	m := newTestApp()
	m.loadCollapsedGroups()
	if len(m.convCollapsed) != 0 {
		t.Errorf("a fresh session started with folds: %v", m.convCollapsed)
	}
}

// A NON-CONVERSATION PAGE'S FOLDS ARE LEFT ALONE, so the workers and workflows lists can adopt the same
// mechanism without either clobbering the other's keys.
func TestCollapsingOnePageDoesNotClobberAnother(t *testing.T) {
	path := useTempConfigDir(t)

	// Seed a fold belonging to a different page.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(
		"active = \"default\"\ncollapsed_groups = [\"workers:cat-w\"]\n"+
			"\n[profiles.default]\nurl = \"https://orch.example.com\"\ntoken = \"oc_keepme\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := newTestApp()
	if m.convCollapsed == nil {
		m.convCollapsed = map[string]bool{}
	}
	m.toggleConvFolder("cat-c")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, `"workers:cat-w"`) {
		t.Errorf("collapsing a conversation folder dropped another page's fold:\n%s", got)
	}
	if !strings.Contains(got, `"conversations:cat-c"`) {
		t.Errorf("the conversation fold was not written:\n%s", got)
	}
	if !strings.Contains(got, "oc_keepme") {
		t.Errorf("the credential was lost by a preference write:\n%s", got)
	}
}
