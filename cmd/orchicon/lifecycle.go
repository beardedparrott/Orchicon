package main

// lifecycle.go — `orchicon start` / `orchicon stop`: ONE verb each for "is it up" and
// "is it down", with the instance named the same way everywhere.
//
// WHY THESE EXIST. The lifecycle was already implemented and individually correct, but
// it was split across the two halves an operator does not think in: the PLANE (the
// process serving the API) and the SERVICES (postgres + NATS). "Stop Orchicon" was
// therefore two commands, and the second required knowing that `services` means "the
// database and the bus":
//
//	orchicon plane stop
//	orchicon services down
//
// That split is real and belongs in the architecture; it does not belong in the
// sentence a user has to remember. `make instance-down` already existed for exactly
// this reason — but it lives in the repo, so it is not reachable by anyone who
// installed the product.
//
// THESE ARE COMPOSITIONS, NOT A THIRD SUPERVISOR. start = the two existing starts, in
// the order the dependency requires; stop = the same two, reversed. Every step
// delegates to the primitive that already owns it, so there is one implementation of
// "start a plane" and one of "start the services". A second one is the defect this
// repo's own headers warn about (cmd/orchicon/services.go: "A THIN WRAPPER, NOT A
// SECOND SUPERVISOR").
//
// THE INSTANCE IS NOW NAMED THE SAME WAY ON EVERY VERB. It was not, and the gap was
// not cosmetic: `orchicon install` and `orchicon restart` took their instance from
// ORCHICON_INSTANCE alone and NEVER READ THEIR ARGUMENTS, so `orchicon install
// --instance prod` silently acted on `dev` — while USERGUIDE.md documented
// `orchicon restart --instance <dev|test|prod>` as if it worked. An operator who
// believed they had chosen an instance acted on a different one, and nothing said so.
// parseInstanceTarget below is the single parser all lifecycle verbs now share.

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// instanceFlag is the one flag a lifecycle verb accepts.
const instanceFlag = "instance"

// parseInstanceTarget reads the instance a lifecycle verb was asked to act on.
//
// EVERY ACCEPTED FORM IS ONE THE DOCS OR THE HABIT ALREADY PROMISED:
//
//	orchicon stop                  → the ORCHICON_INSTANCE fallback, else "dev"
//	orchicon stop prod             → positional
//	orchicon stop --instance prod  → flag
//	orchicon stop --instance=prod  → flag, = form
//
// ANYTHING ELSE IS REFUSED, and that refusal is the fix rather than politeness. A
// lifecycle verb that drops a flag it does not understand is a verb that lies about
// what it did, and this one did: the arguments were ignored outright. Refusing an
// unknown flag costs a user one error message; ignoring it silently repaired the
// wrong instance.
func parseInstanceTarget(args []string, fallback string) (string, error) {
	instance := strings.TrimSpace(fallback)
	if instance == "" {
		instance = "dev"
	}

	positional, flagged := "", false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--"+instanceFlag || arg == "-"+instanceFlag:
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value: --%s <dev|prod|test>", arg, instanceFlag)
			}
			i++
			instance, flagged = strings.TrimSpace(args[i]), true
		case strings.HasPrefix(arg, "--"+instanceFlag+"="), strings.HasPrefix(arg, "-"+instanceFlag+"="):
			_, value, _ := strings.Cut(arg, "=")
			instance, flagged = strings.TrimSpace(value), true
		case strings.HasPrefix(arg, "-"):
			return "", fmt.Errorf("unknown flag %q — the only flag a lifecycle verb takes is --%s <name>",
				arg, instanceFlag)
		default:
			if positional != "" {
				return "", fmt.Errorf("two instances given (%q and %q) — name one", positional, arg)
			}
			positional = arg
		}
	}

	if positional != "" {
		// BOTH FORMS AT ONCE IS AMBIGUOUS, and guessing which one the operator meant is how the wrong
		// instance gets changed.
		if flagged && positional != instance {
			return "", fmt.Errorf("both a positional instance (%q) and --%s %q were given — name one",
				positional, instanceFlag, instance)
		}
		instance = positional
	}
	if instance == "" {
		return "", fmt.Errorf("the instance name may not be empty")
	}
	if err := validateInstanceName(instance); err != nil {
		return "", err
	}
	return instance, nil
}

// validateInstanceName refuses a name that could climb out of the instance state directory, because
// the name forms a DIRECTORY NAME.
func validateInstanceName(instance string) error {
	if strings.ContainsAny(instance, "/\\") || strings.Contains(instance, "..") {
		return fmt.Errorf("invalid instance name %q: it forms the instance state directory name, so it "+
			"may not contain a path separator or \"..\"", instance)
	}
	return nil
}

// awaitHealthy polls the instance's /healthz until it answers or the deadline passes, returning the
// URL it asked.
func awaitHealthy(instance string, ports map[string]int, timeout time.Duration) (string, bool) {
	url := fmt.Sprintf("http://localhost:%d/healthz", ports["ORCHICON_CONTROL_PORT"])
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if httpHealthy(url, 2*time.Second) {
			return url, true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return url, false
}

// instanceExists reports whether anything is actually installed for an instance.
func instanceExists(instance string) bool {
	dir, err := hostDataDir(instance)
	if err != nil {
		return false
	}
	st, err := os.Stat(dir)
	return err == nil && st.IsDir()
}

// instanceResidency is the recorded shape, defaulting to HOST when there is no record.
func instanceResidency(instance string) string {
	if residency, ok := recordedResidency(instance); ok {
		return residency
	}
	return residencyHost
}

// instancePorts is the port set to act on: the RECORDED ports when an install wrote them, else the
// instance's own column in the port table.
func instancePorts(instance string) (map[string]int, error) {
	return servicesPortsFor(instance)
}

// runStart implements `orchicon start [instance]`.
func runStart(args []string, log *slog.Logger) error {
	bin := filepath.Base(os.Args[0])
	instance, err := parseInstanceTarget(args, env("ORCHICON_INSTANCE", "dev"))
	if err != nil {
		return err
	}

	if !instanceExists(instance) {
		return fmt.Errorf("nothing is installed for instance %q (no data directory at %s).\n"+
			"  Install it first:  %s install --instance %s", instance, hostDataDirFor(instance), bin, instance)
	}
	ports, err := instancePorts(instance)
	if err != nil {
		return err
	}

	if residency := instanceResidency(instance); residency != residencyHost {
		// A RECORDED CONTAINER RESIDENCY IS A RETIRED SHAPE, not a shape to act on.
		return fmt.Errorf("instance %q was installed with the retired CONTAINER residency (%s).\n"+
			"  The plane and its services now always run on the HOST, as host processes. Re-install it:\n"+
			"    %s install --instance %s",
			instance, residency, filepath.Base(os.Args[0]), instance)
	} else {
		fmt.Printf("instance %s is host-resident: starting its services, then the plane\n", instance)
		// CLEAR THIS INSTANCE'S OWN LEFTOVERS BEFORE STARTING ANYTHING.
		//
		// A start must converge, and it can only converge from a known state. Without this, a start after
		// an unclean exit races its own orphan: the new supervisor spawns postgres, postgres finds the
		// data directory still held by a re-parented backend from the previous run, and refuses —
		//
		//	FATAL:  lock file "postmaster.pid" already exists
		//	HINT:  Is another postmaster (PID 30488) running in data directory ".../postgres"?
		//
		// which is the failure the operator hit repeatedly, and it blamed a pipe (`orchicon start` →
		// "the services did not come up") rather than naming a process that a stop had left behind.
		//
		// IT ONLY KILLS WHAT IT CAN PROVE IS OURS (an executable inside this instance's own data dir), so
		// a second instance's services and the operator's own PostgreSQL are untouched. Whatever it
		// cannot clear is named, and the start proceeds to fail honestly against it.
		if q := quiesceInstance(instance, ports, os.Stdout); len(q.Killed) > 0 {
			fmt.Printf("cleared %d leftover process(es) from a previous run of %s\n", len(q.Killed), instance)
		}
		if err := startHostServices(instance, ports, hostDataDirFor(instance), os.Stdout); err != nil {
			return err
		}
		// WAIT FOR THE SERVICES BEFORE STARTING THE PLANE, AND FAIL WITH THEIR REASON.
		//
		// This start had NO services gate at all. It launched the supervisor, immediately started
		// the plane, and then waited 60s on the plane's /healthz — so a services stack that never
		// came up was reported as "the host plane did not answer within 60s", which names the
		// SYMPTOM and hides the cause.
		//
		// MEASURED, on the real machine: postgres refused to start because the shell was elevated
		// ("Execution of PostgreSQL by a user with administrative permissions is not permitted"),
		// and `orchicon start dev` spent a full minute and then said:
		//
		//	error: the host plane did not answer http://localhost:8080/healthz within 60s
		//
		// while the actual cause sat in services.log the whole time. The install path already
		// treats this as fatal and quotes that log back (servicesNotReadyError); this is the same
		// check, in the same place in the sequence, so the two entry points cannot disagree about
		// what "started" means.
		//
		// IT IS THE REQUIRED SET, not the whole stack: a missing optional telemetry backend is a
		// degradation the instance survives (the plane degrades too), so refusing to start over one
		// would turn a working instance into a broken command.
		if !waitServicesReady(instance, ports, os.Stdout) {
			return servicesNotReadyError(instance, hostDataDirFor(instance))
		}
		if err := startHostPlane(instance, ports, hostDataDirFor(instance),
			filepath.Join(socketDir(), "runtime.sock"), os.Stdout); err != nil {
			// SAY WHAT IS STILL RUNNING.
			fmt.Fprintf(os.Stderr, "note: the services for %s are still running (postgres :%d, nats :%d).\n"+
				"      stop them with:  %s services down %s\n",
				instance, ports["ORCHICON_POSTGRES_PORT"], ports["ORCHICON_NATS_PORT"],
				filepath.Base(os.Args[0]), instance)
			return err
		}
	}

	// THE RUNTIME DAEMON IS BEST-EFFORT, and deliberately not fatal.
	if containerEngineAvailable() {
		if err := ensureInstallDaemon(socketDir()); err != nil {
			fmt.Fprintf(os.Stderr, "note: the runtime daemon did not come up (%v)\n"+
				"      the plane is up; runtime containers will be unavailable until it does.\n", err)
		}
	} else {
		fmt.Println("no container engine on this host — runtime containers are unavailable, the plane is not")
	}

	url, up := awaitHealthy(instance, ports, 60*time.Second)
	if !up {
		return fmt.Errorf("%s did not answer %s within 60s — it was started but is not serving.\n"+
			"  See the plane's log in %s serve/logs/", instance, url, hostDataDirFor(instance))
	}
	fmt.Printf("✓ %s is serving %s\n", instance, url)
	printInstanceGuidance(instance, ports)
	return nil
}

// printInstanceGuidance tells the operator where to go next, because "it is serving" is the end of
// the OUTPUT and not the end of the JOB.
func printInstanceGuidance(instance string, ports map[string]int) {
	bin := filepath.Base(os.Args[0])
	dir := hostDataDirFor(instance)

	fmt.Println()
	fmt.Printf("  Web UI   : %s\n", planeURL(ports))
	fmt.Printf("  Logs    : %s\n", instanceLogPath(instance))
	fmt.Printf("  Services: %s\n", filepath.Join(dir, "services.log"))
	fmt.Println()
	fmt.Printf("  Follow the logs:  %s logs %s\n", bin, instance)
	fmt.Printf("  Check the status: %s status %s\n", bin, instance)
	fmt.Printf("  Stop it:          %s stop %s\n", bin, instance)
}

// runStop implements `orchicon stop [instance]`.
func runStop(args []string) error {
	instance, err := parseInstanceTarget(args, env("ORCHICON_INSTANCE", "dev"))
	if err != nil {
		return err
	}

	if !instanceExists(instance) {
		fmt.Printf("nothing is installed for instance %q — there is nothing to stop.\n", instance)
		return nil
	}

	// THE PORTS ARE NEEDED BY THE QUIESCE, which verifies them rather than trusting the pid files.
	ports, err := servicesPortsFor(instance)
	if err != nil {
		return err
	}

	if residency := instanceResidency(instance); residency != residencyHost {
		// A RECORDED CONTAINER RESIDENCY IS A RETIRED SHAPE.
		fmt.Printf("instance %s was installed with the retired CONTAINER residency (%s): removing any container-era container\n",
			instance, residency)
		stopInstanceContainer(instance, os.Stdout)
	} else {
		fmt.Printf("instance %s is host-resident: stopping the plane, then its services\n", instance)
		// THE ONE WAY DOWN.
		if err := stopInstanceCompletely(instance, ports, os.Stdout); err != nil {
			return err
		} // A CONTAINER-ERA INSTANCE CONTAINER STILL OWNS THESE PORTS. It was created by an older install of
		// this product under this instance's name, so stopping it is part of stopping the instance — and
		// leaving it running is what makes the NEXT start fail to bind with nothing pointing at the cause.
		stopInstanceContainer(instance, os.Stdout)
	}

	fmt.Printf("✓ %s is stopped\n", instance)
	return nil
}

// socketDir is the per-host runtime socket directory, from the environment.
func socketDir() string {
	return env("ORCHICON_RUNTIME_SOCKET_DIR", filepath.Join(os.TempDir(), "orchicon-runtime"))
}
