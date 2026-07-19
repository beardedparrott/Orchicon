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

// DeploymentMode selects how the control plane validates its runtime
// environment. "local" is the fully-local mode (no cloud dependencies);
// "production" requires an OIDC issuer and external object storage.
type DeploymentMode string

const (
	ModeLocal      DeploymentMode = "local"
	ModeProduction DeploymentMode = "production"
)

// AuthConfig holds OIDC + token-issuance configuration (docs/07 §6.1).
// In local mode, Issuer="local" enables the built-in dev IdP that mints
// short-lived HS256 access tokens + refresh tokens (no external IdP
// required). In production, the control plane validates OIDC ID tokens
// from the configured issuer and issues its own access tokens.
type AuthConfig struct {
	Issuer        string // OIDC issuer URL, or "local" for the dev IdP
	ClientID      string // OIDC client id
	ClientSecret  string // OIDC client secret (only for confidential flows)
	RedirectURL   string // OIDC redirect URL (e.g. http://localhost:5173/auth/callback)
	SigningKey    string // HMAC key for minting/verifying Orchicon access tokens
	AccessTTL     time.Duration
	RefreshTTL    time.Duration
	DevLoginAllowed bool // local mode: allow the synthetic /auth/dev-login endpoint
}

// BlobStoreConfig selects the object-storage backend (docs/01 §2).
// "local" uses the filesystem (production-viable); "s3" uses S3-compatible
// storage.
type BlobStoreConfig struct {
	Kind     string // "local" | "s3"
	LocalDir string
	S3Bucket string
	S3Region string
	S3Endpoint string // empty for AWS; set for MinIO/other S3-compatible
}

// Config holds all control-plane runtime configuration.
type Config struct {
	HTTPAddr      string
	GRPCAddr      string
	PostgresDSN  string
	NATSURL       string
	OTelEndpoint  string
	SigNozURL       string // SigNoz query-service root (UI + API) — docs/08 §5
	ClickHouseDSN   string // ClickHouse HTTP DSN (user:pass@host:port) for direct queries
	BlobStoreDir    string
	MigrateOnBoot bool

	Mode       DeploymentMode
	Auth       AuthConfig
	BlobStore  BlobStoreConfig

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
		ClickHouseDSN:     env("ORCHICON_CLICKHOUSE_DSN", "http://signoz:signoz@localhost:8123"),
		BlobStoreDir:      env("ORCHICON_BLOB_DIR", "./data/blobs"),
		MigrateOnBoot:     envBool("ORCHICON_MIGRATE_ON_BOOT", true),
		Mode:              DeploymentMode(env("ORCHICON_MODE", "local")),
		Auth: AuthConfig{
			Issuer:          env("ORCHICON_OIDC_ISSUER", "local"),
			ClientID:        env("ORCHICON_OIDC_CLIENT_ID", "orchicon"),
			ClientSecret:    env("ORCHICON_OIDC_CLIENT_SECRET", ""),
			RedirectURL:     env("ORCHICON_OIDC_REDIRECT_URL", "http://localhost:5173/auth/callback"),
			SigningKey:      env("ORCHICON_AUTH_SIGNING_KEY", "orchicon-dev-signing-key-change-in-production"),
			AccessTTL:       15 * time.Minute,
			RefreshTTL:      24 * time.Hour,
			DevLoginAllowed: envBool("ORCHICON_DEV_LOGIN", true),
		},
		BlobStore: BlobStoreConfig{
			Kind:     env("ORCHICON_BLOB_STORE", "local"),
			LocalDir: env("ORCHICON_BLOB_DIR", "./data/blobs"),
			S3Bucket: env("ORCHICON_S3_BUCKET", ""),
			S3Region: env("ORCHICON_S3_REGION", ""),
			S3Endpoint: env("ORCHICON_S3_ENDPOINT", ""),
		},
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
// Production mode requires an OIDC issuer (not "local") and a real
// signing key; local mode relaxes these for the dev experience.
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
	switch c.Mode {
	case ModeLocal, ModeProduction:
	default:
		return fmt.Errorf("config: Mode must be %q or %q", ModeLocal, ModeProduction)
	}
	if c.Mode == ModeProduction {
		if c.Auth.Issuer == "" || c.Auth.Issuer == "local" {
			return fmt.Errorf("config: production mode requires ORCHICON_OIDC_ISSUER (not local)")
		}
		if c.Auth.SigningKey == "" || c.Auth.SigningKey == "orchicon-dev-signing-key-change-in-production" {
			return fmt.Errorf("config: production mode requires a real ORCHICON_AUTH_SIGNING_KEY")
		}
		if c.Auth.ClientID == "" {
			return fmt.Errorf("config: production mode requires ORCHICON_OIDC_CLIENT_ID")
		}
	}
	if c.Auth.SigningKey == "" {
		return fmt.Errorf("config: Auth.SigningKey must be set")
	}
	switch c.BlobStore.Kind {
	case "local", "s3":
	default:
		return fmt.Errorf("config: BlobStore.Kind must be \"local\" or \"s3\"")
	}
	if c.BlobStore.Kind == "s3" && c.BlobStore.S3Bucket == "" {
		return fmt.Errorf("config: s3 BlobStore requires ORCHICON_S3_BUCKET")
	}
	return nil
}
