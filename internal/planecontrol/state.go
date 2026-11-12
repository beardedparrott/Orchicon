package planecontrol

// state.go — WHERE an instance's plane state lives, and the port/residency records read back from
// it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The plane's SHAPE, as the installer records it. See RecordedResidency.
const (
	ResidencyHost      = "host"
	ResidencyContainer = "container"
)

// ServeStateDirEnv makes the detached plane's state PER INSTANCE.
const ServeStateDirEnv = "ORCHICON_SERVE_STATE_DIR"

// userHomeDir is a SEAM, not indirection for its own sake: the two-instance isolation test has to place two
// instances under different homes without mutating a process-wide $HOME that the rest of the suite shares.
var userHomeDir = os.UserHomeDir

// HostDataDir is the per-instance host state directory — where the secrets KEK, the blob store, the
// ask history and the detached plane's PID file live.
func HostDataDir(instance string) (string, error) {
	home, err := userHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for the instance data dir: %w", err)
	}
	return filepath.Join(home, ".local", "share", "orchicon-"+instance), nil
}

// HostDataDirFor is HostDataDir for callers that cannot fail usefully: it returns a best-effort path when
// the home directory cannot be resolved, which happens only where the repair is moot.
func HostDataDirFor(instance string) string {
	d, err := HostDataDir(instance)
	if err != nil {
		return filepath.Join(os.TempDir(), "orchicon-"+instance)
	}
	return d
}

// StateDir is the detached plane's state root for an instance (<data dir>/serve) — the directory
// ORCHICON_SERVE_STATE_DIR names for every child this package starts.
func StateDir(instance string) string { return filepath.Join(HostDataDirFor(instance), "serve") }

// ServePIDFile is the PID file `serve --detach` writes for an instance. Derived, never shared: this is the
// one file that decides WHICH process a stop may signal.
func ServePIDFile(instance string) string {
	return filepath.Join(StateDir(instance), "pids", "orchicon.pid")
}

// residencyRecordFile is the per-instance record of the plane's SHAPE, written next to the other
// install records in the instance state dir (ports.json, infra-binaries.json).
const residencyRecordFile = "residency"

// ResidencyRecordPath is <hostDataDir>/residency — the instance's own state dir, never a shared location.
func ResidencyRecordPath(instance string) (string, error) {
	dir, err := HostDataDir(instance)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, residencyRecordFile), nil
}

// THE BOOL IS THE WHOLE POINT: ok=false means the shape was NOT readable — absent, empty, or
// holding something that is not `host`/`container`.
func RecordedResidency(instance string) (string, bool) {
	path, err := ResidencyRecordPath(instance)
	if err != nil {
		return "", false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	switch v := strings.TrimSpace(string(b)); v {
	case ResidencyHost, ResidencyContainer:
		return v, true
	default:
		return "", false
	}
}

// RecordResidency writes the shape down.
func RecordResidency(instance, residency string) {
	path, err := ResidencyRecordPath(instance)
	if err != nil {
		fmt.Printf("warning: could not locate the instance state dir to record the residency: %v\n", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Printf("warning: could not record the residency shape: %v\n", err)
		return
	}
	if err := os.WriteFile(path, []byte(residency+"\n"), 0o644); err != nil {
		fmt.Printf("warning: could not record the residency shape: %v\n", err)
	}
}

// IT IS NAMED "ports.json" AND LIVES IN THE INSTANCE STATE DIR, which matters to uninstall: a state
// dir containing ONLY this file is not an instance.
func PortsRecordPath(instance string) (string, error) {
	dir, err := HostDataDir(instance)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ports.json"), nil
}

// ReadPorts reads back the ports an install recorded.
func ReadPorts(instance string) (map[string]int, error) {
	path, err := PortsRecordPath(instance)
	if err != nil {
		return nil, fmt.Errorf("locate the port record for %s: %w", instance, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No record: either the instance predates the record or was never installed.
			return nil, fmt.Errorf("no port record at %s — this instance predates it, or was never installed.\n"+
				"  Re-run `orchicon install` once to record the ports it bound; it is idempotent and leaves a running instance alone.",
				path)
		}
		return nil, fmt.Errorf("read the port record %s: %w", path, err)
	}
	ports := make(map[string]int)
	if err := json.Unmarshal(b, &ports); err != nil {
		return nil, fmt.Errorf("parse the port record %s: %w", path, err)
	}
	if len(ports) == 0 {
		return nil, fmt.Errorf("the port record at %s is empty — re-run `orchicon install` to rewrite it", path)
	}
	return ports, nil
}

// NOTHING POSIX-ONLY LIVES HERE: the two primitives this rests on — the liveness probe and the
// state read — are the unexported seams processAlive/procStateChar.
func PidAlive(pid int) bool {
	if !processAlive(pid) {
		return false
	}
	if state, ok := procStateChar(pid); ok {
		switch state {
		case 'R', 'S', 'D', 'T', 't', 'W':
			return true
		default:
			// Z (zombie), X (dead), or anything unrecognised: not a running server.
			return false
		}
	}
	return true
}

// ProcStateChar reads the process state character — 'R', 'S', 'Z' and so on — where the platform
// has one.
func ProcStateChar(pid int) (byte, bool) { return procStateChar(pid) }
