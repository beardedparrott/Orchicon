// Package config loads Orchicon control-plane configuration from the
// environment. v0.1 keeps configuration environment-driven; a typed
// config struct is the single source of truth for the running process.
//
// See docs/01_Architecture_Vision.md §2 for the technology direction.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds all control-plane runtime configuration.
type Config struct {
	HTTPAddr      string
	GRPCAddr      string
	PostgresDSN  string
	NATSURL       string
	OTelEndpoint  string
	SigNozURL     string // SigNoz query-service root (UI + API) — docs/08 §5
	BlobStoreDir  string
	MigrateOnBoot bool

	ReadHeaderTimeout time.Duration
	ShutdownTimeout   time.Duration
}

// Default returns a Config populated with local-dev defaults that match
// the docker-compose stack in deploy/compose.
func Default() Config {
	return Config{
		HTTPAddr:          env("ORCHICON_HTTP_ADDR", ":8080"),
		GRPCAddr:          env("ORCHICON_GRPC_ADDR", ":9090"),
		PostgresDSN:       env("ORCHICON_POSTGRES_DSN", "postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable"),
		NATSURL:           env("ORCHICON_NATS_URL", "nats://localhost:4222"),
		OTelEndpoint:      env("ORCHICON_OTEL_ENDPOINT", "localhost:4317"),
		SigNozURL:         env("ORCHICON_SIGNOZ_URL", "http://localhost:3301"),
		BlobStoreDir:      env("ORCHICON_BLOB_DIR", "./data/blobs"),
		MigrateOnBoot:     envBool("ORCHICON_MIGRATE_ON_BOOT", true),
		ReadHeaderTimeout: 10 * time.Second,
		ShutdownTimeout:   15 * time.Second,
	}
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

// Validate reports configuration errors before the process starts serving.
func (c Config) Validate() error {
	if c.HTTPAddr == "" {
		return fmt.Errorf("config: HTTPAddr must be set")
	}
	if c.PostgresDSN == "" {
		return fmt.Errorf("config: PostgresDSN must be set")
	}
	if c.NATSURL == "" {
		return fmt.Errorf("config: NATSURL must be set")
	}
	return nil
}
