package main

// The container-era postgres cluster guard.
//
// THE INCIDENT SHAPE THIS PINS (found 2026-10-17 on this operator's own host).
// Both instances predate the container→host residency move, so their live clusters
// are in docker volumes while the host data dirs hold either nothing or something
// two months old:
//
//	dev   host <dir>/postgres = 48MB PG16 from 2026-08-02   live volume = 282MB
//	prod  host <dir>/postgres = EMPTY (0 entries)           live volume = 5.7GB
//
// Nothing migrated between them (install_host.go:47 — no container-era volume is
// expected to migrate FROM on a fresh install, which is true for a fresh install
// and false for a pre-existing one), and BOTH existing guards miss it because
// they return early on an existing <dir>/secrets/kek — which these instances
// have, since the container bind-mounts $HOME and the two paths share one KEK.
//
// The two outcomes are silent in opposite ways: an empty host dir makes initdb
// create a new database (plane looks fine, has no data), and a stale host dir
// makes postgres start on old data (plane looks fine, rolled back).

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// withVolumeProbe stubs the engine probe AND the candidate engine set so these
// tests never consult a real daemon — otherwise they would pass or fail according
// to what the machine running them happens to have installed.
//
// The candidate set is stubbed too, and that is the point of the regression test
// below: the probe's answer was always stubbed, but WHICH ENGINE was asked was
// not, so the tests could not see that the guard asked the wrong one.
func withVolumeProbe(t *testing.T, exists bool) {
	t.Helper()
	prev := postgresVolumeExists
	postgresVolumeExists = func(string, string) bool { return exists }
	t.Cleanup(func() { postgresVolumeExists = prev })
	withEngineCandidates(t, "/usr/bin/docker")
}

// withEngineCandidates pins the engines the guard will ask. Zero arguments means
// "no engine installed on this host", which is a real, healthy case that must not
// be expressed by mutating PATH.
func withEngineCandidates(t *testing.T, bins ...string) {
	t.Helper()
	prev := engineCandidateBins
	engineCandidateBins = func() []string { return bins }
	t.Cleanup(func() { engineCandidateBins = prev })
}

// withPerEngineProbe answers per engine binary. It is the only way to describe the
// shape that actually bit us: the volume lives in ONE engine while the runtime
// prefers the OTHER.
func withPerEngineProbe(t *testing.T, fn func(bin string) bool) {
	t.Helper()
	prev := postgresVolumeExists
	postgresVolumeExists = func(bin, _ string) bool { return fn(bin) }
	t.Cleanup(func() { postgresVolumeExists = prev })
}

// withEngine forces the resolved-engine branch. An engine that "resolves" is all
// the guard needs — it never runs a container.
func withEngine(t *testing.T) {
	t.Helper()
	t.Setenv("ORCHICON_RUNTIME_ENGINE", "docker")
	t.Setenv("ORCHICON_CONTAINER_BIN", "/bin/true")
}

func TestContainerEraPostgresVolumeNames(t *testing.T) {
	cases := map[string]string{
		// Must match scripts/lib/instances.sh PG_VOLUME, or the two launchers
		// disagree about which volume to guard.
		"dev":  "orchicon_postgres-data",
		"prod": "orchicon-prod_postgres-data",
		// `test` is torn down rather than kept (PG_VOLUME=fresh) — no cluster to
		// protect, and a guard that fired there would block throwaway instances.
		"test": "",
		"":     "",
	}
	for instance, want := range cases {
		t.Setenv("ORCHICON_PG_VOLUME", "")
		if got := containerEraPostgresVolume(instance); got != want {
			t.Errorf("containerEraPostgresVolume(%q) = %q, want %q", instance, got, want)
		}
	}
	// The documented override wins, exactly as instances.sh honours it.
	t.Setenv("ORCHICON_PG_VOLUME", "custom-pg")
	if got := containerEraPostgresVolume("dev"); got != "custom-pg" {
		t.Errorf("ORCHICON_PG_VOLUME ignored: got %q", got)
	}
}

// THE DATA-LOSS CASE. No host cluster + a container-era volume ⇒ refuse, because
// initdb would create an empty database beside the real one (prod's 5.7GB).
func TestGuardRefusesFreshClusterWhenContainerEraVolumeExists(t *testing.T) {
	withEngine(t)
	withVolumeProbe(t, true)
	t.Setenv("ORCHICON_INSTANCE", "prod")
	t.Setenv("ORCHICON_PG_VOLUME", "")
	t.Setenv(allowFreshPostgresEnv, "")

	var out bytes.Buffer
	err := guardContainerEraPostgresCluster("/home/x/.local/share/orchicon-prod", false, &out)

	if err == nil {
		t.Fatal("guard allowed a fresh initdb while a container-era postgres volume exists.\n" +
			"That is the silent data-loss path: the plane comes up healthy with an empty database and the\n" +
			"real one stays in the volume, unread.")
	}
	msg := err.Error()
	// The message must be ACTIONABLE, not merely alarming: it names the volume, the
	// empty dir, the migration command, and the explicit opt-out.
	for _, want := range []string{
		"orchicon-prod_postgres-data",
		"/home/x/.local/share/orchicon-prod/postgres",
		"tar cf - -C /src .",
		allowFreshPostgresEnv,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message does not mention %q:\n%s", want, msg)
		}
	}
}

// THE REGRESSION THIS PINS — found hours after the guard shipped, on the
// operator's own host, by asking it a question it could not answer: "the volumes
// are in DOCKER, are you sure you are looking?".
//
// The guard resolved the RUNTIME's engine (Podman is the default, ADR-0014) and
// asked only that engine about the volume. Both real instances keep their cluster
// in DOCKER, so `podman volume inspect orchicon-prod_postgres-data` answered "no
// such volume", the guard returned nil, and a host-residency rebuild would have
// initdb'd an EMPTY database beside the 5.7 GB one — precisely the failure the
// guard exists to prevent, one layer down and completely silent.
//
// The previous tests could not catch it: the probe's ANSWER was stubbed, but the
// engine it was ASKED about was left to the machine's defaults.
func TestGuardFindsTheVolumeInAnEngineOtherThanTheRuntimeEngine(t *testing.T) {
	// The runtime's default (podman) is deliberately NOT pinned, because the defect
	// was that the default was trusted.
	t.Setenv("ORCHICON_RUNTIME_ENGINE", "")
	t.Setenv("ORCHICON_PG_VOLUME", "")
	t.Setenv("ORCHICON_INSTANCE", "prod")
	t.Setenv(allowFreshPostgresEnv, "")

	withEngineCandidates(t, "/usr/bin/podman", "/usr/bin/docker")
	// Only DOCKER has it — the operator's actual shape.
	withPerEngineProbe(t, func(bin string) bool { return bin == "/usr/bin/docker" })

	var out bytes.Buffer
	err := guardContainerEraPostgresCluster("/home/x/.local/share/orchicon-prod", false, &out)
	if err == nil {
		t.Fatal("guard went silent: it asked only the engine the RUNTIME prefers, and the volume is in the other one.\n" +
			"An empty host dir would then be initdb'd beside the real cluster, which is the data loss this guard is for.")
	}
	msg := err.Error()
	if !strings.Contains(msg, "docker run") {
		t.Errorf("remediation does not name the engine that HOLDS the volume:\n%s", msg)
	}
	if strings.Contains(msg, "podman run") {
		t.Errorf("remediation names the runtime's engine, not the one holding the volume — running it would\n"+
			"move no data and look like it worked:\n%s", msg)
	}
	if !strings.Contains(msg, "in docker") {
		t.Errorf("refusal does not say which engine holds the volume:\n%s", msg)
	}
}

// The escape hatch, for a volume that exists but is genuinely empty (created by a
// run that never reached initdb). The guard cannot see inside one without root, so
// it must let the operator say the word rather than block forever.
func TestGuardAllowsFreshClusterWithExplicitOptOut(t *testing.T) {
	withEngine(t)
	withVolumeProbe(t, true)
	t.Setenv("ORCHICON_INSTANCE", "prod")
	t.Setenv("ORCHICON_PG_VOLUME", "")
	t.Setenv(allowFreshPostgresEnv, "1")

	var out bytes.Buffer
	if err := guardContainerEraPostgresCluster("/d", false, &out); err != nil {
		t.Fatalf("explicit opt-out still refused: %v", err)
	}
	if !strings.Contains(out.String(), allowFreshPostgresEnv) {
		t.Errorf("opt-out path did not say so on stdout: %q", out.String())
	}
}

// THE WRONG-DATA CASE. A host cluster exists AND a container-era volume exists ⇒
// warn, naming which one is live. Not fatal: the operator may have migrated and
// kept the volume as a backup, which is the safest thing they could have done.
func TestGuardWarnsWhenBothClustersExist(t *testing.T) {
	withEngine(t)
	withVolumeProbe(t, true)
	t.Setenv("ORCHICON_INSTANCE", "dev")
	t.Setenv("ORCHICON_PG_VOLUME", "")

	var out bytes.Buffer
	if err := guardContainerEraPostgresCluster("/home/x/.local/share/orchicon-dev", true, &out); err != nil {
		t.Fatalf("both-exist must warn, not refuse: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "orchicon_postgres-data") {
		t.Errorf("warning does not name the volume: %q", got)
	}
	if !strings.Contains(got, "HOST cluster") {
		t.Errorf("warning does not say which cluster is live: %q", got)
	}
}

// The no-ops. Each of these must be silent AND error-free, because every one of
// them is an ordinary, healthy situation.
func TestGuardIsSilentWhenItHasNothingToSay(t *testing.T) {
	cases := []struct {
		name        string
		instance    string
		volumeThere bool
		initialized bool
		engine      bool
	}{
		{name: "no container-era volume at all", instance: "dev", volumeThere: false, initialized: false},
		{name: "instance with no guardable volume (test)", instance: "test", volumeThere: true, initialized: false},
		{name: "host with no engine — cannot have a volume", instance: "dev", volumeThere: true, initialized: false, engine: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// The probe is stubbed to report exactly what this case describes, but
			// a nil engine must short-circuit BEFORE the probe is consulted.
			withVolumeProbe(t, c.volumeThere)
			t.Setenv("ORCHICON_INSTANCE", c.instance)
			t.Setenv("ORCHICON_PG_VOLUME", "")
			t.Setenv(allowFreshPostgresEnv, "")
			if c.engine {
				withEngine(t)
			} else {
				t.Setenv("ORCHICON_RUNTIME_ENGINE", "none")
				t.Setenv("ORCHICON_CONTAINER_BIN", "")
				t.Setenv("PATH", "")
				// An empty candidate set IS "no engine installed" now that the guard
				// asks every engine rather than only the resolved one.
				withEngineCandidates(t)
			}

			var out bytes.Buffer
			if err := guardContainerEraPostgresCluster("/d", c.initialized, &out); err != nil {
				t.Fatalf("guard fired on a %q case: %v", c.name, err)
			}
			// Every case here is an ordinary healthy situation, so none may speak.
			// (The both-exist case is NOT silent and has its own test.)
			if out.Len() != 0 {
				t.Errorf("guard spoke on a %q case: %q", c.name, out.String())
			}
		})
	}
}

// A source-level pin on PLACEMENT. The helper existing is not the fix: it has to
// run BEFORE initdb, or the empty database is created anyway.
func TestGuardRunsBeforeInitdb(t *testing.T) {
	raw := readSupervisorGo(t)
	src := string(raw)

	fn := strings.Index(src, "func (s *supervisor) initPostgres() error {")
	if fn < 0 {
		t.Fatal("initPostgres is gone")
	}
	body := src[fn:]
	if end := strings.Index(body[1:], "\nfunc "); end > 0 {
		body = body[:end+1]
	}
	guardIdx := strings.Index(body, "guardContainerEraPostgresCluster(")
	initdbIdx := strings.Index(body, "s.resolved().Initdb")
	if guardIdx < 0 {
		t.Fatal("initPostgres no longer calls the container-era cluster guard — an empty host dir would\n" +
			"again make initdb create a database beside the real one")
	}
	if initdbIdx < 0 {
		t.Fatal("could not locate the initdb exec to compare against")
	}
	if guardIdx > initdbIdx {
		t.Fatalf("the guard appears AFTER the initdb exec (guard at %d, initdb at %d) — checking too\n"+
			"late leaves the empty database already created", guardIdx, initdbIdx)
	}
}

// And a pin that NOTHING can route around it: there is one service shape, so the
// guard must not be gated on anything a caller could switch off. (The retired
// container-resident supervisor kept its cluster in a volume by design and skipped
// the guard; that shape no longer exists.)
func TestGuardIsUnconditional(t *testing.T) {
	src := string(readSupervisorGo(t))
	fn := strings.Index(src, "func (s *supervisor) initPostgres() error {")
	body := src[fn:]
	if end := strings.Index(body[1:], "\nfunc "); end > 0 {
		body = body[:end+1]
	}
	if strings.Contains(body, "isHost()") {
		t.Fatal("initPostgres still branches on a residency selector — there is exactly one service shape")
	}
	if !strings.Contains(body, "guardContainerEraPostgresCluster(") {
		t.Fatal("initPostgres no longer calls the container-era cluster guard")
	}
}

// readSupervisorGo reads host_services.go from this package's directory. The
// placement assertions above are source-level on purpose: a guard that exists but
// runs AFTER initdb leaves the defect fully intact, and no behavioural test can see
// ordering.
func readSupervisorGo(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("host_services.go")
	if err != nil {
		t.Fatalf("read host_services.go: %v", err)
	}
	return b
}
