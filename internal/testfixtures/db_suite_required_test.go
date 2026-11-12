package testfixtures

// db_suite_required_test.go — THE DB-BACKED SUITES MUST NOT SILENTLY SKIP.
//
// The failure mode this exists for is not a broken test, it is a test that never ran. Every DB-gated test in
// this repo skips when ORCHICON_TEST_DSN is empty, and CI set only DB_URL — so 37 tests across three packages
// (the scheduler's loop routing, the Ask turn lifecycle, the seed roll-forward) had no coverage anywhere. Two
// genuine product bugs shipped through that hole: a worktree that could never be pruned, and a recurring
// cursor computed in the past. Both were found the moment a DSN was set, and both were invisible before.
//
// A skip is indistinguishable from a pass in a green build, so the wiring is asserted here rather than trusted.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCIWiresTheDatabaseSuite is the source gate: it fails if ci.yml stops providing a DSN, which would turn
// every DB-backed test back into a skip without changing a single test file.
func TestCIWiresTheDatabaseSuite(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	if !strings.Contains(string(body), "ORCHICON_TEST_DSN:") {
		t.Fatal("ci.yml no longer sets ORCHICON_TEST_DSN, so every DB-backed test SKIPS and the build stays " +
			"green while the loop routing, the Ask turn lifecycle and the seed roll-forward go untested — " +
			"which is exactly how a worktree-prune leak shipped unnoticed")
	}
}

// TestTheAmbientEnvDoesNotUnsetTheTestDSN is the other half: the `test` target unsets testfixtures.
// AmbientConfigEnv for isolation, so a DSN added to that list would be stripped right before the suites run
// and the wiring above would prove nothing.
func TestTheAmbientEnvDoesNotUnsetTheTestDSN(t *testing.T) {
	for _, v := range AmbientConfigEnv {
		if v == "ORCHICON_TEST_DSN" {
			t.Fatal("ORCHICON_TEST_DSN is in AmbientConfigEnv, so `make test` unsets it and every DB-backed " +
				"test skips even when CI provides a database")
		}
	}
}

// TestCIDoesNotRunWithoutADatabase is the runtime half. A source gate can be satisfied while the variable
// arrives empty, and this is the only place that notice is taken: in CI an absent DSN is a FAILURE, not a
// silent skip.
func TestCIDoesNotRunWithoutADatabase(t *testing.T) {
	ci := strings.TrimSpace(os.Getenv("CI"))
	if ci == "" || ci == "false" {
		t.Skip("not a CI run")
	}
	if strings.TrimSpace(os.Getenv("ORCHICON_TEST_DSN")) == "" {
		t.Fatal("this is CI and ORCHICON_TEST_DSN is empty, so every DB-backed test skipped. The suite is " +
			"gated on that variable by design; without it the loop routing, the Ask turn lifecycle and the " +
			"seed roll-forward have NO coverage. Provide a disposable database (see ci.yml's go-ci job).")
	}
}
