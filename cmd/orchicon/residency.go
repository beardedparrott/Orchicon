package main

// Service shape: WHERE THE SERVICES RUN.
//
// THERE IS EXACTLY ONE SHAPE. The plane and its services are HOST PROCESSES,
// always: postgres, nats and the telemetry children are direct processes of the
// invoking user, launched without a container engine, without setpriv and
// without a chown. A container exists for exactly ONE purpose — a workflow's
// runtime container, opt-in per project — and never as a carrier for an
// instance.
//
// THAT IS A SEPARATE AXIS FROM plane residency (ORCHICON_PLANE_RESIDENCY), which
// says WHERE THE PLANE runs. That knob still names "container" so `orchicon
// install` can REFUSE it by name (install.go) rather than silently
// reinterpreting it; it can no longer select anything.

import (
	"os"
	"path/filepath"
	"strconv"
)

// servicePorts is the single place a supervised service's port is decided, so
// the child's BIND argument and its READINESS probe cannot drift: both read the
// same field. The operator's ORCHICON_*_PORT knobs override the defaults (the
// same names install_ports.go honours, so an install needs no second
// vocabulary).
type servicePorts struct {
	postgres    int
	nats        int
	natsMonitor int
	tempo       int
	loki        int
	vm          int

	// tempoGRPC is Tempo's OWN gRPC listener (its querier/frontend talk over it;
	// the collector does NOT — it uses the OTLP receivers above). It is NOT an
	// HTTP port and nothing outside the process dials it, which is exactly why
	// leaving it at Tempo's default was invisible until a second instance tried
	// to start: two instances cannot both own :9095, so the second one died on
	// `bind: address already in use` and its Traces view reported the backend
	// unreachable for ever. Templated (see tempo.yaml) so the child binds what
	// this table says.
	tempoGRPC int

	// The telemetry children's port fields below are NOT in argv: each lives in
	// an embedded config (tempo.yaml, loki.yaml, otel-collector.yaml) that
	// writeConfigs ships with @TOKEN@ placeholders. Binding and probing read the
	// SAME field here, so a knob that moves one moves the other — the config is
	// templated from this table (see writeConfigs).
	//
	// TEMPO'S OTLP RECEIVERS ARE OFFSET FROM THE COLLECTOR'S ON PURPOSE: the two
	// used to share one network namespace in the retired single-container image,
	// so 14317/14318 keep the collector's own 4317/4318 free. They no longer
	// share a namespace, but the collector's config points at the current ports —
	// changing the offset is a coordinated edit across tempo.yaml,
	// otel-collector.yaml and the readiness probes, not a cleanup.
	// TestCollectorPointsAtTempoOffset pins it.
	tempoOTLPGRPC     int
	tempoOTLPHTTP     int
	collectorOTLPGRPC int
	collectorOTLPHTTP int
	collectorHealth   int

	// collectorSelfMetrics is the collector's OWN Prometheus exporter — the port
	// it serves its internal telemetry on (otelcol_* receivers/exporters/queue
	// stats). It is the THIRD instance of the same bug the two above carry: the
	// collector DEFAULTS this to localhost:8888, so nothing ever assigned it and
	// two instances demanded the same socket. Measured on this host: prod holding
	// 127.0.0.1:8888 and dev's collector crash-looping for ever on
	// "binding address localhost:8888 for Prometheus exporter: bind: address
	// already in use" — 16 restarts — which also meant dev shipped NO OTLP at
	// all, so its Metrics view showed only the plane's own outbox gauges while
	// tokens/cost/executions were emitted into nothing.
	//
	// Templated into otel-collector.yaml's service.telemetry.metrics.address and
	// resolved through internalListenerPort, so it walks off an occupied port the
	// same way Tempo's gRPC listener does.
	collectorSelfMetrics int
}

// servicePortDefaults are the literals every install binds and probes when the
// operator sets no knob. The telemetry children's entries are the ports their
// EMBEDDED CONFIG files declare (tempo.yaml http_listen_port, loki.yaml
// http_listen_port); they are recorded here as the single
// source for that child's readiness probe, and
// TestServiceProbesMatchEmbeddedConfigPorts pins them to the configs so the two
// cannot drift.
func servicePortDefaults() servicePorts {
	return servicePorts{
		postgres:          5432,
		nats:              4222,
		natsMonitor:       8222,
		tempo:             3200,
		loki:              3100,
		vm:                8428,
		tempoGRPC:         9095, // Tempo's own default
		tempoOTLPGRPC:     14317,
		tempoOTLPHTTP:     14318,
		collectorOTLPGRPC: 4317,
		collectorOTLPHTTP: 4318,
		collectorHealth:   13133,
		// The collector's own default (localhost:8888) — recorded here so it can
		// be overridden and walked off a collision, not because we chose it.
		collectorSelfMetrics: 8888,
	}
}

// servicePortsFromEnv returns the ports a supervisor binds and probes:
// servicePortDefaults, overridden by the operator's knobs.
//
// The supervisor's ports ARE host ports, so two instances on one host must not
// collide — that is what makes every port here overridable.
//
// EVERY PORT IN THIS TABLE IS OVERRIDABLE, INCLUDING THE ONES THAT LIVE IN AN
// EMBEDDED CONFIG — because those configs are TEMPLATED from this same table.
// writeConfigs substitutes these fields into tempo.yaml, loki.yaml and
// otel-collector.yaml, so the child BINDS the knob's port and the gate
// PROBES the same number.
func servicePortsFromEnv() servicePorts {
	p := servicePortDefaults()
	overrides := []struct {
		env string
		set *int
	}{
		{"ORCHICON_POSTGRES_PORT", &p.postgres},
		{"ORCHICON_NATS_PORT", &p.nats},
		{"ORCHICON_NATS_MONITOR_PORT", &p.natsMonitor},
		{"ORCHICON_VM_PORT", &p.vm},
		{"ORCHICON_TEMPO_PORT", &p.tempo},
		{"ORCHICON_TEMPO_GRPC_PORT", &p.tempoGRPC},
		{"ORCHICON_LOKI_PORT", &p.loki},
		{"ORCHICON_OTLP_GRPC_PORT", &p.collectorOTLPGRPC},
		{"ORCHICON_OTLP_HTTP_PORT", &p.collectorOTLPHTTP},
		{"ORCHICON_TEMPO_OTLP_GRPC_PORT", &p.tempoOTLPGRPC},
		{"ORCHICON_TEMPO_OTLP_HTTP_PORT", &p.tempoOTLPHTTP},
		{"ORCHICON_OTEL_HEALTH_PORT", &p.collectorHealth},
		{"ORCHICON_OTEL_SELF_METRICS_PORT", &p.collectorSelfMetrics},
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

// defaultDataDirFor is where an instance keeps its state when ORCHICON_DATA_DIR
// is not set: under the INVOKING USER's own home, because the services run as
// that user — which is the whole shape.
func defaultDataDirFor() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".orchicon")
	}
	return filepath.Join(home, ".local", "share", "orchicon")
}
