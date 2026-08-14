package auth

import (
	"context"
	"log/slog"
	"os"
	"testing"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
)

// testBootstrapEnv opens a migrated test DB with the dev tenant seeded.
func testBootstrapEnv(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed bootstrap test")
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
	if err := db.SeedDevTenant(ctx, pool); err != nil {
		t.Fatalf("seed dev tenant: %v", err)
	}
	return pool
}

func bootstrapConfig(username, password, seed string) config.Config {
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.Auth.SigningKey = "test-bootstrap-signing-key"
	cfg.Auth.EmbeddedOP = true
	return cfg
}

func cleanupBootstrap(ctx context.Context, pool *db.Pool, subject string) {
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		return
	}
	defer ttx.Rollback(ctx)
	// role_bindings has no FK on identity_id, so the binding must be
	// removed explicitly before the identity (the local_credentials row
	// cascades via its identity_id FK).
	_, _ = ttx.Exec(ctx, `DELETE FROM role_bindings WHERE identity_id IN (SELECT id FROM identities WHERE subject = $1)`, subject)
	_, _ = ttx.Exec(ctx, `DELETE FROM identities WHERE subject = $1`, subject)
	// Drop the seed-created admin role only when no bindings reference it
	// (keeps a human-provisioned admin role across tests intact).
	_, _ = ttx.Exec(ctx, `DELETE FROM roles r WHERE r.name = 'admin'
		AND NOT EXISTS (SELECT 1 FROM role_bindings b WHERE b.role_id = r.id)`)
	_ = ttx.Commit(ctx)
}

// TestBootstrapLocalAdminSeeds verifies a fresh local plane gets a usable
// first admin: identity + admin role binding + local credential, and that
// the seeded credential actually logs in through the embedded-OP path.
func TestBootstrapLocalAdminSeeds(t *testing.T) {
	pool := testBootstrapEnv(t)
	t.Setenv(localAdminUsernameEnv, "seed-admin")
	t.Setenv(localAdminPasswordEnv, "seed-password-123")
	t.Setenv(localAdminSeedEnv, "")
	ctx := context.Background()
	cfg := bootstrapConfig("", "", "")
	if err := BootstrapLocalAdmin(ctx, pool, slog.New(slog.DiscardHandler), cfg); err != nil {
		t.Fatalf("BootstrapLocalAdmin: %v", err)
	}
	t.Cleanup(func() { cleanupBootstrap(ctx, pool, "seed-admin") })

	// An admin now exists.
	exists, err := tenantAdminExists(ctx, pool, "tnt_dev")
	if err != nil {
		t.Fatalf("tenantAdminExists: %v", err)
	}
	if !exists {
		t.Fatal("no admin after bootstrap")
	}
	// The credential row exists and its hash verifies against the pinned
	// password (never plaintext).
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	row, err := db.GetLocalCredentialByUsername(ctx, ttx.Tx, "tnt_dev", "seed-admin")
	if err != nil {
		t.Fatalf("GetLocalCredentialByUsername: %v", err)
	}
	_ = ttx.Rollback(ctx)
	valid, err := VerifyPassword("seed-password-123", row.PasswordHash)
	if err != nil || !valid {
		t.Fatalf("seeded password does not verify (err=%v)", err)
	}
	if containsPlaintext(row.PasswordHash, "seed-password-123") {
		t.Fatal("plaintext leaked into the stored hash")
	}
}

// TestBootstrapLocalAdminIdempotent pins the seed-once behavior: a second
// boot does not clobber the credential (the hash stays identical to the
// first seed).
func TestBootstrapLocalAdminIdempotent(t *testing.T) {
	pool := testBootstrapEnv(t)
	t.Setenv(localAdminUsernameEnv, "idem-admin")
	t.Setenv(localAdminPasswordEnv, "idem-password")
	t.Setenv(localAdminSeedEnv, "")
	ctx := context.Background()
	cfg := bootstrapConfig("", "", "")
	log := slog.New(slog.DiscardHandler)
	if err := BootstrapLocalAdmin(ctx, pool, log, cfg); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	t.Cleanup(func() { cleanupBootstrap(ctx, pool, "idem-admin") })

	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	first, err := db.GetLocalCredentialByUsername(ctx, ttx.Tx, "tnt_dev", "idem-admin")
	_ = ttx.Rollback(ctx)
	if err != nil {
		t.Fatalf("first credential: %v", err)
	}

	if err := BootstrapLocalAdmin(ctx, pool, log, cfg); err != nil {
		t.Fatalf("second seed: %v", err)
	}
	ttx2, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	second, err := db.GetLocalCredentialByUsername(ctx, ttx2.Tx, "tnt_dev", "idem-admin")
	_ = ttx2.Rollback(ctx)
	if err != nil {
		t.Fatalf("second credential: %v", err)
	}
	if second.PasswordHash != first.PasswordHash {
		t.Fatal("bootstrap re-ran and clobbered the credential hash; want seed-once")
	}
	if second.ID != first.ID {
		t.Fatal("bootstrap re-ran and created a new credential row; want seed-once")
	}
}

// TestBootstrapLocalAdminNoClobber pins the don't-clobber guard: when a
// human already provisioned an admin, the seed never touches the DB.
func TestBootstrapLocalAdminNoClobber(t *testing.T) {
	pool := testBootstrapEnv(t)
	t.Setenv(localAdminUsernameEnv, "seed-admin")
	t.Setenv(localAdminPasswordEnv, "seed-password")
	t.Setenv(localAdminSeedEnv, "")
	ctx := context.Background()

	// Provision a DIFFERENT admin the way a human would.
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	ident, _, err := db.GetOrCreateIdentity(ctx, ttx.Tx, "tnt_dev", "human-admin@orchicon.local", "Human Admin", "user")
	if err != nil {
		t.Fatalf("ensure identity: %v", err)
	}
	if _, err := db.CreateRole(ctx, ttx.Tx, db.RoleRow{TenantID: "tnt_dev", Name: "admin", Scope: "tenant", Entitlements: []string{"*"}}); err != nil {
		t.Fatalf("create role: %v", err)
	}
	roles, err := db.ListRoles(ctx, ttx.Tx, "tnt_dev", 1000, "")
	if err != nil {
		t.Fatalf("list roles: %v", err)
	}
	var adminRoleID string
	for _, rl := range roles {
		if rl.Name == "admin" {
			adminRoleID = rl.ID
		}
	}
	if _, err := db.CreateRoleBinding(ctx, ttx.Tx, db.RoleBindingRow{TenantID: "tnt_dev", IdentityID: ident.ID, RoleID: adminRoleID, Scope: "tenant"}); err != nil {
		t.Fatalf("create binding: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	t.Cleanup(func() { cleanupBootstrap(ctx, pool, "human-admin@orchicon.local") })

	cfg := bootstrapConfig("", "", "")
	if err := BootstrapLocalAdmin(ctx, pool, slog.New(slog.DiscardHandler), cfg); err != nil {
		t.Fatalf("BootstrapLocalAdmin: %v", err)
	}
	// The seed must not have created a credential for the bootstrap user.
	ttx2, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	defer ttx2.Rollback(ctx)
	if _, err := db.GetLocalCredentialByUsername(ctx, ttx2.Tx, "tnt_dev", "seed-admin"); err == nil {
		t.Fatal("bootstrap clobbered a plane with an existing human-provisioned admin")
	}
}

// TestBootstrapLocalAdminOptOut pins guard 3: ORCHICON_LOCAL_ADMIN_SEED=0
// disables the seed entirely.
func TestBootstrapLocalAdminOptOut(t *testing.T) {
	pool := testBootstrapEnv(t)
	t.Setenv(localAdminSeedEnv, "0")
	cfg := bootstrapConfig("", "", "")
	ctx := context.Background()
	if err := BootstrapLocalAdmin(ctx, pool, slog.New(slog.DiscardHandler), cfg); err != nil {
		t.Fatalf("BootstrapLocalAdmin: %v", err)
	}
	exists, err := tenantAdminExists(ctx, pool, "tnt_dev")
	if err != nil {
		t.Fatalf("tenantAdminExists: %v", err)
	}
	if exists {
		t.Fatal("admin seeded despite ORCHICON_LOCAL_ADMIN_SEED=0")
	}
}

// TestBootstrapLocalAdminGuards pins the no-DB guards: production mode and
// a disabled embedded OP are both no-ops (no pool access needed).
func TestBootstrapLocalAdminGuards(t *testing.T) {
	t.Setenv(localAdminSeedEnv, "")

	prod := bootstrapConfig("", "", "")
	prod.Mode = config.ModeProduction
	if err := BootstrapLocalAdmin(context.Background(), nil, slog.New(slog.DiscardHandler), prod); err != nil {
		t.Fatalf("production mode must be a no-op, got %v", err)
	}

	noOP := bootstrapConfig("", "", "")
	noOP.Auth.EmbeddedOP = false
	if err := BootstrapLocalAdmin(context.Background(), nil, slog.New(slog.DiscardHandler), noOP); err != nil {
		t.Fatalf("disabled embedded OP must be a no-op, got %v", err)
	}
}

func containsPlaintext(hash, plaintext string) bool {
	for i := 0; i+len(plaintext) <= len(hash); i++ {
		if hash[i:i+len(plaintext)] == plaintext {
			return true
		}
	}
	return false
}
