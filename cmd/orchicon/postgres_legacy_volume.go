package main

// The container-era postgres cluster guard.
//
// THE FAILURE THIS PREVENTS. The container→host residency move changed WHERE
// postgres keeps its cluster, but the two locations are not the same data:
//
//	container-era   docker volume  orchicon_postgres-data  → mounted at /var/lib/orchicon/postgres
//	host residency  host dir       <dataDir>/postgres      (container.go: -D <dataDir>/postgres)
//
// Nothing migrated between them — install_host.go:47 says so plainly ("the
// container shape that wrote a docker volume is being retired, so there is no
// container-era volume to migrate FROM on a fresh install"), which is true for a
// FRESH install and false for an instance that was installed before the switch.
//
// And the two failure modes are both silent:
//
//   - NO host cluster (an empty <dataDir>/postgres — prod's case, 0 entries):
//     initPostgres runs initdb and the plane comes up on a BRAND-NEW EMPTY
//     DATABASE, while 5.7 GB of the real one sits untouched in the volume. It
//     looks like a working plane with no data in it.
//   - A STALE host cluster (dev's case: a real 48 MB PG16 cluster from
//     2026-08-02): postgres starts on TWO-MONTH-OLD data. It looks like a
//     working plane that silently rolled back.
//
// The existing guards cannot catch either. migrate_host_data_dir
// (scripts/container.sh:165) and ensureHostDataDir (install_host.go:52) both
// return EARLY when <dataDir>/secrets/kek already exists — and it does, for
// these instances, because the container bind-mounts $HOME so the volume and the
// host dir share one KEK (verified identical for dev and prod). Both therefore
// read "host has a KEK" as "already migrated", which is a false positive: the
// keys date from when the volume was seeded FROM the host dir, and the host
// postgres was never touched again.
//
// SO THE GUARD SITS WHERE THE DAMAGE HAPPENS: the supervisor's own postgres
// bring-up, on host residency only, before initdb can run. It is deliberately
// NARROW — a postgres volume named for THIS instance is the only thing it acts
// on, and volume-existence is the only fact it needs, so it never runs a
// container, never pulls an image, and never needs root.

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/runtime"

	"github.com/beardedparrott/orchicon/internal/procwin"
)

// allowFreshPostgresEnv is the explicit opt-out. A postgres volume can exist and
// still be EMPTY (created by a run that never reached initdb), and the guard
// cannot see inside one without root — so without an escape hatch it could block
// a legitimate first start forever. Requiring the operator to say the word turns
// a silent data loss into a deliberate, visible choice.
const allowFreshPostgresEnv = "ORCHICON_ALLOW_FRESH_POSTGRES"

// containerEraPostgresVolume is the docker/podman volume a CONTAINER-era install
// of this instance kept its cluster in, or "" when the instance has none.
//
// It mirrors scripts/lib/instances.sh (PG_VOLUME), including honouring
// ORCHICON_PG_VOLUME, because the two launchers must describe the SAME volume for
// one instance — a disagreement here would guard the wrong thing.
//
// `test` is deliberately absent: instances.sh gives it PG_VOLUME=fresh, i.e. it
// is torn down rather than kept, so there is no cluster to protect and no guard
// to fire.
func containerEraPostgresVolume(instance string) string {
	if v := strings.TrimSpace(os.Getenv("ORCHICON_PG_VOLUME")); v != "" {
		return v
	}
	switch instance {
	case "dev":
		return "orchicon_postgres-data"
	case "prod":
		return "orchicon-prod_postgres-data"
	}
	return ""
}

// postgresVolumeExists is the ENGINE PROBE, kept as a var so tests can stub it.
// Without the seam every host-residency unit test would consult the real daemon
// and depend on what this machine happens to have installed.
var postgresVolumeExists = func(engineBin, name string) bool {
	// Bounded: a missing or wedged daemon must not delay a plane start. Two
	// seconds is generous for `volume inspect` on a live socket and short enough
	// that a host with no engine notices nothing.
	cmd := procwin.Command(engineBin, "volume", "inspect", name)
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return false
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err == nil
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		return false
	}
}

// engineCandidateBins lists the engine binaries whose volume stores could hold a
// CONTAINER-ERA volume for this instance, most likely first.
//
// IT MUST NOT BE THE RUNTIME'S ENGINE ALONE, and that is not hypothetical. The
// runtime prefers Podman (rootless, ADR-0014) while every container-era instance
// in the field was created by Docker — so a guard that probed only the resolved
// runtime engine was blind on exactly the hosts it exists to protect. On this
// operator's machine `podman volume inspect orchicon-prod_postgres-data` answers
// "no such volume" while Docker holds the 5.7 GB cluster the guard is about, so
// the guard stayed silent and a rebuild would have initdb'd an empty database
// beside it: the failure it was written to prevent, one layer down.
//
// WHERE A VOLUME LIVES IS A HISTORICAL FACT about the instance, not a consequence
// of which engine the runtime now prefers, so every installed engine is asked.
//
// A var, so a test pins the candidate set instead of depending on what the
// machine running it happens to have installed.
var engineCandidateBins = func() []string {
	var bins []string
	seen := map[string]bool{}
	add := func(bin string) {
		if bin == "" || seen[bin] {
			return
		}
		seen[bin] = true
		bins = append(bins, bin)
	}
	// The runtime's choice first: when the volume IS there, one probe answers.
	if e, err := runtime.ResolveEngineFromEnv(); err == nil {
		add(e.Bin)
	}
	for _, name := range []string{"docker", "podman"} {
		if p, err := exec.LookPath(name); err == nil {
			add(p)
		}
	}
	return bins
}

// findContainerEraVolume locates the container-era volume in ANY installed engine
// and reports WHICH engine binary holds it. The engine is returned because the
// remediation printed on refusal has to name it — telling an operator to run
// `docker run` against a Podman volume moves no data and looks like it worked.
func findContainerEraVolume(volume string) (engineBin string, found bool) {
	for _, bin := range engineCandidateBins() {
		if postgresVolumeExists(bin, volume) {
			return bin, true
		}
	}
	return "", false
}

// guardContainerEraPostgresCluster decides whether this host-residency boot may
// touch <dataDir>/postgres at all, given a container-era volume for the same
// instance.
//
// hostClusterInitialized says whether <dataDir>/postgres already carries a
// PG_VERSION — the same marker initPostgres uses, so the guard and the action it
// guards can never disagree about "initialized".
func guardContainerEraPostgresCluster(hostDataDir string, hostClusterInitialized bool, out io.Writer) error {
	instance := strings.TrimSpace(os.Getenv("ORCHICON_INSTANCE"))
	if instance == "" {
		instance = "dev"
	}
	volume := containerEraPostgresVolume(instance)
	if volume == "" {
		return nil // nothing this instance could have left behind
	}
	// No engine, no evidence. A host with no docker/podman cannot have a volume,
	// and must never be blocked by one — findContainerEraVolume returns not-found
	// for an empty candidate set.
	engineBin, found := findContainerEraVolume(volume)
	if !found {
		return nil
	}
	// The engine that ACTUALLY holds the cluster, by its CLI name, because every
	// remediation below is a command the operator will run against that engine.
	engine := filepath.Base(engineBin)

	hostCluster := filepath.Join(hostDataDir, "postgres")

	if hostClusterInitialized {
		// BOTH exist. Not fatal — the operator may have migrated already and kept
		// the volume as a backup, which is the safest thing they could have done.
		// But the plane is now running on the host cluster and the other copy is
		// invisible from inside it, so say which one is live.
		fmt.Fprintf(out, "note: %s also has a container-era postgres volume (%s, in %s).\n"+
			"      this plane is running on the HOST cluster %s — the volume is not being read.\n"+
			"      if the volume holds newer data than the host cluster, stop and migrate it before continuing.\n",
			instance, volume, engine, hostCluster)
		return nil
	}

	// NO host cluster, but a container-era volume exists. initdb WOULD run here
	// and create an empty database beside the real one. Refuse.
	if strings.TrimSpace(os.Getenv(allowFreshPostgresEnv)) != "" {
		fmt.Fprintf(out, "continuing past the container-era postgres volume (%s): %s is set, so a fresh cluster will be created in %s\n",
			volume, allowFreshPostgresEnv, hostCluster)
		return nil
	}
	return fmt.Errorf(
		"refusing to initialise a NEW postgres cluster: instance %q has a container-era postgres volume (%s, in %s) "+
			"and %s does not exist.\n"+
			"  Starting here would create an EMPTY database while the real one stays in %s — the plane would come up "+
			"looking healthy with no data in it.\n"+
			"  Either migrate the volume first:\n"+
			"      %s run --rm -v %s:/src alpine tar cf - -C /src . | tar xf - -C %s\n"+
			"  or, if that volume is genuinely empty and you want a fresh instance, say so explicitly:\n"+
			"      %s=1\n",
		instance, volume, engine, hostCluster, volume, engine, volume, hostCluster, allowFreshPostgresEnv)
}
