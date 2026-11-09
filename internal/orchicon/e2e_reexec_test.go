package orchicon

import (
	"os"
	"testing"

	"github.com/beardedparrott/orchicon/internal/mcpclient"
)

// e2e_reexec_test.go — the stdio fixture re-exec entry for THIS package.
func TestMain(m *testing.M) {
	if mcpclient.E2EStdioReexec() {
		return
	}
	os.Exit(m.Run())
}
