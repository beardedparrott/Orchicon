package main

// THE DECISION THIS FILE RECORDS: plane lifecycle control is LOCAL PROCESS INVOCATION ONLY.

import (
	"io"
	"path/filepath"
	"time"

	"github.com/beardedparrott/orchicon/internal/planecontrol"
)

// planeDriftState / planeUnknown / planeCurrent / planeDrifted are ALIASES, not a second enum: the values are
// the shared package's, so a comparison in this package is a comparison of the same type.
type planeDriftState = planecontrol.DriftState

const (
	planeUnknown       = planecontrol.Unknown
	planeCurrent       = planecontrol.Current
	planeDrifted       = planecontrol.Drifted
	residencyHost      = planecontrol.ResidencyHost
	residencyContainer = planecontrol.ResidencyContainer

	// serveStateDirEnv is the shared package's constant: the per-instance detached-plane state root.
	serveStateDirEnv = planecontrol.ServeStateDirEnv
)

// startHostPlane launches the instance's HOST plane (detached) and waits for its /healthz.
func startHostPlane(instance string, ports map[string]int, hostDataDir, runtimeSocket string, out io.Writer) error {
	return planecontrol.Start(planecontrol.Options{
		Instance:    instance,
		Out:         out,
		HostDataDir: hostDataDir,
		// The socket path is handed in whole by the caller, so it is split back into the dir the shared
		// layer expects. The directory is what Options carries; nothing else about it is configurable.
		SocketDir: filepath.Dir(runtimeSocket),
		Ports:     ports,
	})
}

// stopHostPlane stops the instance's detached host plane, gracefully and with a bounded wait.
func stopHostPlane(instance string, out io.Writer) error {
	return planecontrol.Stop(planecontrol.Options{Instance: instance, Out: out})
}

// planeDrift asks whether the serving plane is running the binary in hand.
func planeDrift(instance string, out io.Writer) planeDriftState {
	return planecontrol.Drift(instance, out)
}

// servingPlanePID / servingPlaneAlive answer the "is anything of OURS up?" question from the PID file.
func servingPlanePID(instance string) (int, bool) { return planecontrol.ServingPlanePID(instance) }
func servingPlaneAlive(instance string) bool      { return planecontrol.ServingPlaneAlive(instance) }

// terminateProcess asks a host process this installer started to stop.
func terminateProcess(pid int) error { return planecontrol.TerminateProcess(pid) }

// pidAlive / procStateChar are the zombie-aware liveness pair (see planecontrol.PidAlive).
func pidAlive(pid int) bool                              { return planecontrol.PidAlive(pid) }
func procStateChar(pid int) (byte, bool)                 { return planecontrol.ProcStateChar(pid) }
func httpHealthy(url string, timeout time.Duration) bool { return planecontrol.Healthy(url, timeout) }

// hostDataDir / hostDataDirFor / readResolvedPorts / recordedResidency / recordResidency are the per-instance
// state paths and records.
func hostDataDir(instance string) (string, error) { return planecontrol.HostDataDir(instance) }
func hostDataDirFor(instance string) string       { return planecontrol.HostDataDirFor(instance) }
func readResolvedPorts(instance string) (map[string]int, error) {
	return planecontrol.ReadPorts(instance)
}
func residencyRecordPath(instance string) (string, error) {
	return planecontrol.ResidencyRecordPath(instance)
}
func recordedResidency(instance string) (string, bool) {
	return planecontrol.RecordedResidency(instance)
}
func recordResidency(instance, residency string) { planecontrol.RecordResidency(instance, residency) }

// containerEngineAvailable / runtimeContainersInUse / planeBindEnv / planeEnv / envWithout / bridgeIP are the
// environment the plane is started with, computed in exactly one place.
func containerEngineAvailable() bool { return planecontrol.ContainerEngineAvailable() }
func runtimeContainersInUse() bool   { return planecontrol.RuntimeContainersInUse() }
func planeBindEnv(ports map[string]int) (string, string, error) {
	return planecontrol.PlaneBindEnv(ports)
}
func planeEnv(instance string, ports map[string]int, hostDataDir, runtimeSocket string) ([]string, error) {
	return planecontrol.PlaneEnv(instance, ports, hostDataDir, runtimeSocket)
}
func envWithout(env []string, keys ...string) []string { return planecontrol.EnvWithout(env, keys...) }
func bridgeIP() (string, error)                        { return planecontrol.BridgeIP() }

// terminateProcessTree stops a process and everything it spawned.
func terminateProcessTree(pid int) error { return planecontrol.TerminateProcessTree(pid) }
