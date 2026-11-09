package mcpclient

import (
	"os"
	"testing"
)

// TestMain sets the fixture re-exec marker in the environment so that any stdio child spawned by
// the manager (which inherits os.Environ()) re-execs into fixtureRun() instead of printing test.
func TestMain(m *testing.M) {
	// The E2E probe stdio child (a DIFFERENT server from the bare fixture):
	// it carries its own marker and must not fall through to m.Run().
	if E2EStdioReexec() {
		return
	}
	_ = os.Setenv("ORCHICON_MCP_FIXTURE", "1")
	os.Exit(m.Run())
}

// TestFixtureReexec is the re-exec entry point for the stdio fixture server: when the test binary
// is re-executed with ORCHICON_MCP_FIXTURE=1 it runs the fixture MCP server over stdio.
func TestFixtureReexec(t *testing.T) {
	if os.Getenv("ORCHICON_MCP_FIXTURE") != "1" {
		return
	}
	fixtureRun()
}
