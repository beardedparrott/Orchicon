package planecontrol

import (
	"os"
	"testing"

	"github.com/beardedparrott/orchicon/internal/containerengine"
)

// TestInstalledEngineWithADeadDaemonIsNotInUse — the bug that made a local install demand a Docker
// bridge.
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
