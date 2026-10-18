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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
	out, err := exec.Command(engineBin, "inspect", "-f", "{{.State.Running}}", name).CombinedOutput()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// engineCommandOn runs an engine command against a NAMED binary, so a legacy container can be reached in
// whichever engine actually holds it rather than only in the one the runtime prefers today.
func engineCommandOn(engineBin string, args ...string) error {
	out, err := exec.Command(engineBin, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", filepath.Base(engineBin), strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// lookPathEngine resolves an engine binary by name.
func lookPathEngine(name string) (string, error) { return exec.LookPath(name) }

// ourInstanceProcesses lists pids that are VERIFIABLY this instance's: the executable is one of our binaries
// AND the process carries the instance's name in its environment or argv.
//
// Both conditions matter. Our binary alone would match a DIFFERENT instance's plane — killing it would take
// down a sibling (dev while you meant test); the instance name alone would match nothing useful.
func ourInstanceProcesses(instance string) []int {
	self := os.Getpid()
	var pids []int
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	tag := "ORCHICON_INSTANCE=" + instance
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		dir := filepath.Join("/proc", e.Name())
		exe, err := os.Readlink(filepath.Join(dir, "exe"))
		if err != nil || !isOurBinary(exe) {
			continue
		}
		environ, _ := os.ReadFile(filepath.Join(dir, "environ"))
		args, _ := os.ReadFile(filepath.Join(dir, "cmdline"))
		if strings.Contains(string(environ), tag) || strings.Contains(string(args), instance) {
			pids = append(pids, pid)
		}
	}
	return pids
}

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

// portHolders names the processes listening on the given ports, so a refusal can say WHO instead of telling
// the operator to go run `ss` themselves. It reads /proc directly rather than shelling out to ss/lsof:
// neither is guaranteed present, and the failure path of a teardown must not depend on an optional tool.
//
// BEST-EFFORT AND HONEST ABOUT IT: an unreadable /proc (another user's process, a hardened kernel) yields
// nothing rather than a guess, and the caller's message still stands on its own.
func portHolders(ports ...int) []portHolder {
	want := map[string]bool{}
	for _, p := range ports {
		if p > 0 {
			// /proc/net/tcp lists LOCAL_ADDRESS:PORT as 4 hex digits of the port, upper-case.
			want[fmt.Sprintf(":%04X", p)] = true
		}
	}
	if len(want) == 0 {
		return nil
	}
	inodes := map[string]bool{}
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n")[1:] {
			fields := strings.Fields(line)
			if len(fields) < 10 || !want[fields[1][strings.LastIndex(fields[1], ":")+1:]] {
				continue
			}
			if fields[3] != "0A" { // 0A = TCP_LISTEN
				continue
			}
			inodes["socket:["+fields[9]+"]"] = true
		}
	}
	if len(inodes) == 0 {
		return nil
	}
	var out []portHolder
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		fdDir := filepath.Join("/proc", e.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil || !inodes[target] {
				continue
			}
			exe, _ := os.Readlink(filepath.Join("/proc", e.Name(), "exe"))
			if exe == "" {
				exe = "unknown (not readable by this user)"
			}
			out = append(out, portHolder{pid: pid, exe: exe})
			break
		}
	}
	return out
}
