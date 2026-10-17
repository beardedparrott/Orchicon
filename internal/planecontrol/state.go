package planecontrol

// state.go — WHERE an instance's plane state lives, and the port/residency records read back from it.
//
// EVERY path in this file derives from ONE root: HostDataDir(instance). That is what makes per-instance
// isolation (two instances never controllable as one) a property of the code rather than a discipline each
// caller has to remember: ports.json, the residency record, and the detached plane's PID file all live
// under that one per-instance directory, and ORCHICON_SERVE_STATE_DIR is only ever set as a CHILD
// environment built from it. No global is written.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// The plane's SHAPE, as the installer records it. See RecordedResidency.
const (
	ResidencyHost      = "host"
	ResidencyContainer = "container"
)

// ServeStateDirEnv makes the detached plane's state PER INSTANCE. Two host-resident planes (dev + prod)
// launched from the same working directory would otherwise share one PID file, and `serve --stop` for one
// would kill the other. This layer only ever sets it as a CHILD env, derived from that instance's dir.
const ServeStateDirEnv = "ORCHICON_SERVE_STATE_DIR"

// userHomeDir is a SEAM, not indirection for its own sake: the two-instance isolation test has to place two
// instances under different homes without mutating a process-wide $HOME that the rest of the suite shares.
var userHomeDir = os.UserHomeDir

// HostDataDir is the per-instance host state directory — where the secrets KEK, the blob store, the ask
// history and the detached plane's PID file live.
//
// IT IS DERIVED FROM THE INSTANCE, exactly as container.sh's instance_info does, and ORCHICON_DATA_DIR is
// deliberately NOT read here. That is not an oversight: container.sh's plane_env SETS ORCHICON_DATA_DIR for
// the host plane, so an operator-exported value is overwritten there — and the KEK lives at
// <DataDir>/secrets/kek. Two launchers disagreeing about the data dir is the one disagreement that silently
// orphans every tenant secret, so this follows the launcher rather than inventing a knob.
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

// residencyRecordFile is the per-instance record of the plane's SHAPE, written next to the other install
// records in the instance state dir (ports.json, infra-binaries.json).
//
// RESIDENCY STOPPED BEING SOMETHING AN ENGINE IS ASKED ABOUT. It used to come from `docker inspect` on the
// instance container's create-time environment (ORCHICON_CONTAINER_SERVICES_ONLY=1); with the container gone
// there is nothing to inspect, and on a machine with no engine a `docker` invocation in an install path is
// exactly the thing that fails silently. The installer therefore RECORDS the shape it created, and reads it
// back. It is per-instance by construction (it lives in that instance's state dir), which is what keeps two
// instances from disagreeing about each other's shape.
const residencyRecordFile = "residency"

// ResidencyRecordPath is <hostDataDir>/residency — the instance's own state dir, never a shared location.
func ResidencyRecordPath(instance string) (string, error) {
	dir, err := HostDataDir(instance)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, residencyRecordFile), nil
}

// RecordedResidency reports the shape this install created for the instance.
//
// THE BOOL IS THE WHOLE POINT: ok=false means the shape was NOT readable — absent, empty, or holding
// something that is not `host`/`container` — and every caller must treat that as "leave it alone", NEVER as
// a mismatch. Recreating a live instance because a shape could not be read is worse than leaving a
// wrong-shaped one for the operator to see.
//
// NO ERROR IS SURFACED, deliberately. An unreadable record cannot be turned into an error by a caller,
// which is what makes the fail-safe mechanical rather than a discipline each call site has to remember.
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

// RecordResidency writes the shape down. Best-effort like WriteResolvedPorts: the install has already
// succeeded by the time this runs, and losing the record costs the operator a re-resolved shape, not a
// broken instance.
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

// PortsRecordPath is where an install records the host ports it actually bound, as JSON keyed by the
// ORCHICON_*_PORT names.
//
// IT IS NAMED "ports.json" AND LIVES IN THE INSTANCE STATE DIR, which matters to uninstall: a state dir
// containing ONLY this file is not an instance, and the shell installer's instance_exists must not treat
// it as one — otherwise installing to a scratch dir would look like another instance still needed the
// binaries. See scripts/install.sh.
func PortsRecordPath(instance string) (string, error) {
	dir, err := HostDataDir(instance)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ports.json"), nil
}

// ReadPorts reads back the ports an install recorded.
//
// THE RECORD IS THE POINT OF THE RECORD. A lifecycle command must be able to act on what was ACTUALLY bound
// without re-resolving, because re-resolving ASKS QUESTIONS — resolveInstallPorts prompts on a terminal when
// the spec is ambiguous — and a command that restarts existing work must never ask anything.
func ReadPorts(instance string) (map[string]int, error) {
	path, err := PortsRecordPath(instance)
	if err != nil {
		return nil, fmt.Errorf("locate the port record for %s: %w", instance, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No record: either the instance predates the record or was never installed. The remedy is
			// stated rather than guessed — re-resolving here would prompt, which is the thing this must
			// not do.
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

// PidAlive reports whether a PID is a RUNNING process.
//
// SIGNAL 0 ALONE IS NOT THE TEST, and getting that wrong made stop verification lie in the other direction.
// `signal 0` succeeds for a ZOMBIE — a process that has exited but whose parent has not yet reaped it — and
// for a stopped-but-traced process, so a server that had genuinely died could still read as alive. The
// bounded stop wait uses this to decide whether a stop is confirmed, so a zombie would make a SUCCESSFUL
// stop report failure and keep the PID file, which is exactly the state that misleads the next rebuild.
// It was caught by driving serveStop against a real child that ignored SIGTERM.
//
// So the state is read from /proc where it is available, and only an R or S or D or T process counts as
// running. Any other state (Z for zombie, X for dead) is not a live process. Where /proc is not readable
// (a non-Linux host, a locked-down procfs) the signal probe stands alone, which is the previous behaviour.
func PidAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if proc.Signal(syscall.Signal(0)) != nil {
		return false
	}
	if state, ok := ProcStateChar(pid); ok {
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

// ProcStateChar reads the process state character from /proc/<pid>/stat, whose format is
// `pid (comm) state ...` — and `comm` may itself contain spaces and parentheses, so the state is the first
// field AFTER THE LAST ')' rather than a fixed offset.
func ProcStateChar(pid int) (byte, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 || i+2 >= len(b) {
		return 0, false
	}
	return b[i+2], true
}
