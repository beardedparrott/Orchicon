package runtime

// antigravity_auth_test.go — the tenant's Antigravity method and its GEMINI_API_KEY secret must
// actually reach the create request.
//
// WHY THIS CROSSES THE DATABASE. The method lives in tenant_settings and the key lives in
// tenant_secrets, both behind RLS and both resolved by the PLANE (the daemon that gates the create
// cannot read either). A unit test with a stubbed resolver would prove nothing about the two
// queries, the decryption, or the name the operator must create — which is exactly where this
// wiring can go wrong and look healthy.

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/antigravity/authmethod"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/secretcrypto"
)

func TestAntigravityAuthForResolvesMethodAndSecret(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed antigravity auth resolution test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := migrate.Run(ctx, pool, assets.MigrationsFS, assets.MigrationsDir); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	// UNIQUE PER RUN: a fixed tenant id makes concurrent test processes contend on the same rows.
	antigravityAuthProbeTenant := "tnt_antigravity_auth_" + db.NewID()[10:22]
	if err := db.SeedDevTenant(ctx, pool, antigravityAuthProbeTenant); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 1)
	}
	const keyValue = "gemini-key-from-the-tenant-secret"
	ciphertext, err := secretcrypto.Encrypt([]byte(keyValue), kek)
	if err != nil {
		t.Fatalf("encrypt secret: %v", err)
	}

	ttx, err := pool.BeginTenantTx(ctx, antigravityAuthProbeTenant)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := db.CreateSecret(ctx, ttx.Tx, db.SecretRow{
		ID: db.NewID(), TenantID: antigravityAuthProbeTenant,
		Name: authmethod.APIKeySecretName, Description: "antigravity test key",
		Ciphertext: ciphertext, KeyVersion: 1,
	}); err != nil {
		_ = ttx.Rollback(ctx)
		t.Fatalf("create secret: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		ptx, err := pool.BeginTenantTx(cctx, antigravityAuthProbeTenant)
		if err != nil {
			return
		}
		_, _ = ptx.Exec(cctx, `DELETE FROM tenant_secrets WHERE tenant_id = $1`, antigravityAuthProbeTenant)
		_, _ = ptx.Exec(cctx, `DELETE FROM tenant_settings WHERE tenant_id = $1`, antigravityAuthProbeTenant)
		_ = ptx.Commit(cctx)
		if _, err := pool.Exec(cctx, `DELETE FROM tenants WHERE id = $1`, antigravityAuthProbeTenant); err != nil {
			t.Logf("cleanup tenant: %v", err)
		}
	})

	l := &Lifecycle{
		pool:       pool,
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		secretsKEK: kek,
	}

	// Never-configured: the default method, and no key is read at all.
	method, key := l.antigravityAuthFor(ctx, antigravityAuthProbeTenant)
	if method != authmethod.MethodOAuthPersonal {
		t.Fatalf("fresh tenant method = %q, want oauth-personal", method)
	}
	if key != "" {
		t.Fatalf("fresh tenant returned a key (%d bytes); the default method is not API-key based", len(key))
	}

	// Select gemini-api-key: the tenant secret named GEMINI_API_KEY is now resolved and decrypted,
	// so the daemon's preflight can pass and the child can authenticate.
	stx, err := pool.BeginTenantTx(ctx, antigravityAuthProbeTenant)
	if err != nil {
		t.Fatalf("begin settings tx: %v", err)
	}
	if _, err := db.UpdateTenantSettings(ctx, stx.Tx, antigravityAuthProbeTenant, db.TenantSettingsRow{
		AntigravityAuthMethod:     authmethod.MethodGeminiAPIKey,
		AntigravityShareTelemetry: true,
	}); err != nil {
		_ = stx.Rollback(ctx)
		t.Fatalf("update settings: %v", err)
	}
	if err := stx.Commit(ctx); err != nil {
		t.Fatalf("commit settings: %v", err)
	}
	method, key = l.antigravityAuthFor(ctx, antigravityAuthProbeTenant)
	if method != authmethod.MethodGeminiAPIKey {
		t.Fatalf("method = %q, want gemini-api-key", method)
	}
	if key != keyValue {
		t.Fatalf("resolved key does not match the stored tenant secret")
	}

	// With no KEK the plane cannot decrypt, and the degraded answer must be an EMPTY key (the
	// daemon then refuses actionably) — never the ciphertext or a panic.
	noKEK := &Lifecycle{pool: pool, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if m, k := noKEK.antigravityAuthFor(ctx, antigravityAuthProbeTenant); m != authmethod.MethodGeminiAPIKey || k != "" {
		t.Fatalf("without a KEK got (%q,%q), want (gemini-api-key,\"\")", m, k)
	}
}
