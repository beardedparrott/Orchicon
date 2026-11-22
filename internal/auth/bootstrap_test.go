package auth

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/jackc/pgx/v5"
)

// testBootstrapEnv opens a migrated test DB and seeds a FRESH tenant for this test.
//
// PER-TEST TENANT, and it is the whole reason these tests can run at all. BootstrapLocalAdmin is
// SEED-ONCE: when every admin-bound identity already holds a local credential it returns nil and mints
// nothing. Against a shared, long-lived tenant — `tnt_dev` on any database that has ever had an admin
// provisioned, which includes every reused local test DB — it therefore correctly no-ops, and a test
// asserting "first admin seeding" fails for reasons that have nothing to do with the code under test.
//
// Each test now gets its own tenant, so "fresh plane" is a fact about the fixture rather than an
// assumption about the operator's database. Nothing about the production behaviour changes; the seed is
// the same INSERT of one tenant row.
func testBootstrapEnv(t *testing.T) (*db.Pool, string) {
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
	tenantID := "tnt_boot_" + strings.ToLower(db.NewID())
	if err := db.SeedDevTenant(ctx, pool, tenantID); err != nil {
		t.Fatalf("seed test tenant: %v", err)
	}
	return pool, tenantID
}

// bootstrapConfig pins LOCAL mode + the embedded OP, and takes the tenant so the configuration targets
// the same tenant the fixture seeded.
func bootstrapConfig(tenantID string) config.Config {
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.Auth.SigningKey = "test-bootstrap-signing-key"
	cfg.Auth.EmbeddedOP = true
	cfg.DeploymentTenantID = tenantID
	return cfg
}

func cleanupBootstrap(ctx context.Context, pool *db.Pool, tenantID, subject string) {
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return
	}
	defer ttx.Rollback(ctx)
	// role_bindings has no FK on identity_id, so the binding must be removed explicitly before the
	// identity (the local_credentials row cascades via its identity_id FK).
	_, _ = ttx.Exec(ctx, `DELETE FROM role_bindings WHERE identity_id IN (SELECT id FROM identities WHERE subject = $1)`, subject)
	_, _ = ttx.Exec(ctx, `DELETE FROM identities WHERE subject = $1`, subject)
	// Drop the seed-created admin role only when no bindings reference it
	// (keeps a human-provisioned admin role across tests intact).
	_, _ = ttx.Exec(ctx, `DELETE FROM roles r WHERE r.name = 'admin'
		AND NOT EXISTS (SELECT 1 FROM role_bindings b WHERE b.role_id = r.id)`)
	_ = ttx.Commit(ctx)
}

// adminRoleBound reports whether the tenant admin role has at least one
// role binding (the seed's "an admin exists" signal).
func adminRoleBound(t *testing.T, pool *db.Pool, tenantID string) bool {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	roles, err := db.ListRoles(ctx, ttx.Tx, tenantID, 1000, "")
	if err != nil {
		t.Fatalf("list roles: %v", err)
	}
	var adminRoleID string
	for _, rl := range roles {
		if rl.Name == "admin" {
			adminRoleID = rl.ID
			break
		}
	}
	if adminRoleID == "" {
		return false
	}
	bindings, err := db.ListRoleBindings(ctx, ttx.Tx, tenantID, "", 1000, "")
	if err != nil {
		t.Fatalf("list role bindings: %v", err)
	}
	for _, b := range bindings {
		if b.RoleID == adminRoleID {
			return true
		}
	}
	return false
}

// TestBootstrapLocalAdminSeeds verifies a fresh local plane with BOTH envs pinned gets a usable
// first admin: identity + admin role binding + local credential.
func TestBootstrapLocalAdminSeeds(t *testing.T) {
	pool, tenantID := testBootstrapEnv(t)
	t.Setenv(localAdminUsernameEnv, "seed-admin")
	t.Setenv(localAdminPasswordEnv, "seed-password-123")
	ctx := context.Background()
	cfg := bootstrapConfig(tenantID)
	if err := BootstrapLocalAdmin(ctx, pool, slog.New(slog.DiscardHandler), cfg); err != nil {
		t.Fatalf("BootstrapLocalAdmin: %v", err)
	}
	t.Cleanup(func() { cleanupBootstrap(ctx, pool, tenantID, "seed-admin") })

	// ISOLATION GUARD — this test describes a FRESH plane, so it must run as one.
	//
	// BootstrapLocalAdmin is deliberately SEED-ONCE: when every admin-bound identity already holds a
	// local credential it returns nil and mints nothing. On a database that has ever had an admin
	// provisioned — a restored scratch clone, a developer's reused test DB — it therefore does nothing,
	// and every assertion below fails for a reason that has nothing to do with the code under test.
	// That is exactly what happened here: the shared test database held a real `admin` credential, so
	// the bootstrap correctly no-opped and the test reported "db: not found".
	//
	// Refusing loudly is the honest form: the test cannot verify first-admin seeding against state
	// that already has one.
	if identityIDBySubject(t, pool, tenantID, "seed-admin") == "" {
		if bound := adminBoundSubjectIDs(t, pool, tenantID); len(bound) > 0 {
			t.Fatalf("this test requires a FRESH tenant, but tnt_dev already has admin-bound "+
				"identities (%v) — BootstrapLocalAdmin is seed-once and will no-op, so the assertions "+
				"below would fail for reasons unrelated to the code under test. Point ORCHICON_TEST_DSN "+
				"at a freshly migrated database, or clear the seeded admin first.", bound)
		}
	}

	// The SEEDED identity exists, is admin-bound, and holds the credential.
	//
	// This asserts on the identity the test itself created, NOT on whether ANY admin is bound.
	// adminRoleBound asks the weaker global question, and it answers true on any database that has
	// ever had an admin bound — so the assertion passed without the bootstrap having done anything,
	// and the test then failed one step later with a confusing "db: not found" for a credential it
	// had never checked was supposed to exist.
	identID := identityIDBySubject(t, pool, tenantID, "seed-admin")
	if identID == "" {
		t.Fatal("bootstrap created no identity for the pinned username")
	}
	if !identityHasAdminBinding(t, pool, tenantID, identID) {
		t.Fatal("the seeded identity is not admin-bound")
	}
	// The credential row exists and its hash verifies against the pinned
	// password (never plaintext).
	//
	// The transaction is a FUNCTION with its own defer: this block is followed by Fatalf calls, and a
	// defer here would still leak it on the Fatalf path into the pgx pool's use — see the pool-close
	// note below.
	row := localCredential(t, pool, tenantID, "seed-admin")
	valid, err := VerifyPassword("seed-password-123", row.PasswordHash)
	if err != nil || !valid {
		t.Fatalf("seeded password does not verify (err=%v)", err)
	}
	if containsPlaintext(row.PasswordHash, "seed-password-123") {
		t.Fatal("plaintext leaked into the stored hash")
	}
	// Pinned password: the forced-change flag is NOT set.
	if row.ForcePasswordChange {
		t.Fatal("pinned seed set force_password_change, want false")
	}
}

// TestBootstrapLocalAdminNoEnvIsNoOp pins the opt-in contract: with NO env pin (neither username
// nor password), the bootstrap is a no-op — no credential is minted AND no admin role is created.
func TestBootstrapLocalAdminNoEnvIsNoOp(t *testing.T) {
	pool, tenantID := testBootstrapEnv(t)
	t.Setenv(localAdminUsernameEnv, "")
	t.Setenv(localAdminPasswordEnv, "")
	ctx := context.Background()
	cfg := bootstrapConfig(tenantID)
	if err := BootstrapLocalAdmin(ctx, pool, slog.New(slog.DiscardHandler), cfg); err != nil {
		t.Fatalf("BootstrapLocalAdmin: %v", err)
	}

	// No admin role binding was created.
	// "Created an admin" means an admin BOUND WITH A CREDENTIAL — the only state a pinned password can
	// produce. Asserting adminRoleBound alone makes these no-op tests fail on any database that already
	// has an admin bound, which is a property of the fixture, not of the bootstrap.
	if n := adminCountWithCredential(t, pool, tenantID); n != 0 {
		t.Fatalf("bootstrap created %d credentialed admin(s) with no env pin; want no-op", n)
	}
	// No credential was minted (no default admin/admin, no generated
	// password).
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	roles, err := db.ListRoles(ctx, ttx.Tx, tenantID, 1000, "")
	if err != nil {
		t.Fatalf("list roles: %v", err)
	}
	for _, rl := range roles {
		if rl.Name == "admin" {
			t.Fatal("bootstrap created an admin role with no env pin; want no-op")
		}
	}
}

// TestBootstrapLocalAdminPartialPinIsNoOp pins the opt-in contract: pinning only ONE of the two
// envs is still a no-op — both must be set for a credential to be minted.
func TestBootstrapLocalAdminPartialPinIsNoOp(t *testing.T) {
	pool, tenantID := testBootstrapEnv(t)
	t.Setenv(localAdminUsernameEnv, "partial-admin")
	t.Setenv(localAdminPasswordEnv, "")
	ctx := context.Background()
	cfg := bootstrapConfig(tenantID)
	if err := BootstrapLocalAdmin(ctx, pool, slog.New(slog.DiscardHandler), cfg); err != nil {
		t.Fatalf("BootstrapLocalAdmin: %v", err)
	}
	if n := adminCountWithCredential(t, pool, tenantID); n != 0 {
		t.Fatalf("bootstrap created %d credentialed admin(s) with only the username pinned; want no-op", n)
	}
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	if _, err := db.GetLocalCredentialByUsername(ctx, ttx.Tx, tenantID, "partial-admin"); err == nil {
		t.Fatal("bootstrap minted a credential with only the username pinned; want no-op")
	}
}

// TestBootstrapLocalAdminIdempotent pins the seed-once behavior: a second boot does not clobber the
// credential (the hash stays identical to the first seed).
func TestBootstrapLocalAdminIdempotent(t *testing.T) {
	pool, tenantID := testBootstrapEnv(t)
	t.Setenv(localAdminUsernameEnv, "idem-admin")
	t.Setenv(localAdminPasswordEnv, "idem-password")
	ctx := context.Background()
	cfg := bootstrapConfig(tenantID)
	log := slog.New(slog.DiscardHandler)
	if err := BootstrapLocalAdmin(ctx, pool, log, cfg); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	t.Cleanup(func() { cleanupBootstrap(ctx, pool, tenantID, "idem-admin") })

	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	first, err := db.GetLocalCredentialByUsername(ctx, ttx.Tx, tenantID, "idem-admin")
	_ = ttx.Rollback(ctx)
	if err != nil {
		t.Fatalf("first credential: %v", err)
	}

	if err := BootstrapLocalAdmin(ctx, pool, log, cfg); err != nil {
		t.Fatalf("second seed: %v", err)
	}
	ttx2, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	second, err := db.GetLocalCredentialByUsername(ctx, ttx2.Tx, tenantID, "idem-admin")
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

// TestBootstrapLocalAdminNoClobber pins the seed-once don't-clobber guard: when a human already
// provisioned an admin WITH a local credential.
func TestBootstrapLocalAdminNoClobber(t *testing.T) {
	pool, tenantID := testBootstrapEnv(t)
	t.Setenv(localAdminUsernameEnv, "seed-admin")
	t.Setenv(localAdminPasswordEnv, "seed-password")
	ctx := context.Background()

	// Provision an admin the way a human would: identity + admin role
	// binding + an existing local credential.
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	ident, _, err := db.GetOrCreateIdentity(ctx, ttx.Tx, tenantID, "human-admin@orchicon.local", "Human Admin", "user")
	if err != nil {
		t.Fatalf("ensure identity: %v", err)
	}
	if _, err := db.CreateRole(ctx, ttx.Tx, db.RoleRow{TenantID: tenantID, Name: "admin", Scope: "tenant", Entitlements: []string{"*"}}); err != nil {
		t.Fatalf("create role: %v", err)
	}
	roles, err := db.ListRoles(ctx, ttx.Tx, tenantID, 1000, "")
	if err != nil {
		t.Fatalf("list roles: %v", err)
	}
	var adminRoleID string
	for _, rl := range roles {
		if rl.Name == "admin" {
			adminRoleID = rl.ID
		}
	}
	if _, err := db.CreateRoleBinding(ctx, ttx.Tx, db.RoleBindingRow{TenantID: tenantID, IdentityID: ident.ID, RoleID: adminRoleID, Scope: "tenant"}); err != nil {
		t.Fatalf("create binding: %v", err)
	}
	hash, err := HashPassword("human-password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if _, err := db.UpsertLocalCredential(ctx, ttx.Tx, tenantID, ident.ID, "human-admin", hash, false); err != nil {
		t.Fatalf("upsert credential: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	t.Cleanup(func() { cleanupBootstrap(ctx, pool, tenantID, "human-admin@orchicon.local") })

	cfg := bootstrapConfig(tenantID)
	if err := BootstrapLocalAdmin(ctx, pool, slog.New(slog.DiscardHandler), cfg); err != nil {
		t.Fatalf("BootstrapLocalAdmin: %v", err)
	}
	// The seed must not have created a credential for the bootstrap user...
	ttx2, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	defer ttx2.Rollback(ctx)
	if _, err := db.GetLocalCredentialByUsername(ctx, ttx2.Tx, tenantID, "seed-admin"); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("bootstrap clobbered a plane with an existing human-provisioned admin (err=%v)", err)
	}
	// ...and the existing human credential was left untouched.
	row, err := db.GetLocalCredentialByIdentity(ctx, ttx2.Tx, tenantID, ident.ID)
	if err != nil {
		t.Fatalf("existing credential: %v", err)
	}
	if valid, _ := VerifyPassword("human-password", row.PasswordHash); !valid {
		t.Fatal("existing human admin credential was clobbered by the seed")
	}
}

// TestBootstrapLocalAdminCredentialGap pins option B (no-lockout upgrade).
func TestBootstrapLocalAdminCredentialGap(t *testing.T) {
	pool, tenantID := testBootstrapEnv(t)
	t.Setenv(localAdminUsernameEnv, "gap-admin")
	t.Setenv(localAdminPasswordEnv, "gap-password")
	t.Setenv(localAdminResetEnv, "")
	ctx := context.Background()

	// A dev-login-era admin: role binding, but NO local credential, and a
	// subject that is NOT the pinned username.
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	ident, _, err := db.GetOrCreateIdentity(ctx, ttx.Tx, tenantID, "dev@orchicon.local", "Dev Admin", "user")
	if err != nil {
		t.Fatalf("ensure identity: %v", err)
	}
	if _, err := db.CreateRole(ctx, ttx.Tx, db.RoleRow{TenantID: tenantID, Name: "admin", Scope: "tenant", Entitlements: []string{"*"}}); err != nil {
		t.Fatalf("create role: %v", err)
	}
	roles, err := db.ListRoles(ctx, ttx.Tx, tenantID, 1000, "")
	if err != nil {
		t.Fatalf("list roles: %v", err)
	}
	var adminRoleID string
	for _, rl := range roles {
		if rl.Name == "admin" {
			adminRoleID = rl.ID
		}
	}
	if _, err := db.CreateRoleBinding(ctx, ttx.Tx, db.RoleBindingRow{TenantID: tenantID, IdentityID: ident.ID, RoleID: adminRoleID, Scope: "tenant"}); err != nil {
		t.Fatalf("create binding: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	t.Cleanup(func() { cleanupBootstrap(ctx, pool, tenantID, "dev@orchicon.local") })

	cfg := bootstrapConfig(tenantID)
	if err := BootstrapLocalAdmin(ctx, pool, slog.New(slog.DiscardHandler), cfg); err != nil {
		t.Fatalf("BootstrapLocalAdmin: %v", err)
	}

	// The pinned credential was provisioned and resolves to the EXISTING
	// identity — identity + role binding preserved.
	ttx2, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	defer ttx2.Rollback(ctx)
	row, err := db.GetLocalCredentialByUsername(ctx, ttx2.Tx, tenantID, "gap-admin")
	if err != nil {
		t.Fatalf("GetLocalCredentialByUsername(gap-admin): %v", err)
	}
	if row.IdentityID != ident.ID {
		t.Fatalf("credential bound to identity %s, want the existing admin %s (identity must be preserved)", row.IdentityID, ident.ID)
	}
	valid, err := VerifyPassword("gap-password", row.PasswordHash)
	if err != nil || !valid {
		t.Fatalf("pinned password does not verify against the seeded credential (err=%v)", err)
	}
	// A pinned credential is an explicit operator choice: no forced change.
	if row.ForcePasswordChange {
		t.Fatal("upgrade credential seed set force_password_change, want false")
	}
	// The admin identity's subject/type were NOT clobbered.
	kept, err := db.GetIdentity(ctx, ttx2.Tx, tenantID, ident.ID)
	if err != nil {
		t.Fatalf("GetIdentity: %v", err)
	}
	if kept.Subject != "dev@orchicon.local" || kept.IdentityType != "user" {
		t.Fatalf("bootstrap mutated the existing identity: subject=%q type=%q", kept.Subject, kept.IdentityType)
	}
	// The admin role binding is intact.
	bindings, err := db.ListRoleBindings(ctx, ttx2.Tx, tenantID, ident.ID, 1000, "")
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}
	found := false
	for _, b := range bindings {
		if b.RoleID == adminRoleID {
			found = true
		}
	}
	if !found {
		t.Fatal("admin role binding lost after the upgrade credential seed")
	}
}

// TestBootstrapLocalAdminGuards pins the no-DB guards: production mode and
// a disabled embedded OP are both no-ops (no pool access needed).
func TestBootstrapLocalAdminGuards(t *testing.T) {
	prod := bootstrapConfig("tnt_guards")
	prod.Mode = config.ModeProduction
	if err := BootstrapLocalAdmin(context.Background(), nil, slog.New(slog.DiscardHandler), prod); err != nil {
		t.Fatalf("production mode must be a no-op, got %v", err)
	}

	noOP := bootstrapConfig("tnt_guards")
	noOP.Auth.EmbeddedOP = false
	if err := BootstrapLocalAdmin(context.Background(), nil, slog.New(slog.DiscardHandler), noOP); err != nil {
		t.Fatalf("disabled embedded OP must be a no-op, got %v", err)
	}
}

// TestBootstrapLocalAdminReset verifies the lockout-recovery override
// (ORCHICON_LOCAL_ADMIN_RESET=1): on a plane that already has an admin.
func TestBootstrapLocalAdminReset(t *testing.T) {
	pool, tenantID := testBootstrapEnv(t)
	t.Setenv(localAdminUsernameEnv, "reset-admin")
	t.Setenv(localAdminPasswordEnv, "first-password")
	t.Setenv(localAdminResetEnv, "")
	ctx := context.Background()
	cfg := bootstrapConfig(tenantID)
	log := slog.New(slog.DiscardHandler)
	if err := BootstrapLocalAdmin(ctx, pool, log, cfg); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	t.Cleanup(func() { cleanupBootstrap(ctx, pool, tenantID, "reset-admin") })

	// ONE transaction, released by defer — not a live ttx handed to a block full of Fatalfs. A leaked
	// transaction holds a pooled connection, so the cleanup pool.Close blocks forever and the package
	// dies of a timeout instead of reporting the assertion that failed.
	first, firstIdent, bindings := func() (db.LocalCredentialRow, db.IdentityRow, []db.RoleBindingRow) {
		ttx, err := pool.BeginTenantTx(ctx, tenantID)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		defer ttx.Rollback(ctx)
		cred, err := db.GetLocalCredentialByUsername(ctx, ttx.Tx, tenantID, "reset-admin")
		if err != nil {
			t.Fatalf("first credential: %v", err)
		}
		ident, err := db.GetIdentity(ctx, ttx.Tx, tenantID, cred.IdentityID)
		if err != nil {
			t.Fatalf("first identity: %v", err)
		}
		b, err := db.ListRoleBindings(ctx, ttx.Tx, tenantID, cred.IdentityID, 1000, "")
		if err != nil {
			t.Fatalf("list bindings: %v", err)
		}
		return cred, ident, b
	}()
	if len(bindings) == 0 {
		t.Fatal("no admin role binding after first seed")
	}

	// The operator lost the password: set the reset override + a new
	// pinned password and boot again.
	t.Setenv(localAdminResetEnv, "1")
	t.Setenv(localAdminPasswordEnv, "second-password")
	if err := BootstrapLocalAdmin(ctx, pool, log, cfg); err != nil {
		t.Fatalf("reset seed: %v", err)
	}

	ttx2, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	defer ttx2.Rollback(ctx)
	second, err := db.GetLocalCredentialByUsername(ctx, ttx2.Tx, tenantID, "reset-admin")
	if err != nil {
		t.Fatalf("credential after reset: %v", err)
	}
	// The identity and credential row are the SAME ones — only the hash
	// was overwritten.
	if second.IdentityID != first.IdentityID {
		t.Fatalf("reset replaced the identity (want %s, got %s); want credential overwrite only", first.IdentityID, second.IdentityID)
	}
	if second.ID != first.ID {
		t.Fatal("reset replaced the credential row; want in-place overwrite")
	}
	// The old password no longer verifies; the new one does.
	if valid, _ := VerifyPassword("first-password", second.PasswordHash); valid {
		t.Fatal("old password still verifies after reset")
	}
	valid, err := VerifyPassword("second-password", second.PasswordHash)
	if err != nil || !valid {
		t.Fatalf("new password does not verify after reset (err=%v)", err)
	}
	// A PINNED reset is an explicit operator choice: no forced change.
	if second.ForcePasswordChange {
		t.Fatal("pinned reset set force_password_change, want false")
	}
	// The admin role binding survived the reset.
	bindings2, err := db.ListRoleBindings(ctx, ttx2.Tx, tenantID, first.IdentityID, 1000, "")
	if err != nil {
		t.Fatalf("list bindings after reset: %v", err)
	}
	if len(bindings2) != len(bindings) {
		t.Fatalf("admin binding count changed across reset (%d → %d)", len(bindings), len(bindings2))
	}
	// Identity subject/type unchanged.
	secondIdent, err := db.GetIdentity(ctx, ttx2.Tx, tenantID, second.IdentityID)
	if err != nil {
		t.Fatalf("identity after reset: %v", err)
	}
	if secondIdent.Subject != firstIdent.Subject || secondIdent.IdentityType != firstIdent.IdentityType {
		t.Fatal("reset mutated the identity beyond the credential")
	}
}

// TestBootstrapLocalAdminResetRequiresPin pins the opt-in reset rule: a lockout reset WITHOUT both
// envs pinned is a no-op — no default credential is ever minted as a fallback.
func TestBootstrapLocalAdminResetRequiresPin(t *testing.T) {
	pool, tenantID := testBootstrapEnv(t)
	t.Setenv(localAdminUsernameEnv, "resetc-admin")
	t.Setenv(localAdminPasswordEnv, "first-password")
	t.Setenv(localAdminResetEnv, "")
	ctx := context.Background()
	cfg := bootstrapConfig(tenantID)
	log := slog.New(slog.DiscardHandler)
	if err := BootstrapLocalAdmin(ctx, pool, log, cfg); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	t.Cleanup(func() { cleanupBootstrap(ctx, pool, tenantID, "resetc-admin") })

	// The operator lost the password: set the reset override but UNPIN the
	// password (only the username is pinned) and boot again.
	t.Setenv(localAdminResetEnv, "1")
	t.Setenv(localAdminPasswordEnv, "")
	if err := BootstrapLocalAdmin(ctx, pool, log, cfg); err != nil {
		t.Fatalf("unpinned reset seed: %v", err)
	}

	// The credential is untouched — the reset was a no-op (no default
	// credential fallback).
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	row, err := db.GetLocalCredentialByUsername(ctx, ttx.Tx, tenantID, "resetc-admin")
	if err != nil {
		t.Fatalf("credential after unpinned reset: %v", err)
	}
	if valid, _ := VerifyPassword("first-password", row.PasswordHash); !valid {
		t.Fatal("unpinned reset clobbered the existing credential; want no-op")
	}
}

// TestBootstrapLocalAdminResetGuards pins the reset override's guards: it must never fire in
// production mode or with the embedded OP disabled (nil pool would panic if it tried to run).
func TestBootstrapLocalAdminResetGuards(t *testing.T) {
	t.Setenv(localAdminResetEnv, "1")
	t.Setenv(localAdminUsernameEnv, "guard-admin")
	t.Setenv(localAdminPasswordEnv, "guard-password")

	prod := bootstrapConfig("tnt_guards")
	prod.Mode = config.ModeProduction
	if err := BootstrapLocalAdmin(context.Background(), nil, slog.New(slog.DiscardHandler), prod); err != nil {
		t.Fatalf("production mode must be a no-op for reset, got %v", err)
	}

	noOP := bootstrapConfig("tnt_guards")
	noOP.Auth.EmbeddedOP = false
	if err := BootstrapLocalAdmin(context.Background(), nil, slog.New(slog.DiscardHandler), noOP); err != nil {
		t.Fatalf("disabled embedded OP must be a no-op for reset, got %v", err)
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

// --- helpers that scope their own transactions ----------------------------
//
// EVERY HELPER BELOW OPENS, USES AND CLOSES ITS OWN TENANT TRANSACTION, and none is ever called from a
// defer. That is deliberate: a helper that returned a live ttx would hand the caller a transaction to
// leak on its next t.Fatalf, and a leaked transaction is not a small thing in this suite — it holds a
// pooled connection, so the t.Cleanup pool.Close at the end of the test blocks on it forever and the
// whole package dies of a 5-10 minute TIMEOUT instead of reporting the one-line assertion that
// actually failed.

// identityIDBySubject resolves an identity id by subject, or "" when absent.
func identityIDBySubject(t *testing.T, pool *db.Pool, tenantID, subject string) string {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	var id string
	if err := ttx.QueryRow(ctx, `SELECT id FROM identities WHERE subject = $1`, subject).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ""
		}
		t.Fatalf("query identity: %v", err)
	}
	return id
}

// identityHasAdminBinding reports whether THIS identity carries the admin role. Distinct from
// adminRoleBound, which asks whether any identity does.
func identityHasAdminBinding(t *testing.T, pool *db.Pool, tenantID, identityID string) bool {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	var n int
	err = ttx.QueryRow(ctx, `SELECT count(*) FROM role_bindings rb
		JOIN roles r ON r.id = rb.role_id
		WHERE rb.identity_id = $1 AND r.name = 'admin'`, identityID).Scan(&n)
	if err != nil {
		t.Fatalf("query binding: %v", err)
	}
	return n > 0
}

// localCredential reads the credential row for a username.
func localCredential(t *testing.T, pool *db.Pool, tenantID, username string) db.LocalCredentialRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	row, err := db.GetLocalCredentialByUsername(ctx, ttx.Tx, tenantID, username)
	if err != nil {
		t.Fatalf("GetLocalCredentialByUsername(%q): %v", username, err)
	}
	return row
}

// adminBoundSubjectIDs returns the subjects of every admin-bound identity, for the isolation guard's
// diagnostic message.
func adminBoundSubjectIDs(t *testing.T, pool *db.Pool, tenantID string) []string {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	rows, err := ttx.Query(ctx, `SELECT i.subject FROM role_bindings rb
		JOIN roles r ON r.id = rb.role_id
		JOIN identities i ON i.id = rb.identity_id
		WHERE r.name = 'admin' ORDER BY i.subject`)
	if err != nil {
		t.Fatalf("query admin identities: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, s)
	}
	return out
}

// adminCountWithCredential counts the admin-bound identities that hold a local credential — the
// "an admin was actually created" signal. adminRoleBound answers "is ANY admin bound", which is true on
// any reused database and therefore cannot witness a no-op.
func adminCountWithCredential(t *testing.T, pool *db.Pool, tenantID string) int {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	var n int
	err = ttx.QueryRow(ctx, `SELECT count(DISTINCT rb.identity_id) FROM role_bindings rb
		JOIN roles r ON r.id = rb.role_id
		JOIN local_credentials c ON c.identity_id = rb.identity_id
		WHERE r.name = 'admin'`).Scan(&n)
	if err != nil {
		t.Fatalf("count credentialed admins: %v", err)
	}
	return n
}
