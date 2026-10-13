// Package testfixtures — AMBIENT CONFIG ISOLATION for tests that exec a
// subprocess.
//
// THE PROBLEM THIS SOLVES. An operator runs a host plane, and the launcher
// exports that plane's configuration into their shell (scripts/container.sh:
// ORCHICON_SERVE_STATE_DIR, ORCHICON_GUARD_POLICY, …). A test that runs a
// subprocess and hands it `os.Environ()` therefore passes the operator's REAL
// plane configuration to the thing under test. The subprocess then behaves
// differently from the clean-CI case, and the test fails on the operator's
// machine while passing in CI — the least actionable kind of failure, because
// the code is fine and the environment is the defect.
//
// It was real. Running `go test ./...` in a shell with a live host plane
// produced five failing packages, every one of them a test that spawns a
// process:
//
//	cmd/orchicon     TestServeStatePathsShape — asserts the DEFAULT `.dev`
//	                 paths, but ORCHICON_SERVE_STATE_DIR made the package vars
//	                 resolve to the live instance's state dir instead.
//	internal/guard   InteractiveEnviron's vars were already in the ambient
//	                 env, so the shim ran the INTERACTIVE profile while the
//	                 tests asserted the WORKER profile.
//	internal/runtime TestStdioChildRunsUnderTheExecutionGuard — the same
//	                 leak through a guard dir built with prependGuard.
//	internal/claude  childEnv() appends to os.Environ(), so an ambient
//	                 ORCHICON_GUARD_POLICY widened/clamped the Ask shim's
//	                 profile differently from the documented default. The
//	                 guard's job is to REFUSE; a leaked policy made it refuse
//	                 differently rather than not at all.
//	internal/askorchicon
//	                 AskGuardEnvironFor appends to os.Environ() too, and its OFF
//	                 case emits NOTHING ("unset is the absence of the mode"), so an
//	                 ambient ORCHICON_GUARD_FULLSEND survived into the shim; where the
//	                 policy is readable, fullsend skips the sanctioned-set tests by
//	                 design and `rm <out-of-project>` RAN where the test asserts a
//	                 refusal (measured: a clean env REFUSES that target, an ambient
//	                 fullsend DELETES it; a denying policy entry and the never-allow
//	                 class still refuse under fullsend). The policy is readable via
//	                 the launcher's ORCHICON_DATA_DIR, which permpolicy.DefaultPath
//	                 resolves through — and which these tests inherit too, so the
//	                 same element is worth watching. Found later than the four
//	                 above, by running the suite from inside an Ask conversation.
//
// All five pass the moment those variables are unset, which is what identified
// the cause rather than any behaviour of the tests.
//
// WHY IT LIVES HERE. The variables are per-INSTANCE, so no package owns them,
// and five packages need the same list. Deriving it from the one place that
// already knows the per-instance set (scripts/container.sh) keeps a var added
// there from silently reappearing as a failure here.
//
// THIS IS NOT A BLANKET UNSET. Only variables that SWITCH ON behaviour in an
// exec'd child are listed. A test that wants a subprocess to use a temp
// database legitimately sets a temp DSN first; this function only removes the
// ambient plane's, so it makes such a test reach its own setup rather than
// inheriting the operator's.
package testfixtures

import "os"

// AmbientConfigEnv is every per-instance variable the host-plane launcher
// exports into the operator's shell that a spawned child reads back.
//
// It is deliberately the three GROUPS that proved to leak, not "all ORCHICON_*":
// the wider prefix also carries intentional opt-ins (ORCHICON_TEST_DSN,
// ORCHICON_SKIP_NETWORK_TESTS, ORCHICON_LIVE_*, ORCHICON_TEST_LIVE_*) which a
// blanket unset would silently disable — turning a handful of noisy failures
// into a suite that quietly stops testing what the operator asked it to.
var AmbientConfigEnv = []string{
	// The execution guard's profile switches. These are the ones that decide
	// WHICH policy a spawned shim enforces, so a leaked value changes what the
	// guard DOES rather than merely where it writes.
	"ORCHICON_GUARD_POLICY",
	"ORCHICON_GUARD_GRANTS",
	"ORCHICON_GUARD_ONCE",
	"ORCHICON_GUARD_PROJECT",
	"ORCHICON_GUARD_FULLSEND",
	// The detached-serve state root, which the serve command's package vars
	// resolve at init.
	"ORCHICON_SERVE_STATE_DIR",
	// The plane's STATE DIRECTORY. FOUR packages resolve their own default from it —
	// internal/config (Config.DataDir), internal/permpolicy (DefaultPath, whose
	// result the execution guard BAKES INTO a shim script), internal/orchicon's
	// model registry and internal/backup — and internal/server turns a whole
	// behaviour on whether it is SET (project mounts "only run in container mode"),
	// so an ambient value puts a test into a different MODE, not merely a different
	// directory. What that meant in practice: this suite resolved the operator's
	// LIVE plane state, and the guard tests read the operator's own
	// permission-policy.yaml — which is the reason an ambient ORCHICON_GUARD_FULLSEND
	// was reachable at all. Unsetting is the CI-equivalent (config.Default() falls
	// back to /var/lib/orchicon when unset, which is what CI resolves), and a test
	// that wants a data dir of its own sets one, which is scoped and overrides this.
	"ORCHICON_DATA_DIR",
}

// UnsetAmbientConfigEnv removes the ambient plane configuration from this test
// binary's environment, so a subprocess it spawns starts from the same
// configuration CI has.
//
// Call it from an `init` in a package's tests, not from individual test
// functions: `init` runs before any test and is never cleared, so a test
// written later cannot forget — which is the property that matters. An opt-out
// each test must remember is one the next test will not.
//
// A test that WANTS one of these set calls t.Setenv for it, which is scoped to
// that test and overrides whatever this removed.
func UnsetAmbientConfigEnv() {
	for _, name := range AmbientConfigEnv {
		_ = os.Unsetenv(name)
	}
}
