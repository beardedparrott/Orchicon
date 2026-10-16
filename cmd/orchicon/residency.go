package main

// Service residency: WHERE THE SERVICES RUN.
//
// THERE ARE TWO ORTHOGONAL AXES, and conflating them is the bug this file
// exists to prevent:
//
//	plane residency (ORCHICON_PLANE_RESIDENCY, install-time, default "host")
//	    WHERE THE CONTROL PLANE RUNS. "host" (the shape shipped today) means
//	    the plane is a host process of its own while the SERVICES STAY IN THE
//	    CONTAINER — the container runs with ORCHICON_CONTAINER_SERVICES_ONLY=1.
//	    It is NOT a signal that the supervisor should stop using a container.
//
//	service residency (ORCHICON_SERVICE_RESIDENCY, this file)
//	    WHERE THE SERVICES RUN. "host" = postgres, nats and the telemetry
//	    children are direct processes of the invoking user, launched without a
//	    container engine, without setpriv and without a chown. "container" (the
//	    default, and the zero value of supervisor.residency) = today's
//	    behaviour, byte for byte.
//
// A NEW VARIABLE RATHER THAN REUSING THE OLD ONE: ORCHICON_PLANE_RESIDENCY=host
// is the default shape, so reading it here would make every existing
// host-plane install silently host-resident for its services too — postgres
// would try to run as the invoking user against /var/lib/orchicon with no
// container to hold it. Container residency stays the zero value, so every
// existing call site and test keeps today's behaviour.

import (
	"os"
	"path/filepath"
	"strconv"
)

// serviceResidencyEnv selects where the supervised services run.
const serviceResidencyEnv = "ORCHICON_SERVICE_RESIDENCY"

// servicePorts is the single place a supervised service's port is decided, so
// the child's BIND argument and its READINESS probe cannot drift: both read the
// same field. In container residency these are today's literals; in host
// residency the operator's ORCHICON_*_PORT knobs override them (the same names
// install_ports.go and scripts/container.sh already honour, so a host-resident
// install needs no second vocabulary).
type servicePorts struct {
	postgres    int
	nats        int
	natsMonitor int
	tempo       int
	loki        int
	vm          int
	grafana     int
}

// serviceResidencyFromEnv resolves the service residency.
//
// ANYTHING OTHER THAN "host" IS CONTAINER — including empty, whitespace and
// typos. The supervisor runs the product's services and must never refuse to
// boot over a knob; container residency is both the default and the safe
// interpretation of an unrecognised value.
func serviceResidencyFromEnv() string {
	if os.Getenv(serviceResidencyEnv) == residencyHost {
		return residencyHost
	}
	return residencyContainer
}

// isHost reports whether the supervisor should run the services as host
// processes. The zero value of supervisor.residency is "", which is container
// residency — that is what makes acceptance criterion 4 (no container-resident
// regression) mechanical rather than a review exercise.
func (s *supervisor) isHost() bool {
	return s.residency == residencyHost
}

// asUser builds a child command, dropping privileges to uid/gid only where that
// is meaningful: container residency running as a NON-ROOT user (setpriv is how
// the container hands a child to the operator's uid so files it creates in
// mounted dirs are theirs). Everywhere else — host residency, or container
// residency already at uid 0 — the command is launched verbatim.
//
// IT IS THE ONE LAUNCH SEAM. Host residency NEVER emits setpriv, and that is
// asserted by the test suite.
func (s *supervisor) asUser(uid, gid int, command string, args []string) (string, []string) {
	if !s.isHost() && uid != 0 {
		argv := []string{
			"--reuid=" + strconv.Itoa(uid),
			"--regid=" + strconv.Itoa(gid),
			"--clear-groups",
			command,
		}
		return "setpriv", append(argv, args...)
	}
	return command, args
}

// servicePortDefaults are the in-container literals, unchanged from before this
// change — the container residency must bind and probe exactly what it does
// today. The telemetry children's entries are the ports their EMBEDDED CONFIG
// files declare (tempo.yaml http_listen_port, loki.yaml http_listen_port,
// grafana.ini http_port); they are recorded here as the single source for that
// child's readiness probe, and TestServiceProbesMatchTheirBind pins them to the
// configs so the two cannot drift.
func servicePortDefaults() servicePorts {
	return servicePorts{
		postgres:    5432,
		nats:        4222,
		natsMonitor: 8222,
		tempo:       3200,
		loki:        3100,
		vm:          8428,
		grafana:     3000,
	}
}

// servicePortsFor returns the ports a residency binds and probes.
//
// Container residency ignores the operator's port knobs entirely: the
// container's ports are its own private namespace, and the host-side mapping is
// done by -p in the launcher. Host residency honours them, because there the
// supervisor's ports ARE host ports and two instances on one host must not
// collide.
//
// ONLY THE PORTS THE SUPERVISOR ITSELF DECIDES ARE OVERRIDABLE — the ones it
// passes to the child as command-line arguments (postgres -p, nats -p/-m, vm
// -httpListenAddr). Honouring a knob for a child whose listen port lives in an
// EMBEDDED CONFIG (tempo/loki/grafana) would move only that child's readiness
// PROBE: the child would bind the config's port while the gate waited on the
// knob's, so the gate could never pass and the supervisor would report a healthy
// service as never-ready. A knob that breaks readiness is worse than a knob that
// does not exist, so those three keep their config's port in BOTH residencies
// (which is a no-op, since the defaults ARE the config's ports); moving them
// per-instance needs the config templated, which is a flagged follow-on.
func servicePortsFor(residency string) servicePorts {
	p := servicePortDefaults()
	if residency != residencyHost {
		return p
	}
	overrides := []struct {
		env string
		set *int
	}{
		{"ORCHICON_POSTGRES_PORT", &p.postgres},
		{"ORCHICON_NATS_PORT", &p.nats},
		{"ORCHICON_NATS_MONITOR_PORT", &p.natsMonitor},
		{"ORCHICON_VM_PORT", &p.vm},
	}
	for _, o := range overrides {
		if v := os.Getenv(o.env); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*o.set = n
			}
		}
	}
	return p
}

// defaultDataDirFor is where a residency keeps its state when ORCHICON_DATA_DIR
// is not set. Container residency keeps the image's /var/lib/orchicon; host
// residency lives under the INVOKING USER's own home, because running as that
// user is the whole point of host residency.
func defaultDataDirFor(residency string) string {
	if residency != residencyHost {
		return "/var/lib/orchicon"
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".orchicon")
	}
	return filepath.Join(home, ".local", "share", "orchicon")
}
