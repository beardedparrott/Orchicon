package tui

// prefs.go — DISPLAY PREFERENCES THAT OUTLIVE THE PROCESS.
//
// The operator: "Conversation categories don't stay collapsed when you leave orch and come back in."
//
// The collapse WAS deliberately session-only, and the comment on convCollapsed said so by analogy: the
// GUI keeps its per-page collapse in local state, not on the server. The analogy was wrong on the detail
// that matters — the GUI's "local state" is in the BROWSER, and localStorage survives a relaunch. So
// "local" there still means REMEMBERED, and the TUI's session-only map meant a restart forgot.
//
// WHY THE CONFIG FILE AND NOT THE SERVER. These are per-OPERATOR display preferences, exactly like the
// palette: which folders one person keeps closed is not a tenant fact, and two operators on the same
// tenant should not fight over it. They go in ~/.orchicon/config beside `theme`, at the TOP LEVEL rather
// than inside a profile, for the reason the theme's own note gives — a session launched from
// ORCHICON_URL/ORCHICON_TOKEN resolves to a synthetic "env" profile that is deliberately never written,
// so a preference stored inside a profile would silently fail to persist in exactly the setup most
// likely to be used for a real deployment.
//
// EVERY FAILURE HERE IS SILENT-AND-HARMLESS BY DESIGN. A preference that cannot be read or written must
// never stop the operator connecting or cost them their session — so a bad config load leaves the
// in-memory preference alone and says nothing. The one thing that must NOT happen is the reverse: a
// preference write must never be able to corrupt a credential, which is why this reads-modifies-writes
// through the same Load/Save the theme does rather than rewriting the file from scratch.

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/beardedparrott/orchicon/internal/tui/config"
)

// groupPrefKey namespaces a collapsed grouping by the PAGE it belongs to.
//
// A category id is unique within a target type but the TUI groups THREE lists (conversations, workers,
// workflows) from three independent id spaces, and the same id could in principle appear in two of them.
// Namespacing by page costs one string and removes the question.
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
//
// Keep-and-merge rather than replace: this runs once at startup, but a caller that ran it later (after
// the operator had already toggled something) must not have that toggle discarded by a stale read.
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
//
// It re-derives the WHOLE list from the live map on every write rather than appending to what is on disk:
// the map is the truth about this session's page, and an append would keep resurrecting a folder the
// operator has since expanded. Other pages' keys are preserved (read-modify-write), so the workers and
// workflows lists can adopt the same mechanism without either clobbering the other.
//
// A write failure is REPORTED rather than swallowed, but it does not undo the toggle: the operator gets
// the collapse they asked for this session either way, and the notice tells them whether it will come
// back (the same honesty the theme's persistence chose).
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
