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
		// BOTH FORMS AT ONCE IS AMBIGUOUS, and guessing which one the operator meant is
		// how the wrong instance gets changed. The flag was the explicit request, so this
		// reports the conflict rather than silently preferring either.
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

// validateInstanceName refuses a name that could climb out of the instance state
// directory, because the name forms a DIRECTORY NAME.
//
// It is one function rather than the same four lines in `plane`, `restart` and the new
// verbs: three copies of a path-safety check is three chances for one of them to be
// edited and the others not.
func validateInstanceName(instance string) error {
	if strings.ContainsAny(instance, "/\\") || strings.Contains(instance, "..") {
		return fmt.Errorf("invalid instance name %q: it forms the instance state directory name, so it "+
			"may not contain a path separator or \"..\"", instance)
	}
	return nil
}

// awaitHealthy polls the instance's /healthz until it answers or the deadline passes,
// returning the URL it asked. The caller words the outcome, because "came back after a
// restart" and "came up on a start" are different claims.
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

// runStart implements `orchicon start [instance]`.
//
// IT STARTS WHAT `install` CREATED — it never installs. A start with no install record
// is refused with the command that would create one, because "start" on a machine that
// has nothing installed should not silently initialise a database: that is `install`'s
// job, and it asks the questions start must not ask.
//
// ORDER IS THE DEPENDENCY: the services first (the plane boots against postgres and
// NATS), then the plane. Reversed, the plane comes up with no database and looks broken.
func runStart(args []string, log *slog.Logger) error {
	bin := filepath.Base(os.Args[0])
	instance, err := parseInstanceTarget(args, env("ORCHICON_INSTANCE", "dev"))
	if err != nil {
		return err
	}

	residency, ok := recordedResidency(instance)
	if !ok {
		return fmt.Errorf("no install record for instance %q — nothing is installed for it.\n"+
			"  Install it first:  %s install --instance %s", instance, bin, instance)
	}
	ports, err := readResolvedPorts(instance)
	if err != nil {
		return err
	}

	if residency == residencyContainer {
		// A container-resident instance keeps the whole stack in its container, so the
		// container IS the start. Say which shape won, so "start" is never a command whose
		// effect the operator has to infer.
		name := "orchicon-cnt-" + instance
		fmt.Printf("instance %s is container-resident: starting container %s\n", instance, name)
		if out, err := engineCommand("start", name).CombinedOutput(); err != nil {
			return fmt.Errorf("start %s: %v: %s", name, err, strings.TrimSpace(string(out)))
		}
	} else {
		fmt.Printf("instance %s is host-resident: starting its services, then the plane\n", instance)
		if err := startHostServices(instance, ports, hostDataDirFor(instance), os.Stdout); err != nil {
			return err
		}
		if err := startHostPlane(instance, ports, hostDataDirFor(instance),
			filepath.Join(socketDir(), "runtime.sock"), os.Stdout); err != nil {
			return err
		}
	}

	// THE RUNTIME DAEMON IS BEST-EFFORT, and deliberately not fatal.
	//
	// `install` treats a daemon that will not come up as a failed install, which is right
	// for a first run. It is wrong here: "no container engine" is a SUPPORTED shape (that
	// is the whole no-engine acceptance procedure), and a plane whose runtime containers
	// are unavailable still serves the UI, the API and every non-container workflow. So a
	// start reports it and carries on rather than refusing to finish bringing the instance
	// up over an optional component.
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
	return nil
}

// runStop implements `orchicon stop [instance]`.
//
// STOP IS IDEMPOTENT — stopping what is already stopped is a successful no-op, because
// a stop is something a person runs when they are unsure whether anything is up, and a
// command that only works when it was needed is a command nobody can trust in a script.
// Both primitives underneath already guarantee it (planecontrol.Stop's header, and
// stopHostServices' "nothing to stop" arm).
//
// ORDER IS THE MIRROR OF START, and it matters for a different reason: stopping the
// SERVICES out from under a live plane leaves it logging connection errors and looking
// broken, which is precisely the state that makes the next status check lie.
func runStop(args []string) error {
	instance, err := parseInstanceTarget(args, env("ORCHICON_INSTANCE", "dev"))
	if err != nil {
		return err
	}

	residency, ok := recordedResidency(instance)
	if !ok {
		fmt.Printf("no install record for instance %q — nothing is installed, so there is nothing to stop.\n",
			instance)
		return nil
	}

	if residency == residencyContainer {
		name := "orchicon-cnt-" + instance
		fmt.Printf("instance %s is container-resident: stopping container %s\n", instance, name)
		if out, err := engineCommand("stop", name).CombinedOutput(); err != nil {
			return fmt.Errorf("stop %s: %v: %s", name, err, strings.TrimSpace(string(out)))
		}
	} else {
		fmt.Printf("instance %s is host-resident: stopping the plane, then its services\n", instance)
		if err := stopHostPlane(instance, os.Stdout); err != nil {
			return err
		}
		if err := stopHostServices(instance, os.Stdout); err != nil {
			return err
		}
	}

	fmt.Printf("✓ %s is stopped\n", instance)
	return nil
}

// socketDir is the per-host runtime socket directory, from the environment.
func socketDir() string {
	return env("ORCHICON_RUNTIME_SOCKET_DIR", filepath.Join(os.TempDir(), "orchicon-runtime"))
}
