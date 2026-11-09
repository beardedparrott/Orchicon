package orchicon

// isolate_test.go — TEST HERMETICITY for the model registry's data directory.

import (
	"os"
	"testing"

	"github.com/beardedparrott/orchicon/internal/testfixtures"
)

func init() {
	testfixtures.UnsetAmbientConfigEnv()
}

// TestTheAmbientConfigIsActuallyUnset proves the init above took effect, rather than trusting that
// a file naming the function was enough.
func TestTheAmbientConfigIsActuallyUnset(t *testing.T) {
	for _, name := range testfixtures.AmbientConfigEnv {
		if v := os.Getenv(name); v != "" {
			t.Errorf("%s is still set to %q at test time — the isolate file did not run, so every test in this "+
				"package is reading the operator's live plane instead of an isolated one", name, v)
		}
	}
}
