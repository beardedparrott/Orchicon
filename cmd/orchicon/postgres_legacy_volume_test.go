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

// withVolumeProbe stubs the engine probe so these tests never consult a real
// daemon — otherwise they would pass or fail according to what the machine
// running them happens to have installed.
func withVolumeProbe(t *testing.T, exists bool) {
	t.Helper()
	prev := postgresVolumeExists
	postgresVolumeExists = func(string, string) bool { return exists }
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
	raw := readContainerGo(t)
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

// And a pin that it is HOST-ONLY: a container-resident supervisor keeps its
// cluster in the volume BY DESIGN, so guarding it there would refuse every normal
// container boot.
func TestGuardIsHostResidencyOnly(t *testing.T) {
	src := string(readContainerGo(t))
	fn := strings.Index(src, "func (s *supervisor) initPostgres() error {")
	body := src[fn:]
	if end := strings.Index(body[1:], "\nfunc "); end > 0 {
		body = body[:end+1]
	}
	guardIdx := strings.Index(body, "guardContainerEraPostgresCluster(")
	hostIdx := strings.Index(body, "s.isHost()")
	if hostIdx < 0 || hostIdx > guardIdx {
		t.Fatal("the container-era cluster guard is not gated on s.isHost() — a container-resident\n" +
			"supervisor keeps its cluster in the volume by design and must never be refused")
	}
}

// readContainerGo reads container.go from this package's directory. The placement
// assertions above are source-level on purpose: a guard that exists but runs AFTER
// initdb leaves the defect fully intact, and no behavioural test can see ordering.
func readContainerGo(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("container.go")
	if err != nil {
		t.Fatalf("read container.go: %v", err)
	}
	return b
}
