package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/beardedparrott/orchicon/internal/config"
)

// applyProdDefaults overrides config fields with prod-instance defaults
// when the binary is named orchicon-prod. This lets the user run
//
//	orchicon-prod start
//	orchicon-prod serve
//
// without needing any env vars — the binary detects its own name and
// uses the correct ports and DSNs for the prod stack. When the
// corresponding env var IS set to a non-empty value, it takes precedence.
//
// The empty-string check matches config.env() behaviour: setting
// ORCHICON_NATS_URL="" in the env should fall through to the default,
// not override with an empty DSN.
func applyProdDefaults(cfg *config.Config) {
	bin := filepath.Base(os.Args[0])
	if !strings.Contains(bin, "orchicon-prod") {
		return
	}
	if v, ok := os.LookupEnv("ORCHICON_HTTP_ADDR"); !ok || v == "" {
		cfg.HTTPAddr = ":8091"
	}
	if v, ok := os.LookupEnv("ORCHICON_POSTGRES_DSN"); !ok || v == "" {
		cfg.PostgresDSN = "postgres://orchicon:orchicon@localhost:5433/orchicon?sslmode=disable"
	}
	if v, ok := os.LookupEnv("ORCHICON_NATS_URL"); !ok || v == "" {
		cfg.NATSURL = "nats://localhost:4223"
	}
	if v, ok := os.LookupEnv("ORCHICON_OTEL_ENDPOINT"); !ok || v == "" {
		cfg.OTelEndpoint = "localhost:4319"
	}
	if v, ok := os.LookupEnv("ORCHICON_GRAFANA_URL"); !ok || v == "" {
		cfg.GrafanaURL = "http://localhost:3003"
	}
	if v, ok := os.LookupEnv("ORCHICON_TEMPO_URL"); !ok || v == "" {
		cfg.TempoURL = "http://localhost:3201"
	}
	if v, ok := os.LookupEnv("ORCHICON_LOKI_URL"); !ok || v == "" {
		cfg.LokiURL = "http://localhost:3101"
	}
	if v, ok := os.LookupEnv("ORCHICON_VM_URL"); !ok || v == "" {
		cfg.VMURL = "http://localhost:8429"
	}
	if v, ok := os.LookupEnv("ORCHICON_BLOB_DIR"); !ok || v == "" {
		cfg.BlobStoreDir = "./data/prod-blobs"
	}
}
