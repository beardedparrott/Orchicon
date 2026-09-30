package main

// isolate_test.go — TEST HERMETICITY for the serve command's package vars.
//
// servePIDFile / serveLogFile are resolved from the environment at package INIT,
// so an operator whose shell carries a live host plane's ORCHICON_SERVE_STATE_DIR
// (exported by scripts/container.sh) gets the LIVE instance's state dir baked in
// before any test runs — and TestServeStatePathsShape, which asserts the DEFAULT
// `.dev` paths, then fails on their machine while passing in CI. It did.
//
// The var is a legitimate override (that is its whole purpose: two host-resident
// planes must not share a PID file), so the FIX is not to weaken the assertion.
// It is to stop the operator's plane configuration reaching the test binary, which
// is what testfixtures.UnsetAmbientConfigEnv does for the per-instance variables
// no package owns. See that file for the full cause and the other three packages
// it fixes.
//
// `init` rather than TestMain: there is nothing to tear down, the variables stay
// unset for the whole test binary, and — the property that matters — a test
// written later cannot forget to opt out.
//
// TestServeStateDir still exercises the OVERRIDE deliberately, by setting the var
// itself; that is unaffected, because t.Setenv is scoped to its own test.

import "github.com/beardedparrott/orchicon/internal/testfixtures"

func init() {
	testfixtures.UnsetAmbientConfigEnv()
}
