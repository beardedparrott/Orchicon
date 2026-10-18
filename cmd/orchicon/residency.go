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

	// The telemetry children's port fields below are NOT in argv: each lives in
	// an embedded config (tempo.yaml, loki.yaml, otel-collector.yaml) that
	// writeConfigs ships with @TOKEN@ placeholders. Binding and probing read the
	// SAME field here, so a knob that moves one moves the other — the config is
	// templated from this table (see writeConfigs).
	//
	// TEMPO'S OTLP RECEIVERS ARE OFFSET FROM THE COLLECTOR'S ON PURPOSE
	// (deploy/container/Dockerfile header comment): the two used to share one
	// network namespace, so 14317/14318 keep the collector's own 4317/4318
	// free. On a host they no longer share a namespace, but the collector's
	// config points at the current ports — changing the offset is a coordinated
	// edit across tempo.yaml, otel-collector.yaml and the readiness probes, not
	// a cleanup. TestCollectorPointsAtTempoOffset pins it.
	tempoOTLPGRPC     int
	tempoOTLPHTTP     int
	collectorOTLPGRPC int
	collectorOTLPHTTP int
	collectorHealth   int
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
// asserted by the test suite. NEITHER DOES A PLATFORM WITHOUT THE BINARY: `setprivSupported` is a
// compile-time constant (setpriv_unix.go / setpriv_windows.go), checked BEFORE the residency branch so a
// Windows supervisor can never emit `setpriv --reuid=70` into a command line that has no such program —
// and so the absence of the drop is a stated property rather than a silently dropped argv.
func (s *supervisor) asUser(uid, gid int, command string, args []string) (string, []string) {
	if !setprivSupported {
		return command, args
	}
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
// files declare (tempo.yaml http_listen_port, loki.yaml http_listen_port);
// they are recorded here as the single source for that
// child's readiness probe, and TestServiceProbesMatchTheirBind pins them to the
// configs so the two cannot drift.
func servicePortDefaults() servicePorts {
	return servicePorts{
		postgres:          5432,
		nats:              4222,
		natsMonitor:       8222,
		tempo:             3200,
		loki:              3100,
		vm:                8428,
		tempoOTLPGRPC:     14317,
		tempoOTLPHTTP:     14318,
		collectorOTLPGRPC: 4317,
		collectorOTLPHTTP: 4318,
		collectorHealth:   13133,
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
// EVERY PORT IN THIS TABLE IS OVERRIDABLE IN HOST RESIDENCY, INCLUDING THE ONES
// THAT LIVE IN AN EMBEDDED CONFIG — because those configs are now TEMPLATED from
// this same table. The earlier limitation ("honouring a knob for a child whose
// listen port lives in an embedded config would move only that child's readiness
// PROBE") no longer exists: writeConfigs substitutes these fields into tempo.yaml,
// loki.yaml and otel-collector.yaml, so the child BINDS the knob's
// port and the gate PROBES the same number. In CONTAINER residency nothing is
// overridable — the container's ports are its own private namespace and the host
// mapping is done by -p in the launcher.
//
// The env names match install_ports.go / scripts/container.sh, so a host-resident
// install needs no second vocabulary.
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
		{"ORCHICON_TEMPO_PORT", &p.tempo},
		{"ORCHICON_LOKI_PORT", &p.loki},
		{"ORCHICON_OTLP_GRPC_PORT", &p.collectorOTLPGRPC},
		{"ORCHICON_OTLP_HTTP_PORT", &p.collectorOTLPHTTP},
		{"ORCHICON_TEMPO_OTLP_GRPC_PORT", &p.tempoOTLPGRPC},
		{"ORCHICON_TEMPO_OTLP_HTTP_PORT", &p.tempoOTLPHTTP},
		{"ORCHICON_OTEL_HEALTH_PORT", &p.collectorHealth},
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
		// PER-OS (platform_paths_unix.go / _windows.go): `/var/lib/orchicon` is the container contract on
		// Unix and an unreachable drive-root path on Windows.
		return platformContainerDataDir()
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".orchicon")
	}
	return filepath.Join(home, ".local", "share", "orchicon")
}
