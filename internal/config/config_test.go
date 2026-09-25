package config

import (
	"strings"
	"testing"
	"time"
)

// baseConfig returns a valid local-mode config with the embedded OP on
// (the default). Tests override specific fields to exercise the rules.
func baseConfig() Config {
	return Config{
		HTTPAddr:           ":8080",
		PostgresDSN:        "postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable",
		NATSURL:            "nats://localhost:4222",
		Mode:               ModeLocal,
		DeploymentTenantID: "tnt_dev",
		Auth: AuthConfig{
			Issuer:      "local",
			ClientID:    "orchicon",
			RedirectURL: "http://localhost:5173/auth/callback",
			SigningKey:  "test-signing-key",
			AccessTTL:   15 * time.Minute,
			RefreshTTL:  24 * time.Hour,
			EmbeddedOP:  true,
		},
		BlobStore: BlobStoreConfig{Kind: "local"},
	}
}

func TestValidateAllModesRequireIssuer(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(c *Config)
		wantErr string
	}{
		{
			name:   "local embedded OP on, issuer local (default) passes",
			mutate: func(c *Config) {},
		},
		{
			name: "local embedded OP off, issuer local fails",
			mutate: func(c *Config) {
				c.Auth.EmbeddedOP = false
			},
			wantErr: "an OIDC issuer is required in every mode",
		},
		{
			name: "local embedded OP off, empty issuer fails",
			mutate: func(c *Config) {
				c.Auth.EmbeddedOP = false
				c.Auth.Issuer = ""
			},
			wantErr: "an OIDC issuer is required in every mode",
		},
		{
			name: "local embedded OP off, external issuer passes",
			mutate: func(c *Config) {
				c.Auth.EmbeddedOP = false
				c.Auth.Issuer = "https://sso.example.com"
			},
		},
		{
			name: "local embedded OP on, external issuer passes",
			mutate: func(c *Config) {
				c.Auth.Issuer = "https://sso.example.com"
			},
		},
		{
			name: "production embedded OP on, issuer local still fails",
			mutate: func(c *Config) {
				c.Mode = ModeProduction
			},
			wantErr: "production mode requires ORCHICON_OIDC_ISSUER",
		},
		{
			name: "production embedded OP off, issuer local fails",
			mutate: func(c *Config) {
				c.Mode = ModeProduction
				c.Auth.EmbeddedOP = false
			},
			wantErr: "an OIDC issuer is required in every mode",
		},
		{
			name: "production embedded OP on, external issuer + real key passes",
			mutate: func(c *Config) {
				c.Mode = ModeProduction
				c.Auth.Issuer = "https://sso.example.com"
				c.Auth.SigningKey = "a-real-strong-production-secret"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := baseConfig()
			tc.mutate(&c)
			err := c.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %q, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestDefaults(t *testing.T) {
	// Isolate the defaults from any ambient environment.
	t.Setenv("ORCHICON_DEPLOYMENT_TENANT_ID", "")
	c := Default()
	if !c.Auth.EmbeddedOP {
		t.Fatal("EmbeddedOP defaults to false; want true (the embedded OP is the local default IdP)")
	}
	if c.DeploymentTenantID != "tnt_dev" {
		t.Fatalf("DeploymentTenantID defaults to %q, want %q", c.DeploymentTenantID, "tnt_dev")
	}
	// The default config is still valid: the embedded OP satisfies the
	// all-modes issuer requirement.
	if err := c.Validate(); err != nil {
		t.Fatalf("Default() does not pass Validate(): %v", err)
	}
}

// TestDeploymentTenantIDEnvOverrides pins that ORCHICON_DEPLOYMENT_TENANT_ID
// overrides the default deployment tenant at load time.
func TestDeploymentTenantIDEnvOverrides(t *testing.T) {
	t.Setenv("ORCHICON_DEPLOYMENT_TENANT_ID", "acme")
	c := Default()
	if c.DeploymentTenantID != "acme" {
		t.Fatalf("DeploymentTenantID = %q, want %q", c.DeploymentTenantID, "acme")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("config with ORCHICON_DEPLOYMENT_TENANT_ID=acme does not validate: %v", err)
	}
}

// TestDeploymentTenantIDValidation pins the boot-time tenant-id guards: a
// misconfigured ORCHICON_DEPLOYMENT_TENANT_ID must fail boot, never
// silently seed a second tenant.
func TestDeploymentTenantIDValidation(t *testing.T) {
	valid := []string{"tnt_dev", "acme", "prod-2", "my_tenant", "a", "a-b_c"}
	for _, id := range valid {
		c := baseConfig()
		c.DeploymentTenantID = id
		if err := c.Validate(); err != nil {
			t.Errorf("DeploymentTenantID %q: Validate() = %v, want nil", id, err)
		}
	}
	invalid := []string{
		"",                      // empty
		"Acme",                  // uppercase
		"-acme",                 // leading separator
		"acme-",                 // trailing separator
		"a b",                   // space
		"acme!x",                // punctuation
		"a/b",                   // slash
		"a.b",                   // dot
		strings.Repeat("a", 64), // over-long
	}
	for _, id := range invalid {
		c := baseConfig()
		c.DeploymentTenantID = id
		if err := c.Validate(); err == nil {
			t.Errorf("DeploymentTenantID %q: Validate() = nil, want error", id)
		}
	}
}

// TestValidateExtraBind pins the second HTTP bind (ORCHICON_HTTP_EXTRA_BIND)
// rules. The load-bearing one is the wildcard rejection: a 0.0.0.0/:: bind
// would expose the plane to the operator's LAN, which was explicitly
// rejected. A bad value must fail closed at boot, not degrade quietly to
// loopback-only (that surfaces much later as "workers cannot reach the
// plane" from inside a runtime container).
func TestValidateExtraBind(t *testing.T) {
	valid := []string{
		"",                // unset: loopback/primary only
		"172.17.0.1:8091", // prod's bridge bind
		"172.17.0.1:8080", // dev's bridge bind
		"127.0.0.1:8091",  // loopback is a concrete IP, not a wildcard
		"10.42.0.1:1",     // any concrete bridge address
		"[fd00::1]:8091",  // IPv6 literal with host part
	}
	for _, a := range valid {
		c := baseConfig()
		c.ExtraBind = a
		if err := c.Validate(); err != nil {
			t.Errorf("ExtraBind %q: Validate() = %v, want nil", a, err)
		}
	}

	invalid := []string{
		"0.0.0.0:8091",              // wildcard: LAN exposure (rejected explicitly)
		":8091",                     // wildcard: no host part
		"[::]:8091",                 // wildcard v6
		"host.docker.internal:8091", // hostname: cannot be bound
		"172.17.0.1:0",              // port 0
		"172.17.0.1:99999",          // port out of range
		"172.17.0.1",                // no port at all
		"172.17.0.1:http",           // non-numeric port
	}
	for _, a := range invalid {
		c := baseConfig()
		c.ExtraBind = a
		if err := c.Validate(); err == nil {
			t.Errorf("ExtraBind %q: Validate() = nil, want error", a)
		}
	}
}

// TestDefaultExtraBindFromEnv pins the env wiring end to end (the launcher
// sets ORCHICON_HTTP_EXTRA_BIND per instance; Default() must read it).
func TestDefaultExtraBindFromEnv(t *testing.T) {
	t.Setenv("ORCHICON_HTTP_EXTRA_BIND", "172.17.0.1:8091")
	if got := Default().ExtraBind; got != "172.17.0.1:8091" {
		t.Fatalf("Default().ExtraBind = %q, want 172.17.0.1:8091", got)
	}
	t.Setenv("ORCHICON_HTTP_EXTRA_BIND", "")
	if got := Default().ExtraBind; got != "" {
		t.Fatalf("Default().ExtraBind = %q, want empty", got)
	}
}
