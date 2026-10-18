package main

// instance_verbs.go — `orchicon start|stop [instance]`: ONE verb for the whole instance.
//
// THE GAP THIS CLOSES. Every other lifecycle verb is a COMPONENT verb: `install` sets a machine
// up, `restart` re-reads the environment, `plane` drives one half, `services` drives the other. So
// "stop Orchicon" was TWO commands, and the second one required an operator to know that
// "services" means Postgres and NATS:
//
//	orchicon plane stop
//	orchicon services down
//
// That is an internal decomposition leaking into the user's vocabulary, and it is why the repo's
// own launcher (`make instance-down` → scripts/orchicon.sh) exists at all — the launcher composes
// the same two calls. A user who installed the BINARY should not need a checkout to stop it, so
// these are the missing top-level verbs: the same calls, in the order that is safe, under names
// that mean what they say.
//
// THEY ARE COMPOSITIONS, NOT A SECOND LIFECYCLE. Every step below is an existing verb's own
// implementation — planecontrol.Start/Stop, servicesUp/servicesDown, ensureInstallDaemon. A third
// supervisor here is exactly the defect this repo keeps warning about.
//
// INSTANCE TARGETING IS PART OF THE CONTRACT, not an afterthought. A host can carry dev, prod and
// test side by side, so each verb takes an optional instance name, falling back to
// ORCHICON_INSTANCE and then to dev — the same resolution the rest of the CLI uses.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/planecontrol"
)

// instanceArg reads an OPTIONAL positional instance name and refuses a flag, because a silently
// ignored `-i prod` would act on dev while the operator watched for prod to change. It returns ""
// when no instance was given, leaving the choice to the caller (each verb's own env default).
//
// ONE PARSER, because the alternative is the drift this repo already has: `plane` and `services`
// take a positional instance, while `install` and `restart` read only the environment. An operator
// who has learned one spelling should not discover the next verb spells it differently.
func instanceArg(args []string, verb string) (string, error) {
	if len(args) == 0 {
		return "", nil
	}
	first := strings.TrimSpace(args[0])
	if strings.HasPrefix(first, "-") {
		return "", fmt.Errorf("%s takes an optional instance name, not the flag %q\n"+
			"  usage: orchicon %s [dev|prod|test]\n"+
			"  (the instance may also be set with ORCHICON_INSTANCE)", verb, first, verb)
	}
	return first, nil
}

// validInstanceName refuses a name that could climb out of the instance state directory, which is
// built from it. Shared by every instance-addressed verb so the guard cannot be forgotten by one.
func validInstanceName(instance string) error {
	if strings.ContainsAny(instance, "/\\") || strings.Contains(instance, "..") {
		return fmt.Errorf("invalid instance name %q: it forms the instance state directory name, so it "+
			"may not contain a path separator or \"..\"", instance)
	}
	return nil
}

// requireInstalled resolves the instance's recorded shape, refusing an instance that was never
// installed. It is NOT validateInstance (a PORT-DEFAULTS table check): start/stop read the ports
// that were actually bound from the record, so asking about port defaults would answer a question
// these verbs never ask — the same reasoning `orchicon restart` documents.
func requireInstalled(instance, verb string) (string, error) {
	residency, ok := recordedResidency(instance)
	if !ok {
		return "", fmt.Errorf("no install record for instance %q — nothing is installed for it.\n"+
			"  Install it first:   orchicon install %s\n"+
			"  A different one:    orchicon %s <name>   (or ORCHICON_INSTANCE=<name>)",
			instance, instance, verb)
	}
	return residency, nil
}

// containerResidentVerb drives the single-CONTAINER shape, where the whole stack (services and
// plane) is inside one container. `planecontrol` deliberately refuses that shape rather than
// guessing, so the verb handles it — the division of labour `orchicon restart` already follows.
func containerResidentVerb(verb, instance string) error {
	name := "orchicon-cnt-" + instance
	if !containerEngineAvailable() {
		return fmt.Errorf("instance %q runs in a container, but no container engine is available on this host\n"+
			"  start docker (or podman) and retry: the container %s is where its stack lives",
			instance, name)
	}
	out, err := engineCommand(verb, name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", verb, name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// waitPlaneServing polls the instance's own recorded control port. Used for the container shape,
// where the start happens inside the container and nothing else is watching it — a container that
// starts but never serves must not be reported as a successful start.
func waitPlaneServing(instance string) bool {
	ports, err := readResolvedPorts(instance)
	if err != nil {
		return false
	}
	url := fmt.Sprintf("http://localhost:%d/healthz", ports["ORCHICON_CONTROL_PORT"])
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if planecontrol.Healthy(url, 2*time.Second) {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// runStart is `orchicon start [instance]` — the whole instance, in one verb.
//
// ORDER IS THE SAME AS `orchicon install`, and for the same reasons: the runtime daemon before
// anything that could dispatch work, then the SERVICES, then the plane. The plane runs migrations
// on boot, so starting it beside a half-started Postgres would race them.
func runStart(args []string) int {
	verb := "start"
	instance := env("ORCHICON_INSTANCE", "dev")
	if given, err := instanceArg(args, verb); err != nil {
		fmt.Fprintf(os.Stderr, "orchicon %s: %v\n", verb, err)
		return 2
	} else if given != "" {
		instance = given
	}
	if err := validInstanceName(instance); err != nil {
		fmt.Fprintf(os.Stderr, "orchicon %s: %v\n", verb, err)
		return 2
	}
	residency, err := requireInstalled(instance, verb)
	if err != nil {
		fmt.Fprintf(os.Stderr, "orchicon %s: %v\n", verb, err)
		return 1
	}

	if residency == residencyContainer {
		// The container IS the instance: its services and its plane live inside it, so starting the
		// container is the whole start. There are no host services to start, and starting them here
		// would double-bind the ports the container publishes.
		if err := containerResidentVerb("start", instance); err != nil {
			fmt.Fprintf(os.Stderr, "orchicon %s: %v\n", verb, err)
			return 1
		}
		if !waitPlaneServing(instance) {
			fmt.Fprintf(os.Stderr, "orchicon %s: container orchicon-cnt-%s started but its plane did not answer /healthz\n"+
				"  its logs are the place to look: the container's stdout\n", verb, instance)
			return 1
		}
		printStartSummary(instance, "container")
		return 0
	}

	socketDir := env("ORCHICON_RUNTIME_SOCKET_DIR", filepath.Join(os.TempDir(), "orchicon-runtime"))
	// The daemon first: it is what a dispatched workflow run needs, and a "started" instance whose
	// work cannot execute is the kind of half-up that `install` already refuses to report.
	fmt.Println("==> starting the runtime daemon")
	if err := ensureInstallDaemon(socketDir); err != nil {
		fmt.Fprintf(os.Stderr, "orchicon %s: %v\n", verb, err)
		return 1
	}

	fmt.Println("==> starting the local services (postgres, nats)")
	if code := servicesUp(instance); code != 0 {
		return code
	}

	fmt.Println("==> starting the plane")
	if err := planecontrol.Start(planecontrol.Defaults(instance, os.Stdout)); err != nil {
		fmt.Fprintf(os.Stderr, "orchicon %s: %v\n", verb, err)
		return 1
	}
	printStartSummary(instance, "host")
	return 0
}

// runStop is `orchicon stop [instance]` — the whole instance, in one verb.
//
// THE PLANE BEFORE THE SERVICES, which is the reverse of start. A plane left running over a dead
// database is the state that makes a status check lie: it holds its port, answers nothing useful,
// and the next start fails with a bind error that names no cause. scripts/orchicon.sh's stop_host
// documents the same ordering for the same reason.
func runStop(args []string) int {
	verb := "stop"
	instance := env("ORCHICON_INSTANCE", "dev")
	if given, err := instanceArg(args, verb); err != nil {
		fmt.Fprintf(os.Stderr, "orchicon %s: %v\n", verb, err)
		return 2
	} else if given != "" {
		instance = given
	}
	if err := validInstanceName(instance); err != nil {
		fmt.Fprintf(os.Stderr, "orchicon %s: %v\n", verb, err)
		return 2
	}
	residency, err := requireInstalled(instance, verb)
	if err != nil {
		fmt.Fprintf(os.Stderr, "orchicon %s: %v\n", verb, err)
		return 1
	}

	if residency == residencyContainer {
		if err := containerResidentVerb("stop", instance); err != nil {
			fmt.Fprintf(os.Stderr, "orchicon %s: %v\n", verb, err)
			return 1
		}
		fmt.Printf("orchicon %s is stopped (container orchicon-cnt-%s)\n", instance, instance)
		return 0
	}

	fmt.Println("==> stopping the plane")
	// Stop is idempotent: a plane that is not running is not an error, so `stop` twice is safe.
	if err := planecontrol.Stop(planecontrol.Defaults(instance, os.Stdout)); err != nil {
		fmt.Fprintf(os.Stderr, "orchicon %s: %v\n", verb, err)
		return 1
	}

	fmt.Println("==> stopping the local services (postgres, nats)")
	// servicesDown exits non-zero ONLY when the ports did not come free, and it names the holder.
	// Taking it at face value is the point: a stop that has not been verified is only a claim.
	if code := servicesDown(instance); code != 0 {
		return code
	}

	fmt.Printf("orchicon %s is stopped — start it again with: orchicon start %s\n", instance, instance)
	return 0
}

// printStartSummary says where the instance is, so an operator does not have to guess a port or a
// log path. A start that succeeds silently leaves the next question ("where is it?") unanswered.
func printStartSummary(instance, shape string) {
	fmt.Printf("\norchicon %s is up (%s)\n", instance, shape)
	if ports, err := readResolvedPorts(instance); err == nil {
		if p := ports["ORCHICON_CONTROL_PORT"]; p > 0 {
			fmt.Printf("  web UI   http://localhost:%d\n", p)
		}
	}
	fmt.Printf("  stop it  orchicon stop %s\n", instance)
}
