package screenkit

import (
	"strings"
	"testing"
)

// TestFriendlyFetchErrClassifications pins the auth-cascade cleanup: a
// bare "unauthenticated" RPC error is rewritten to a human-readable retry
// state naming /connect (never shown raw); other classes name their fix.
func TestFriendlyFetchErrClassifications(t *testing.T) {
	cases := []struct{ in, wantContains, wantNotContains string }{
		{"rpc error: code = Unauthenticated desc = invalid token", "run /connect", "Unauthenticated"},
		{"permission_denied", "scopes", "permission_denied"},
		{"context deadline exceeded", "press r to refresh", "deadline"},
		{"connection refused", "unreachable", "refused"},
	}
	for _, c := range cases {
		out := friendlyFetchErr(c.in)
		if !strings.Contains(out, c.wantContains) {
			t.Errorf("friendlyFetchErr(%q) = %q, want it to contain %q", c.in, out, c.wantContains)
		}
		if c.wantNotContains != "" && strings.Contains(out, c.wantNotContains) {
			t.Errorf("friendlyFetchErr(%q) = %q, must not keep the raw error text", c.in, out)
		}
	}
	// Unknown errors pass through (still human-readable).
	if out := friendlyFetchErr("something odd"); out != "something odd" {
		t.Errorf("default pass-through broken: %q", out)
	}
}

// TestListErrRendersRetryState pins the List pane's error render: never a
// bare "error: …" line — an explicit retry hint accompanies the friendly
// error text.
func TestListErrRendersRetryState(t *testing.T) {
	l := &List{Title: "Projects", Width: 40, Height: 10, Err: "session needs re-authentication — run /connect"}
	out := l.View(false)
	if strings.Contains(out, `"error:`) || strings.Contains(out, "error: session") {
		t.Fatalf("bare error: prefix must be gone: %s", out)
	}
	if !strings.Contains(out, "press r to refresh") {
		t.Fatalf("error state must name the retry key: %s", out)
	}
}
