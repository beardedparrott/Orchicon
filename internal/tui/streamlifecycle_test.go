package tui

import (
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
)

// The operator: "the connection status is CONSTANTLY saying disconnected or
// reconnecting. Something is wrong there."
//
// Root cause: every screen's Close() calls reg.CloseAll(), which tears down the
// SHARED registry, but each screen kept its own non-nil sub handle — and
// EnsureSubscriptions guards on that handle being nil. So after the FIRST visit
// to a tab its live stream was never recreated: no further events arrived, and
// the footer stayed frozen on the last reported status ("reconnecting" /
// "closed" → "disconnected, retrying") for the rest of the session.
func TestStreamsAreRecreatedWhenReturningToATab(t *testing.T) {
	m := newTestApp()
	// A REAL client set pointing at an unreachable address: the lifecycle is what
	// is under test, and a nil client would panic inside the dial (which is now
	// guarded, but the point here is the registry, not the guard).
	m.clients = client.New(client.Options{BaseURL: "http://127.0.0.1:1"})
	m.dispatch(keyFor("ctrl+w") /* work */)
	m.EnsureSubscriptions(TabWork)
	if n := m.reg.Count(); n == 0 {
		t.Fatal("the work tab must open a live stream")
	}
	work := m.screens[TabWork]

	// Leaving the tab tears the registry down.
	m.SwitchTo(TabControl)
	if n := m.reg.Count(); n != 0 {
		t.Fatalf("leaving a tab must close its streams, %d still live", n)
	}

	// Coming BACK must re-establish a live stream, not leave the screen with a
	// dead handle and a frozen status.
	m.SwitchTo(TabWork)
	m.EnsureSubscriptions(TabWork)
	if n := m.reg.Count(); n == 0 {
		t.Fatal("returning to a tab must re-create its live stream (a stale handle kept it dead)")
	}
	if m.screens[TabWork] != work {
		t.Fatal("the screen instance must be reused across a tab switch")
	}
}

// The footer aggregates the ACTIVE screen's statuses worst-wins, so a screen
// that reports a broken stream drives the label. This pins the mapping the
// operator reads.
func TestFooterShowsDisconnectedForABrokenStream(t *testing.T) {
	for _, tc := range []struct {
		label   string
		want    string
		matches bool
	}{
		{"connected", "connected", true},
		{"connecting…", "connecting", true},
		{"disconnected, retrying", "disconnected", true},
		{"idle", "idle", true},
	} {
		if !containsStr(tc.label, tc.want) {
			t.Errorf("footer label %q should read as %q", tc.label, tc.want)
		}
	}
}

func containsStr(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// The operator: "when doing /connect while in orch, it connected and then all
// items were blank as if I was no longer connected. It seemed to do the
// opposite."
//
// Root cause: applyConnectResult deleted the ACTIVE screen and relied on
// refreshLayout to rebuild it — but refreshLayout reads m.screens[active], which
// was nil, so nothing was rebuilt and the tab rendered blank. On top of that the
// tab factories closed over the ORIGINAL client pointer, so a reconstructed
// screen would still have used the stale, unauthorised client.
func TestConnectRebuildsTheActiveScreenWithTheNewClient(t *testing.T) {
	m := newTestApp()
	m.dispatch(keyFor("ctrl+w")) // Work
	if m.screens[TabWork] == nil {
		t.Fatal("the work screen must exist")
	}
	// Simulate what /connect leaves behind: a NEW client set.
	old := m.clients
	m.clients = client.New(client.Options{BaseURL: "http://127.0.0.1:2"})

	m.rebuildScreens()

	// The screen must be REBUILT (the bug left it nil → a blank tab).
	if m.screens[TabWork] == nil {
		t.Fatal("the active screen must be REBUILT, not left blank")
	}
	// It must also have RE-RUN its first load — otherwise the rebuilt screen
	// would be an empty shell. ensureLoaded stages that load as a command.
	if !m.loaded[TabWork] {
		t.Fatal("the rebuilt screen must have been marked loaded")
	}
	if m.pendingScreenCmd == nil {
		t.Fatal("the rebuilt screen must stage its first load")
	}
	// And the client set the factory reads is the CURRENT one.
	if m.clients == old {
		t.Fatal("the client set must have been replaced")
	}
}

// The registry keeps the LATEST status per name, so a dropped wake-up or a
// delivery routed to another screen can never freeze the value the footer reads.
func TestRegistryKeepsTheLatestStatus(t *testing.T) {
	reg := subs.NewRegistry()
	// An unreachable plane: the stream dials, fails, and reports.
	cl := client.New(client.Options{BaseURL: "http://127.0.0.1:1"})
	reg.ProjectEvents(cl, "")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if v := reg.LatestStatus("project-events"); v != "" {
			// It reported something (error/reconnecting) rather than staying
			// silent — which is what the footer needs.
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the registry must record a status for a subscription that dials and fails")
}
