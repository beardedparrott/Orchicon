package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// A STALE LOCK MUST NOT STOP THE DATABASE FROM STARTING.
//
// WINDOWS-ONLY, and that is the point: the sweep exists because Windows has no process-group
// kill, so a stopped supervisor can orphan its postgres. On Unix the group kill takes the
// children with it and the lock never survives, so the Unix half is a no-op — and a test that
// ran against the no-op would pass while proving nothing.
//
// REPORTED, in the services log of a real install:
//
//	started component=postgres pid=1604
//	FATAL:  lock file "postmaster.pid" already exists
//	HINT:  Is another postmaster (PID 26376) running in data directory "…/orchicon-dev/postgres"?
//	postgres exited during startup: exit status 1
//
// The lock names a DEAD process (the supervisor was killed without reaping its children), so it
// is a leftover and the next postgres must be able to start.
//
// A LIVE PROCESS'S LOCK MUST NOT BE TOUCHED: postgres refuses to start for that reason on
// purpose — two postmasters on one data directory corrupt it — and removing the lock would let
// exactly that happen.
func TestStalePostmasterLockIsClearedButALiveOneIsNot(t *testing.T) {
	dir := t.TempDir()
	pgDir := filepath.Join(dir, "postgres")
	if err := os.MkdirAll(pgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(pgDir, "postmaster.pid")

	// A LIVE pid: this process. Its lock must survive.
	live := strconv.Itoa(os.Getpid())
	if err := os.WriteFile(lock, []byte(live+"\n"+dir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := clearStalePostmasterLock(dir, os.Stderr, true); err != nil {
		t.Fatalf("a live process's lock must not be an error: %v", err)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Errorf("a LIVE process's lock was removed — postgres must be allowed to refuse, or two "+
			"postmasters can share one data directory: %v", err)
	}

	// A DEAD pid: one that cannot be running. A very high pid is the reliable way to say
	// "no such process" without racing a real one.
	dead := "4294967294"
	if err := os.WriteFile(lock, []byte(dead+"\n"+dir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := clearStalePostmasterLock(dir, os.Stderr, true); err != nil {
		t.Fatalf("a stale lock must be cleared, not reported: %v", err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("the stale lock was not removed (err=%v) — the next postgres cannot start", err)
	}

	// AN UNREADABLE LOCK: it cannot say which process it protects, so it protects nothing, and
	// refusing to start over it would make the database unrecoverable by hand.
	if err := os.WriteFile(lock, []byte("not a pid at all\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := clearStalePostmasterLock(dir, os.Stderr, true); err != nil {
		t.Fatalf("an unreadable lock must be cleared, not reported: %v", err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("the unreadable lock was not removed (err=%v)", err)
	}

	// NO LOCK AT ALL: the ordinary case, and it must be silent.
	if err := clearStalePostmasterLock(dir, os.Stderr, true); err != nil {
		t.Errorf("no lock must not be an error: %v", err)
	}
}
