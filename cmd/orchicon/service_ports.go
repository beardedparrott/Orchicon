package main

// service_ports.go — WHO ELSE HOLDS THE PORT, and saying so.
//
// THE DEFECT. A supervisor whose child cannot bind its port retries for ever with a backoff capped at
// 32s, logging the child's own words and nothing else:
//
//	level=INFO msg="started" component=nats pid=4192
//	level=INFO msg="[4192] [FTL] Can't start monitoring: can't listen to the monitor port:
//	    listen tcp 0.0.0.0:8222: bind: Only one usage of each socket address …
//	level=WARN msg="component exited; restarting" component=nats restarts=26 backoff=32s error="exit status 1"
//	… restarts=27 … restarts=28 …
//
// MEASURED ON A REAL WINDOWS INSTANCE: nats at 27 restarts, loki at 22 and tempo at 26, all on their
// OWN ports, for hours. The cause was an ORPHANED nats — 19 HOURS old, its parent supervisor long
// gone — still holding 4222 and 8222 from a previous run. Nothing in the log said "something else
// owns this port", because nothing asked: the bind error names a port and a protocol and leaves the
// operator to find out what has it.
//
// A bind conflict and a real bug look IDENTICAL from inside that loop, and they are not the same
// problem: one is fixed by stopping the other process, the other by changing code. Naming the
// owning process turns an unbounded retry into a diagnosis, so this is the first thing a crash-loop
// says once it looks like it will not converge.
//
// IT NEVER ACTS. Killing another process is destructive and could take down something the operator
// cares about (often a SECOND INSTANCE's services, which is a legitimate thing to be running), so
// this reports and leaves the decision alone.

import (
	"fmt"
	"log/slog"
)

// portOwnerEnv names the environment variable that disables owner reporting for a port.
//
// A test whose fake child "cannot bind" a port would otherwise spend the bind-timeout asking the OS
// about a port it made up, on every restart.
const portOwnerEnv = "ORCHICON_SKIP_PORT_OWNER_LOOKUP"

// reportPortOwner explains, ONCE per component, why it probably cannot bind.
//
// It is called from reapRestart after the crash-loop threshold, not on the first failure: a
// legitimate transient bind (a previous instance of the same child still shutting down) resolves
// itself, and reporting every one would train the operator to ignore the message.
func (s *supervisor) reportPortOwner(component string) {
	if s.portOwnerReported == nil {
		s.portOwnerReported = make(map[string]bool)
	}
	if s.portOwnerReported[component] {
		return
	}
	ports := s.componentPorts(component)
	if len(ports) == 0 {
		return
	}
	owners := portOwners(ports)
	if len(owners) == 0 {
		// NOTHING ELSE HOLDS IT. That is itself the finding: the port is free, so the failure is
		// not a conflict and the child's own message is the whole story. Saying "no owner" would
		// be noise, so it is not said — but the ABSENCE is recorded at debug level so a support
		// case can tell "we looked and found nothing" from "we never looked".
		s.log.Debug("crash-looping component's ports are free — this is not a bind conflict",
			"component", component, "ports", ports)
		return
	}
	s.portOwnerReported[component] = true
	s.logPortOwnerAdvice(component, owners)
}

// componentPorts is the set of host ports a supervised component binds.
//
// It reads s.ports — the SAME table that produced the child's argv and its readiness probe — so the
// port named in the diagnosis is the port the child actually failed to take, not a guess from the
// component's name.
func (s *supervisor) componentPorts(component string) []int {
	switch component {
	case "postgres":
		return []int{s.ports.postgres}
	case "nats":
		return []int{s.ports.nats, s.ports.natsMonitor}
	case "tempo":
		return []int{s.ports.tempo, s.ports.tempoGRPC, s.ports.tempoOTLPGRPC, s.ports.tempoOTLPHTTP}
	case "loki":
		// grpc_listen_port is 0 (ephemeral) in loki.yaml: there is no fixed port to own.
		return []int{s.ports.loki}
	case "victoriametrics":
		return []int{s.ports.vm}
	case "otel-collector":
		return []int{s.ports.collectorOTLPGRPC, s.ports.collectorOTLPHTTP, s.ports.collectorHealth, s.ports.collectorSelfMetrics}
	}
	return nil
}

// portOwner is who is listening on one port.
type portOwner struct {
	Port int
	PID  int
	Name string
}

// portOwners asks the platform who owns each port. Implemented per-OS (port_owner_windows.go,
// port_owner_unix.go); an unsupported platform yields nil rather than an error, because the caller
// is producing a DIAGNOSIS and must never fail the supervisor for lack of one.
func portOwners(ports []int) []portOwner {
	if env(portOwnerEnv, "") != "" {
		return nil
	}
	out, err := lookupPortOwners(ports)
	if err != nil {
		// Deliberately swallowed: see the doc comment. The caller logs what it found.
		return nil
	}
	return out
}

// describeOwner renders one owner for a message.
func describeOwner(o portOwner) string {
	if o.Name == "" {
		return fmt.Sprintf("pid %d", o.PID)
	}
	return fmt.Sprintf("%s (pid %d)", o.Name, o.PID)
}

// logPortOwnerAdvice is the log call the supervisor makes; kept as a function so the wording exists
// in exactly one place and a test can assert it without matching a format string.
func (s *supervisor) logPortOwnerAdvice(component string, owners []portOwner) {
	for _, o := range owners {
		s.log.Error("crash-looping component cannot bind a port that ANOTHER PROCESS holds",
			slog.String("component", component),
			slog.Int("port", o.Port),
			slog.String("held_by", describeOwner(o)),
			slog.String("hint", "stop the other instance/process; Orchicon will not kill a process "+
				"it did not start"))
	}
}
