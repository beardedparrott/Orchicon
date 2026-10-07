package guard

// isolate_test.go — TEST HERMETICITY for the execution guard's profile switches.
//
// The guard's tests run the REAL shim as a subprocess and hand it the ambient
// environment (runGuardEnv: `cmd.Env = append(os.Environ(), env...)`), which is
// the honest way to test it — the shim reads its profile from the environment, so
// the test must provide one. The problem is that on an operator's machine the
// ambient environment ALREADY carries the interactive profile, because their shell
// has a live Ask/worker plane's ORCHICON_GUARD_POLICY, _GRANTS, _ONCE and
// _PROJECT exported by scripts/container.sh.
//
// So the shim ran the INTERACTIVE profile while the tests asserted the WORKER
// profile — the profile with no ORCHICON_GUARD_* at all. Eight tests failed on
// that machine and none in CI, which is the signature of ambient leakage rather
// than a broken assertion.
//
// Unsetting the group in `init` makes the ambient environment match CI: a test
// that wants a profile sets it through InteractiveEnviron (or t.Setenv), which is
// what all of them already do.

import "github.com/beardedparrott/orchicon/internal/testfixtures"

func init() {
	testfixtures.UnsetAmbientConfigEnv()
}
