package main

// services_health.go — WHAT "THE SERVICES ARE UP" MEANS, in one place.
//
// THE DEFECT THIS REPLACES. The install decided the services were ready by asking two questions:
// "does pg_isready answer" and "does the NATS monitor answer". Both are about the CRITICAL CHAIN,
// and the critical chain is not the whole services set. The four telemetry children — Loki, Tempo,
// VictoriaMetrics and the OTel collector — were never asked about at all, so an install could
// start them, watch all four die, and then declare the stack healthy:
//
//	== skipping the image pull — no container engine
//	waiting for dev services (postgres 127.0.0.1:5432, nats monitor 127.0.0.1:8222) …
//	dev services ready (postgres 127.0.0.1:5432, nats monitor 127.0.0.1:8222)
//	✓ attached to the host runtime …
//
// ON WINDOWS ALL FOUR WERE IN FACT CRASH-LOOPING (a Windows data dir is not a legal YAML escape in
// the config the supervisor writes — see TestEmbeddedConfigsParseWithAWindowsDataDir), so the
// operator's Logs and Traces views reported their backends unreachable while the installer, the
// services log's own readiness gate, `orchicon status` and `orchicon services status` all said the
// services were up. Every one of them was reading that same two-component answer.
//
// SO THE ANSWER IS PER COMPONENT, and the two SEVERITIES exist because the components do not have
// the same standing:
//
//   - postgres and nats are REQUIRED. The plane cannot run without them; a missing one is not a
//     degraded instance, it is a broken one.
//   - the telemetry children are OPTIONAL. The plane degrades without them (its OTel dial is
//     non-blocking and its query clients are optional), so a plane with no Loki still serves every
//     request — it just cannot show you logs. That is a real degradation and it is NOT an install
//     failure, which is exactly the distinction the old boolean could not express.
//
// The optional set is empty when telemetry is not embedded, so `ORCHICON_TELEMETRY=none` is not
// reported as a permanent failure.

import (
	"strconv"
	"time"
)

// serviceComponent is one member of the services set and how to ask if it is up.
type serviceComponent struct {
	name string
	// required marks a component the plane cannot work without.
	required bool
	// ready asks the component. It must be CHEAP and SIDE-EFFECT FREE: it runs in the install's
	// wait loop, in `status`, and in `services status`.
	ready func() bool
}

// telemetryEmbedded reports whether this install runs the telemetry children at all.
//
// THE DEFAULT IS embedded, matching runHostServices, because a wrong answer here makes `status`
// report four permanently-missing components on an instance that was never meant to have them.
func telemetryEmbedded() bool {
	mode := env(telemetryEnv, "embedded")
	return mode == "" || mode == "embedded"
}

// serviceHealthOpinion is what one component answered, kept in the report so a caller can say
// WHICH component is missing rather than that "something" is.
type serviceHealthOpinion struct {
	name     string
	required bool
	up       bool
}

// servicesHealth asks every component and returns the per-component answer.
//
// A COMPONENT WITH NO HOST ENDPOINT IS NOT ASKED AND IS NOT REPORTED — see the container-resident
// note on servicesProbeableFromHost. This function only runs for a shape whose ports were resolved,
// so in practice that is the host shape.
func servicesHealth(instance string, ports map[string]int) []serviceHealthOpinion {
	return servicesHealthTimeout(instance, ports, 3*time.Second)
}

// servicesHealthTimeout is servicesHealth with the per-component probe timeout made explicit.
//
// THE WAIT PATH NEEDS A SHORTER ONE, and the reason is a race rather than impatience: runHostServices
// spawns the four telemetry children only AFTER postgres and nats are ready, and the install's services
// gate passes the moment those two answer. Asking the optional components at their ordinary 3s timeout
// therefore guarantees "not answering" for a stack that is a fraction of a second from being fine —
// measured, twice, as a spurious "DEGRADED — tempo, loki not answering" immediately after a start.
func servicesHealthTimeout(instance string, ports map[string]int, probeTimeout time.Duration) []serviceHealthOpinion {
	if !servicesProbeableFromHost(ports) {
		return nil
	}
	pgPort := ports["ORCHICON_POSTGRES_PORT"]
	natsMonitor := ports["ORCHICON_NATS_MONITOR_PORT"]

	// THE TELEMETRY PROBES ARE BUILT ONLY WHEN THE RECORD NAMES A PORT FOR THEM.
	//
	// A port of 0 in the record is not "a component on port 0" — it is the ABSENCE OF AN ADDRESS, and
	// probing it would manufacture a negative: http://127.0.0.1:0/ never answers, so a perfectly
	// healthy collector would be reported down and every instance would look degraded for ever. That
	// is exactly what happened when ports.json (correctly, at the time) omitted the internal,
	// unpublished ports — see everyServicePortEnv. The fail-safe direction is the same one
	// servicesProbeableFromHost takes: never invent a negative you cannot observe.
	comps := []serviceComponent{
		{
			name: "postgres", required: true,
			ready: func() bool {
				// quietCommand, NOT exec.Command: THIS IS THE LOOP THAT MADE THE SCREEN BLINK. The
				// install and the start gate call this up to sixty times, once a second, from a
				// process with no console — and every bare exec.Command there opened a window.
				return quietCommand(pgIsReadyBin(instance),
					"-h", "127.0.0.1", "-p", strconv.Itoa(pgPort), "-U", "orchicon").Run() == nil
			},
		},
		{
			name: "nats", required: true,
			// The MONITOR port answers /healthz only once the server is actually serving; the
			// client port accepting a socket is not the same question (a server can bind and
			// then fail to start JetStream, which is the state this instance needs — its event
			// bus carries JetStream streams).
			ready: func() bool {
				return httpHealthy("http://127.0.0.1:"+strconv.Itoa(natsMonitor)+"/healthz", 3*time.Second)
			},
		},
	}
	if telemetryEmbedded() {
		// THE ENDPOINTS ARE THE CHILDREN'S OWN READINESS ENDPOINTS, which is the same question
		// their supervisor's startAndWait asks. Nothing here is a port-reachability stand-in:
		// Loki's /ready answers 503 while it is still replaying its WAL (measured: 15.3s on a
		// cold boot), and treating a bound socket as "ready" would report a backend the plane
		// cannot yet query.
		// Each entry is added ONLY if the record names a port for it, so a missing key degrades to
		// "not asked" rather than to "not answering".
		httpComp := func(name, env, path string) (serviceComponent, bool) {
			port := ports[env]
			if port <= 0 {
				return serviceComponent{}, false
			}
			url := "http://127.0.0.1:" + strconv.Itoa(port) + path
			return serviceComponent{name: name, ready: func() bool { return httpHealthy(url, probeTimeout) }}, true
		}
		for _, c := range []struct{ name, env, path string }{
			{"tempo", "ORCHICON_TEMPO_PORT", "/ready"},
			{"loki", "ORCHICON_LOKI_PORT", "/ready"},
			{"victoriametrics", "ORCHICON_VM_PORT", "/health"},
			// The collector's health EXTENSION serves the health check on "/", not /healthz:
			// graphhealthcheck with no path configured answers at the root.
			{"otel-collector", "ORCHICON_OTEL_HEALTH_PORT", "/"},
		} {
			if comp, ok := httpComp(c.name, c.env, c.path); ok {
				comps = append(comps, comp)
			}
		}
	}

	out := make([]serviceHealthOpinion, 0, len(comps))
	for _, c := range comps {
		out = append(out, serviceHealthOpinion{name: c.name, required: c.required, up: c.ready()})
	}
	return out
}

// requiredServicesReady reports whether every REQUIRED component answers.
//
// This is the question `orchicon services status`'s exit code and a start's gate ask: a plane with
// no database is broken, while a plane with no logs is degraded but working.
func requiredServicesReady(instance string, ports map[string]int) bool {
	return requiredServicesReadyWithin(instance, ports, 3*time.Second)
}

// requiredServicesReadyWithin is requiredServicesReady with the probe timeout made explicit, for the install
// and start gates — which ask immediately after spawning a supervisor.
func requiredServicesReadyWithin(instance string, ports map[string]int, probeTimeout time.Duration) bool {
	for _, c := range servicesHealthTimeout(instance, ports, probeTimeout) {
		if c.required && !c.up {
			return false
		}
	}
	return true
}

// degradedServices lists the OPTIONAL components that are not answering, in table order.
//
// Empty means either everything is up or telemetry is not embedded. A caller decides how loudly to
// say it; the install says it once, after the plane is confirmed up, which is the only point where
// "the instance works but you cannot see its logs" is a fact worth stating.
func degradedServices(instance string, ports map[string]int) []string {
	var down []string
	for _, c := range servicesHealth(instance, ports) {
		if !c.required && !c.up {
			down = append(down, c.name)
		}
	}
	return down
}
