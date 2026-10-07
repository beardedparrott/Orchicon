package orchicon

import (
	"os"
	"testing"

	"github.com/beardedparrott/orchicon/internal/mcpclient"
)

// e2e_reexec_test.go — the stdio fixture re-exec entry for THIS package.
//
// The end-to-end proof registers a PROJECT-OWNED stdio MCP server (child 3/4's
// transport with child-lifecycle risk in production), and a stdio child here
// re-executes THIS test binary. A package may have exactly ONE TestMain and none
// existed in internal/orchicon (verified: only internal/mcpclient/reexec_test.go
// declares one), so this file CREATES it.
//
// E2EStdioReexec is a no-op unless the spawned child carries the E2E marker, so
// this TestMain behaves exactly like the default (os.Exit(m.Run())) for every
// ordinary run.
func TestMain(m *testing.M) {
	if mcpclient.E2EStdioReexec() {
		return
	}
	os.Exit(m.Run())
}
