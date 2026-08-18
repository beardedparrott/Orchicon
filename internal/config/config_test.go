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
