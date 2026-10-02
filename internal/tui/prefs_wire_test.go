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

// THE RAIL'S WIDTH SURVIVES A RESTART. This is the operator's report end to end: drag (or chord) the width,
// then "leave orch and come back in" — a NEW App over the SAME sandboxed config dir — and the RESTARTED
// model must render the pane at the chosen width.
//
// The assertion is on the RESTARTED model's COMPOSED view, not on the struct: a field that round-trips
// through the file but is never consulted by diffPaneWidth would look correct in a struct assertion and
// draw the wrong pane.
func TestDiffRailWidthSurvivesARelaunch(t *testing.T) {
	path := useTempConfigDir(t)

	// Session one: the operator sizes the rail.
	m := newDiffRailApp(t, 240, 40)
	m.setDiffPaneW(96)
	m.persistDiffRailWidth()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("resizing wrote no config file (%s): %v — the width cannot come back", path, err)
	}
	if got := contentStartColumn(t, m); got != 96 {
		t.Fatalf("session one draws the pane %d cells wide, want the set 96", got)
	}

	// Session two: a NEW App, constructed the way a relaunch is, at the SAME terminal size.
	m2 := newDiffRailApp(t, 240, 40)
	if m2.diffPaneW != 96 {
		t.Fatalf("a fresh session restored override %d, want 96 — the width did not come back", m2.diffPaneW)
	}
	if got := contentStartColumn(t, m2); got != 96 {
		t.Errorf("the RESTARTED model draws the pane %d cells wide, want the persisted 96", got)
	}
	if got := m2.contentWidth(); got != 240-96 {
		t.Errorf("the restarted model's contentWidth is %d, want %d", got, 240-96)
	}
}

// RESET-TO-AUTO IS REMEMBERED TOO: a width the operator abandoned must not come back on the next launch.
func TestDiffRailResetToAutoSurvivesARelaunch(t *testing.T) {
	path := useTempConfigDir(t)

	m := newDiffRailApp(t, 240, 40)
	m.setDiffPaneW(96)
	m.persistDiffRailWidth()

	// The operator resets to auto.
	if !m.diffRailWidthReset() {
		t.Fatal("the reset was refused with the pane open")
	}

	// The FILE no longer pins a width...
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "diff_rail_width") {
		t.Errorf("a reset-to-auto left the width in the file:\n%s", data)
	}

	// ...and the restarted model returns to the PROPORTIONAL width.
	m2 := newDiffRailApp(t, 240, 40)
	if m2.diffPaneW != 0 {
		t.Errorf("the restarted model restored override %d after a reset, want 0 (auto)", m2.diffPaneW)
	}
	if got, want := contentStartColumn(t, m2), m2.diffPaneAutoWidth(); got != want {
		t.Errorf("the restarted model draws %d cells after a reset, want the auto width %d", got, want)
	}
}
