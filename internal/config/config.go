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
// OIDC is the base authentication path in every mode: the embedded
// OpenID Provider (EmbeddedOP, default on) is the default IdP in
// local/non-prod; production validates ID tokens from the configured
// external issuer. Issuer="local" is the vestigial "no external IdP"
// marker — it is NOT an issuer and only boots when the embedded OP is
// enabled (Validate enforces this in every mode).
type AuthConfig struct {
	Issuer        string // OIDC issuer URL; "local" = no external IdP (embedded OP is the issuer)
	ClientID      string // OIDC client id
	ClientSecret  string // OIDC client secret (only for confidential flows)
	RedirectURL   string // OIDC redirect URL (e.g. http://localhost:5173/auth/callback)
	SigningKey    string // HMAC key for minting/verifying Orchicon access tokens
	AccessTTL     time.Duration
	RefreshTTL    time.Duration
	// DevLoginAllowed enables the synthetic /auth/dev-login endpoint
	// (local mode only). Default false: it is a dev escape hatch, not
	// the default path — a fresh plane authenticates through the
	// embedded OP (or an external IdP), and the local-mode first-admin
	// bootstrap seed (internal/auth/bootstrap.go) makes that usable.
	DevLoginAllowed bool

	// EmbeddedOP enables the in-process OpenID Provider (zitadel/oidc v3,
	// pkg/op) mounted on the plane origin. Defaults to true; a strict
	// production plane that only trusts its BYO IdP can disable the
	// surface entirely.
	EmbeddedOP bool
	// OPRedirectURIs are the registered redirect URIs for the OP's
	// built-in public client, comma-separated. Empty derives the default:
	// the auth RedirectURL plus the plane-origin /auth/callback when
	// OPIssuer is pinned.
	OPRedirectURIs string
	// OPIssuer pins the OP issuer URL (ORCHICON_OP_ISSUER). Empty derives
	// the issuer from the request Host on every request.
	OPIssuer string
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
	OTelEndpoint string
	// Telemetry selects the OTel pipeline mode. "none" skips telemetry
	// entirely (no exporters, no OTLP dial) — used by the runtime-container
	// sandbox plane, which has no Grafana stack. Empty/"embedded"/"remote"
	// keep the default pipeline (the same env the single-container
	// supervisor reads for the embedded stack decision).
	Telemetry string
	// Grafana stack backends (docs/08 §5). GrafanaURL is the Grafana UI
	// root (proxied same-origin under /grafana); TempoURL/LokiURL/VMURL
	// are the query endpoints the control plane reads tenant-scoped
	// telemetry from.
	GrafanaURL    string
	TempoURL      string
	LokiURL       string
	VMURL         string
	BlobStoreDir  string
	MigrateOnBoot bool

	// RuntimeSocket is the unix socket of the host-side workflow runtime
	// daemon. The daemon's socket lives inside a bind-mounted DIRECTORY
	// (/var/run/orchicon-runtime, mounted from the daemon's socket dir on
	// the host) so a daemon restart — which recreates the socket file —
	// never staleness the container's mount. Empty/unreachable degrades
	// the control plane to in-process execution (no per-workflow runtime
	// containers).
	RuntimeSocket string

	// Instance identifies this control plane ("dev"/"prod"). It labels
	// the runtime containers this plane creates and scopes its orphan
	// reaping, so two instances sharing one runtime daemon do not reap
	// each other's containers.
	Instance string

	Mode       DeploymentMode
	Auth       AuthConfig
	BlobStore  BlobStoreConfig

	ReadHeaderTimeout time.Duration
	ShutdownTimeout   time.Duration

	// IndexCheckInterval controls how often the control plane runs the
	// amcheck-based index-integrity sweep (boot + every interval). A
	// corrupted btree index silently hides rows from `=` lookups (the
	// planner uses the index) while seq scans still see them — the
	// field incident where a hard host sleep corrupted
	// worker_versions_worker_version_idx and the workflow reconciler
	// failed to load a worker that existed. 0 disables the periodic
	// sweep (the boot check still runs).
	IndexCheckInterval time.Duration
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
		Telemetry:         env("ORCHICON_TELEMETRY", ""),
		GrafanaURL:        env("ORCHICON_GRAFANA_URL", "http://localhost:3002"),
		TempoURL:          env("ORCHICON_TEMPO_URL", "http://localhost:3200"),
		LokiURL:           env("ORCHICON_LOKI_URL", "http://localhost:3100"),
		VMURL:             env("ORCHICON_VM_URL", "http://localhost:8428"),
		BlobStoreDir:      env("ORCHICON_BLOB_DIR", "./data/blobs"),
		MigrateOnBoot:     envBool("ORCHICON_MIGRATE_ON_BOOT", true),
		RuntimeSocket:     env("ORCHICON_RUNTIME_SOCKET", "/var/run/orchicon-runtime/runtime.sock"),
		Instance:          env("ORCHICON_INSTANCE", "dev"),
		Mode:              DeploymentMode(env("ORCHICON_MODE", "local")),
		Auth: AuthConfig{
			Issuer:          env("ORCHICON_OIDC_ISSUER", "local"),
			ClientID:        env("ORCHICON_OIDC_CLIENT_ID", "orchicon"),
			ClientSecret:    env("ORCHICON_OIDC_CLIENT_SECRET", ""),
			RedirectURL:     env("ORCHICON_OIDC_REDIRECT_URL", "http://localhost:5173/auth/callback"),
			SigningKey:      env("ORCHICON_AUTH_SIGNING_KEY", "orchicon-dev-signing-key-change-in-production"),
			AccessTTL:       15 * time.Minute,
			RefreshTTL:      24 * time.Hour,
			DevLoginAllowed: envBool("ORCHICON_DEV_LOGIN", false),
			EmbeddedOP:      envBool("ORCHICON_OP_ENABLED", true),
			OPRedirectURIs:  env("ORCHICON_OP_REDIRECT_URIS", ""),
			OPIssuer:        env("ORCHICON_OP_ISSUER", ""),
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
		IndexCheckInterval: envDuration("ORCHICON_INDEX_CHECK_INTERVAL", 6*time.Hour),
	}
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
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
// Every mode requires a real IdP (the embedded OP or an external issuer);
// production additionally requires a real external issuer and signing key
// (BYO-IdP posture), which local mode relaxes for the dev experience.
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
	// OIDC is the base auth path in every mode: a plane must have a real
	// IdP — the embedded OP or an external issuer. Issuer="local" is the
	// vestigial "no external IdP" marker, not an issuer; with the embedded
	// OP disabled it would leave the plane with no way to authenticate
	// (the old local-mode anonymous bypass is gone), so it is a config
	// error, not a runtime surprise.
	if !c.Auth.EmbeddedOP && (c.Auth.Issuer == "" || c.Auth.Issuer == "local") {
		return fmt.Errorf("config: an OIDC issuer is required in every mode: enable the embedded OP (ORCHICON_OP_ENABLED) or set ORCHICON_OIDC_ISSUER to an external IdP")
	}
	if c.Mode == ModeProduction {
		// Production keeps its stricter BYO-IdP posture: a real external
		// issuer is required even when the embedded OP is enabled — the
		// embedded OP alone is not a production trust surface.
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
