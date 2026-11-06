package db

// testguard.go — A TEST BINARY MAY NEVER OPEN THE PRODUCTION DATABASE.
//
// WHY THIS EXISTS, STATED PLAINLY, because it is not hypothetical.
//
// A full `go test ./...` was run from a shell that had a live PROD plane in it — the launcher had exported that
// plane's ORCHICON_INSTANCE=prod and its ORCHICON_POSTGRES_DSN. The DB-backed suites read ORCHICON_TEST_DSN, and
// that variable was pointed at the SAME prod database so the suites could run. They ran. Every one of them wrote
// to production: ~125 test projects appeared in the deployment tenant and 281 throwaway tenants were created in
// a live instance, because the tests create real rows, real projects and real work items by design.
//
// The mistake was a single variable assignment, and the reason it was POSSIBLE is that nothing in the code knew
// the difference between "a database" and "THIS database". A test that opens whatever DSN it is handed will
// happily open the operator's production instance, and every guard around it — the skip when the variable is
// unset, the per-test temp tenant, the cleanup hook — is about isolation BETWEEN tests, not about protection
// FROM the live one.
//
// SO THE GUARD IS HERE, at db.Open, because that is the ONE place every database handle is created: all 69
// DB-backed test call sites funnel through out, and no test in the tree reaches pgxpool directly except one that
// points at a deliberately unreachable host. A guard anywhere else would be a guard some test could walk past.
//
// HOW IT DECIDES. Under `go test` the binary is a test binary, and it is about to open a DSN. The question is
// only "is this the instance the operator is RUNNING?", and that is answered by the environment the shell
// already carries: an exported ORCHICON_INSTANCE=prod, or an ORCHICON_POSTGRES_DSN that matches the DSN being
// opened. Both are evidence that the shell belongs to a live plane rather than to CI.
//
// IT REFUSES RATHER THAN WARNS. A warning in a test log is read by nobody, and the damage is already done by the
// time it prints. A refusal is a failing test with a message that names the variable to change.
//
// AND IT IS ESCAPABLE, deliberately. There are legitimate reasons to run a DB test against a prod-like database
// (a restore rehearsal on a scratch clone that happens to be labelled prod), so a test may set
// ORCHICON_TEST_ALLOW_PROD=1 to state that it means it. The point is that reaching production becomes a
// DECISION WITH A NAME, never an accident of which shell the operator happened to be in.

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	// THE LAYERING TRADE IS ACCEPTED DELIBERATELY. A storage package reading plane state is a smell, but
	// planecontrol imports only the standard library, so there is no cycle — and the alternative is
	// duplicating the state-dir and port-record layout here, which is the kind of second implementation that
	// drifts. The guard is worth one import.
	"github.com/beardedparrott/orchicon/internal/planecontrol"
)

// allowProdEnv is the explicit opt-in. Set it to "1" to run a DB-backed test against a database the guard would
// otherwise refuse.
const allowProdEnv = "ORCHICON_TEST_ALLOW_PROD"

// instanceEnv and dsnEnv are the ambient plane's own variables — the ones the launcher exports into the
// operator's shell. They are READ here, never written: the guard's job is to notice them, and a guard that
// mutated the operator's environment would break the shell it is protecting.
const (
	instanceEnv = "ORCHICON_INSTANCE"
	dsnEnv      = "ORCHICON_POSTGRES_DSN"
)

// isTestBinary reports whether this process is a `go test` binary.
//
// `go test` compiles the test binary with a `-test.*` flag already registered, so a registered flag is the
// reliable signal — and it is a flag REGISTRATION rather than a flag VALUE, so it holds however the binary is
// invoked. (The alternative, strings.HasSuffix(os.Args[0], ".test"), misses `go test -c` output that was
// renamed, and `go test` sets no TESTING variable of its own.)
func isTestBinary() bool {
	return flagLookup("test.v") != nil || flagLookup("test.run") != nil || flagLookup("test.timeout") != nil
}

// guardAgainstProdDSN refuses a test-binary Open of the live plane's database.
//
// It returns an error ONLY for the case it is for. Everything else — a dev instance, a temp database, a DSN that
// is not the ambient plane's, a non-test process — falls straight through, because a guard that refuses
// legitimate work is a guard that gets deleted.
func guardAgainstProdDSN(dsn string) error {
	if !isTestBinary() {
		return nil
	}
	if os.Getenv(allowProdEnv) == "1" {
		return nil
	}

	ambientInstance := strings.TrimSpace(os.Getenv(instanceEnv))
	ambientDSN := strings.TrimSpace(os.Getenv(dsnEnv))

	// The strongest signal: the DSN being opened IS the plane's own DSN. This is the exact shape of the
	// incident — ORCHICON_TEST_DSN was assigned the live ORCHICON_POSTGRES_DSN.
	sameDatabase := ambientDSN != "" && dsnEqual(ambientDSN, dsn)

	// The weaker one: the shell says it is a production instance, and the test is opening a REAL database. A
	// test is allowed to open a throwaway database on a prod-labelled host only by saying so explicitly
	// (ORCHICON_TEST_ALLOW_PROD), because "it is only a scratch db" is not something this function can verify
	// and the cost of being wrong is a live tenant full of test rows.
	prodInstance := strings.EqualFold(ambientInstance, "prod")

	// ORDER MATTERS HERE, AND GETTING IT WRONG IS HOW THIS GUARD FAILED ITS OWN LIVE PROOF.
	//
	// The first version checked `prodInstance && isLocalDSN(dsn)` BEFORE deciding, on the assumption that a
	// loopback database is the test's own. In THIS installation that assumption is false in the worst way: the
	// production plane runs as a HOST process on localhost:5433 while the dev container publishes on 5432. So
	// the live production DSN is a "local" DSN, the escape hatch swallowed it, and running a DB-backed test with
	// the shell's own prod DSN — the exact incident — was ALLOWED. The guard reproduced the bug it was written
	// to prevent, and only the end-to-end run showed it: every unit case passed, because they asserted the
	// decision function on a REMOTE prod DSN and never on the local one this machine actually uses.
	//
	// So: MATCHING THE PLANE'S OWN DATABASE IS REFUSED FIRST, unconditionally, however local it looks. There is
	// no reading of "the test is opening the database the running plane is using" that this should permit
	// silently, and the explicit opt-out remains for the operator who means it.
	if sameDatabase {
		return guardRefusal(sameDatabase, dsn, ambientInstance, ambientDSN)
	}
	if !prodInstance {
		return nil
	}

	// A PROD-LABELLED SHELL, opening something that is NOT the plane's database. A loopback DSN is the test's
	// own (the dev instance / the services container that `make test` and CI use), and refusing it would break
	// the very command this guard protects — in the shell it was written for. Note this is safe NOW only because
	// the equal case has already returned above: a "local" DSN that IS the plane's can no longer reach here.
	if isLocalDSN(dsn) {
		return nil
	}

	return guardRefusal(false, dsn, ambientInstance, ambientDSN)
}

// guardAgainstLivePlaneDSN refuses a test-binary Open of a database a RUNNING plane is serving, whether or not
// that plane is labelled prod.
//
// THE GAP THIS CLOSES. The guard above answers "is this the PRODUCTION database", and the README answers "which
// DSN should I use" with "the DEV instance's database". Those two together left the commonest local case
// completely unprotected: a developer with the dev plane running points ORCHICON_TEST_DSN at
// localhost:5432/orchicon — which IS the database that plane's own reconcilers are serving — and the two sets of
// reconcilers converge on the same rows.
//
// MEASURED on a real workstation: the scheduler suite failed 3-5 tests per run, varying run to run, with
//
//	park task blocked: db: version conflict
//	fail sequence ancestor: db: version conflict
//	precondition: parent should be failed (halted), got "running"
//
// — every one an optimistic-concurrency collision with the live plane, which scans the same pending/running rows
// the test just created. CI never sees it because CI's database has no plane attached. The same session also
// found 1,576 project rows of accumulated test residue in that live database.
//
// HOW IT DECIDES, and each condition is required so this cannot refuse legitimate work:
//
//  1. the DSN names the SAME database an instance's plane opens — compared as host:port/database, so a SEPARATE
//     database on the same cluster (orchicon_test) is untouched;
//  2. that instance's plane is ALIVE — polled at its own /healthz, the same question the desktop asks;
//  3. the instance is one this host has a port record for, so an unrelated DSN is never guessed at.
//
// The escape hatch is separate from the prod one, because the decision is different: this is "I know a plane is
// up and I want to run against it anyway", not "this database happens to be labelled prod".
func guardAgainstLivePlaneDSN(dsn string) error {
	if !isTestBinary() || os.Getenv(allowSharedPlaneEnv) == "1" {
		return nil
	}
	target, ok := dsnTarget(dsn)
	if !ok {
		return nil
	}
	for _, name := range []string{strings.TrimSpace(os.Getenv(instanceEnv)), "dev", "prod"} {
		if name == "" {
			continue
		}
		if served := livePlaneTargetFn(name); served != "" && sameDatabaseTarget(served, target) {
			return fmt.Errorf("db: REFUSING to open a database a RUNNING plane is serving, from a test binary.\n"+
				"  opening: %s\n"+
				"  instance %q is live on that database (answered /healthz), so its reconcilers and this test "+
				"are converging on the same rows — which is the `db: version conflict` flakiness, not a bug in "+
				"the test.\n"+
				"  Two ways forward, both deliberate: stop that plane for the duration of the run, or point "+
				"ORCHICON_TEST_DSN at a database no plane serves (a separate database on the same cluster is "+
				"fine — this guard compares host:port/database). Set %s=1 to run against it anyway.",
				redactDSN(dsn), name, allowSharedPlaneEnv)
		}
	}
	return nil
}

// livePlaneTargetFn is the DETECTION SEAM, so the guard has coverage on a CI host with no plane rather than only
// on a developer's workstation — the same reason `isTestBinary` is built on a `flagLookup` seam. Tests replace it
// to drive both branch outcomes without a running plane.
var livePlaneTargetFn = livePlaneTarget

// livePlaneTarget returns the "host:port/database" an instance's plane opens WHEN THAT PLANE IS RUNNING, or ""
// when the instance has no port record or its plane does not answer.
//
// The database name and host are the ones planeEnv builds for the plane itself
// (internal/planecontrol/env.go: `postgres://orchicon:orchicon@localhost:<port>/orchicon`). They are spelled here
// rather than derived because that DSN is what the plane is HANDED, and it is a fixed shape — but the PORT comes
// from the instance's own record, so a non-default install is honoured.
func livePlaneTarget(instance string) string {
	ports, err := planecontrol.ReadPorts(instance)
	if err != nil {
		return ""
	}
	pgPort := ports["ORCHICON_POSTGRES_PORT"]
	controlPort := ports["ORCHICON_CONTROL_PORT"]
	if pgPort <= 0 || controlPort <= 0 {
		return ""
	}
	if !planeAnswers(controlPort) {
		return ""
	}
	return fmt.Sprintf("localhost:%d/orchicon", pgPort)
}

// planeAnswers asks the plane's own readiness endpoint, bounded tightly: this runs once per test-binary Open and
// must never be the reason a suite is slow.
func planeAnswers(controlPort int) bool {
	c := &http.Client{Timeout: 400 * time.Millisecond}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", controlPort))
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK
}

// allowSharedPlaneEnv is the deliberate opt-out for running DB-backed tests against a live plane's database.
const allowSharedPlaneEnv = "ORCHICON_TEST_ALLOW_SHARED_DB"
// sameDatabaseTarget compares two "host:port/database" targets, treating every LOOPBACK SPELLING as the same host.
//
// IT IS NOT A COSMETIC NORMALISATION. The plane is handed `localhost`; an operator's ORCHICON_TEST_DSN very
// commonly says `127.0.0.1` (that is what the README's own example and most copy-pasted DSNs use). Compared as
// raw strings those two never match, so the guard silently did nothing on the exact workstation it was written
// for — found by running it, not by reading it.
func sameDatabaseTarget(a, b string) bool {
	ah, ap, ad, ok := splitDatabaseTarget(a)
	bh, bp, bd, bok := splitDatabaseTarget(b)
	if !ok || !bok {
		return false
	}
	return loopbackEquivalent(ah, bh) && ap == bp && ad == bd
}

// splitDatabaseTarget splits "host:port/database" into its three parts.
func splitDatabaseTarget(t string) (host, port, database string, ok bool) {
	slash := strings.LastIndex(t, "/")
	if slash < 0 {
		return "", "", "", false
	}
	hostPort, database := t[:slash], t[slash+1:]
	colon := strings.LastIndex(hostPort, ":")
	if colon < 0 {
		return "", "", "", false
	}
	return hostPort[:colon], hostPort[colon+1:], database, database != ""
}

// loopbackEquivalent reports whether two hosts both name the local machine.
//
// `localhost`, `127.0.0.1` and `::1` are the same database to a test; anything else is a different host and must
// never be treated as equivalent, because the guard would then refuse a genuinely remote database.
func loopbackEquivalent(a, b string) bool {
	if strings.EqualFold(a, b) {
		return true
	}
	loop := func(h string) bool {
		h = strings.ToLower(strings.TrimSpace(h))
		return h == "localhost" || h == "127.0.0.1" || h == "::1" || h == "[::1]"
	}
	return loop(a) && loop(b)
}

// guardRefusal builds the one refusal message, so both reasons read the same way and redact the same way.
func guardRefusal(sameDatabase bool, dsn, ambientInstance, ambientDSN string) error {
	reason := "the shell this test was run from has a LIVE plane in it"
	if sameDatabase {
		reason = fmt.Sprintf("the DSN being opened IS the ambient %s — the database the running plane uses", dsnEnv)
	}
	return fmt.Errorf("db: REFUSING to open a production database from a test binary — %s.\n"+
		"  opening: %s\n"+
		"  %s=%q  %s=%q\n"+
		"  This guard exists because a `go test ./...` run once wrote ~125 test projects and 281 throwaway "+
		"tenants into a live instance: the suites create real rows by design, and nothing here could tell a "+
		"database from THEIR database.\n"+
		"  Point %s at the DEV instance's database instead (this is the rule: tests use dev, never prod). If you "+
		"genuinely mean to run against this one, set %s=1.",
		reason, redactDSN(dsn), instanceEnv, ambientInstance, dsnEnv, redactDSN(ambientDSN), "ORCHICON_TEST_DSN", allowProdEnv)
}

// isLocalDSN reports whether a DSN points at a host-local database (a loopback address or a unix socket), which
// is where the dev instance and the test services containers live.
//
// It is deliberately permissive in what it RECOGNISES and strict in what it concludes: only an explicitly local
// host qualifies, so an unrecognised DSN is treated as remote — the safe direction, since treating a remote
// database as local is the failure that costs a tenant.
func isLocalDSN(dsn string) bool {
	d := strings.ToLower(dsn)
	for _, local := range []string{"localhost", "127.0.0.1", "::1", "host=/", "host =/"} {
		if strings.Contains(d, local) {
			return true
		}
	}
	return false
}

// redactDSN removes the password from a DSN so a guard error can name the target without printing a credential.
func redactDSN(dsn string) string {
	// scheme://user:password@host — redact the password only.
	at := strings.LastIndex(dsn, "@")
	if at < 0 {
		return dsn
	}
	head := dsn[:at]
	schemeEnd := strings.Index(head, "://")
	if schemeEnd < 0 {
		return dsn
	}
	creds := head[schemeEnd+3:]
	colon := strings.Index(creds, ":")
	if colon < 0 {
		return dsn
	}
	return head[:schemeEnd+3] + creds[:colon] + ":***" + dsn[at:]
}

// dsnEqual compares two DSNs for "the same database", tolerating the two differences that do not change the
// target: surrounding whitespace, and a trailing query string that only sets sslmode.
//
// IT IS DELIBERATELY CONSERVATIVE. A false negative here (failing to notice two spellings of one database) means
// the guard misses and the weaker prodInstance rule still applies; a false POSITIVE would refuse a legitimate
// test database. So it compares the host/port/database triple, not the whole string.
func dsnEqual(a, b string) bool {
	if strings.TrimSpace(a) == strings.TrimSpace(b) {
		return true
	}
	ka, oka := dsnTarget(a)
	kb, okb := dsnTarget(b)
	if !oka || !okb {
		return false
	}
	return ka == kb
}

// dsnTarget extracts "host:port/database" from a URL-form DSN, which is what identifies the database.
func dsnTarget(dsn string) (string, bool) {
	d := strings.TrimSpace(dsn)
	schemeEnd := strings.Index(d, "://")
	if schemeEnd < 0 {
		return "", false
	}
	rest := d[schemeEnd+3:]
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	if q := strings.Index(rest, "?"); q >= 0 {
		rest = rest[:q]
	}
	if rest == "" {
		return "", false
	}
	return strings.ToLower(rest), true
}
