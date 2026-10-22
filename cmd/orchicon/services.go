package main

// `orchicon services up|down|status` — the LOCAL SERVICES the inner loop needs.
//
// WHY THIS EXISTS AS A VERB. `make run` starts the control plane from source and
// `make migrate` applies migrations to a database, and both used to assume the
// services came from the instance CONTAINER (a published 5432) or from a Postgres
// the developer installed themselves. Remove the container and neither works: the
// plane boots with no postgres and no bus, and the migrations have nothing to
// reach. This verb is what the Makefile calls to bring those two up ON THE HOST,
// with no container engine and no Postgres installed.
//
// IT IS A THIN WRAPPER, NOT A SECOND SUPERVISOR. The de-containerized service
// layer already ships: `startHostServices` (install_host.go) re-execs this binary
// as `container` with ORCHICON_SERVICE_RESIDENCY=host, which runs postgres, nats
// and (optionally) telemetry as DIRECT PROCESSES OF THE INVOKING USER, acquiring
// the pinned binaries via detect-else-download (binaries.go) when the host has
// none. Writing another supervisor here would be a second implementation of the
// same thing — the exact defect the repo's own comments warn against. So this
// file only decides the PORTS, dispatches the verbs, and adds the two things a
// raw `container` boot does not do: an idempotent "already running" report, and
// a stop that VERIFIES the ports came free.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// runServices is the `orchicon services` entrypoint.
//
// VERB is up | down | status; a BARE `services` means status, so a no-argument
// command never starts a database as a side effect.
//
// INSTANCE comes from the first positional after the verb, else ORCHICON_INSTANCE,
// else dev — through parseInstanceTarget, the same resolution every other subcommand
// uses, so `--instance <name>` works on this verb too.
func runServices(args []string) int {
	verb := "status"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		verb = args[0]
		rest = args[1:]
	}
	// --force is consumed HERE, before the instance parser, because the parser refuses unknown flags by
	// design (an ignored flag on a lifecycle verb is how the wrong instance gets changed). Taking it out
	// first keeps that guarantee and still lets `services down --force prod` mean one thing.
	force := false
	kept := make([]string, 0, len(rest))
	for _, a := range rest {
		if a == "--force" || a == "-force" {
			force = true
			continue
		}
		kept = append(kept, a)
	}
	instance, err := parseInstanceTarget(kept, env("ORCHICON_INSTANCE", "dev"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "orchicon services: %v\n", err)
		return 1
	}

	switch verb {
	case "up":
		return servicesUp(instance)
	case "down":
		return servicesDown(instance, force)
	case "status":
		return servicesStatus(instance)
	default:
		fmt.Fprintf(os.Stderr, "orchicon services: unknown verb %q — use up | down | status\n", verb)
		return 2
	}
}

// servicesPortsFor decides the host ports this instance's local services use.
//
// THE RECORD FIRST, THEN DEFAULTS + OVERRIDES, AND NEVER `resolveInstallPorts`.
// The record (ports.json) is what the instance ACTUALLY bound, so re-running
// `services up` over a live stack keeps the ports it is already on instead of
// re-deriving defaults under it. When there is no record — a developer who never
// ran `orchicon install` — the instance's own column of the shared port table is
// used, plus the same ORCHICON_*_PORT knobs the installer honours. resolveInstallPorts
// is deliberately NOT called: it PROMPTS on a terminal when the spec is
// ambiguous, and a start that asks a question is unusable from a Makefile
// prerequisite (this is AC5's "no-op that says so", not a dialogue).
func servicesPortsFor(instance string) (map[string]int, error) {
	if err := validateInstance(instance); err != nil {
		return nil, err
	}
	// THE RECORD IS USED ONLY WHEN IT IS COMPLETE AND COHERENT.
	//
	// It used to be accepted whenever it merely HAD postgres and nats, and the record is
	// written per-resolve — so a re-resolve that moved ONE port (a collision on a rebuild,
	// or an operator who typed a different one) left a record mixing a moved port with the
	// others' old values. The report is exact about it:
	//
	//	starting the host services for dev (postgres, nats, telemetry …)
	//	note: the services for dev are still running (postgres :5433, nats :4222)
	//	error: the host plane did not answer http://localhost:8080/healthz within 60s
	//
	// — 5433 is PROD's postgres port and 4222 is dev's nats port, in the same instance. The
	// plane was pointed at a database that was never bound, and the services were told a
	// different port than the one the plane used. Two halves of one instance, disagreeing.
	//
	// A record is only trustworthy if it names EVERY port this instance binds: then there is
	// nothing to mix. A partial record is IGNORED, and the defaults are used, because the
	// defaults are coherent by construction and a mixed record is not.
	if rec, err := readResolvedPorts(instance); err == nil && portsRecordIsComplete(rec) {
		return rec, nil
	}
	ports := map[string]int{}
	for _, p := range activeInstallPorts(residencyHost) {
		if d, ok := p.defaultFor(instance); ok {
			ports[p.Env] = d
		}
	}
	for _, env := range hostServicesPortEnvs {
		v := strings.TrimSpace(os.Getenv(env))
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("%s=%q is not a port (1..65535)", env, v)
		}
		ports[env] = n
	}
	return ports, nil
}

// servicesDSN is the Postgres DSN for a resolved port set — DERIVED from the
// postgres port, never a literal, so the string a developer pastes and the port
// the services bound cannot disagree.
func servicesDSN(ports map[string]int) string {
	return fmt.Sprintf("postgres://orchicon:orchicon@localhost:%d/orchicon?sslmode=disable",
		ports["ORCHICON_POSTGRES_PORT"])
}

// telemetryOffForTheInnerLoop turns telemetry OFF unless the operator asked for
// it, by setting the variable the re-exec'd supervisor inherits.
//
// THE INNER LOOP NEEDS THE DB AND THE BUS. The four telemetry backends are
// ~300 MB of downloads and a wider port set for no inner-loop benefit, and a
// first `make run` that spends minutes fetching the backends' dependencies before it
// can serve a request is the wrong default. An explicit ORCHICON_TELEMETRY
// always wins.
func telemetryOffForTheInnerLoop() {
	if strings.TrimSpace(os.Getenv("ORCHICON_TELEMETRY")) == "" {
		_ = os.Setenv("ORCHICON_TELEMETRY", "none")
	}
}

// servicesUp starts this instance's local services (idempotently) and reports the DSN.
func servicesUp(instance string) int {
	ports, err := servicesPortsFor(instance)
	if err != nil {
		fmt.Fprintf(os.Stderr, "orchicon services: %v\n", err)
		return 1
	}
	telemetryOffForTheInnerLoop()
	dataDir := hostDataDirFor(instance)

	// startHostServices IS ITSELF IDEMPOTENT: already-ready services are left
	// running and it says so, which is exactly the "starting what is already
	// running is a no-op that reports the state" the acceptance criteria ask for.
	if err := startHostServices(instance, ports, dataDir, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "orchicon services: %v\n", err)
		return 1
	}
	if !waitServicesReadyFor(instance, ports, 60, time.Second, os.Stdout) {
		fmt.Fprintf(os.Stderr, "orchicon services: the services for %s did not become ready — see %s/services.log\n",
			instance, dataDir)
		return 1
	}
	fmt.Printf("services for %s are up. DSN: %s\n", instance, servicesDSN(ports))
	return 0
}

// servicesDown stops this instance's local services, then VERIFIES the ports came
// free before reporting success.
//
// THE VERIFICATION IS THE POINT. An orphaned Postgres still holding 5432 is the
// classic way a developer's next start fails confusingly: `services down` would
// say "stopped", the start would then fail with a bind error that names nothing,
// and the real cause (a process the pid file no longer knew about) is invisible.
// So a stop is only successful when the ports are actually free, and when they
// are not, the port is NAMED.
func servicesDown(instance string, force bool) int {
	ports, err := servicesPortsFor(instance)
	if err != nil {
		fmt.Fprintf(os.Stderr, "orchicon services: %v\n", err)
		return 1
	}
	if err := stopHostServices(instance, os.Stdout); err != nil {
		// NOT FATAL BY ITSELF. stopHostServices bounds its wait on the supervisor's
		// PID at 10s, and a supervisor caught mid-restart can take slightly longer
		// than that even though it IS shutting down (observed: SIGTERM handled, the
		// process exited a beat after the bound). The AUTHORITATIVE question is
		// whether the PORTS came free, and that is what decides below — so a bounded
		// stop is reported, then verified rather than believed.
		fmt.Fprintf(os.Stderr, "orchicon services: %v — waiting for the ports to come free\n", err)
	}
	pg := ports["ORCHICON_POSTGRES_PORT"]
	nats := ports["ORCHICON_NATS_PORT"]

	deadline := time.Now().Add(20 * time.Second)
	for {
		pgFree := pg <= 0 || portFree(pg)
		natsFree := nats <= 0 || portFree(nats)
		if pgFree && natsFree {
			// A stale record after a slow stop would make a later `services down`
			// look like there is something to stop; the ports being free is the
			// truth, so the record follows it.
			if pf, perr := hostServicesPIDFile(instance); perr == nil {
				_ = os.Remove(pf)
			}
			fmt.Printf("services for %s are stopped; postgres :%d and nats :%d are free\n", instance, pg, nats)
			return 0
		}
		if time.Now().After(deadline) {
			// BEFORE FAILING, TAKE BACK WHAT IS OURS. The deadline above only proves that the HOST services
			// are gone; it says nothing about a container-era instance container, which this product created
			// and which publishes the same ports. That container is why a host that ever ran the old shape
			// could never rebuild: the teardown reported "an orphaned process still holds the port" and left
			// the operator to find a container nothing in the codebase would stop.
			reclaimed := stopInstanceContainer(instance, os.Stdout)
			if force {
				// --force additionally terminates leftover processes OF THIS INSTANCE that the graceful stop
				// missed. It is still bounded by identity: a pid is only touched when its executable is one of
				// our binaries AND it carries this instance's name. An unrelated postgres on :5432 is reported
				// below, never killed — see cmd/orchicon/reclaim.go for why that boundary is not negotiable.
				if killed := forceKillOurLeftovers(instance, os.Stdout); len(killed) > 0 {
					reclaimed = true
				}
			}
			if reclaimed {
				// Re-verify rather than assume: the point of this whole block is that a stop is a CLAIM, and
				// only the ports coming free settles it.
				deadline = time.Now().Add(20 * time.Second)
				continue
			}

			var held []string
			if !pgFree {
				held = append(held, fmt.Sprintf("postgres :%d", pg))
			}
			if !natsFree {
				held = append(held, fmt.Sprintf("nats :%d", nats))
			}
			fmt.Fprintf(os.Stderr, "orchicon services: %s did not release %s — and the holder is NOT this instance's\n",
				instance, strings.Join(held, " and "))
			holders := portHolders(pg, nats)
			for _, pid := range holders {
				fmt.Fprintf(os.Stderr, "    pid %d  %s\n", pid.pid, pid.exe)
			}
			if len(holders) == 0 {
				// NOTHING COULD BE IDENTIFIED — which is the NORMAL answer on Windows (no /proc), so the
				// operator is pointed at a tool that exists on THEIR platform rather than at `ss`/`lsof`.
				// See reclaim_proc_windows.go.
				fmt.Fprintf(os.Stderr, "  find the holder with %s\n", portHolderHint(pg))
			}
			fmt.Fprintf(os.Stderr, "  nothing was killed: a process this instance does not own may be a database you care about.\n"+
				"  Stop it yourself, or re-run with --force to let Orchicon stop only its OWN leftovers.\n")
			return 1
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// servicesStatus reports whether this instance's local services answer, and the
// DSN to use. Exit 0 when up, 1 when down — so a shell can gate on it.
func servicesStatus(instance string) int {
	ports, err := servicesPortsFor(instance)
	if err != nil {
		fmt.Fprintf(os.Stderr, "orchicon services: %v\n", err)
		return 1
	}
	pg := ports["ORCHICON_POSTGRES_PORT"]
	nats := ports["ORCHICON_NATS_PORT"]
	dsn := servicesDSN(ports)

	if servicesReady(instance, ports) {
		supervisor := "supervisor not recorded in the pid file (started outside `orchicon services`)"
		if hostServicesAlive(instance) {
			supervisor = "supervisor running"
		}
		fmt.Printf("services for %s: up (postgres 127.0.0.1:%d, nats 127.0.0.1:%d, %s)\n  DSN: %s\n",
			instance, pg, nats, supervisor, dsn)
		return 0
	}
	fmt.Printf("services for %s: down (nothing answering postgres 127.0.0.1:%d or nats 127.0.0.1:%d)\n"+
		"  DSN when up: %s\n  Start them with `make services instance=%s` (or `%s services up %s`).\n",
		instance, pg, nats, dsn, instance, "orchicon", instance)
	return 1
}

// portsRecordIsComplete reports whether a recorded port set names EVERY port this instance
// binds.
//
// COMPLETENESS IS THE WHOLE TEST. The record is a per-resolve snapshot, so a re-resolve that
// moved one port leaves the others' previous values in place — and the record then describes
// two different instances at once. A record that names every port cannot be mixed like that,
// because there is nothing left over from an earlier resolve to disagree with.
func portsRecordIsComplete(rec map[string]int) bool {
	for _, key := range everyServicePortEnv() {
		if rec[key] <= 0 {
			return false
		}
	}
	return true
}

// everyServicePortEnv names EVERY port a host instance binds, regardless of whether the install
// publishes it on a host interface.
//
// NOT activeInstallPorts — THAT WAS THE BUG. activeInstallPorts filters to the ports a residency
// EXPOSES (boundIn: HostBind != bindNone || HostPlaneBinds), which is the right question for "what
// do we prompt for and what can collide on this host". It is the WRONG question for "can I trust
// this record to be coherent": these ports are BOUND but not exposed, yet the child still binds them
// and the readiness gate still probes them:
//
//	ORCHICON_TEMPO_OTLP_GRPC_PORT, ORCHICON_TEMPO_OTLP_HTTP_PORT,
//	ORCHICON_OTEL_HEALTH_PORT, ORCHICON_OTEL_SELF_METRICS_PORT, ORCHICON_TEMPO_GRPC_PORT
//
// Every real ports.json therefore omits them, so portsRecordIsComplete said INCOMPLETE for every
// correct record — silently, because its only caller falls back to the defaults. The fallback
// happened to be identical, so nothing broke; but a record that is NEVER trusted cannot do its job
// either, and anything reading the record directly (a readiness probe asking for
// ORCHICON_OTEL_HEALTH_PORT) saw port 0 and reported a healthy collector as "not answering".
func everyServicePortEnv() []string {
	out := make([]string, 0, len(installerPorts()))
	for _, p := range installerPorts() {
		out = append(out, p.Env)
	}
	return out
}
