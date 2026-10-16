package main

// restart.go — `orchicon restart`: restart an instance's running components so
// they re-read their environment, without re-resolving anything or asking.

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// runRestart implements `orchicon restart`.
//
// WHY IT EXISTS, and it is the whole reason the name matters. `orchicon install` is idempotent, but it
// deliberately does NOT restart a plane that is already healthy and running the installed binary (see
// startHostPlane's planeCurrent arm). That is right for what install is FOR — replacing a superseded
// binary — and wrong for the other thing operators use it for: REFRESHING THE ENVIRONMENT. A plane
// inherits its PATH when it starts (startHostPlane passes os.Environ() on, and serveDetach does the
// same), so installing an adapter CLI afterwards changes nothing until the plane restarts, and
// re-running `install` does not restart it. Reported: opencode on PATH in the shell, absent from the
// plane's /proc/<pid>/environ, model discovery "not configured" for ever.
//
// The only way to refresh it before this was `orchicon serve --stop` followed by an install — a
// restart in disguise, nested under a command about serving. Now it is named.
//
// THE PORTS ARE READ BACK, never re-resolved: a restart must not ask a question, and the ports it must
// bind are already recorded (writeResolvedPorts at install time).
//
// THE SHAPE IS READ BACK TOO (recordedResidency), never asked of an engine: with the container removed from
// the install path there may be no engine to ask, and the record is per-instance. The container arm below
// therefore only ever touches a container when an engine-backed container shape was RECORDED — a host-
// resident instance never restarts a container, and a host with no engine never runs a docker command at all.
func runRestart(args []string, log *slog.Logger) error {
	bin := filepath.Base(os.Args[0])
	instance := env("ORCHICON_INSTANCE", "dev")
	name := "orchicon-cnt-" + instance
	socketDir := env("ORCHICON_RUNTIME_SOCKET_DIR", filepath.Join(os.TempDir(), "orchicon-runtime"))

	// NOT validateInstance: that is a PORT-DEFAULTS table check ("has no port defaults (known:
	// dev, prod, test)"), and a restart needs no port defaults — it reads the ports that were
	// actually bound, from the record. Running it here rejected any instance outside the table,
	// including a legitimate one with pinned ports, and answered a question about ports that a
	// restart never asks.
	//
	// The name still guards a PATH (it forms the instance state dir), so a name that could climb
	// out of that directory is refused — the check validateInstance was incidentally providing.
	if strings.ContainsAny(instance, "/\\") || strings.Contains(instance, "..") {
		return fmt.Errorf("invalid instance name %q: it forms the instance state directory name, so it "+
			"may not contain a path separator or \"..\"", instance)
	}

	residency, ok := recordedResidency(instance)
	if !ok {
		return fmt.Errorf("no install record for instance %q — nothing is installed for it.\n"+
			"  Install it first:  %s install\n"+
			"  Another instance:  ORCHICON_INSTANCE=<name> %s restart", instance, bin, bin)
	}

	ports, err := readResolvedPorts(instance)
	if err != nil {
		return err
	}
	healthURL := fmt.Sprintf("http://localhost:%d/healthz", ports["ORCHICON_CONTROL_PORT"])

	if residency == residencyHost {
		// THE PLANE IS ON THIS HOST, so restarting the services container does not touch it. Saying so
		// out loud, because that mismatch is exactly what made this hard to diagnose: the container was
		// restarted, the plane was not, and nothing pointed at the plane.
		fmt.Printf("instance %s is host-resident: the plane runs on THIS host, so the PLANE restarts and the services container is left running\n", instance)
		if err := stopHostPlane(instance, os.Stdout); err != nil {
			return err
		}
		if err := startHostPlane(instance, ports, hostDataDirFor(instance), filepath.Join(socketDir, "runtime.sock"), os.Stdout); err != nil {
			return err
		}
	} else {
		fmt.Printf("instance %s is container-resident: restarting container %s\n", instance, name)
		if out, err := exec.Command("docker", "restart", name).CombinedOutput(); err != nil {
			return fmt.Errorf("restart %s: %v: %s", name, err, strings.TrimSpace(string(out)))
		}
	}

	// REPORT WHETHER IT CAME BACK. A restart that silently did not take effect is the same class of
	// failure this command exists to remove, so it is not reported as success here.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if httpHealthy(healthURL, 2*time.Second) {
			fmt.Printf("✓ %s is serving %s\n", instance, healthURL)
			fmt.Printf("  It re-read its environment on this restart, so a PATH change — a newly installed adapter CLI, for instance — has taken effect.\n")
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("%s did not answer %s within 60s — it was restarted but is not serving.\n"+
		"  See `%s install` for the full setup output, and the plane's log in the instance state dir.", instance, healthURL, bin)
}
