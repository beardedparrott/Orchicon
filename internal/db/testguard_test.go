package db

// testguard_test.go — the prod guard must refuse the incident, and must not refuse anything else.
//
// A guard is only worth having if BOTH halves hold: it stops the thing it exists for, and it does not stop the
// work it sits in front of. The second half is not decoration — a guard that refuses `make test` gets deleted
// within a day, and then the first half is gone with it.
//
// The incident, for the record: ORCHICON_INSTANCE=prod and ORCHICON_POSTGRES_DSN were in the shell, and
// ORCHICON_TEST_DSN was assigned that same live DSN. ~125 test projects landed in the deployment tenant and 281
// throwaway tenants were created in a live instance. Every case below is a spelling of that mistake, or a
// legitimate run that must still work.

import (
	"flag"
	"strings"
	"testing"
)

// asTestBinary forces isTestBinary() to answer true, and restores the real lookup afterwards. Without this the
// guard's test binary detection would depend on the flags the real `go test` registered — which is true in
// practice but leaves the NEGATIVE half (a server process must never be refused) untestable.
func asTestBinary(t *testing.T) {
	t.Helper()
	prev := flagLookup
	flagLookup = func(name string) *flag.Flag {
		if name == "test.v" {
			return &flag.Flag{Name: name}
		}
		return nil
	}
	t.Cleanup(func() { flagLookup = prev })
}

func asServerProcess(t *testing.T) {
	t.Helper()
	prev := flagLookup
	flagLookup = func(string) *flag.Flag { return nil }
	t.Cleanup(func() { flagLookup = prev })
}

const (
	prodDSN = "postgres://orchicon:secretpw@prod-db.internal:5432/orchicon?sslmode=disable"
	devDSN  = "postgres://orchicon:orchicon@localhost:5433/orchicon?sslmode=disable"
	testdDSN = "postgres://orchicon:orchicon@localhost:5432/orchicon_test?sslmode=disable"
)

// ── it refuses ───────────────────────────────────────────────────────────────────────────────────────────

// THE INCIDENT ITSELF: the test DSN is the plane's own DSN. This is the exact assignment that did the damage.
func TestGuardRefusesThePlanesOwnDSN(t *testing.T) {
	asTestBinary(t)
	t.Setenv(instanceEnv, "prod")
	t.Setenv(dsnEnv, prodDSN)
	t.Setenv(allowProdEnv, "")

	err := guardAgainstProdDSN(prodDSN)
	if err == nil {
		t.Fatal("opening the plane's own database from a test binary must be REFUSED — this is the case the " +
			"guard exists for")
	}
	if !strings.Contains(err.Error(), "REFUSING") {
		t.Errorf("the refusal must be unmistakable, got: %v", err)
	}
	// And it must say what to do, not only what happened.
	if !strings.Contains(err.Error(), "ORCHICON_TEST_DSN") {
		t.Errorf("the refusal must name the variable to point at dev, got: %v", err)
	}
}

// A DIFFERENT SPELLING OF THE SAME DATABASE is still the same database. sslmode is not part of the target, and a
// guard that compared whole strings would miss this and let the incident through in a slightly different shell.
func TestGuardRefusesTheSameDatabaseSpelledDifferently(t *testing.T) {
	asTestBinary(t)
	t.Setenv(instanceEnv, "dev") // not prod — so ONLY the DSN comparison can catch this
	t.Setenv(dsnEnv, prodDSN)
	t.Setenv(allowProdEnv, "")

	sameTarget := "postgres://orchicon:differentpw@prod-db.internal:5432/orchicon?sslmode=require"
	if err := guardAgainstProdDSN(sameTarget); err == nil {
		t.Fatal("the same host/port/database under another password or sslmode is the SAME database and must " +
			"be refused")
	}
}

// ON A PROD-LABELLED HOST, an unknown remote database is refused too: the guard cannot verify that "some other
// host" is a scratch clone, and the cost of being wrong is a live tenant full of test rows.
func TestGuardRefusesARemoteDatabaseOnAProdHost(t *testing.T) {
	asTestBinary(t)
	t.Setenv(instanceEnv, "prod")
	t.Setenv(dsnEnv, "") // no ambient DSN to compare against — only the instance label is evidence
	t.Setenv(allowProdEnv, "")

	err := guardAgainstProdDSN("postgres://u:p@elsewhere.example.com:5432/scratch")
	if err == nil {
		t.Fatal("a remote database opened from a test on a prod-labelled host must be refused")
	}
	if !strings.Contains(err.Error(), allowProdEnv) {
		t.Errorf("the refusal must name the opt-out for a deliberate run, got: %v", err)
	}
}

// THE CASE THE FIRST VERSION OF THIS GUARD GOT WRONG, and the reason it is pinned separately.
//
// In this installation PRODUCTION RUNS ON LOOPBACK (a host plane on localhost:5433) while the dev container
// publishes 5432. The first version allowed any loopback DSN as "the test's own", so the live prod DSN slipped
// through the escape hatch and a DB-backed test with the shell's own prod DSN was ALLOWED — the incident,
// reproduced by the guard meant to prevent it. Every other case here passed while that was true, because they
// asserted against a REMOTE prod DSN and never the local one this machine actually uses.
func TestGuardRefusesALocalProdDSNBecauseProdIsLocalHere(t *testing.T) {
	asTestBinary(t)
	// The real shape of this installation: a loopback production plane.
	t.Setenv(instanceEnv, "prod")
	t.Setenv(dsnEnv, devDSN)
	t.Setenv(allowProdEnv, "")

	if err := guardAgainstProdDSN(devDSN); err == nil {
		t.Fatal("a LOOPBACK database that IS the ambient plane's must still be refused — production runs on " +
			"localhost here, so 'it is local' cannot mean 'it is safe'")
	}
}

// ── it does NOT refuse (or it would be deleted) ──────────────────────────────────────────────────────────

// THE EXPLICIT OPT-OUT WORKS. A restore rehearsal against a labelled-prod scratch clone is a real thing, and it
// must be reachable deliberately.
func TestGuardAllowsExplicitOptOut(t *testing.T) {
	asTestBinary(t)
	t.Setenv(instanceEnv, "prod")
	t.Setenv(dsnEnv, prodDSN)
	t.Setenv(allowProdEnv, "1")

	if err := guardAgainstProdDSN(prodDSN); err != nil {
		t.Fatalf("the explicit %s=1 opt-out must allow the open, got: %v", allowProdEnv, err)
	}
}

// A SERVER PROCESS IS NEVER REFUSED. The guard is about test binaries; the control plane opening its own
// production database is the whole point of the product.
func TestGuardNeverRefusesAServerProcess(t *testing.T) {
	asServerProcess(t)
	t.Setenv(instanceEnv, "prod")
	t.Setenv(dsnEnv, prodDSN)

	if err := guardAgainstProdDSN(prodDSN); err != nil {
		t.Fatalf("a non-test process must never be refused (this would break the server), got: %v", err)
	}
}

// THE DEV LOOP KEEPS WORKING, which is what `make test` does: no prod in the shell, a local DSN.
func TestGuardAllowsTheOrdinaryDevRun(t *testing.T) {
	asTestBinary(t)
	t.Setenv(instanceEnv, "dev")
	t.Setenv(dsnEnv, "")
	t.Setenv(allowProdEnv, "")

	if err := guardAgainstProdDSN(devDSN); err != nil {
		t.Fatalf("an ordinary dev-instance test DB must be allowed, got: %v", err)
	}
}

// AND THE OPERATOR'S OWN `make test` KEEPS WORKING even with a prod plane in the shell — because that is exactly
// the shell the incident happened in, and forbidding it would forbid the fix. A LOOPBACK database is the dev
// instance / the services container, never the remote prod one.
func TestGuardAllowsALocalDatabaseEvenOnAProdLabelledHost(t *testing.T) {
	asTestBinary(t)
	t.Setenv(instanceEnv, "prod")
	t.Setenv(dsnEnv, prodDSN) // a remote prod plane in the shell
	t.Setenv(allowProdEnv, "")

	// ...but the DSN being opened is the LOCAL dev instance (5433), which is not that database.
	if err := guardAgainstProdDSN(devDSN); err != nil {
		t.Fatalf("a local (loopback) test database must be allowed even with a prod plane in the shell — "+
			"otherwise the guard breaks the very `make test` it is meant to make safe, got: %v", err)
	}
	if err := guardAgainstProdDSN(testdDSN); err != nil {
		t.Fatalf("a second local test database must be allowed, got: %v", err)
	}
}

// ── the refusal does not leak a credential ───────────────────────────────────────────────────────────────

func TestGuardRefusalRedactsThePassword(t *testing.T) {
	asTestBinary(t)
	t.Setenv(instanceEnv, "prod")
	t.Setenv(dsnEnv, prodDSN)
	t.Setenv(allowProdEnv, "")

	err := guardAgainstProdDSN(prodDSN)
	if err == nil {
		t.Fatal("want a refusal")
	}
	if strings.Contains(err.Error(), "secretpw") {
		t.Errorf("the refusal printed the password, got: %v", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Errorf("the refusal should show a redacted credential so the target is identifiable, got: %v", err)
	}
}
