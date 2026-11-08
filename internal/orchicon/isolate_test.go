package orchicon

// isolate_test.go — TEST HERMETICITY for the model registry's data directory.
//
// THE MISSING HALF OF THE PACKAGE'S OWN SOURCING TESTS. SourcingService.registryCacheDir falls back to
// ORCHICON_DATA_DIR, and on an operator's machine that is exported — by scripts/lib/instances.sh, which the
// host-plane launcher sources — pointing at whichever plane their shell was last launched against. So on that
// machine this package's tests resolved the LIVE PLANE's model-registry cache instead of an isolated one:
// nineteen NewSourcingService call sites, every one of them reading a 6MB registry the running plane rewrites.
//
// In CI the variable is unset, so the same code resolves /var/lib/orchicon and fails soft — which is why this
// only ever failed on a real machine. That signature (fails on the operator's box, green in CI) is ambient
// leakage, not a broken assertion.
//
// Unsetting the group in `init` makes the ambient environment match CI for EVERY test in the package, present
// and future — a test that wants a data dir of its own passes CacheDir, which is scoped and overrides this.
// The four other affected packages (askorchicon, claude, guard, runtime, tui) carry the same file; this one was
// simply missed, and internal/testfixtures/ambient_makefile_test.go is the gate that stops the next omission.

import (
	"os"
	"testing"

	"github.com/beardedparrott/orchicon/internal/testfixtures"
)

func init() {
	testfixtures.UnsetAmbientConfigEnv()
}

// TestTheAmbientConfigIsActuallyUnset proves the init above took effect, rather than trusting that a file
// naming the function was enough. On an operator's machine ORCHICON_DATA_DIR is exported, so a failure here is
// the mechanism itself failing; in CI it is vacuous (there was nothing to remove) and harmless.
func TestTheAmbientConfigIsActuallyUnset(t *testing.T) {
	for _, name := range testfixtures.AmbientConfigEnv {
		if v := os.Getenv(name); v != "" {
			t.Errorf("%s is still set to %q at test time — the isolate file did not run, so every test in this "+
				"package is reading the operator's live plane instead of an isolated one", name, v)
		}
	}
}
