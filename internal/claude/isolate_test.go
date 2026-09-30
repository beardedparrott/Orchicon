package claude

// isolate_test.go — TEST HERMETICITY for the execution guard's profile switches.
//
// askSession.childEnv() builds the child's environment from os.Environ() and
// deliberately adds NO InteractiveEnviron — that omission is the Ask/worker
// parity this package asserts. An operator whose shell has a live plane's
// ORCHICON_GUARD_POLICY (and friends) exported therefore hands the shim an
// interactive profile the code never intended to apply, and a shim whose job is
// to REFUSE then refuses DIFFERENTLY rather than not at all.
//
// See testfixtures for the full account and the other three packages affected.

import "github.com/beardedparrott/orchicon/internal/testfixtures"

func init() {
	testfixtures.UnsetAmbientConfigEnv()
}
