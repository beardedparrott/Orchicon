package main

// install_engine_test.go — the engine BINARY the installer drives.
//
// The ladder (resolveContainerEngine) decides the engine and records it in engineCLI, but
// several paths never run the ladder — `orchicon restart` on a recorded container shape, the
// bridge probe, the port read-back. Those paths used to hardcode "docker", which on the
// podman-only host this feature exists to produce is a command that is not there. These tests
// pin the two halves of the seam: the install's decision wins when there is one, and a lazy
// resolution through internal/containerengine is what stands in when there is not.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resetEngineBinary drops the memoised resolution so a test sees its own PATH.
func resetEngineBinary(t *testing.T) {
	t.Helper()
	engineMu.Lock()
	engineResolved, engineCLI = "", ""
	engineMu.Unlock()
	t.Cleanup(func() {
		engineMu.Lock()
		engineResolved, engineCLI = "", ""
		engineMu.Unlock()
	})
}

// TestEngineCommandUsesTheInstallsDecision: once the ladder reached "ready", every later
// engine call goes through the binary it resolved — that is the whole point of the variable.
func TestEngineCommandUsesTheInstallsDecision(t *testing.T) {
	resetEngineBinary(t)
	engineCLI = "/fake/install/decision/podman"

	if got := engineBinary(); got != "/fake/install/decision/podman" {
		t.Fatalf("the install's decision must win, got %q", got)
	}
	if cmd := engineCommand("restart", "orchicon-cnt-dev"); cmd.Path != "/fake/install/decision/podman" {
		t.Fatalf("engineCommand must drive the resolved binary, got %q", cmd.Path)
	}
}

// TestEngineCommandResolvesWithoutRunningTheLadder is the regression this file exists for: a
// path that never runs the ladder must NOT assume "docker". With podman the only engine
// visible, `orchicon restart` has to call podman — on the host this feature creates there is
// no docker binary at all.
//
// The fake binary is a file in a t.TempDir() on PATH and is never executed (Detect is a
// LookPath/stat probe), which is this repo's posture for engine probing.
func TestEngineCommandResolvesWithoutRunningTheLadder(t *testing.T) {
	resetEngineBinary(t)

	dir := t.TempDir()
	fake := filepath.Join(dir, "podman")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake podman: %v", err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("ORCHICON_CONTAINER_ENGINE", "")

	cmd := engineCommand("restart", "orchicon-cnt-dev")
	if got := filepath.Base(cmd.Path); got != "podman" {
		t.Fatalf("a path that never runs the ladder must resolve the engine, got %q (want podman, not docker)", got)
	}
	if !strings.HasSuffix(strings.Join(cmd.Args, " "), "restart orchicon-cnt-dev") {
		t.Fatalf("the arguments must survive resolution, got %v", cmd.Args)
	}
	// The resolution is memoised: a restart asks more than once and each ask must not re-probe.
	if first, second := engineBinary(), engineBinary(); first != second {
		t.Fatalf("resolution must be memoised: %q then %q", first, second)
	}
}
