package main

// lifecycle_cmds.go — the two verbs an operator reaches for that had no top-level spelling:
// `orchicon status` and `orchicon logs`.
//
// REPORTED: "Running orchicon.exe bare with a start/stop/status should do the full gambit (start
// services in background, start serve in background, and print where you can get the logs and how
// to stop/start it again)."
//
// `start` and `stop` already existed and already did the right thing. `status` existed only as
// `plane status`, and NOTHING tailed the logs — so the answer to "what is the plane doing" was a
// file path printed by another command, which the operator then had to open by hand.
//
// These are deliberately thin: they delegate to the implementations that already exist, so there
// is one behaviour and two spellings of it.

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/beardedparrott/orchicon/internal/planecontrol"
)

// runStatus answers "is it up, and where" for an instance.
func runStatus(args []string) error {
	instance, err := parseInstanceTarget(args, env("ORCHICON_INSTANCE", "dev"))
	if err != nil {
		return err
	}

	// The plane first: it is the thing the operator is asking about.
	plane, err := planecontrol.StatusOf(planecontrol.Options{Instance: instance})
	if err != nil {
		fmt.Printf("instance %s\n", instance)
		fmt.Printf("  plane      : unknown (%v)\n", err)
	} else {
		fmt.Printf("instance %s\n", instance)
		fmt.Printf("  plane      : %s\n", planeLine(plane))
	}

	// Then the services, because a plane with no database is the failure that looks like a
	// working plane.
	ports, err := servicesPortsFor(instance)
	if err != nil {
		fmt.Printf("  services   : unknown (%v)\n", err)
	} else if servicesReady(instance, ports) {
		fmt.Printf("  services   : up (postgres %d, nats %d)\n",
			ports["ORCHICON_POSTGRES_PORT"], ports["ORCHICON_NATS_PORT"])
	} else {
		fmt.Printf("  services   : DOWN (postgres %d, nats %d)\n",
			ports["ORCHICON_POSTGRES_PORT"], ports["ORCHICON_NATS_PORT"])
	}

	fmt.Printf("  logs       : %s\n", instanceLogPath(instance))
	fmt.Printf("  web UI     : %s\n", planeURL(ports))
	fmt.Println()
	fmt.Printf("  start / stop:  orchicon start %s   /   orchicon stop %s\n", instance, instance)
	fmt.Printf("  tail logs    :  orchicon logs %s\n", instance)
	return nil
}

// planeLine renders the plane's state as one readable phrase.
//
// FOUR STATES, because "running" and "working" are different: a plane whose process is alive but
// whose /healthz does not answer is the failure that looks like a working plane, and it is the
// one the operator most needs named.
func planeLine(st planecontrol.Status) string {
	switch {
	case st.Alive && st.Healthy:
		return fmt.Sprintf("running and healthy (pid %d)", st.PID)
	case st.Alive:
		return fmt.Sprintf("running but NOT answering /healthz (pid %d) — see the log", st.PID)
	case st.PID > 0:
		return fmt.Sprintf("stale pid file (pid %d is gone) — run `orchicon start %s`", st.PID, st.Instance)
	default:
		return "stopped"
	}
}

// instanceLogPath is where this instance's plane log lives.
func instanceLogPath(instance string) string {
	return filepath.Join(hostDataDirFor(instance), "serve", "logs", "orchicon.log")
}

// planeURL is the web UI's address for this instance's resolved control port.
func planeURL(ports map[string]int) string {
	p := ports["ORCHICON_CONTROL_PORT"]
	if p == 0 {
		return "(unknown — the ports are not resolved yet)"
	}
	return fmt.Sprintf("http://localhost:%d", p)
}

// runLogs tails an instance's plane log. It prints the tail and then follows it, which is what
// "tail" means to a person: the recent lines, and then the new ones as they arrive.
func runLogs(args []string) error {
	instance, err := parseInstanceTarget(args, env("ORCHICON_INSTANCE", "dev"))
	if err != nil {
		return err
	}
	path := instanceLogPath(instance)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("no log for %s at %s\n"+
			"  The plane writes it once it starts. If the instance is not running, start it:\n"+
			"    orchicon start %s", instance, path, instance)
	}
	fmt.Printf("tailing %s  (ctrl-c to stop)\n\n", path)

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	// THE TAIL FIRST: a person who asks for the logs wants the recent ones NOW, not a cursor
	// parked at the end of the file.
	if st, err := f.Stat(); err == nil && st.Size() > 0 {
		start := st.Size() - 32<<10
		if start < 0 {
			start = 0
		}
		if _, err := f.Seek(start, 0); err == nil {
			buf := make([]byte, st.Size()-start)
			if _, err := f.Read(buf); err == nil {
				fmt.Print(string(buf))
			}
		}
	}
	fmt.Println()

	// THEN FOLLOW. Read-at-offset is what `tail -f` does, and it needs no external binary.
	offset, _ := f.Seek(0, 1)
	buf := make([]byte, 32<<10)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			os.Stdout.Write(buf[:n])
			offset += int64(n)
		}
		if err != nil {
			time.Sleep(300 * time.Millisecond)
			// Re-open on rotation/truncation: the file was replaced underneath us.
			if st, serr := os.Stat(path); serr == nil && st.Size() < offset {
				if nf, nerr := os.Open(path); nerr == nil {
					f.Close()
					f = nf
					offset = 0
				}
			}
		}
	}
}
