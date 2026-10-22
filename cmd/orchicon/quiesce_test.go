package main

// quiesce_test.go — the stop primitive, and the two defects it exists to fix.
//
// The failures below were reproduced on a real Windows machine using nothing but `orchicon stop` and
// `orchicon start`. They are asserted here as PROPERTIES of the code rather than as one machine's story,
// because the machine is not in CI and the next regression will not reproduce the same pids.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A STOP THAT LEAVES A PROCESS HOLDING THE DATA DIRECTORY MUST NOT PRINT A TICK.
//
// This is the operator's report, verbatim:
//
//	the host services for dev (pid 14284) are no longer running
//	✓ dev is stopped
//	… while postgres.exe (orphaned by the postmaster's exit and re-parented) still held 5432.
//
// The assertion is on the SHAPE of the code: runStop must consult the quiesce and must not print success
// when it is not clean. A wiring assertion is the right tool because the defect was a MISSING STEP, not a
// wrong value — the same reason TestHostInstallRecordsItsShape is a wiring assertion.
func TestStopConsultsTheQuiesceAndRefusesToLie(t *testing.T) {
	src := readGoFile(t, "lifecycle.go")

	if callSite(t, src, "quiesceInstance(instance, ports, os.Stdout)") < 0 {
		t.Error("runStop does not quiesce the instance. Terminating the SUPERVISOR's tree is not the same " +
			"as stopping the instance: postgres's per-connection backends are RE-PARENTED when the " +
			"postmaster exits, so they survive it and keep the data directory and port 5432.")
	}
	if !strings.Contains(src, "!q.Clean()") {
		t.Error("runStop does not branch on whether the quiesce came out clean, so it can print " +
			"'✓ stopped' over an instance whose database is still running")
	}
	// And it must not print the tick unconditionally afterwards.
	if strings.Contains(src, `fmt.Printf("✓ %s is stopped\n", instance)`) &&
		!strings.Contains(src, "reportQuiesceFailure") {
		t.Error("runStop prints '✓ … is stopped' without a failure path that could prevent it")
	}
}

// A START AFTER AN UNCLEAN EXIT MUST CLEAR THE INSTANCE'S OWN LEFTOVERS FIRST.
//
// Without it the new supervisor's postgres meets the old run's orphan and refuses — the FATAL /
// "lock file postmaster.pid already exists" the operator saw — and the report blames the services
// pipeline instead of naming a process.
func TestStartClearsLeftoversBeforeStartingServices(t *testing.T) {
	src := readGoFile(t, "lifecycle.go")
	q := callSite(t, src, "quiesceInstance(instance, ports, os.Stdout)")
	start := callSite(t, src, "startHostServices(instance, ports, hostDataDirFor(instance), os.Stdout)")
	if q < 0 {
		t.Fatal("runStart never quiesces, so a start races its own orphan")
	}
	if start < 0 {
		t.Fatal("could not locate the startHostServices call — this guard needs repointing")
	}
	if q > start {
		t.Error("the quiesce runs AFTER startHostServices. It must run BEFORE, or the new postgres meets " +
			"the old run's orphan and refuses with 'lock file postmaster.pid already exists'.")
	}
}

// AN INSTALL MUST CONVERGE OVER A RUNNING INSTANCE, NOT PUT A SECOND STACK ON ITS PORTS.
func TestInstallQuiescesBeforeStartingServices(t *testing.T) {
	src := readGoFile(t, "install.go")
	q := callSite(t, src, "quiesceInstance(instance, ports, os.Stdout)")
	start := callSite(t, src, "startHostServices(instance, ports, hostDataDirFor(instance), os.Stdout)")
	if q < 0 {
		t.Fatal("install never quiesces, so a REINSTALL over a running instance starts a second " +
			"supervisor on the same ports — which is 'it did NOT start orchicon after install'")
	}
	if start < 0 {
		t.Fatal("could not locate the install's startHostServices call")
	}
	if q > start {
		t.Error("install quiesces AFTER starting the services, which is too late to prevent a second stack")
	}
}

// OWNERSHIP IS THE DEPLOYMENT, NOT A PORT NUMBER.
//
// instanceProcessRoots must be inside the instance's OWN data dir. A kill justified by "it holds my port"
// would reach the operator's own PostgreSQL or a sibling instance's services — the boundary reclaim.go
// draws, and the reason this is asserted rather than reviewed.
func TestInstanceProcessRootsAreInsideTheInstancesOwnDataDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	instance := "dev"
	dataDir := hostDataDirFor(instance)
	roots := instanceProcessRoots(instance)
	if len(roots) == 0 {
		t.Fatal("no roots means ownership can never be established, which is the Windows no-op this fixes")
	}
	for _, r := range roots {
		rel, err := filepath.Rel(dataDir, r)
		if err != nil || strings.HasPrefix(rel, "..") {
			t.Errorf("root %q is not inside the instance's data dir %q — a process there is NOT provably "+
				"this instance's, and terminating it could kill the operator's own database", r, dataDir)
		}
	}
}

// A LIVE CHILD MUST PROTECT ITS DATA DIRECTORY EVEN WHEN THE LOCK NAMES A DEAD PARENT.
//
// This is the corruption guard, and it is the highest-stakes assertion here: removing postmaster.pid while
// a re-parented backend still has the cluster open is how two postmasters end up on one data directory.
func TestStaleLockIsNotBrokenWhileTheInstanceStillRuns(t *testing.T) {
	src := readGoFile(t, "postgres_lock.go")
	if !strings.Contains(src, "ownedProcsForDataDir(dataDir)") {
		t.Error("clearStalePostmasterLock removes the lock as soon as the pid in it is gone. postgres " +
			"RE-PARENTS its backends, so the lock can name a dead postmaster while a live backend still " +
			"holds the cluster open — unlinking it lets a second postmaster start on a live data directory.")
	}
	// The removal must be reachable only after that check.
	check := strings.Index(src, "ownedProcsForDataDir(dataDir)")
	remove := strings.Index(src, "os.Remove(lockPath)")
	if check < 0 || remove < 0 {
		t.Fatal("could not locate the guard and the removal")
	}
	if strings.Index(src[check:], "os.Remove(lockPath)") < 0 {
		t.Error("the lock removal is not guarded by the survivor check")
	}
}

// AN EXISTING INSTANCE'S PORT MUST NEVER BE RENUMBERED BY A RE-RUN.
//
// Measured: 5432 was held by an unidentifiable leftover, so planInstallPorts walked to 5433 and — because
// the MSI runs --non-interactive — PERSISTED it. Every later start then waited on 5433 where postgres
// would never listen. The database port of an installed instance is a decision, not a suggestion.
func TestRecordedPortIsNeverImplicitlyRenumbered(t *testing.T) {
	src := readGoFile(t, "install_ports.go")
	if !strings.Contains(src, "An existing instance's port is not renumbered by a re-run") {
		t.Error("resolving an installed instance's ports can still walk to the next free one and persist " +
			"it. A leftover on 5432 silently moved the operator's database to 5433, after which every " +
			"start waited on a port postgres would never bind.")
	}
	// The refusal must come AFTER the operator's own pin is read, so an explicit override still works.
	pin := strings.Index(src, `os.Getenv(p.Env)`)
	refusal := strings.Index(src, "An existing instance's port is not renumbered")
	if pin < 0 || refusal < 0 {
		t.Fatal("could not locate the pin read and the refusal")
	}
	if pin > refusal {
		t.Error("the refusal is evaluated before the operator's explicit ORCHICON_*_PORT is read, so a " +
			"deliberate move would be refused")
	}
}

// readGoFile reads a file from this package's directory, so a wiring assertion can be made about the real
// source rather than a copy.
func readGoFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// callSite returns the byte offset of the first line that ASSIGNS from a call, so a guard cannot be
// satisfied by the same string appearing inside a comment. Every guard in this file that asks "is this
// call present, and where" uses it for that reason: a control run proved a plain substring match stayed
// green after the call was deleted, because the explanation of the defect still named it.
func callSite(t *testing.T, src, call string) int {
	t.Helper()
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
			continue
		}
		if strings.Contains(trimmed, call) && strings.Contains(trimmed, ":=") {
			return strings.Index(src, line)
		}
	}
	return -1
}
