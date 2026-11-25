package main

// services_health_isolate_test.go — THE READINESS PROBE MUST NOT ASK THE OPERATOR'S MACHINE.
//
// services_health.go's postgres component probes with pgIsReadyBin(instance), which resolves the
// RECORDED pg_isready out of <host data dir>/infra-binaries.json — an ABSOLUTE path into the
// operator's real install — before it ever considers PATH:
//
//	rec, err := loadInfraBinaries(hostDataDirFor(instance))
//	if err == nil && rec.PgIsReady != "" { return rec.PgIsReady }
//	return "pg_isready"
//
// So a test that stubbed PATH with a fake `pg_isready` was still running the operator's REAL binary
// against 127.0.0.1:5432. On a machine that has never installed Orchicon there is no record, the
// stub wins, and the suite is green. On the machine of anyone who HAS installed it the recorded
// absolute path wins instead, and two tests in services_health_test.go failed on host state that has
// nothing to do with what they assert.
//
// IT FAILED IN THE QUIET DIRECTION: the symptom was "postgres reads REQUIRED-down" in tests about the
// required/optional severity split — a plausible-looking failure about severity, three files away
// from the environment that caused it. cmd/orchicon/isolate_test.go clears the ambient PLANE env;
// nothing cleared the instance STATE DIR, because that is not env at all — it is a file under $HOME.
//
// THE SEAM is the one postgres_legacy_volume.go already uses for the engine probe: a var, "so tests
// can stub it". Stubbing it in init makes the whole test binary hermetic by default — a test written
// later cannot forget — and realPgIsReadyBin keeps the production resolver reachable so its two
// branches stay covered (services_health_probe_test.go).

// realPgIsReadyBin is the production resolver, preserved before init replaces the seam. Tests that
// assert on the RESOLUTION itself call this, so hermeticity costs no coverage.
var realPgIsReadyBin = pgIsReadyBin

func init() {
	// The BARE name, deliberately: it re-enters PATH resolution, so a test's fake `pg_isready` on PATH
	// is the binary that actually runs — which is what these tests always meant by stubbing PATH.
	pgIsReadyBin = func(string) string { return "pg_isready" }
}
