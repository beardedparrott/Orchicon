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
		HTTPAddr:    ":8080",
		PostgresDSN: "postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable",
		NATSURL:     "nats://localhost:4222",
		Mode:        ModeLocal,
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

func TestDevLoginDefaultsToFalse(t *testing.T) {
	// Isolate the default from any ambient environment.
	t.Setenv("ORCHICON_DEV_LOGIN", "")
	c := Default()
	if c.Auth.DevLoginAllowed {
		t.Fatal("DevLoginAllowed defaults to true; want false (dev-login must be flag-gated off)")
	}
	if !c.Auth.EmbeddedOP {
		t.Fatal("EmbeddedOP defaults to false; want true (the embedded OP is the local default IdP)")
	}
	// The default config is still valid: the embedded OP satisfies the
	// all-modes issuer requirement.
	if err := c.Validate(); err != nil {
		t.Fatalf("Default() does not pass Validate(): %v", err)
	}
}
