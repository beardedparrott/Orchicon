package main

// quiesce.go — "is this instance ACTUALLY stopped", and making it so.
//
// THE DEFECT THIS REPLACES, reproduced on a real Windows machine with nothing but `orchicon stop` and
// `orchicon start`:
//
//	PS> orchicon stop dev
//	instance dev is host-resident: stopping the plane, then its services
//	serve is not running
//	the host services for dev (pid 14284) are no longer running
//	✓ dev is stopped                                  <-- AND IT IS NOT
//
//	PS> Get-CimInstance Win32_Process | ? Name -match postgres
//	26236  30488  postgres.exe  "postgres.exe --forkaux 3892 0"      <-- STILL RUNNING
//	PS> Get-NetTCPConnection -LocalPort 5432 -State Listen
//	5432   OwningProcess 30488                                        <-- the DEAD postmaster still owns the port
//
//	PS> orchicon start dev
//	waiting for dev services (postgres 127.0.0.1:5433, ...)           <-- THE PORT MOVED (see below)
//	dev services supervisor exited during startup — it cannot become ready.
//
// THREE DISTINCT FAILURES, EACH ENOUGH ON ITS OWN:
//
// 1. A STOP CHECKED ITS OWN PID FILE AND CALLED IT DONE. The supervisor was terminated, so
//    stopHostServices reported success — while the processes it had spawned were still running.
//    `postgres.exe` execs its per-connection backends, and those are RE-PARENTED when the postmaster
//    exits: they keep running, keep the data directory open, and keep the listening socket, so the port
//    reads as owned by a pid that no longer exists. "✓ stopped" was true of the PID and false of the
//    instance.
//
// 2. THE LEFTOVER WAS NEVER CLEARED. Windows reclaim identifies nothing (no /proc — see
//    reclaim_proc_windows.go), so a leftover is REPORTED and never removed. That is the right boundary
//    for an UNKNOWN process, but an instance's own supervised children are not unknown: they run from
//    the instance's own bin directory. Refusing to identify the ones we demonstrably own is what let the
//    orphan survive a stop, a reinstall and a reboot.
//
// 3. THE REINSTALL THEN RENUMBERED THE DATABASE. With 5432 held by an unidentifiable process,
//    planInstallPorts did what it is designed to do — walk to the next free port — and because the MSI
//    runs non-interactive it ACCEPTED its own suggestion and persisted it:
//
//	PostgreSQL: port 5432 is in use — suggesting 5433
//	{"ORCHICON_POSTGRES_PORT": 5433, ...}   <-- written to ports.json
//
//    From then on every start waited on 5433, where postgres would never listen. The operator's logs
//    show exactly that: "waiting for dev services (postgres 127.0.0.1:5433 ...)".
//
// SO THE ANSWER IS ONE PRIMITIVE, used by stop, start and install, on every platform: enumerate what
// this instance owns, terminate the part it demonstrably owns, WAIT for it to actually go, and report
// what survived by name. A stop that cannot stop is a failure, not a success message.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/beardedparrott/orchicon/internal/planecontrol"
)

// instanceProcessRoots are the directories whose executables belong to ONE instance.
//
// IDENTITY IS THE DEPLOYMENT, NOT A NAMING CONVENTION. The binary resolver unpacks postgres, nats and the
// telemetry children under <dataDir>/bin, and postgres execs its per-connection backends from that same
// bin directory — so "an executable inside this instance's data dir" is precisely "a process this
// instance started", with no guessing. It also cannot collide with the operator's own PostgreSQL install
// (a different directory) or with a sibling instance (a different data dir), which is what makes a kill
// on this rule safe where a kill by port number would not be.
func instanceProcessRoots(instance string) []string {
	dir := hostDataDirFor(instance)
	return []string{
		filepath.Join(dir, "bin"),      // postgres, nats, telemetry — everything the resolver unpacks
		filepath.Join(dir, "postgres"), // the cmddeps/backends live here for some distributions
	}
}

// ownedProcs lists the processes this instance demonstrably owns, across every root.
//
// It is deliberately NOT "everything holding my ports": a port's holder may be the operator's own
// database or a colleague's instance, and killing that is unrecoverable. Ownership is proven by the
// executable's location, and nothing else.
func ownedProcs(instance string) []planecontrol.ProcInfo {
	seen := map[int]bool{}
	var out []planecontrol.ProcInfo
	for _, root := range instanceProcessRoots(instance) {
		procs, err := planecontrol.ProcessesUnder(root)
		if err != nil {
			continue // an unreadable tree is not evidence of anything; the port check decides below
		}
		for _, p := range procs {
			if !seen[p.PID] {
				seen[p.PID] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// quiesceResult is what a quiesce found and what it did, so a caller can report honestly rather than
// printing a tick.
type quiesceResult struct {
	// Leftovers are the processes that were running when the quiesce started.
	Leftovers []planecontrol.ProcInfo
	// Killed are the pids this quiesce terminated.
	Killed []int
	// Survivors are the pids still alive after the wait — the failure case.
	Survivors []int
	// HeldPorts names ports still bound after the quiesce, for a caller that needs to say why the next
	// bind will fail.
	HeldPorts []int
	// SupervisorAlive records that this instance's supervisor was running, so nothing was touched: its
	// children are managed, not orphaned.
	SupervisorAlive bool
}

// Clean reports whether nothing of this instance is left running and no port of it is still held.
func (q quiesceResult) Clean() bool { return len(q.Survivors) == 0 && len(q.HeldPorts) == 0 }

// quiesceInstance terminates every process this instance demonstrably owns and WAITS for the ports to
// come free.
//
// IT IS THE STOP PRIMITIVE, and it is used by all three lifecycle entry points — `stop`, `start` (before
// starting, so a half-dead instance cannot collide with itself) and `install` (before starting, so a
// reinstall over a running instance is a convergence rather than a second stack racing the first). One
// implementation is what makes their answers agree; three copies is how one of them came to report
// success over a running database.
//
// ports is what to verify, and it is checked rather than assumed because the port is the thing the next
// start actually needs.
func quiesceInstance(instance string, ports map[string]int, out io.Writer) quiesceResult {
	var res quiesceResult

	// A LIVE SUPERVISOR'S CHILDREN ARE NOT LEFTOVERS.
	//
	// This is the rule that makes the primitive safe to call from `start` and `install`, and it was learned
	// the hard way: without it, an install over a RUNNING instance killed every child the supervisor was
	// managing, the supervisor saw them die and restarted them all with backoff, and the instance churned
	// through a restart storm (measured: postgres restarts=6, nats=4, loki=3, vm=3, collector=3 — from one
	// install command).
	//
	// The supervisor owns its children: it started them, it restarts them, and it is the thing to stop when
	// the instance is being stopped. So while it is alive, its fleet is left alone — the caller proceeds to
	// startHostServices, which already reports "already ready" and leaves it running. Orphans only exist
	// when there is NO supervisor to own them.
	if hostServicesAlive(instance) {
		res.SupervisorAlive = true
		return res
	}

	// ONE PASS IS NOT ENOUGH, and the reason is postgres-shaped: terminating the postmaster orphans its
	// backends, and those are only VISIBLE as this instance's once the enumeration is repeated. So the
	// loop terminates, re-enumerates, and only stops when a pass finds nothing new — bounded, so a process
	// that respawns itself cannot spin here for ever.
	const maxPasses = 4
	for pass := 0; pass < maxPasses; pass++ {
		leftovers := ownedProcs(instance)
		if len(leftovers) == 0 {
			break
		}
		if pass == 0 {
			res.Leftovers = leftovers
		}
		// EVERY ENUMERATED PROCESS IS TERMINATED DIRECTLY, INCLUDING THE FIRST ONE.
		//
		// This used to hand the list to TerminateSubtree with leftovers[0] as the root — and that function
		// deliberately does NOT terminate the root ("the caller terminates the root itself, so it controls
		// the ordering and the error"). So the FIRST leftover was never killed, and with a single orphan
		// (the reported case: one re-parented postgres backend) the quiesce killed nothing at all while
		// reporting that it had tried:
		//
		//	1 process(es) of dev are STILL RUNNING after the stop: [26236]
		//
		// There is no subtree walk to do here: ProcessesUnder already enumerates every process whose image
		// lives in this instance's own directory, so a re-parented child is in the list on its own merit
		// rather than by its parent links. Terminating each one directly is both simpler and complete.
		var pids []int
		for _, p := range leftovers {
			pids = append(pids, p.PID)
			if err := planecontrol.TerminateProcess(p.PID); err == nil {
				res.Killed = append(res.Killed, p.PID)
			}
		}
		// Wait for THIS pass's pids to go before deciding whether another pass is needed.
		if survivors := planecontrol.WaitGone(pids, 15*time.Second); len(survivors) > 0 {
			res.Survivors = survivors
			break
		}
	}
	if res.Survivors == nil {
		// Re-check anything still alive after the passes: a process that appeared mid-quiesce.
		for _, p := range ownedProcs(instance) {
			res.Survivors = append(res.Survivors, p.PID)
		}
	}

	// THE PORTS ARE THE REAL GATE. A terminated process can take a moment to release a socket, and a
	// socket can outlive the process that opened it, so "all pids are gone" is not the same question as
	// "the port is free". This is the question the next bind asks, so it is the one answered here.
	res.HeldPorts = heldPortsOf(instance, ports, out)
	return res
}

// heldPortsOf reports which of the instance's ports are still bound, after a bounded wait.
//
// The wait exists because TerminateProcess is asynchronous: the caller re-binds immediately, so it would
// otherwise race the kernel's teardown and report a conflict that is a microsecond from resolving.
func heldPortsOf(instance string, ports map[string]int, out io.Writer) []int {
	relevant := []string{
		"ORCHICON_POSTGRES_PORT", "ORCHICON_NATS_PORT", "ORCHICON_NATS_MONITOR_PORT",
		"ORCHICON_TEMPO_PORT", "ORCHICON_LOKI_PORT", "ORCHICON_VM_PORT",
		"ORCHICON_OTLP_GRPC_PORT", "ORCHICON_OTLP_HTTP_PORT", "ORCHICON_OTEL_HEALTH_PORT",
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		var held []int
		for _, env := range relevant {
			p := ports[env]
			if p > 0 && !portFree(p) {
				held = append(held, p)
			}
		}
		if len(held) == 0 || time.Now().After(deadline) {
			return held
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// reportQuiesceFailure explains a quiesce that could not finish, naming the pids and ports that survived
// and — where the platform can say — who holds them.
//
// A stop that cannot stop must not print a tick. This is the same posture serveStop takes for the plane
// ("a stop that cannot stop is not a success"), applied to the services, because a start that follows a
// fake stop fails with a bind error that names nothing.
func reportQuiesceFailure(instance string, res quiesceResult, out io.Writer) {
	if len(res.Survivors) > 0 {
		fmt.Fprintf(out, "  %d process(es) of %s are STILL RUNNING after the stop", len(res.Survivors), instance)
		fmt.Fprintf(out, ": %v\n", res.Survivors)
		fmt.Fprintln(out, "  they hold this instance's data directory and its ports; the next start will fail")
		fmt.Fprintf(out, "  until they exit. identically-owned processes are killed by `%s stop %s`;\n",
			filepath.Base(os.Args[0]), instance)
		fmt.Fprintln(out, "  one that survives it is not ours to kill — check it with Task Manager → Details.")
	}
	for _, p := range res.HeldPorts {
		fmt.Fprintf(out, "  port %d is still held", p)
		if owner := describePortHolder(p); owner != "" {
			fmt.Fprintf(out, " by %s", owner)
		} else {
			fmt.Fprintf(out, " — %s", portHolderHint(p))
		}
		fmt.Fprintln(out)
	}
}

// describePortHolder names the process bound to a port, or "" when this platform cannot say.
//
// It reuses the crash-loop diagnosis's lookup (service_ports.go) rather than a second netstat parser: the
// same question deserves one implementation, and that one already handles the IPv6 form the telemetry
// children bind.
func describePortHolder(port int) string {
	owners := portOwners([]int{port})
	if len(owners) == 0 {
		return ""
	}
	return describeOwner(owners[0])
}

// stopInstanceCompletely brings an instance fully down: the plane, then the supervisor (with its tree), then
// any leftover that outlived it — and finally verifies the ports.
//
// THIS IS THE ONE WAY DOWN, used by `orchicon stop` and by `orchicon install` before it starts a fresh stack.
// It exists so those two cannot disagree, which is the defect that produced the operator's report: `stop`
// verified its own pid files while a re-parented postgres backend kept the data directory and port 5432, and
// an install over that state put a second stack on ports the first was still serving.
//
// THE ORDER IS THE DEPENDENCY, MIRRORED: the plane talks to the services, so it goes first; the supervisor
// owns the services, so it goes next; whatever outlived the supervisor is an orphan and goes last.
func stopInstanceCompletely(instance string, ports map[string]int, out io.Writer) error {
	if err := stopHostPlane(instance, out); err != nil {
		return err
	}
	// A supervisor caught mid-restart can take a beat longer than its bounded stop, and that is not a
	// failure while its CHILDREN are still dying — the quiesce and the port check below decide.
	if err := stopHostServices(instance, out); err != nil {
		fmt.Fprintf(out, "  (%v — checking what is left)\n", err)
	}
	q := quiesceInstance(instance, ports, out)
	if !q.Clean() {
		reportQuiesceFailure(instance, q, out)
		return fmt.Errorf("%s could not be fully stopped — see what survived above", instance)
	}
	return nil
}
