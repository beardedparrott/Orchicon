//go:build !windows

package planecontrol

// hostproc_unix.go — the host plane's PROCESS IDENTITY probe. It is Unix-shaped by construction: it
// reads `/proc/<pid>/exe` to learn what a running plane is actually executing, and it asks
// `syscall.Kill(pid, 0)` whether a PID from a stale PID file is still a live process. Neither exists
// on Windows, and `syscall.Kill` is ABSENT from the Windows syscall package — so these three functions
// sitting in the untagged install_host.go broke the BUILD for windows/amd64 and windows/arm64 rather
// than degrading at runtime.
//
// THAT IS THE SAME BUG THIS REPOSITORY ALREADY FIXED ONCE, and serve_reap_unix.go states the lesson in
// as many words: A FILE THAT SHELLS OUT TO /bin/sh OR CALLS syscall.Kill IS A UNIX FILE, and if it
// lives in an untagged file it is a Windows build failure waiting for the next release. The warning did
// not travel with the code, so the platform pair is re-stated here. hostproc_windows.go is the other
// half, and the pair is what makes "Windows degrades deliberately" true in the compiler rather than in a
// comment.
//
// THE PAIR MOVED HERE from cmd/orchicon so the TUI (`cmd/orch`) and the Wails app can reach it at all: a
// `package main` cannot be imported, so a lifecycle layer left there was callable by exactly one caller.

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// processAlive is the POSIX liveness probe: signal 0 succeeds while the PID exists. It is a SEAM because
// the same call is impossible on Windows (`syscall.Kill` does not exist, and `(*os.Process).Signal` refuses
// every signal but Kill), where the equivalent is a process handle plus its exit code.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// procStateChar reads the process state character from /proc/<pid>/stat, whose format is
// `pid (comm) state ...` — and `comm` may itself contain spaces and parentheses, so the state is the first
// field AFTER THE LAST ')' rather than a fixed offset.
func procStateChar(pid int) (byte, bool) {
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

// procExe resolves /proc/<pid>/exe — the APEX of what a running process executes. It is what makes "is the
// serving plane the binary in hand" answerable at all, and it is a seam so Status's binary reporting is
// assertable without spawning processes.
func procExe(pid int) (string, error) { return os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)) }

// drift identifies the serving plane from the instance's PID file and compares its executable with this
// one.
//
// WHY THE PID FILE AND /proc: they are the only truthful source. A version endpoint would answer with
// whatever the OLD process believes about itself, and a timestamp comparison cannot see a rebuilt binary of
// the same version. `/proc/<pid>/exe` shows the APEX of the running process, and a replaced path reports
// `(deleted)` — the exact fingerprint of "the installer overwrote the binary under a live plane".
func Drift(instance string, out io.Writer) DriftState {
	// THE PLANE BINARY, NOT MERELY THIS ONE: a thin client (`orch`, the desktop app) is not the binary that
	// serves the plane, so comparing the live process against SELF would report every healthy plane as
	// "serving from a different path" and collapse drift to Unknown from the TUI forever. See
	// resolvePlaneBinary.
	self, err := planeBinary()
	if err != nil {
		return Unknown
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return Unknown
	}
	// effectivePlanePID, NOT ServingPlanePID: a plane that outlived its PID file is still a plane, and its
	// drift is the reason it MUST be replaced rather than left — see the note on effectivePlanePID. Answering
	// Unknown there is what let `Start` report success over a stale binary.
	pid, ok := effectivePlanePID(instance)
	if !ok {
		return Unknown
	}
	exe, err := procExe(pid)
	if err != nil {
		return Unknown
	}
	// A path is `(deleted)`-suffixed once the file it names has been replaced. Anything else is compared
	// against the binary we are running from.
	if strings.HasSuffix(exe, " (deleted)") {
		return Drifted
	}
	if exe != self {
		// Serving from a DIFFERENT path than the binary this install just placed in the install dir. It may
		// be a dev checkout serving deliberately; only a plane the INSTALLER started is ours to restart, and
		// the PID file is what distinguishes them.
		fmt.Fprintf(out, "  (plane runs from %s, not %s — leaving it)\n", exe, self)
		return Unknown
	}
	return Current
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
	// The PID file outlives the process; /proc is what says whether it is still that process.
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err != nil {
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
	return syscall.Kill(pid, 0) == nil
}

// TerminateProcess asks a host process to stop. It is the ONE place that sends a signal to a host process
// this layer started, so it lives in the Unix half with the other syscall.Kill users (see the file
// header): a Windows build must not carry a SIGTERM it cannot send.
func TerminateProcess(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// TerminateProcessTree terminates a process AND EVERYTHING IT SPAWNED.
//
// ON UNIX THIS IS THE GROUP KILL, and it is exact: the services supervisor is started with
// setProcAttrBackground (the setsid analogue), so it leads its own process group, and signalling
// the group reaches every child — postgres, nats, the telemetry backends — at once.
//
// That is why the Windows half needs taskkill: there is no process-group signal there.
func TerminateProcessTree(pid int) error {
	return TerminateProcess(pid)
}

// ----- instance-scoped process identity (the Unix half) -----
//
// The pair to hostproc_windows.go. On this platform identity is a /proc read, so ProcessesUnder and
// TerminateSubtree are exact where Windows has to derive them from image paths. The API is deliberately
// identical on both, because the caller (a quiesce that must clear an instance's leftovers) is one piece
// of logic and must not fork per platform.

// ProcInfo is one process's identity, in the shape the quiesce needs on every platform.
//
// Created is what separates a LEFTOVER from a process that is starting RIGHT NOW, and that distinction is
// load-bearing rather than tidy: a check that asks "is anything of this instance running?" while the
// instance is being started answers YES about the postgres it just spawned, and would refuse to start on
// its own new process. Measured: a lock guard built on that question failed every start with
//
//	the postgres lock … names pid 33384, which is gone, but 1 process(es) of this instance are STILL
//	RUNNING ([27540]) — they hold the data directory open
//
// where 27540 was a DIFFERENT pid on every run and was the postmaster being started at that moment.
type ProcInfo struct {
	PID     int
	PPID    int
	Exe     string
	Created time.Time
}

// ProcessesUnder returns every process whose executable resolves inside root.
//
// /proc/<pid>/exe IS A SYMLINK to the real path, and reading it fails for a process this user may not
// inspect — which is correct: an uninspectable process is one we could not vouch for, so it must not
// appear in a list whose only use is to justify a kill.
func ProcessesUnder(root string) ([]ProcInfo, error) {
	root = filepath.Clean(root)
	if root == "" || root == "." {
		return nil, nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	self := os.Getpid()
	var out []ProcInfo
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		exe, err := os.Readlink(filepath.Join("/proc", e.Name(), "exe"))
		if err != nil || exe == "" {
			continue
		}
		// "path (deleted)" is what a replaced binary looks like; the path itself is what we compare.
		exe = strings.TrimSuffix(exe, " (deleted)")
		if exe == root || strings.HasPrefix(exe, root+string(os.PathSeparator)) {
			out = append(out, ProcInfo{PID: pid, PPID: readPPID(e.Name()), Exe: exe,
				Created: processStartTime(e.Name())})
		}
	}
	return out, nil
}

// readPPID parses /proc/<pid>/stat for the parent PID. The comm field is parenthesised and may contain
// spaces and parentheses, so the parse starts AFTER the LAST ')', which is the only robust way to do it.
func readPPID(pidDir string) int {
	b, err := os.ReadFile(filepath.Join("/proc", pidDir, "stat"))
	if err != nil {
		return 0
	}
	s := string(b)
	i := strings.LastIndex(s, ")")
	if i < 0 || i+1 >= len(s) {
		return 0
	}
	fields := strings.Fields(s[i+1:])
	// After the comm field: state ppid ... — so ppid is fields[1].
	if len(fields) < 2 {
		return 0
	}
	n, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return n
}

// processStartTime reads a process's start time from /proc: field 22 of stat is the start time in clock
// ticks since boot, and /proc/stat's btime is that boot.
//
// A zero time means "this platform could not say", and the callers treat that as NOT a leftover — the
// conservative direction, because the cost of that mistake is a refusal that names the process, while the
// cost of the other is a start that fights its own child.
func processStartTime(pidDir string) time.Time {
	stat, err := os.ReadFile(filepath.Join("/proc", pidDir, "stat"))
	if err != nil {
		return time.Time{}
	}
	s := string(stat)
	i := strings.LastIndex(s, ")")
	if i < 0 {
		return time.Time{}
	}
	fields := strings.Fields(s[i+1:])
	// After comm: state ppid pgrp session tty_nr tpgid flags minflt cminflt majflt cmajflt utime stime
	// cutime cstime priority nice num_threads itrealvalue starttime — so starttime is index 19.
	if len(fields) < 20 {
		return time.Time{}
	}
	ticks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return time.Time{}
	}
	boot, ok := bootTime()
	if !ok {
		return time.Time{}
	}
	hz := 100 // USER_HZ; Linux reports starttime in USER_HZ ticks.
	return boot.Add(time.Duration(ticks) * time.Second / time.Duration(hz))
}

// bootTime is the monotonic origin starttime is measured from.
func bootTime() (time.Time, bool) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "btime ") {
			continue
		}
		secs, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "btime ")), 10, 64)
		if err != nil {
			return time.Time{}, false
		}
		return time.Unix(secs, 0), true
	}
	return time.Time{}, false
}

// TerminateSubtree terminates every process in procs that belongs to the tree rooted at root, or that
// the caller's ownership test accepts. See the Windows half for why an ownership test is not optional:
// a process we cannot vouch for is reported, never killed.
func TerminateSubtree(root int, procs []ProcInfo, eligible func(ProcInfo) bool) []int {
	if root <= 0 {
		return nil
	}
	children := map[int][]int{}
	for _, p := range procs {
		children[p.PPID] = append(children[p.PPID], p.PID)
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
	for _, p := range procs {
		if !seen[p.PID] && eligible(p) {
			seen[p.PID] = true
			order = append(order, p.PID)
		}
	}
	var killed []int
	for _, pid := range order {
		if pid == root {
			continue
		}
		if err := TerminateProcess(pid); err == nil {
			killed = append(killed, pid)
		}
	}
	return killed
}

// WaitGone blocks until none of pids is alive, up to timeout, and reports the survivors.
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

// ───────────────────────── the plane that outlived its PID file ─────────────────────────
//
// REPORTED (Linux): `make instance-rebuild instance=prod` refused with
//
//	Control plane (web UI + API): this instance is already installed on port 8091, which is held by a
//	process this tool cannot identify
//
// and, one layer under it, `orchicon status prod` said "plane: stopped" while :8091 answered /healthz 200.
// MEASURED on the reporter's machine: the listener was OUR OWN `orchicon serve` with ORCHICON_INSTANCE=prod,
// five days old, and its `/proc/<pid>/exe` read `…/bin/orchicon (deleted)` — a rebuild had replaced the
// binary underneath it. Its `serve/pids/` directory was EMPTY.
//
// THAT IS A STATE THE PRODUCT CAN REACH, and every consumer here assumed it could not. A later
// `serve --detach` that loses the port race writes its own PID and then dies; the cleanup of that dead PID
// removes the file, and the ORIGINAL plane keeps serving with no record of it. From then on:
//
//   - Drift answers Unknown (no PID file) so `Start` says "could not identify the process; leaving it" and
//     reports success — a silent failure to upgrade, which is the "I thought I was on the newest code but was
//     still running a stale binary" complaint this codebase already records;
//   - `Stop` runs `serve --stop`, which reads the same missing file, says "serve is not running", and the
//     caller concludes there was nothing to stop;
//   - an install then either refuses on the held port or leaves the stale binary serving.
//
// SO IDENTITY IS RECOVERABLE FROM THE PORT, which is the one fact that is always observable. A listener on the
// instance's RECORDED control port whose executable is our plane binary and whose environment names this
// instance is this instance's plane — the same three-part proof `processBelongsTo` and `isOurBinary` use
// elsewhere, applied to the only handle left.
//
// IT REFUSES ANYTHING IT CANNOT PROVE. A process that fails any of the three checks is not stopped: an
// unidentified listener is a port conflict to report, never a process to kill.

// effectivePlanePID is the pid of the process SERVING this instance: the PID file's process when that file
// names a live one, and otherwise the verified listener on the recorded control port.
func effectivePlanePID(instance string) (int, bool) {
	if pid, ok := ServingPlanePID(instance); ok {
		return pid, true
	}
	// THROUGH THE SEAM, so Drift is assertable in the state it exists for. The seam's production value IS
	// unrecordedPlanePID; a test substitutes a verified orphan without needing a real stale plane serving.
	return unrecordedPlane(instance)
}

// unrecordedPlanePID finds the instance's plane when the PID file does not name it.
//
// Returns ok=false for every case it cannot PROVE: no port record, no listener, a listener that is not our
// binary, or one whose environment names a different instance. Callers treat false as "not identifiable" —
// never as "free" and never as "safe to kill".
func unrecordedPlanePID(instance string) (int, bool) {
	port := recordedControlPort(instance)
	if port <= 0 {
		return 0, false
	}
	pid, ok := listenerOnPort(port)
	if !ok || pid == os.Getpid() {
		return 0, false
	}
	exe, err := procExe(pid)
	if err != nil || !isPlaneBinaryName(exe) {
		return 0, false
	}
	// THE INSTANCE CHECK IS NOT OPTIONAL. Two instances have different control ports, so a listener here that
	// names ANOTHER instance means the port record is wrong or stale — and stopping that process would take
	// down a sibling. Refusing is the only safe answer.
	if !carriesInstance(pid, instance) {
		return 0, false
	}
	return pid, true
}

// isPlaneBinaryName reports whether exe is the binary that SERVES a plane.
//
// The `(deleted)` suffix is the COMMON case here rather than an edge one: replacing the binary is exactly what
// leaves a process unnamed by its PID file, so the suffix is stripped rather than treated as a mismatch. Only
// `orchicon` matches — `orch` is the thin client and never serves a plane.
func isPlaneBinaryName(exe string) bool {
	exe = strings.TrimSuffix(exe, " (deleted)")
	return filepath.Base(exe) == planeBinaryName
}

// carriesInstance reports whether pid's environment names this instance.
//
// /proc/<pid>/environ is NUL-separated with a trailing NUL, so the tag is matched delimiter-terminated OR at
// the end — the same rule cmd/orchicon's processBelongsTo applies, and for the same reason: a bare substring
// would let `ORCHICON_INSTANCE=prod2` answer for `prod`.
func carriesInstance(pid int, instance string) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return false
	}
	tag := "ORCHICON_INSTANCE=" + instance
	env := string(b)
	return strings.Contains(env, tag+"\x00") || strings.HasSuffix(env, tag)
}

// listenerOnPort is the pid listening on a TCP port, read from /proc: the kernel's own table plus the socket
// inodes each process holds open. No external tool, because a stop path must not depend on lsof existing.
func listenerOnPort(port int) (int, bool) {
	if port <= 0 {
		return 0, false
	}
	// LOCAL_ADDRESS:PORT in /proc/net/tcp is 4 upper-case hex digits after the last colon.
	want := fmt.Sprintf("%04X", port)
	// `socket:[<inode>]` for every LISTEN row on that port; state 0A is TCP_LISTEN.
	// A connection to the port is not a holder, so the state is checked rather than the port alone.
	inodes := map[string]bool{}
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n")[1:] {
			fields := strings.Fields(line)
			if len(fields) < 10 || fields[3] != "0A" {
				continue
			}
			local := fields[1]
			if local[strings.LastIndex(local, ":")+1:] != want {
				continue
			}
			inodes["socket:["+fields[9]+"]"] = true
		}
	}
	if len(inodes) == 0 {
		return 0, false
	}
	self := os.Getpid()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, false
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		fdDir := filepath.Join("/proc", e.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err == nil && inodes[target] {
				return pid, true
			}
		}
	}
	return 0, false
}
