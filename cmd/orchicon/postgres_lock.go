package main

// postgres_lock.go — a stale postmaster.pid must not stop the database from starting.
//
// REPORTED, in the services log of a real install:
//
//	started component=postgres pid=1604
//	FATAL:  lock file "postmaster.pid" already exists
//	HINT:  Is another postmaster (PID 26376) running in data directory "…/orchicon-dev/postgres"?
//	postgres exited during startup: exit status 1
//
// POSTGRES USES postmaster.pid AS A LOCK, and it is a real one: if a postmaster is alive the
// new one MUST refuse to start, because two postmasters on one data directory corrupt it. So
// the check cannot be "does the file exist" — it must be "is the process it names still
// alive".
//
// AND WHY IT WAS ALIVE. The services supervisor is stopped by PID, and terminating one process
// does not take that process's CHILDREN with it. So postgres was ORPHANED: it kept the data
// directory's lock and its port, and the next install's postgres refused to start.
//
// Two things must hold, and fixing only one leaves the other:
//  1. a stop must kill the TREE (see terminateProcessTree), and
//  2. a start must survive a lock left by a supervisor that was killed outright, because such
//     a supervisor cannot clean up after itself.
//
// A lock naming a DEAD process is stale and is removed. A lock naming a LIVE process is left
// alone, so postgres gets to refuse with its own accurate diagnosis.
//
// THE LOGIC IS PLATFORM-NEUTRAL ON PURPOSE. The sweep is only NEEDED where the tree kill is
// unavailable, but the DECISION ("is this lock stale?") is pure and is tested on every host —
// a bug here is a database that cannot start, and a test that only ran on the one platform
// nobody has in CI is a test nobody runs.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// clearStalePostmasterLock removes a postmaster.pid whose process is gone.
//
// It returns an error only for a lock it cannot READ. A live process's lock is left alone (and
// postgres refuses, with a better diagnosis than this could compose); a dead process's lock is
// removed and the caller continues.
//
// enabled is false on the platforms where the tree kill makes an orphan impossible — there the
// lock cannot survive a stop, and the sweep would be dead code that still had to be right.
func clearStalePostmasterLock(dataDir string, out io.Writer, enabled bool) error {
	if !enabled {
		return nil
	}
	lockPath := filepath.Join(dataDir, "postgres", "postmaster.pid")

	raw, err := os.ReadFile(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no lock: the ordinary case
		}
		return fmt.Errorf("read the postgres lock file %s: %w", lockPath, err)
	}

	// THE FIRST LINE IS THE POSTMASTER'S PID.
	pid := 0
	if line := strings.TrimSpace(strings.SplitN(string(raw), "\n", 2)[0]); line != "" {
		if n, cerr := strconv.Atoi(line); cerr == nil {
			pid = n
		}
	}
	if pid <= 0 {
		// UNPARSEABLE: treat it as stale. A lock file that cannot say which process it
		// protects is protecting nothing, and refusing to start over it would make the
		// database unrecoverable without hand-editing the data directory.
		if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove the unreadable postgres lock file %s: %w", lockPath, err)
		}
		fmt.Fprintf(out, "removed an unreadable postgres lock file (stale): %s\n", lockPath)
		return nil
	}

	if pidAlive(pid) {
		// A LIVE PROCESS HOLDS IT. Leave it: postgres will refuse to start, and its refusal
		// names the pid and the directory, which is the diagnosis. Silently removing a live
		// process's lock would let a second postmaster start on the same data directory.
		return nil
	}

	// DEAD PROCESS: the lock is a leftover from a supervisor that was killed outright.
	if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove the stale postgres lock file %s: %w", lockPath, err)
	}
	fmt.Fprintf(out, "removed a stale postgres lock file (pid %d is gone): %s\n", pid, lockPath)
	return nil
}

// staleLockSweepNeeded reports whether this platform can leave an orphaned postgres behind.
//
// TRUE ONLY ON WINDOWS. There, terminating the supervisor terminates ONE process, so its children
// survive it; on Unix the supervisor leads its own process group and the group kill takes every
// child with it, so the lock cannot survive a stop.
func staleLockSweepNeeded() bool { return runtime.GOOS == "windows" }
