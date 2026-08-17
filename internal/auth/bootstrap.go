package auth

import (
	"context"
	"log/slog"
	"os"

	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/jackc/pgx/v5"
)

// Local-mode first-admin bootstrap (D4 of the OIDC-base design). A fresh
// local plane has no way to authenticate: the anonymous dev bypass is gone
// and dev-login is off by default, but the embedded-OP local login cannot
// work until an admin provisions a credential (SetLocalCredential is
// admin-only). BootstrapLocalAdmin seeds the FIRST credential so a fresh
// plane is immediately usable — the operator signs in with the built-in
// default admin/admin (or a password pinned via env) through the embedded
// OP. When the built-in default is seeded, the credential is flagged for a
// forced password change on first login, so the default never persists.
//
// It is strictly local-mode, explicit-opt-out, and idempotent: it never
// runs outside local mode, never touches an existing admin, and never
// overwrites an existing credential. The one exception is the explicit
// reset override (ORCHICON_LOCAL_ADMIN_RESET=1), a manual maintenance
// action that re-arms the seed on a plane that already has an admin — the
// operator lost the credential and would otherwise be locked out. It keeps
// the same local-mode + embedded-OP guards and is never a default.
func BootstrapLocalAdmin(ctx context.Context, pool *db.Pool, log *slog.Logger, cfg config.Config) error {
	// Guard 1: local mode only — never in production.
	if cfg.Mode != config.ModeLocal {
		return nil
	}
	// Guard 2: the seed creates a credential for the embedded-OP login;
	// without the OP there is no local-login surface.
	if !cfg.Auth.EmbeddedOP {
		return nil
	}
	// Reset override (ORCHICON_LOCAL_ADMIN_RESET=1): explicit, manual
	// lockout recovery. The operator sets it in the plane env before boot
	// when they lost the admin credential; the seed then runs even though
	// an admin binding already exists, overwriting the credential (identity
	// + role binding preserved). It can only fire under guards 1+2, so a
	// production plane is unaffected, and it requires a deliberate env
	// change + restart — never a default.
	reset := os.Getenv(localAdminResetEnv) == "1"

	// Guard 3: explicit opt-out (ORCHICON_LOCAL_ADMIN_SEED=0). The
	// container prod dogfooding instance also boots in local mode, so the
	// opt-out is the documented escape hatch there. An explicit reset
	// override is NOT disabled by it: the no-lockout guarantee requires a
	// locked-out operator to be able to re-arm the credential even on a
	// plane configured to never auto-provision.
	if !reset && os.Getenv(localAdminSeedEnv) == "0" {
		return nil
	}

	tenantID := cfg.DeploymentTenantID
	username := os.Getenv(localAdminUsernameEnv)
	if username == "" {
		username = localAdminDefaultUsername
	}
	if len(username) > maxUsernameLen {
		log.Warn("local admin bootstrap: username exceeds length limit, skipping", "username", username)
		return nil
	}

	// Guard 4 (idempotence / don't-clobber): skip when the tenant admin
	// role already has a binding to ANY identity — a human has provisioned
	// an admin, so the seed never touches the DB. The reset override
	// bypasses this by design: the operator explicitly asked for the admin
	// credential to be overwritten.
	if !reset {
		exists, err := tenantAdminExists(ctx, pool, tenantID)
		if err != nil {
			return err
		}
		if exists {
			return nil
		}
	}

	// Credential: env pin wins; otherwise the built-in default admin/admin
	// (GitLab/Grafana first-admin pattern). The default is discoverable —
	// no password to hunt for in a boot log — and self-expiring: seeding
	// it flags the credential for a forced password change on first login,
	// so the default stops working as soon as a new password is set. A
	// pinned password is an explicit operator choice and is never flagged.
	// The seed is a no-op once an admin exists, so the chosen credential is
	// never repeated on subsequent boots.
	password := os.Getenv(localAdminPasswordEnv)
	forceChange := false
	if password == "" {
		password = localAdminDefaultPassword
		forceChange = true
	}
	if len(password) > MaxPasswordLen {
		log.Warn("local admin bootstrap: password exceeds length limit, skipping", "username", username)
		return nil
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}

	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return err
	}
	defer ttx.Rollback(ctx)

	ident, _, err := db.GetOrCreateIdentity(ctx, ttx.Tx, tenantID, username, username, "user")
	if err != nil {
		return err
	}
	adminRoleID, err := ensureAdminRole(ctx, ttx.Tx, tenantID)
	if err != nil {
		return err
	}
	if err := bindAdminRole(ctx, ttx.Tx, tenantID, ident.ID, adminRoleID); err != nil {
		return err
	}
	if _, err := db.UpsertLocalCredential(ctx, ttx.Tx, tenantID, ident.ID, username, hash, forceChange); err != nil {
		return err
	}
	if err := ttx.Commit(ctx); err != nil {
		return err
	}

	action := "created"
	if reset {
		action = "reset"
	}
	log.Warn("local-mode bootstrap admin "+action+": username "+username+", password "+password,
		"hint", "sign in at /login via the embedded OP local login; pin ORCHICON_LOCAL_ADMIN_PASSWORD to skip the forced password change, ORCHICON_LOCAL_ADMIN_SEED=0 to disable the auto-seed (the explicit reset override still works)")
	if forceChange {
		log.Warn("local-mode bootstrap admin was seeded with the default credential admin/admin — it MUST be changed on first login (the SPA gates the plane until then)")
	}
	return nil
}

// tenantAdminExists reports whether the tenant's admin role already has a
// binding to any identity. Used as the seed's don't-clobber guard.
func tenantAdminExists(ctx context.Context, pool *db.Pool, tenantID string) (bool, error) {
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return false, err
	}
	defer ttx.Rollback(ctx)
	roles, err := db.ListRoles(ctx, ttx.Tx, tenantID, 1000, "")
	if err != nil {
		return false, err
	}
	var adminRoleID string
	for _, rl := range roles {
		if rl.Name == "admin" {
			adminRoleID = rl.ID
			break
		}
	}
	if adminRoleID == "" {
		return false, nil
	}
	bindings, err := db.ListRoleBindings(ctx, ttx.Tx, tenantID, "", 1000, "")
	if err != nil {
		return false, err
	}
	for _, b := range bindings {
		if b.RoleID == adminRoleID {
			return true, nil
		}
	}
	return false, nil
}

// ensureAdminRole finds or creates the tenant admin role, returning its id.
func ensureAdminRole(ctx context.Context, tx pgx.Tx, tenantID string) (string, error) {
	roles, err := db.ListRoles(ctx, tx, tenantID, 1000, "")
	if err != nil {
		return "", err
	}
	for _, rl := range roles {
		if rl.Name == "admin" {
			return rl.ID, nil
		}
	}
	role, err := db.CreateRole(ctx, tx, db.RoleRow{
		TenantID:     tenantID,
		Name:         "admin",
		Scope:        "tenant",
		Entitlements: []string{"*"},
	})
	if err != nil {
		return "", err
	}
	return role.ID, nil
}

// bindAdminRole binds an identity to the tenant admin role (idempotent).
func bindAdminRole(ctx context.Context, tx pgx.Tx, tenantID, identityID, adminRoleID string) error {
	bindings, err := db.ListRoleBindings(ctx, tx, tenantID, identityID, 1000, "")
	if err != nil {
		return err
	}
	for _, b := range bindings {
		if b.RoleID == adminRoleID {
			return nil
		}
	}
	_, err = db.CreateRoleBinding(ctx, tx, db.RoleBindingRow{
		TenantID:   tenantID,
		IdentityID: identityID,
		RoleID:     adminRoleID,
		Scope:      "tenant",
	})
	return err
}

const (
	// localAdminSeedEnv is the explicit opt-out for the bootstrap seed.
	// Any value other than "0" enables it (default on in local mode).
	localAdminSeedEnv = "ORCHICON_LOCAL_ADMIN_SEED"
	// localAdminUsernameEnv pins the bootstrap username.
	localAdminUsernameEnv = "ORCHICON_LOCAL_ADMIN_USERNAME"
	// localAdminPasswordEnv pins the bootstrap password. When unset the
	// built-in default admin/admin is seeded with the forced-change flag.
	localAdminPasswordEnv = "ORCHICON_LOCAL_ADMIN_PASSWORD"
	// localAdminResetEnv is the explicit lockout-recovery override. When
	// set to "1", the seed re-arms on a plane that already has an admin
	// and overwrites the admin credential (identity + role binding kept).
	// Local mode + embedded OP only, same guards as the seed; it is never
	// a default and is not disabled by ORCHICON_LOCAL_ADMIN_SEED=0.
	localAdminResetEnv = "ORCHICON_LOCAL_ADMIN_RESET"
	// localAdminDefaultUsername is the default bootstrap username.
	localAdminDefaultUsername = "admin"
	// localAdminDefaultPassword is the built-in default bootstrap password,
	// used when ORCHICON_LOCAL_ADMIN_PASSWORD is unset. Seeding it flags
	// the credential for a forced password change on first login, so the
	// default never persists past the operator's first sign-in.
	localAdminDefaultPassword = "admin"
	// maxUsernameLen bounds the username at the boundary (mirrors the
	// SetLocalCredential validator).
	maxUsernameLen = 255
)
