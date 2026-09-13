package tui

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/client"
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
