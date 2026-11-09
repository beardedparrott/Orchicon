package tui

// prefs.go — DISPLAY PREFERENCES THAT OUTLIVE THE PROCESS.

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/beardedparrott/orchicon/internal/tui/config"
)

// groupPrefKey namespaces a collapsed grouping by the PAGE it belongs to.
func groupPrefKey(page, catID string) string { return page + ":" + catID }

// convGroupPage is the key namespace for the conversations rail.
const convGroupPage = "conversations"

// collapsePrefsDisabled is set under `go test`, so a test never reads or writes the DEVELOPER's own
// ~/.orchicon/config. See collapsedPrefsPath.
var collapsePrefsDisabled = false

// collapsedPrefsPath returns the config path for the fold preferences, or "" when they must not be
// touched at all.
//
// THE ZERO VALUE UNDER TEST IS THE POINT. An earlier version read config.DefaultPath() unconditionally,
// and because it runs from NewApp that meant a shell built by a TEST inherited the DEVELOPER's own
// collapse state. The symptom was a test that passed on one machine and failed on another: an operator
// who had collapsed a folder had "conversations:cat-1" in their config, a fixture using that same
// category id then came up collapsed, and a test about marking a folder's members marked nothing.
//
// Precedence, most explicit first:
//
//  1. under `go test` with prefs DISABLED — "", meaning isolated. This is the DEFAULT in a test binary,
//     derived from the binary itself rather than from each test remembering to opt out, so a test written
//     later cannot forget. It now outranks the env var below, and the reason is a change worth stating:
//     the suite sets ORCHICON_CONFIG_DIR for the WHOLE package to contain OTHER config writers (see
//     isolate_test.go). If a redirected value also counted as a prefs opt-in, every test in the package
//     would share one sandbox file — a collapse in one test would collapse a folder in the next, which is
//     the exact cross-test contamination this function was written to stop. A SANDBOX IS NOT AN OPT-IN.
//  2. ORCHICON_CONFIG_DIR — a deliberate relocation, honoured when prefs are enabled (a test that cleared
//     the flag, or a real run). This is how a test that WANTS to exercise persistence opts in.
//  3. otherwise the real config path.
func collapsedPrefsPath() string {
	if collapsePrefsDisabled {
		return ""
	}
	if dir := strings.TrimSpace(os.Getenv("ORCHICON_CONFIG_DIR")); dir != "" {
		return filepath.Join(dir, config.FileName)
	}
	path, err := config.DefaultPath()
	if err != nil {
		return ""
	}
	return path
}

// loadCollapsedGroups reads the persisted collapsed set into m.convCollapsed, keeping whatever is
// ALREADY in memory.
func (m *App) loadCollapsedGroups() {
	path := collapsedPrefsPath()
	if path == "" {
		return // no config location (or a test): nothing persisted to restore
	}
	cfg, err := config.Load(path)
	if err != nil {
		return
	}
	if len(cfg.CollapsedGroups) == 0 {
		return
	}
	if m.convCollapsed == nil {
		m.convCollapsed = map[string]bool{}
	}
	prefix := convGroupPage + ":"
	for _, key := range cfg.CollapsedGroups {
		catID, ok := strings.CutPrefix(key, prefix)
		if !ok || catID == "" {
			continue // another page's grouping
		}
		if !m.convCollapsed[catID] {
			m.convCollapsed[catID] = true
		}
	}
}

// persistCollapsedGroups writes the conversations rail's collapsed set to the config file.
func (m *App) persistCollapsedGroups() {
	path := collapsedPrefsPath()
	if path == "" {
		return // no config location (or a test): nothing to write, and nothing to report
	}
	cfg, err := config.Load(path)
	if err != nil {
		m.dock.SetNotice("folder collapsed (this run only — cannot read " + path + ")")
		return
	}
	// Keep every OTHER page's keys, drop this page's, then re-add this page's from the live map.
	prefix := convGroupPage + ":"
	kept := make([]string, 0, len(cfg.CollapsedGroups)+len(m.convCollapsed))
	for _, key := range cfg.CollapsedGroups {
		if !strings.HasPrefix(key, prefix) {
			kept = append(kept, key)
		}
	}
	for catID, closed := range m.convCollapsed {
		if closed && catID != "" {
			kept = append(kept, groupPrefKey(convGroupPage, catID))
		}
	}
	cfg.CollapsedGroups = kept
	if err := config.Save(path, cfg); err != nil {
		m.dock.SetNotice("folder collapsed (this run only — cannot write " + path + ": " + err.Error() + ")")
		return
	}
}

// diffRailPrefNote is the noun both the load and the write failure report under, so the operator sees ONE
// phrase for the preference regardless of which direction failed.
const diffRailPrefNote = "diff rail width"

// It runs from NewApp, so it MUST honour collapsedPrefsPath's sandbox rules (empty under `go test`
// unless the test opted in).
func (m *App) loadDiffRailWidth() {
	path := collapsedPrefsPath()
	if path == "" {
		return // no config location (or a test): nothing persisted to restore
	}
	cfg, err := config.Load(path)
	if err != nil {
		return
	}
	if cfg.DiffRailWidth > 0 {
		m.diffPaneW = cfg.DiffRailWidth
	}
}

// persistDiffRailWidth writes the operator's diff-rail width (0 = auto) to the config file.
func (m *App) persistDiffRailWidth() {
	path := collapsedPrefsPath()
	if path == "" {
		return // no config location (or a test): nothing to write, and nothing to report
	}
	cfg, err := config.Load(path)
	if err != nil {
		m.dock.SetNotice(diffRailPrefNote + " (this run only — cannot read " + path + ")")
		return
	}
	cfg.DiffRailWidth = m.diffPaneW
	if err := config.Save(path, cfg); err != nil {
		m.dock.SetNotice(diffRailPrefNote + " (this run only — cannot write " + path + ": " + err.Error() + ")")
	}
}

// listSharePrefNote is the noun the split's load/write failures report under.
const listSharePrefNote = "list/detail split"

// loadListShare restores the operator's master-detail split from the config file, on the same
// silent-on-failure rule as the other display preferences.
func (m *App) loadListShare() {
	path := collapsedPrefsPath()
	if path == "" {
		return
	}
	cfg, err := config.Load(path)
	if err != nil {
		return
	}
	m.listSharePct = cfg.ListSharePct // 0 = the default split
}

// persistListShare writes the split (0 = default) back to the config file, read-modify-write through the same
// config.Load/Save the other preferences use so it cannot clobber a credential.
func (m *App) persistListShare() {
	path := collapsedPrefsPath()
	if path == "" {
		return // no config location (or a test): nothing to write
	}
	cfg, err := config.Load(path)
	if err != nil {
		m.dock.SetNotice(listSharePrefNote + " (this run only — cannot read " + path + ")")
		return
	}
	cfg.ListSharePct = m.listSharePct
	if err := config.Save(path, cfg); err != nil {
		m.dock.SetNotice(listSharePrefNote + " (this run only — cannot write " + path + ": " + err.Error() + ")")
	}
}
