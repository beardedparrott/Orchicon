package main

// install_engine_test.go — the engine BINARY the installer drives.

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

// TestEngineCommandResolvesWithoutRunningTheLadder is the regression this file exists for: a path
// that never runs the ladder must NOT assume "docker".
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
