package runtime

// argv0_boundary_test.go — the CONTRACT between the plane and the container, pinned at the boundary
// that broke.

import (
	"os"
	"path/filepath"
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

// The other side of the contract: what the claude adapter actually sends for a container is
// accepted.
func TestStdioAcceptsTheNameAContainerDispatchSends(t *testing.T) {
	// "claude" is internal/claude.ClaudeBinaryName. It is spelled out rather than imported: importing
	// the adapter into the daemon's test would let a change to BOTH sides pass while dispatch broke.
	if !runtimeBinAllowlist["claude"] {
		t.Fatal("runtimeBinAllowlist no longer accepts \"claude\" — that IS the name a container claude " +
			"dispatch sends, so every claude container worker would be refused before spawn")
	}
	// A bare name must get past the allowlist gate.
	//
	// THE BINARY IS A STAND-IN, AND IT HAS TO BE. This asserts ONE thing — that the allowlist gate lets the
	// name "claude" through — and it must not reach a real CLI to prove it. The hazard is invisible from
	// here: runStdio spawns req.Argv[0] by BARE NAME, and os/exec resolves it at Start via LookPath against
	// the TEST PROCESS's PATH. The Env override below therefore cannot constrain the lookup; it only sets
	// the PATH the child would run under.
	//
	// So on a developer's machine (where /usr/bin or ~/.local/bin really has claude) this test STARTED the
	// operator's own claude CLI. Measured before the fix: it rewrote ~/.claude.json and dropped a fresh
	// ~/.claude/backups/.claude.json.backup.<ms> — in 0.30s, while reporting PASS. On an authenticated host
	// that same path is a billed session.
	//
	// Substituting the PATH ENTRY, not the argv, is what keeps the assertion honest: the name under test is
	// still "claude", the allowlist gate under test is still the real one, and the child that would run is a
	// binary that does nothing at all.
	//
	// t.Setenv, NOT a request Env, because the lookup happens in THIS process before any child exists:
	// stdioEvents overwrites the request's PATH with the ambient one anyway (shimFreePath), so pinning it
	// through the request could never have constrained the resolution. Or the stand-in dir leaves the name
	// "claude" resolving — to a binary that does nothing.
	t.Setenv("PATH", harmlessClaudeDir(t))
	events := stdioEvents(t, AgentRequest{
		Cmd:    "stdio",
		ExecID: "exec-argv0-ok",
		Argv:   []string{"claude", "-p"},
	})
	for _, ev := range events {
		if ev.Event == "error" && strings.Contains(ev.Error, "not allowlisted") {
			t.Fatalf("a bare \"claude\" was refused at the boundary: %q — the container's PATH binary must "+
				"be attemptable, or no container claude worker can ever start", ev.Error)
		}
	}
}

// harmlessClaudeDir is a directory containing an executable named "claude" that does nothing, for tests
// that need the adapter name to RESOLVE without the real CLI being started.
//
// A stand-in is necessary rather than merely tidy: the supervisor resolves argv[0] by bare name against
// the test process's PATH (see the note in TestStdioAcceptsTheNameAContainerDispatchSends), so a test
// cannot prevent a real `claude` from being found by pinning the CHILD's environment.
func harmlessClaudeDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// Sleeping briefly rather than exiting instantly keeps the shape of a real spawn (the supervisor opens
	// pipes and reads the session) while producing no output and touching no state.
	script := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatalf("write stand-in claude: %v", err)
	}
	return dir
}
