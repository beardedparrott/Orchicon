package db

// live_plane_guard_test.go — A TEST BINARY MUST NOT OPEN A DATABASE A RUNNING PLANE IS SERVING.
//
// Measured: the scheduler suite failed 3-5 tests per run, varying run to run, with
//
//	park task blocked: db: version conflict
//	fail sequence ancestor: db: version conflict
//	precondition: parent should be failed (halted), got "running"
//
// because the workstation's dev plane was serving the same database the tests opened — its reconcilers scan the
// pending/running rows the tests create. CI never sees it: its database has no plane attached.
//
// Each case below is a branch of the decision, driven through the seam so CI (no plane) can cover both outcomes.

import (
	"flag"
	"os"
	"strings"
	"testing"
)

// withLivePlane stubs the detection: instance -> "host:port/database", as if that plane were answering.
func withLivePlane(t *testing.T, served map[string]string) {
	t.Helper()
	prev := livePlaneTargetFn
	t.Cleanup(func() { livePlaneTargetFn = prev })
	livePlaneTargetFn = func(instance string) string { return served[instance] }
}

// withTestBinary stubs the test-binary detection, so a case can exercise the non-test path.
func withTestBinary(t *testing.T, isTest bool) {
	t.Helper()
	prev := flagLookup
	t.Cleanup(func() { flagLookup = prev })
	if isTest {
		// A registered `test.*` flag is what go test guarantees; the VALUE is irrelevant, which is why
		// isTestBinary checks registration rather than a value.
		f := flag.NewFlagSet("guard-test", flag.ContinueOnError)
		f.Bool("test.v", false, "")
		flagLookup = f.Lookup
		return
	}
	flagLookup = func(string) *flag.Flag { return nil }
}

// TestATestBinaryIsRefusedTheLivePlanesDatabase is the reported flakiness, prevented.
func TestATestBinaryIsRefusedTheLivePlanesDatabase(t *testing.T) {
	withTestBinary(t, true)
	withLivePlane(t, map[string]string{"dev": "localhost:5432/orchicon"})
	t.Setenv(allowSharedPlaneEnv, "")

	err := guardAgainstLivePlaneDSN("postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable")
	if err == nil {
		t.Fatal("a test binary was allowed to open the database a RUNNING plane serves — that is the " +
			"`db: version conflict` flakiness, and it must be a failing test with a named remedy")
	}
	for _, want := range []string{"RUNNING plane", "healthz", "version conflict", allowSharedPlaneEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %q so the operator can act; got:\n%s", want, err)
		}
	}
	// The credential must not be printed.
	if strings.Contains(err.Error(), "orchicon:orchicon@") {
		t.Errorf("the refusal leaked the password:\n%s", err)
	}
}

// TestASeparateDatabaseOnTheSameClusterIsAllowed is the false-positive guard: the commonest legitimate shape is a
// dedicated database on the dev cluster, and refusing it would make the guard an obstacle.
func TestASeparateDatabaseOnTheSameClusterIsAllowed(t *testing.T) {
	withTestBinary(t, true)
	withLivePlane(t, map[string]string{"dev": "localhost:5432/orchicon"})
	t.Setenv(allowSharedPlaneEnv, "")

	if err := guardAgainstLivePlaneDSN("postgres://orchicon:orchicon@localhost:5432/orchicon_test?sslmode=disable"); err != nil {
		t.Fatalf("a SEPARATE database on the same cluster was refused: %v\n"+
			"The comparison is host:port/database precisely so this case is allowed.", err)
	}
}

// TestAStoppedPlaneIsNotRefused keeps the guard from blocking a run just because the instance has RECORDS.
func TestAStoppedPlaneIsNotRefused(t *testing.T) {
	withTestBinary(t, true)
	withLivePlane(t, map[string]string{}) // nothing answers
	t.Setenv(allowSharedPlaneEnv, "")

	if err := guardAgainstLivePlaneDSN("postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable"); err != nil {
		t.Fatalf("refused with no plane running: %v", err)
	}
}

// TestTheLivePlaneGuardOnlyAppliesToTestBinaries keeps the product out of it: a plane opening its own database is
// the product working, not a guarded act.
func TestTheLivePlaneGuardOnlyAppliesToTestBinaries(t *testing.T) {
	withTestBinary(t, false)
	withLivePlane(t, map[string]string{"dev": "localhost:5432/orchicon"})
	t.Setenv(allowSharedPlaneEnv, "")

	if err := guardAgainstLivePlaneDSN("postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable"); err != nil {
		t.Fatalf("a NON-test binary was refused its own database: %v\nthis guard must never affect the plane", err)
	}
}

// TestTheLivePlaneGuardIsEscapable pins the deliberate opt-out: reaching it must be a decision with a name.
func TestTheLivePlaneGuardIsEscapable(t *testing.T) {
	withTestBinary(t, true)
	withLivePlane(t, map[string]string{"dev": "localhost:5432/orchicon"})
	t.Setenv(allowSharedPlaneEnv, "1")

	if err := guardAgainstLivePlaneDSN("postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable"); err != nil {
		t.Fatalf("%s=1 did not opt out: %v", allowSharedPlaneEnv, err)
	}
	_ = os.Getenv
}

// TestLoopbackSpellingsAreTheSameDatabase is the defect found by RUNNING the guard rather than reading it.
//
// The plane is handed `localhost`; the README's own example DSN and most copy-pasted ones say `127.0.0.1`. Raw
// string comparison never matched, so the guard did nothing on the exact workstation it was written for.
func TestLoopbackSpellingsAreTheSameDatabase(t *testing.T) {
	same := [][2]string{
		{"localhost:5432/orchicon", "127.0.0.1:5432/orchicon"},
		{"127.0.0.1:5432/orchicon", "localhost:5432/orchicon"},
		{"localhost:5432/orchicon", "::1:5432/orchicon"},
		{"LOCALHOST:5432/orchicon", "localhost:5432/orchicon"},
	}
	for _, c := range same {
		if !sameDatabaseTarget(c[0], c[1]) {
			t.Errorf("%q and %q are the same local database but compared as different", c[0], c[1])
		}
	}
	// AND NOTHING ELSE IS EQUIVALENT, or the guard would refuse a genuinely remote database.
	differ := [][2]string{
		{"localhost:5432/orchicon", "localhost:5433/orchicon"},       // different port
		{"localhost:5432/orchicon", "localhost:5432/orchicon_test"},  // different database
		{"localhost:5432/orchicon", "db.example.com:5432/orchicon"},  // remote host
		{"10.0.0.5:5432/orchicon", "127.0.0.1:5432/orchicon"},        // remote vs local
	}
	for _, c := range differ {
		if sameDatabaseTarget(c[0], c[1]) {
			t.Errorf("%q and %q are DIFFERENT databases but compared as the same", c[0], c[1])
		}
	}
}

// TestTheLivePlaneGuardFiresOnA127DSN is the same defect, driven end to end through the guard.
func TestTheLivePlaneGuardFiresOnA127DSN(t *testing.T) {
	withTestBinary(t, true)
	withLivePlane(t, map[string]string{"dev": "localhost:5432/orchicon"})
	t.Setenv(allowSharedPlaneEnv, "")

	err := guardAgainstLivePlaneDSN("postgres://orchicon:orchicon@127.0.0.1:5432/orchicon?sslmode=disable")
	if err == nil {
		t.Fatal("a 127.0.0.1 DSN for the live plane's database was NOT refused — the guard compares " +
			"host strings without normalising loopback spellings, which is how it silently did nothing")
	}
}
