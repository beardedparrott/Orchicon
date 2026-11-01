package runtime

// argv0_boundary_test.go — the CONTRACT between the plane and the container, pinned at the boundary
// that broke.
//
// REPORTED: every claude container dispatch on Windows died in ~400ms with 0 tokens and "claude
// session exited without a terminal result message (exit code 1); the CLI emitted no stderr at all
// before exiting". The cause was argv[0]: internal/claude sent the operator's HOST install path
// (C:\Users\...\claude.exe) into a Linux container, and this handler refused it by basename before
// spawning anything — so no child existed to write stderr, and "the CLI was silent" was true and
// misleading at once.
//
// TWO SIDES, both pinned here:
//  1. a HOST path is refused, and the refusal says what it is (this is the operator's diagnostic now
//     that internal/claude surfaces it verbatim);
//  2. the name the claude adapter sends for a container (internal/claude.ClaudeBinaryName) is ACCEPTED,
//     so the two halves cannot drift apart in silence.

import (
	"strings"
	"testing"
)

// The reported failure, at its exact point of refusal.
func TestStdioRefusesAHostInstallPathAndSaysSo(t *testing.T) {
	const windowsClaudePath = `C:\Users\justi\AppData\Local\Microsoft\WinGet\Packages\Anthropic.ClaudeCode\claude.exe`

	events := stdioEvents(t, AgentRequest{
		Cmd:    "stdio",
		ExecID: "exec-argv0",
		Argv:   []string{windowsClaudePath, "-p", "--input-format", "stream-json"},
	})
	if len(events) != 1 || events[0].Event != "error" {
		t.Fatalf("runStdio emitted %+v, want exactly one {event:error} — a host path must be refused BEFORE "+
			"any child is spawned", events)
	}
	got := events[0].Error
	if !strings.Contains(got, "not allowlisted") {
		t.Errorf("refusal lost the allowlist framing: %q", got)
	}
	// THE HALF THAT MAKES IT ACTIONABLE. Without this the message reads as a list-membership mistake,
	// which is how the investigation ended up on mounts and credentials instead of on argv.
	if !strings.Contains(got, "HOST path") {
		t.Errorf("refusal does not name the HOST-path case, so it reads as a missing allowlist entry "+
			"rather than a boundary violation: %q", got)
	}
	if !strings.Contains(got, "IMAGE") {
		t.Errorf("refusal does not say what to send instead (the image's binary name): %q", got)
	}
}

// The other side of the contract: what the claude adapter actually sends for a container is accepted.
// If this ever fails, container dispatch is broken again — and it fails HERE, in the daemon's own
// package, rather than at the end of a 400ms no-op on an operator's Windows box.
func TestStdioAcceptsTheNameAContainerDispatchSends(t *testing.T) {
	// "claude" is internal/claude.ClaudeBinaryName. It is spelled out rather than imported: importing
	// the adapter into the daemon's test would let a change to BOTH sides pass while dispatch broke.
	if !runtimeBinAllowlist["claude"] {
		t.Fatal("runtimeBinAllowlist no longer accepts \"claude\" — that IS the name a container claude " +
			"dispatch sends, so every claude container worker would be refused before spawn")
	}
	// A bare name must get past the allowlist gate. It will then fail to exec (this host need not have
	// claude installed) — which is fine and is a DIFFERENT failure: the point is that it is not refused
	// at the boundary, so the child-exit path is reachable.
	events := stdioEvents(t, AgentRequest{
		Cmd:    "stdio",
		ExecID: "exec-argv0-ok",
		Argv:   []string{"claude", "-p"},
		Env:    []string{"PATH=/nonexistent-so-exec-fails-fast"},
	})
	for _, ev := range events {
		if ev.Event == "error" && strings.Contains(ev.Error, "not allowlisted") {
			t.Fatalf("a bare \"claude\" was refused at the boundary: %q — the container's PATH binary must "+
				"be attemptable, or no container claude worker can ever start", ev.Error)
		}
	}
}
