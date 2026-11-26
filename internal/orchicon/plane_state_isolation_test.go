package orchicon

// plane_state_isolation_test.go — A WORKER MUST NOT BE ABLE TO REACH THE PLANE'S OWN STATE.
//
// THE INCIDENT. An unguarded `go test ./internal/runtime/` inside a worker's worktree overwrote four
// live opencode stores with a 29-byte stub reading "this is not a sqlite database". The test resolved
// the store path from XDG_DATA_HOME and wrote to whatever it found; every opencode serve start then
// failed with SQLiteError: database disk image is malformed, and every recovery pass failed with it, for
// six hours.
//
// #1008 fixed the SOURCE — the resolver is now home-scoped and the test refuses to write outside its
// temp dir. That protects worktrees cut from a current develop and NO OLDER ONE: eight worktrees
// predating it still carried the unguarded test and kept re-poisoning the store.
//
// This is the runtime half, and it is the half that covers every worktree at once: the worker's bash
// environment is scrubbed of the plane's state pointers and re-pointed at the worker's own scratch, so
// whatever the source says, a test run in a worker finds no plane store to damage.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE SCRUB. Every ambient pointer at the plane's state is either removed or redirected.
func TestAWorkerBashEnvironmentCannotSeeThePlaneState(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"XDG_DATA_HOME=/home/operator/.local/share",
		"ORCHICON_DATA_DIR=/home/operator/.local/share/orchicon",
		"ORCHICON_SERVE_STATE_DIR=/home/operator/.local/share/orchicon-prod/serve",
		"ORCHICON_SERVE_LOG_FILE=/home/operator/.local/share/orchicon-prod/serve/logs/orchicon.log",
		"HOME=/home/operator",
	}
	scratch := t.TempDir()
	got := sandboxedBashEnviron(base, scratch)

	joined := strings.Join(got, "\n")
	for _, banned := range []string{
		"/home/operator/.local/share",
		"orchicon-prod",
		"ORCHICON_SERVE_LOG_FILE",
		"ORCHICON_SERVE_STATE_DIR",
	} {
		if strings.Contains(joined, banned) {
			t.Errorf("the worker environment still carries %q:\n%s", banned, joined)
		}
	}
	// Redirected, not merely stripped: a fixture that needs a store still gets one, in the worker's own
	// scratch. Hiding it entirely would break tests and teach nobody anything.
	if !strings.Contains(joined, "XDG_DATA_HOME="+filepath.Join(scratch, "xdg")) {
		t.Errorf("XDG_DATA_HOME was not redirected into the worker's scratch:\n%s", joined)
	}
	if !strings.Contains(joined, "ORCHICON_DATA_DIR="+filepath.Join(scratch, "data")) {
		t.Errorf("ORCHICON_DATA_DIR was not redirected into the worker's scratch:\n%s", joined)
	}
	// Unrelated variables survive.
	if !strings.Contains(joined, "PATH=/usr/bin") || !strings.Contains(joined, "HOME=/home/operator") {
		t.Errorf("the scrub removed unrelated variables:\n%s", joined)
	}
}

// THE EXEC SITE USES IT. A scrub nobody calls is the bug this is fixing.
func TestInProcessBashUsesTheScrubbedEnvironment(t *testing.T) {
	h := NewHostTools(t.TempDir(), t.TempDir())

	// With NO factory wired the inherited environment must still be scrubbed — an unwired worker is
	// exactly the case that leaked.
	t.Setenv("XDG_DATA_HOME", "/home/operator/.local/share")
	t.Setenv("ORCHICON_DATA_DIR", "/home/operator/.local/share/orchicon")

	env := h.bashEnviron()
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "/home/operator/.local/share") {
		t.Fatalf("bashEnviron() still exposes the plane state:\n%s", joined)
	}
	for _, e := range env {
		if strings.HasPrefix(e, "XDG_DATA_HOME=") && !strings.HasPrefix(e, "XDG_DATA_HOME="+filepath.Join(h.scratchSessionDir(), "xdg")) {
			t.Errorf("XDG_DATA_HOME points somewhere unexpected: %s", e)
		}
	}
}

// AND THE BASH CALL SITE SETS cmd.Env UNCONDITIONALLY. The old code only set it when a factory was
// wired, so a native worker with no factory inherited the plane's environment verbatim.
func TestTheBashCallSiteAlwaysSetsTheEnvironment(t *testing.T) {
	raw, err := os.ReadFile("hosttools.go")
	if err != nil {
		t.Fatalf("read hosttools.go: %v", err)
	}
	src := string(raw)
	if !strings.Contains(src, "cmd.Env = h.bashEnviron()") {
		t.Error("the bash exec site does not set cmd.Env unconditionally — a worker with no wired " +
			"environment factory would inherit the plane's own, which is how a test reached the live store")
	}
	if strings.Contains(src, "if h.envForBash != nil {\n\t\tcmd.Env = h.envForBash()") {
		t.Error("the old conditional assignment is back — it leaves the unwired case unscrubbed")
	}
}
