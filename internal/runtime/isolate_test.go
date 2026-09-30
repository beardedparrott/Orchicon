package runtime

// isolate_test.go — TEST HERMETICITY for the execution guard's profile switches.
//
// The runtime tests exec a child through the guard
// (prependGuard(os.Environ(), dir)), so an operator whose shell carries a live
// plane's ORCHICON_GUARD_* exports runs the child under the INTERACTIVE profile
// where the test asserts the default one. Same cause as internal/guard; see
// testfixtures for the full account.

import "github.com/beardedparrott/orchicon/internal/testfixtures"

func init() {
	testfixtures.UnsetAmbientConfigEnv()
}
