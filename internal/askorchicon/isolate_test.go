package askorchicon

// isolate_test.go — TEST HERMETICITY for the execution guard's profile switches.
//
// AskGuardEnvironFor builds bash's environment from os.Environ() and then
// APPENDS the interactive profile's ORCHICON_GUARD_* key/values. The append
// overrides the ambient value for every variable the profile EMITS — but the OFF
// case deliberately emits NOTHING, because "unset is the absence of the mode"
// (guard.go): a shim driven by an environment this code did not build must not be
// able to read a mode it does not mean. So an operator whose shell has a live
// plane's ORCHICON_GUARD_FULLSEND exported hands the shim a mode the tests never
// set — and fullsend skips the sanctioned-set test by design, so `rm
// <out-of-project>` RAN where the test asserts a refusal. MEASURED, not
// deduced: against a READABLE policy a clean environment REFUSES that target and
// an ambient ORCHICON_GUARD_FULLSEND=1 DELETES it, while a policy entry that
// denies it still refuses under fullsend (so an operator exclusion is safe) and
// the never-allow class is untouched. The policy is readable in practice because
// permpolicy.DefaultPath() resolves through the launcher's ORCHICON_DATA_DIR,
// which these tests inherit too; with the policy ABSENT the fail-closed read
// refuses first and the fullsend arm is never reached — which is why this stays
// invisible until an operator has a live plane. The same holds for
// ORCHICON_GUARD_GRANTS / _ONCE / _PROJECT whenever the caller passes none of
// them, which is exactly the shape of these tests.
//
// That is a defect of the TEST ENVIRONMENT, not of the product: the code is
// correct and the operator's shell is the difference, which is the least
// actionable kind of failure because it reds on the operator's machine and
// passes in CI. See internal/testfixtures for the full account and the other
// packages that need this.

import "github.com/beardedparrott/orchicon/internal/testfixtures"

func init() {
	testfixtures.UnsetAmbientConfigEnv()
}
