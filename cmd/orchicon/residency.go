package main

// Service shape: WHERE THE SERVICES RUN.

import (
	"os"
	"path/filepath"
	"strconv"
)

// servicePorts is the single place a supervised service's port is decided, so the child's BIND
// argument and its READINESS probe cannot drift: both read the same field.
type servicePorts struct {
	postgres    int
	nats        int
	natsMonitor int
	tempo       int
	loki        int
	vm          int

	// tempoGRPC is Tempo's OWN gRPC listener (its querier/frontend talk over it; the collector does NOT
	// — it uses the OTLP receivers above).
	tempoGRPC int

	// The telemetry children's port fields below are NOT in argv: each lives in an embedded config
	// (tempo.yaml, loki.yaml, otel-collector.yaml) that writeConfigs ships with @TOKEN@ placeholders.
	tempoOTLPGRPC     int
	tempoOTLPHTTP     int
	collectorOTLPGRPC int
	collectorOTLPHTTP int
	collectorHealth   int

	// collectorSelfMetrics is the collector's OWN Prometheus exporter — the port it serves its internal
	// telemetry on (otelcol_* receivers/exporters/queue stats).
	collectorSelfMetrics int
}

// servicePortDefaults are the literals every install binds and probes when the operator sets no
// knob.
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

// servicePortsFromEnv returns the ports a supervisor binds and probes: servicePortDefaults,
// overridden by the operator's knobs.
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

// defaultDataDirFor is where an instance keeps its state when ORCHICON_DATA_DIR is not set: under
// the INVOKING USER's own home, because the services run as that user — which is the whole shape.
func defaultDataDirFor() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".orchicon")
	}
	return filepath.Join(home, ".local", "share", "orchicon")
}
