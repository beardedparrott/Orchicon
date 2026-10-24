package main

// reclaim.go — taking back the ports an instance's OWN leftovers still hold.
//
// THE FAILURE THIS EXISTS FOR. A `make rebuild-dev` stops the instance and then starts it again, and the
// start cannot bind if something still owns postgres/nats. The teardown was honest about that (it named the
// held ports and refused), but it could not DO anything about the one holder that is unambiguously ours:
//
//	orchicon services: dev did not release postgres :5432 and nats :4222
//	  — an orphaned process still holds the port (find it with `ss -ltnp` / `lsof -i :5432` ...)
//
// THE IDENTIFICATION ITSELF IS PLATFORM-DEPENDENT and lives behind a build tag: reclaim_proc_unix.go reads
// /proc to name a holder, and reclaim_proc_windows.go CANNOT (no /proc), so it identifies nothing and every
// message routes through portHolderHint() — which names a PowerShell/Task Manager recipe there rather than a
// command that does not exist. The BOUNDARY below is unchanged on both: an unidentified holder is REPORTED,
// never killed.
//
// That message was printed while `orchicon-cnt-dev` — an instance container THIS PRODUCT created, named by
// OUR convention — was up and publishing both ports. Nothing in the teardown would stop it, because the
// container-era instance shape was retired and the teardown only knows about HOST services. So a host that
// ever ran a container-era instance could never rebuild: it was stuck behind a container nothing would
// remove, with an error telling the operator to go find it themselves.
//
// THE BOUNDARY, AND WHY IT IS THIS NARROW. "Kill whatever holds the port" is not a safe rule: 5432 is a
// postgres port, and the thing holding it may be the operator's own database, a colleague's instance, or a
// service that has nothing to do with this product. Killing on port number alone is how a convenience
// becomes data loss. So reclaiming is allowed only for a holder that is VERIFIABLY this instance's:
//
//	1. a container named exactly `orchicon-cnt-<instance>` — our naming, created by our installer;
//	2. a process whose executable is one of OUR binaries and which carries this instance's name.
//
// Anything else is REPORTED, never killed: the operator gets the pid, the executable and the port, and
// decides. A tool that kills a stranger's database to make its own start succeed has traded a clear error
// for an invisible catastrophe.

import (
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/procwin"
)

// instanceContainerName is the name this product gives an instance's container. It is a CONVENTION shared
// with the installer and the lifecycle verbs, and reclaim depends on it being exact: a near-match is a
// different instance.
func instanceContainerName(instance string) string { return "orchicon-cnt-" + instance }

// instanceEngines returns the engine binaries a legacy instance container could live in — BOTH, because
// which engine created it is a historical fact (docker for everything installed before the podman default,
// podman for everything after). Asking only the current default is the same mistake that made the postgres
// guard blind; see cmd/orchicon/postgres_legacy_volume.go.
func instanceEngines() []string {
	var bins []string
	seen := map[string]bool{}
	for _, name := range []string{"podman", "docker"} {
		if p, err := lookPathEngine(name); err == nil && !seen[p] {
			seen[p] = true
			bins = append(bins, p)
		}
	}
	// The engine the runtime itself resolved comes first: when it created the container, one probe answers.
	if b := engineBinary(); b != "" && !seen[b] {
		bins = append([]string{b}, bins...)
	}
	return bins
}

// stopInstanceContainer stops this instance's legacy container if one is running, and reports whether it
// did. It never removes a container or a volume: stopping frees the ports, and an operator who wants the
// data gone has `scripts/orchicon.sh uninstall` for that — a rebuild must not be a delete.
func stopInstanceContainer(instance string, out io.Writer) bool {
	name := instanceContainerName(instance)
	for _, bin := range instanceEngines() {
		if !containerRunningOn(bin, name) {
			continue
		}
		fmt.Fprintf(out, "  the instance's own container %s is still running (it holds this instance's ports) — stopping it\n", name)
		if err := engineCommandOn(bin, "stop", name); err != nil {
			fmt.Fprintf(out, "  could not stop %s: %v\n", name, err)
			continue
		}
		// BOUNDED, and the ports are re-checked by the caller: a container that was told to stop has not
		// necessarily released its published ports yet, and reporting success before it does is the kind of
		// claim this codebase treats as a bug.
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if !containerRunningOn(bin, name) {
				fmt.Fprintf(out, "  %s stopped\n", name)
				return true
			}
			time.Sleep(250 * time.Millisecond)
		}
		fmt.Fprintf(out, "  %s did not stop within 20s\n", name)
	}
	return false
}

// containerRunningOn asks ONE engine whether a container of that exact name is running.
func containerRunningOn(engineBin, name string) bool {
	out, err := procwin.Command(engineBin, "inspect", "-f", "{{.State.Running}}", name).CombinedOutput()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// engineCommandOn runs an engine command against a NAMED binary, so a legacy container can be reached in
// whichever engine actually holds it rather than only in the one the runtime prefers today.
func engineCommandOn(engineBin string, args ...string) error {
	out, err := procwin.Command(engineBin, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", filepath.Base(engineBin), strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// lookPathEngine resolves an engine binary by name.
func lookPathEngine(name string) (string, error) { return exec.LookPath(name) }

// isOurBinary reports whether an executable path is an orchicon binary (the plane, a supervisor, or the TUI
// client). Used only to decide whether a process is plausibly ours before the instance name confirms it.
func isOurBinary(exe string) bool {
	exe = strings.TrimSuffix(exe, " (deleted)")
	base := filepath.Base(exe)
	return base == "orchicon" || base == "orch"
}

// forceKillOurLeftovers terminates any process this instance owns that the graceful stop missed, and says
// what it killed. Returns the pids, so a caller can re-verify.
//
// IT REFUSES TO TOUCH ANYTHING UNIDENTIFIED. The alternative — killing whatever holds the port — is a rule
// that cannot tell an orphaned supervisor from the operator's own database.
func forceKillOurLeftovers(instance string, out io.Writer) []int {
	var killed []int
	for _, pid := range ourInstanceProcesses(instance) {
		if err := terminateProcess(pid); err != nil {
			fmt.Fprintf(out, "  could not terminate pid %d: %v\n", pid, err)
			continue
		}
		fmt.Fprintf(out, "  force-stopped pid %d (this instance's own process)\n", pid)
		killed = append(killed, pid)
	}
	return killed
}

// portHolder is one process holding a port, as much as can be said about it without root.
type portHolder struct {
	pid int
	exe string
}

// portHeldByThisInstance reports whether a port is held by something belonging to THIS instance: its own
// plane/supervisor process, or its own container-era container.
//
// It answers "may I keep my recorded port?" and nothing else — a false answer only costs a re-resolve, so it
// errs toward false whenever the holder cannot be identified.
func portHeldByThisInstance(port int, instance string) bool {
	name := instanceContainerName(instance)
	for _, bin := range instanceEngines() {
		if containerRunningOn(bin, name) {
			// The container publishes this instance's ports; confirm it is actually the one holding THIS
			// port rather than assuming, because a stopped/partial container still answers inspect.
			if containerPublishesPort(bin, name, port) {
				return true
			}
		}
	}
	for _, h := range portHolders(port) {
		if isOurBinary(h.exe) && processBelongsTo(h.pid, instance) {
			return true
		}
	}
	return false
}

// containerPublishesPort reports whether a container publishes a host port.
func containerPublishesPort(engineBin, name string, port int) bool {
	out, err := procwin.Command(engineBin, "inspect", "-f",
		"{{range $p, $b := .NetworkSettings.Ports}}{{(index $b 0).HostPort}} {{end}}", name).CombinedOutput()
	if err != nil {
		return false
	}
	for _, f := range strings.Fields(string(out)) {
		if f == strconv.Itoa(port) {
			return true
		}
	}
	return false
}

// portOwnerLabel describes who holds a port, for the message that names a genuine conflict. Empty when the
// holder is this instance (nothing to report) or cannot be identified.
func portOwnerLabel(port int, instance string) string {
	for _, h := range portHolders(port) {
		if isOurBinary(h.exe) {
			if processBelongsTo(h.pid, instance) {
				return "" // ours; not a conflict
			}
			return fmt.Sprintf("another Orchicon instance (pid %d, %s) — pick a different port or stop it",
				h.pid, h.exe)
		}
		return fmt.Sprintf("pid %d (%s), which Orchicon did not start — pick a different port",
			h.pid, h.exe)
	}
	return ""
}
