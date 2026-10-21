package planecontrol

import (
	"os"
	"testing"

	"github.com/beardedparrott/orchicon/internal/containerengine"
)

// TestInstalledEngineWithADeadDaemonIsNotInUse — the bug that made a local install demand a
// Docker bridge.
//
// REPORTED: "orchicon start dev" refused with "cannot resolve the docker bridge address … refusing
// to guess or bind 0.0.0.0", on a machine where the operator had chosen LOCAL execution and told
// the installer to fetch no images. Docker Desktop was installed with its daemon wedged.
//
// THE CAUSE. RuntimeContainersInUse's last rung returned ContainerEngineAvailable(), which asks
// internal/containerengine.Detect — an EXECUTION-FREE probe answering "is a binary installed?"
// from PATH and stat. An installed-but-dead engine answers yes, so the plane computed a bridge
// bind it could never use and refused to start.
//
// THE DISTINCTION IS THE WHOLE POINT: installed is not available. This drives it with a fake
// engine binary that reports a version (so Detect finds it) but fails on `info` (so the daemon is
// plainly dead).
func TestInstalledEngineWithADeadDaemonIsNotInUse(t *testing.T) {
	dir := t.TempDir()
	// A binary that looks like docker to Detect and fails like a wedged daemon to the probe.
	script := "#!/bin/sh\ncase \"$1\" in\n  info) echo 'failed to connect to the docker API at npipe:////./pipe/dockerDesktopLinuxEngine' >&2; exit 1;;\n  *) echo 'Docker version 27.1.1'; exit 0;;\nesac\n"
	if err := os.WriteFile(dir+"/docker", []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir)
	t.Setenv(runtimeContainersEnv, "")        // no explicit answer: the fallback decides
	t.Setenv("ORCHICON_DOCKER_BRIDGE_IP", "") // and nothing is pinned
	t.Setenv(containerengine.EnginePinEnv, "docker")

	if !ContainerEngineAvailable() {
		t.Skip("the fixture did not present an installed engine; this test would prove nothing")
	}
	if RuntimeContainersInUse() {
		t.Error("an INSTALLED engine whose daemon is dead must not count as in use.\n" +
			"  That is what made a local install demand a docker bridge it could never bind, and " +
			"refuse to start: \"cannot resolve the docker bridge address\".")
	}
}
