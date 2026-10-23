//go:build windows

package planecontrol

// hostproc_windows.go — the Windows half of the host plane's PROCESS IDENTITY probe. It is IMPLEMENTED, not
// stubbed, and the reasons are worth stating because the previous version of this file argued the opposite.
//
// WHY THE STUBS WERE WRONG. `Drift` returning `Unknown` is a genuine fail-safe: the caller prints "could not
// identify the process; leaving it" and does not restart. But `ServingPlaneAlive` returning `false` is NOT a
// fail-safe — it makes `plane status` report "no plane PID file" for a plane that IS running, and it makes
// `plane stop` a silent no-op (`planecontrol.go`'s Stop treats `!Alive` as "nothing to stop" and returns
// nil). An operator tool that cannot see its own plane is not erring on the safe side; it is lying. So the
// three probes below are real: a process handle plus its exit code, and a process image path.
//
// WHY NOT /proc: Windows has none, and `syscall.Kill` is absent from its syscall package — which is why the
// untagged POSIX versions broke the BUILD here rather than merely misbehaving at runtime.
//
// WHY STDLIB `syscall` AND NOT golang.org/x/sys/windows: imports_test.go forbids every module path but
// internal/containerengine, so this package must stay a leaf. `OpenProcess`, `GetExitCodeProcess`,
// `TerminateProcess` and `UTF16ToString` are all in GOROOT's Windows syscall package;
// `QueryFullProcessImageNameW` is not, and is reached through `syscall.NewLazyDLL("kernel32.dll")` — still
// stdlib, so the imports test stays honest.
//
// THE CONSTANTS STDLIB DOES NOT DECLARE are declared here, with their values, rather than pulling in a
// dependency for three integers: processQueryLimitedInformation (0x1000), stillActive (259) and, in
// cmd/orchicon's Windows files, DETACHED_PROCESS (0x8).
//
// WHAT WINDOWS LOSES, STATED SO NOBODY HAS TO DISCOVER IT LATER:
//
//   - `Drift` NEVER reports `Drifted` here. The Unix fingerprint of drift is a `/proc/<pid>/exe` link whose
//     path ends in " (deleted)" — the exact signature of an installer replacing a binary under a live plane.
//     A RUNNING WINDOWS IMAGE CANNOT BE UNLINKED (the file is locked), so that state cannot arise: replacing
//     a live plane's binary fails loudly at the copy, which is a better failure than a silent drift. The
//     remaining answer is `Current` when the image path matches ours and `Unknown` (leave it alone) when it
//     does not — the same direction the Unix `exe != self` arm fails in.
//   - There is no POSIX zombie state, so `procStateChar` reports nothing and the exit-code test is the whole
//     liveness answer. `PidAlive` copes: an unreadable state leaves the probe's verdict standing.
//   - THE ORPHAN SWEEP IS INERT HERE, and that is a consequence of this platform, not a bug to fix later:
//     see cmd/orchicon/serve_reap_windows.go, which now says so at boot instead of being silently empty.
//   - Process identity for RECLAIM cannot be established (no `/proc/<pid>/exe` and no `/proc/<pid>/environ`),
//     so a stopped instance's leftovers are REPORTED and never terminated: see
//     cmd/orchicon/reclaim_proc_windows.go.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// The three Windows facts stdlib syscall does not spell out. See the file header.
const (
	// processQueryLimitedInformation (PROCESS_QUERY_LIMITED_INFORMATION) is the smallest access right that
	// can read an exit code and an image path, so it works against a plane this process did not start and
	// against one running at another integrity level.
	processQueryLimitedInformation = 0x1000
	// stillActive is STILL_ACTIVE: GetExitCodeProcess reports it while the process has not terminated.
	stillActive = 259
	// maxImagePath is the buffer QueryFullProcessImageNameW wants; Windows paths are capped at 32767 UTF-16
	// units, which is what the API's own MAX_LONG_PATH is.
	maxImagePath = 32767
)

var (
	modkernel32                    = syscall.NewLazyDLL("kernel32.dll")
	procQueryFullProcessImageNameW = modkernel32.NewProc("QueryFullProcessImageNameW")
)

// processAlive is the Windows liveness probe: open the process, ask for its exit code, and treat
// STILL_ACTIVE as running. An unopenable PID (gone, or not ours to open at all) is not alive.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

// procStateChar reports nothing: Windows has no POSIX process state, and in particular no zombie state that
// a live PID could linger in. The exit-code test in processAlive is the whole answer. ok=false means "this
// platform cannot say", which PidAlive already handles.
func procStateChar(pid int) (byte, bool) { return 0, false }

// procExe resolves the IMAGE PATH of a running process — the Windows equivalent of reading
// /proc/<pid>/exe, and what makes "is the serving plane the binary in hand" answerable here.
func procExe(pid int) (string, error) {
	if pid <= 0 {
		return "", os.ErrNotExist
	}
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	buf := make([]uint16, maxImagePath)
	n := uint32(len(buf))
	r, _, callErr := procQueryFullProcessImageNameW.Call(uintptr(h), 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)))
	if r == 0 {
		return "", callErr
	}
	if n == 0 || int(n) > len(buf) {
		return "", os.ErrNotExist
	}
	return syscall.UTF16ToString(buf[:n]), nil
}

// Drift identifies the serving plane from the instance's PID file and compares its image path with this
// binary's.
//
// IT RETURNS `Current` OR `Unknown`, NEVER `Drifted` — see the file header for why the Unix drift
// fingerprint cannot arise on Windows, which makes that arm unreachable rather than unimplemented.
//
// The comparison is CASE-INSENSITIVE (`strings.EqualFold`) because Windows paths are: `filepath.EvalSymlinks`
// does not normalise case, so a byte comparison would report "not ours" for the same file spelled two ways
// and leave a stale plane running after an upgrade.
func Drift(instance string, out io.Writer) DriftState {
	self, err := executable()
	if err != nil {
		return Unknown
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return Unknown
	}
	pid, ok := ServingPlanePID(instance)
	if !ok {
		return Unknown
	}
	exe, err := procExe(pid)
	if err != nil {
		return Unknown
	}
	if strings.EqualFold(filepath.Clean(exe), filepath.Clean(self)) {
		return Current
	}
	// Serving from a DIFFERENT path than the binary in hand. It may be a dev checkout serving deliberately;
	// only a plane the INSTALLER started is ours to restart, so leave it and say where it runs from.
	fmt.Fprintf(out, "  (plane runs from %s, not %s — leaving it)\n", exe, self)
	return Unknown
}

// ServingPlanePID reads the instance's detached-plane PID file and reports whether that process is alive.
func ServingPlanePID(instance string) (int, bool) {
	b, err := os.ReadFile(ServePIDFile(instance))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	// The PID file outlives the process: the handle/exit-code test is what says whether it is still that
	// process.
	if !processAlive(pid) {
		return 0, false
	}
	return pid, true
}

// ServingPlaneAlive reports whether the instance's PID-file process is still running.
func ServingPlaneAlive(instance string) bool {
	pid, ok := ServingPlanePID(instance)
	if !ok {
		return false
	}
	return processAlive(pid)
}

// TerminateProcess force-stops a host process. WINDOWS HAS NO SIGTERM, so this is SIGKILL semantics:
// TerminateProcess runs no shutdown hook, so the plane never flushes or stops its children, and its own
// children are NOT reaped (the sweep that would collect them is inert here — see serve_reap_windows.go).
//
// IT IS LOAD-BEARING, NOT VESTIGIAL: cmd/orchicon's sendForceStop is this function on Windows, and that is
// the whole stop path for `serve --stop` and the supervisor's shutdown escalation (signals_windows.go) — so
// every Windows stop is a force stop with the consequences above. `orchicon install` and reclaim's
// force-kill arm reach it through cmd/orchicon's terminateProcess; reclaim itself identifies no process on
// this platform (reclaim_proc_windows.go), so that arm is unreachable HERE and the two paths above are the
// live ones. It refuses this process's own PID.
func TerminateProcess(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("refusing to terminate pid %d", pid)
	}
	if pid == os.Getpid() {
		return fmt.Errorf("refusing to terminate this process (pid %d)", pid)
	}
	h, err := syscall.OpenProcess(syscall.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("open process %d for termination: %w", pid, err)
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	if err := syscall.TerminateProcess(h, 1); err != nil {
		return fmt.Errorf("terminate process %d: %w", pid, err)
	}
	return nil
}

// TerminateProcessTree terminates a process AND EVERYTHING IT SPAWNED.
//
// WHY THIS EXISTS ALONGSIDE TerminateProcess. TerminateProcess ends ONE process. The services
// supervisor's children — postgres, nats, the telemetry backends — are separate processes, so
// stopping the supervisor left them RUNNING: postgres kept the data directory's postmaster.pid
// lock and its port, and the next install's postgres refused to start against it.
//
//	started component=postgres pid=1604
//	FATAL:  lock file "postmaster.pid" already exists
//	HINT:  Is another postmaster (PID 26376) running in data directory "…/orchicon-dev/postgres"?
//
// taskkill /T is the documented way to terminate a process tree on Windows. It is a child
// process rather than a syscall, so it can fail where TerminateProcess would not — which is
// why the caller still gets a useful error and the lock sweep covers the residue.
func TerminateProcessTree(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("refusing to terminate pid %d", pid)
	}
	if pid == os.Getpid() {
		return fmt.Errorf("refusing to terminate this process (pid %d)", pid)
	}
	// The tree first: it reaps the children.
	if out, err := quietCommand("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").CombinedOutput(); err == nil {
		return nil
	} else {
		// FALL BACK to terminating the supervisor itself. The children may survive — the lock
		// sweep exists for exactly that — but the supervisor is the thing that was asked to
		// stop, so it must go.
		_ = out
	}
	return TerminateProcess(pid)
}

// ----- instance-scoped process identity -----

// procsUnder is the set of processes whose IMAGE lives inside a directory tree, with their parents.
//
// WINDOWS HAS NO /proc, SO IDENTITY IS DERIVED FROM THE IMAGE PATH — which is enough here, and better
// than a heuristic, because an instance's supervised children are run from the instance's OWN bin and
// data directories. That is a property of the deployment, not of a naming convention: the resolver
// unpacks postgres, nats and the telemetry binaries under <dataDir>/bin, and postgres itself execs its
// per-connection backends from that same bin directory.
//
// It is what makes "did my stop actually stop everything?" answerable on this platform at all. Today it
// is not: see reclaim_proc_windows.go, where leftovers are REPORTED and never cleared, so a stopped
// instance can leave a process holding the postgres socket for ever.
// ProcInfo is one process's identity. Created is what separates a LEFTOVER from a process that is starting
// RIGHT NOW — see the Unix half for the measured failure a check without it produced.
type ProcInfo struct {
	PID     int
	PPID    int
	Exe     string
	Created time.Time
}

// ProcessesUnder returns every process whose executable path is inside root, using the Toolhelp32
// snapshot API. os.ErrNotExist means "no such processes"; an error means the snapshot itself failed.
//
// PATHS ARE COMPARED AFTER NORMALISATION, case-insensitively, because Windows is: a process started as
// C:\Users\x\... and a root recorded as c:/users/x/... are the same tree.
func ProcessesUnder(root string) ([]ProcInfo, error) {
	if strings.TrimSpace(root) == "" {
		return nil, nil
	}
	normRoot := normalisePath(root)
	procs, err := snapshotProcesses()
	if err != nil {
		return nil, err
	}
	var out []ProcInfo
	for _, p := range procs {
		if p.Exe == "" {
			continue
		}
		exe := normalisePath(p.Exe)
		if exe == normRoot || strings.HasPrefix(exe, normRoot+string(os.PathSeparator)) ||
			strings.HasPrefix(exe, normRoot+"/") {
			out = append(out, p)
		}
	}
	return out, nil
}

// normalisePath folds separators and case so two spellings of one Windows path compare equal.
func normalisePath(p string) string {
	p = strings.ReplaceAll(p, "/", string(os.PathSeparator))
	return strings.ToLower(filepath.Clean(p))
}

// processEntry is the Toolhelp32 shape, sized for the platform's PROCESSENTRY32W.
type processEntry struct {
	dwSize              uint32
	cntUsage            uint32
	th32ProcessID       uint32
	th32DefaultHeapID   uintptr
	th32ModuleID        uint32
	cntThreads          uint32
	th32ParentProcessID uint32
	pcPriClassBase      int32
	dwFlags             uint32
	szExeFile           [260]uint16
}

var (
	procProcess32FirstW          = modkernel32.NewProc("Process32FirstW")
	procProcess32NextW           = modkernel32.NewProc("Process32NextW")
	procCreateToolhelp32Snapshot = modkernel32.NewProc("CreateToolhelp32Snapshot")
)

const (
	th32csSnapProcess = 0x00000002
	invalidHandle     = ^uintptr(0)
)

// snapshotProcesses enumerates every process's PID, parent PID and image path.
//
// IMAGE PATHS COME FROM QueryFullProcessImageNameW, NOT from PROCESSENTRY32W's szExeFile, which holds
// only the BASE NAME. A base name cannot distinguish this instance's postgres from the operator's own
// database install, and that distinction is the whole point of asking.
func snapshotProcesses() ([]ProcInfo, error) {
	h, _, err := procCreateToolhelp32Snapshot.Call(th32csSnapProcess, 0)
	if uintptr(h) == invalidHandle {
		return nil, fmt.Errorf("toolhelp snapshot: %w", err)
	}
	defer func() { _ = syscall.CloseHandle(syscall.Handle(h)) }()

	var e processEntry
	e.dwSize = uint32(unsafe.Sizeof(e))
	r, _, _ := procProcess32FirstW.Call(uintptr(h), uintptr(unsafe.Pointer(&e)))
	if r == 0 {
		return nil, nil
	}
	var out []ProcInfo
	for {
		pid := int(e.th32ProcessID)
		if pid > 0 && pid != os.Getpid() {
			exe, _ := procExe(pid) // unreadable for a process we may not inspect; keep it anyway
			out = append(out, ProcInfo{PID: pid, PPID: int(e.th32ParentProcessID), Exe: exe,
				Created: processCreated(pid)})
		}
		e.dwSize = uint32(unsafe.Sizeof(e))
		r, _, _ = procProcess32NextW.Call(uintptr(h), uintptr(unsafe.Pointer(&e)))
		if r == 0 {
			break
		}
	}
	return out, nil
}

// TerminateSubtree terminates every process in procs that is part of the tree rooted at root.
//
// TWO SHAPES MUST GO, AND THE SECOND ONE IS WHY THIS EXISTS:
//
//  1. the tree taskkill can see — children of the root, still parent-linked;
//
//  2. an ORPHAN. postgres's per-connection backends are re-parented when the postmaster they belong to
//     exits, so after a force-stop they are still running, still hold the data directory open and still
//     share the listening socket — while their PPID names a process that no longer exists:
//
//     pid 30488 postgres.exe (postmaster)   -> terminated
//     pid 26236 postgres.exe "postgres.exe --forkaux 3892 0"  parent=30488 (DEAD), still serving
//     Get-NetTCPConnection -LocalPort 5432  -> OwningProcess 30488   (a pid that is gone)
//
// A re-parented backend keeps its ORIGINAL executable, which is inside the instance's own bin tree, so
// it is identified by ProcessesUnder rather than by walking parents.
//
// IT KILLS NOTHING IT CANNOT VOUCH FOR: `eligible` decides ownership, and this only terminates what it
// returns true for. A process outside the caller's ownership test is reported, never killed — the same
// boundary reclaim.go draws, and for the same reason (a wrong kill is unrecoverable).
func TerminateSubtree(root int, procs []ProcInfo, eligible func(ProcInfo) bool) []int {
	if root <= 0 {
		return nil
	}
	// Breadth-first over the parent links, so a child of an eligible process is eligible even if its own
	// image were replaced mid-flight.
	children := map[int][]int{}
	for _, p := range procs {
		children[p.PPID] = append(children[p.PPID], p.PID)
	}
	byPID := map[int]ProcInfo{}
	for _, p := range procs {
		byPID[p.PID] = p
	}

	var order []int
	seen := map[int]bool{}
	queue := []int{root}
	seen[root] = true
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		order = append(order, cur)
		for _, c := range children[cur] {
			if !seen[c] {
				seen[c] = true
				queue = append(queue, c)
			}
		}
	}
	// Everything in the caller's ownership set goes too, parent-linked or not: that is the orphan case.
	for _, p := range procs {
		if !seen[p.PID] && eligible(p) {
			seen[p.PID] = true
			order = append(order, p.PID)
		}
	}

	var killed []int
	for _, pid := range order {
		if pid == root {
			continue // the caller terminates the root itself, so it controls the ordering and the error
		}
		if err := TerminateProcess(pid); err == nil {
			killed = append(killed, pid)
		}
	}
	return killed
}

// processCreated reads a process's creation time via GetProcessTimes.
//
// A zero time means "could not read", which callers treat as NOT a leftover — the direction whose mistake
// is a refusal that names the process, rather than a start that fights its own child.
var procGetProcessTimes = modkernel32.NewProc("GetProcessTimes")

func processCreated(pid int) time.Time {
	if pid <= 0 {
		return time.Time{}
	}
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return time.Time{}
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	var creation, exit, kernel, user syscall.Filetime
	r, _, _ := procGetProcessTimes.Call(uintptr(h),
		uintptr(unsafe.Pointer(&creation)), uintptr(unsafe.Pointer(&exit)),
		uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user)))
	if r == 0 {
		return time.Time{}
	}
	return time.Unix(0, creation.Nanoseconds())
}

// WaitGone blocks until none of pids is alive, up to timeout, and reports what survived.
//
// A TERMINATE IS ASYNCHRONOUS: TerminateProcess returns as soon as the request is accepted, so a caller
// that immediately re-binds the port races the kernel's teardown. Every one of this tool's stop paths
// needs the WAIT, not the signal, and the survivors it names are what turn "it did not work" into a
// diagnosis.
func WaitGone(pids []int, timeout time.Duration) []int {
	deadline := time.Now().Add(timeout)
	for {
		var alive []int
		for _, pid := range pids {
			if processAlive(pid) {
				alive = append(alive, pid)
			}
		}
		if len(alive) == 0 || time.Now().After(deadline) {
			return alive
		}
		time.Sleep(50 * time.Millisecond)
	}
}
