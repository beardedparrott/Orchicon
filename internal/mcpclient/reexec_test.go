package mcpclient

import (
	"os"
	"testing"
)

// TestMain sets the fixture re-exec marker in the environment so that any
// stdio child spawned by the manager (which inherits os.Environ()) re-execs
// into fixtureRun() instead of printing test output to stdout (which would
// corrupt the MCP JSON-RPC stream).
func TestMain(m *testing.M) {
	_ = os.Setenv("ORCHICON_MCP_FIXTURE", "1")
	os.Exit(m.Run())
}

// TestFixtureReexec is the re-exec entry point for the stdio fixture
// server: when the test binary is re-executed with ORCHICON_MCP_FIXTURE=1
// it runs the fixture MCP server over stdio instead of running tests. The
// server blocks until stdin closes, so this test only returns when the
// parent closes the transport.
func TestFixtureReexec(t *testing.T) {
	if os.Getenv("ORCHICON_MCP_FIXTURE") != "1" {
		return
	}
	fixtureRun()
}
