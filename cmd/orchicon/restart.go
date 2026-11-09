package main

// restart.go — `orchicon restart`: restart an instance's running components so
// they re-read their environment, without re-resolving anything or asking.

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// runRestart implements `orchicon restart`.
func runRestart(args []string, log *slog.Logger) error {
	bin := filepath.Base(os.Args[0])

	// THE INSTANCE IS NAMED THE SAME WAY HERE AS ON EVERY OTHER LIFECYCLE VERB, and it is parsed from
	// the ARGUMENTS as well as the environment.
	instance, err := parseInstanceTarget(args, env("ORCHICON_INSTANCE", "dev"))
	if err != nil {
		return err
	}
	name := "orchicon-cnt-" + instance
	socketDir := env("ORCHICON_RUNTIME_SOCKET_DIR", filepath.Join(os.TempDir(), "orchicon-runtime"))

	// NOT validateInstance: that is a PORT-DEFAULTS table check ("has no port defaults (known: dev,
	// prod, test)"), and a restart needs no port defaults — it reads the ports that were actually
	// bound.

	residency, ok := recordedResidency(instance)
	if !ok {
		return fmt.Errorf("no install record for instance %q — nothing is installed for it.\n"+
			"  Install it first:  %s install --instance %s\n"+
			"  Another instance:  %s restart <name>", instance, bin, instance, bin)
	}

	ports, err := readResolvedPorts(instance)
	if err != nil {
		return err
	}
	if residency == residencyHost {
		// THE PLANE IS ON THIS HOST, so restarting the services container does not touch it.
		fmt.Printf("instance %s is host-resident: the plane runs on THIS host, so the PLANE restarts and the services container is left running\n", instance)
		if err := stopHostPlane(instance, os.Stdout); err != nil {
			return err
		}
		if err := startHostPlane(instance, ports, hostDataDirFor(instance), filepath.Join(socketDir, "runtime.sock"), os.Stdout); err != nil {
			return err
		}
	} else {
		fmt.Printf("instance %s is container-resident: restarting container %s\n", instance, name)
		if out, err := engineCommand("restart", name).CombinedOutput(); err != nil {
			return fmt.Errorf("restart %s: %v: %s", name, err, strings.TrimSpace(string(out)))
		}
	}

	// REPORT WHETHER IT CAME BACK.
	if url, up := awaitHealthy(instance, ports, 60*time.Second); up {
		fmt.Printf("✓ %s is serving %s\n", instance, url)
		fmt.Printf("  It re-read its environment on this restart, so a PATH change — a newly installed adapter CLI, for instance — has taken effect.\n")
		return nil
	} else {
		return fmt.Errorf("%s did not answer %s within 60s — it was restarted but is not serving.\n"+
			"  See `%s install` for the full setup output, and the plane's log in the instance state dir.", instance, url, bin)
	}
}
