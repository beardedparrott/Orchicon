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

	if callSite(t, src, "stopInstanceCompletely(instance, ports, os.Stdout)") < 0 {
		t.Error("runStop does not use stopInstanceCompletely. Terminating the SUPERVISOR's tree is not the " +
			"same as stopping the instance: postgres's per-connection backends are RE-PARENTED when the " +
			"postmaster exits, so they survive it and keep the data directory and port 5432.")
	}
	if !strings.Contains(src, "return fmt.Errorf(\"%s could not be fully stopped") &&
		!strings.Contains(readGoFile(t, "quiesce.go"), "%s could not be fully stopped") {
		t.Error("nothing refuses to report success over a stop that left processes running")
	}
}

// A START AFTER AN UNCLEAN EXIT MUST CLEAR THE INSTANCE'S OWN LEFTOVERS FIRST.
//
// Without it the new supervisor's postgres meets the old run's orphan and refuses — the FATAL /
// "lock file postmaster.pid already exists" the operator saw — and the report blames the services
// pipeline instead of naming a process.
func TestStartClearsLeftoversBeforeStartingServices(t *testing.T) {
	src := readGoFile(t, "lifecycle.go")
	// runStart uses a BARE quiesce, and that is correct: a live supervisor's children are left alone by the
	// quiesce's own rule, so what it clears is genuinely an orphan. Either call shape is acceptable here —
	// what matters is that it happens BEFORE the services start.
	q := callSite(t, src, "quiesceInstance(instance, ports, os.Stdout)")
	if q < 0 {
		q = callSite(t, src, "stopInstanceCompletely(instance, ports, os.Stdout)")
	}
	start := callSite(t, src, "startHostServices(instance, ports, hostDataDirFor(instance), os.Stdout)")
	if q < 0 {
		t.Fatal("runStart never clears the instance's leftovers, so a start races its own orphan")
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
	q := callSite(t, src, "stopInstanceCompletely(instance, ports, os.Stdout)")
	start := callSite(t, src, "startHostServices(instance, ports, hostDataDirFor(instance), os.Stdout)")
	if q < 0 {
		t.Fatal("install never stops what is already running, so a REINSTALL over a running instance " +
			"starts a second supervisor on the same ports — 'it did NOT start orchicon after install'")
	}
	if start < 0 {
		t.Fatal("could not locate the install's startHostServices call")
	}
	if q > start {
		t.Error("install stops the instance AFTER starting the services, which is too late to prevent a " +
			"second stack on the same ports")
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
	if !strings.Contains(src, "survivorsOlderThanBoot(dataDir, bootCutoff())") {
		t.Error("clearStalePostmasterLock removes the lock as soon as the pid in it is gone. postgres " +
			"RE-PARENTS its backends, so the lock can name a dead postmaster while a live backend still " +
			"holds the cluster open — unlinking it lets a second postmaster start on a live data directory.")
	}
	// The removal AT ISSUE must come AFTER that check. There is an EARLIER os.Remove for an UNPARSEABLE
	// lock — one that cannot say which process it protects — so the comparison starts from the guard.
	check := strings.Index(src, "survivorsOlderThanBoot(dataDir, bootCutoff())")
	if check < 0 {
		t.Fatal("could not locate the survivor guard")
	}
	if !strings.Contains(src[check:], "os.Remove(lockPath)") {
		t.Error("no lock removal follows the survivor check, so the guard protects nothing")
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

// EVERY ENUMERATED PROCESS MUST BE TERMINATED, INCLUDING THE FIRST.
//
// A real bug of my own, caught by running this against a live orphan rather than by reading it: the quiesce
// handed the list to TerminateSubtree with leftovers[0] as the ROOT — and that function deliberately does not
// terminate the root, because its callers terminate the root themselves. With a single leftover (the reported
// case: one re-parented postgres backend) the result was that NOTHING was killed, while the report said it
// had tried:
//
//	1 process(es) of dev are STILL RUNNING after the stop: [26236]
//
// The quiesce does not need a subtree walk at all: ProcessesUnder already enumerates every process whose
// image is inside the instance's own directory, so a re-parented child earns its place in the list directly
// rather than through its parent links. This guard pins that every pid in the list is terminated.
func TestQuiesceTerminatesEveryEnumeratedProcess(t *testing.T) {
	src := readGoFile(t, "quiesce.go")

	if strings.Contains(src, "TerminateSubtree(") {
		t.Error("the quiesce still routes termination through TerminateSubtree. That function skips its ROOT " +
			"by design, so a single leftover is never killed — and a single re-parented postgres backend is " +
			"exactly the reported case.")
	}
	if !strings.Contains(src, "planecontrol.TerminateProcess(p.PID)") {
		t.Error("the quiesce does not terminate each enumerated process directly, so an orphan — which is " +
			"enumerated on its own merit — can survive the stop it is supposed to be removed by")
	}
}

// A TASKLIST MESSAGE IS NOT A PROCESS NAME.
//
// When no process matches, tasklist prints
//
//	INFO: No tasks are running which match the specified criteria.
//
// and that string reached an operator-facing message as
//
//	port 5432 is still held by INFO: No tasks are running which match the specified criteria. (pid 30488)
//
// which is actively misleading — it reads like a process named INFO:. Only a quoted name ending in .exe is a
// name; everything else means "gone, or not ours to see".
func TestTasklistMessageIsNotTreatedAsAName(t *testing.T) {
	src := readGoFile(t, "port_owner_windows.go")
	if !strings.Contains(src, `strings.HasSuffix(strings.ToLower(name), ".exe")`) {
		t.Error("processName accepts whatever tasklist printed as a process name, so its 'No tasks are " +
			"running' message is reported to the operator as the name of the process holding the port")
	}
}

// "AN ENGINE IS INSTALLED" IS NOT "CONTAINERS ARE AVAILABLE".
//
// Measured on the affected Windows machine: Docker Desktop was installed but its daemon was NOT running,
// so the install correctly decided containers were unavailable and said so — and then made that same
// condition fatal one step later at the runtime daemon, aborting BEFORE the data dir, the services and
// the plane:
//
//	orchicon: the container engine is installed but cannot run a container: failed to connect to the
//	          docker API at npipe:////./pipe/dockerDesktopLinuxEngine …
//	orchicon: runtime containers are unavailable — … work runs in-process
//	== starting the runtime daemon
//	error: runtime daemon did not become ready
//
// That is the operator's "it did NOT start orchicon after install". The gate must be the DECISION the
// install already made (engineStatus.Available), not a second, coarser presence probe.
func TestRuntimeDaemonFailureIsFatalOnlyWhenContainersAreAvailable(t *testing.T) {
	src := readGoFile(t, "install.go")
	if !strings.Contains(src, "if !engineStatus.Available {") {
		t.Error("install does not gate the runtime-daemon failure on engineStatus.Available, so an " +
			"installed-but-not-running container engine aborts the whole install before the services " +
			"and the plane — which is exactly 'it did not start after install'")
	}
	if strings.Contains(src, "if !containerEngineAvailable() {\n\t\t\t// NO ENGINE") {
		t.Error("install still gates the runtime daemon on container PRESENCE rather than on whether a " +
			"container can actually run")
	}
}

// A LIVE SUPERVISOR'S CHILDREN ARE NOT LEFTOVERS.
//
// Learned by breaking it: a bare quiesce on the INSTALL path killed every child a running supervisor was
// managing, the supervisor saw them die and restarted them all with backoff, and one install command
// produced a restart storm —
//
//	component exited; restarting component=postgres         restarts=6 backoff=32s
//	component exited; restarting component=nats             restarts=4 backoff=16s
//	component exited; restarting component=loki             restarts=3 backoff=8s
//	component exited; restarting component=victoriametrics  restarts=3 backoff=8s
//	component exited; restarting component=otel-collector   restarts=3 backoff=8s
//
// — and an instance that then reported "DEGRADED — loki not answering" while it thrashed.
//
// The supervisor owns its children: it started them and it restarts them. While it is alive they are
// managed, not orphaned, and the thing to stop is the supervisor. An orphan exists only when there is no
// supervisor to own it.
func TestQuiesceLeavesALiveSupervisorsChildrenAlone(t *testing.T) {
	src := readGoFile(t, "quiesce.go")
	if !strings.Contains(src, "if hostServicesAlive(instance) {") {
		t.Error("the quiesce does not check whether a supervisor is alive, so calling it while the instance " +
			"is UP kills a managed fleet and triggers a supervisor restart storm — measured at postgres " +
			"restarts=6 from a single install command")
	}
	// And it must return before touching anything, not merely note the fact.
	guard := strings.Index(src, "if hostServicesAlive(instance) {")
	kill := strings.Index(src, "planecontrol.TerminateProcess(p.PID)")
	if guard < 0 || kill < 0 {
		t.Fatal("could not locate the guard and the termination")
	}
	if guard > kill {
		t.Error("the alive-supervisor guard runs AFTER termination begins, so it protects nothing")
	}
}

// ONE WAY DOWN. `stop` and `install` must both use it, or they can again disagree about what "stopped"
// means — which is the defect in its original form.
func TestStopAndInstallShareTheOneWayDown(t *testing.T) {
	for _, f := range []string{"lifecycle.go", "install.go"} {
		src := readGoFile(t, f)
		if !strings.Contains(src, "stopInstanceCompletely(instance, ports, os.Stdout)") {
			t.Errorf("%s does not call stopInstanceCompletely, so it brings the instance down its own way "+
				"and the two can disagree about what stopped means", f)
		}
	}
}

// A PROCESS THIS BOOT JUST SPAWNED IS NOT A LEFTOVER.
//
// The bug this pins was mine, and it made the product unable to start at all. "Is anything of this instance
// running?" is the wrong question while the instance is being started, because the answer is about the
// postgres it has just spawned. Measured:
//
//	the postgres lock … names pid 33384, which is gone, but 1 process(es) of this instance are STILL
//	RUNNING ([27540]) — they hold the data directory open, so removing the lock would let a second
//	postmaster start on a live cluster.
//
// 27540 was a DIFFERENT pid on every run, and was the postmaster being started at that instant. The guard
// must therefore compare CREATION TIME against the moment this boot began, so only something older than the
// current run can be a leftover.
func TestLockSurvivorsOnlyCountProcessesOlderThanThisBoot(t *testing.T) {
	src := readGoFile(t, "postgres_lock.go")
	if !strings.Contains(src, "survivorsOlderThanBoot(dataDir, bootCutoff())") {
		t.Error("the lock guard asks whether anything is running, not whether anything OLDER THAN THIS BOOT " +
			"is running — so it refuses to start on the postgres it has just spawned, and reports its own " +
			"newborn as a leftover blocking the data directory")
	}
	// And the comparison must use a creation time, not mere existence.
	if !strings.Contains(src, "p.Created.IsZero() || p.Created.Before(cutoff)") {
		t.Error("survivorsOlderThanBoot does not compare process creation time against the cutoff")
	}
}

// ProcInfo must CARRY a creation time on BOTH platforms, or the distinction above is unavailable where it is
// needed most.
func TestProcInfoCarriesACreationTime(t *testing.T) {
	for _, f := range []string{"hostproc_unix.go", "hostproc_windows.go"} {
		src := readGoFile(t, filepath.Join("..", "..", "internal", "planecontrol", f))
		if !strings.Contains(src, "Created time.Time") {
			t.Errorf("%s: ProcInfo has no creation time, so a caller cannot tell a leftover from a process "+
				"that is starting right now", f)
		}
		if !strings.Contains(src, "Created:") {
			t.Errorf("%s: the enumeration does not populate Created", f)
		}
	}
}

// readFileBytes reads an arbitrary path, for assertions that must look at the OTHER half of a build-tagged
// pair (cmd/orchicon cannot compile internal/planecontrol's platform files directly).
func readFileBytes(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
