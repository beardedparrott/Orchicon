package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"os"

	"github.com/beardedparrott/orchicon/internal/auth/op"
	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/jackc/pgx/v5"
)

// Local-mode first-admin bootstrap (D4 of the OIDC-base design). A fresh
// local plane has no way to authenticate: the anonymous dev bypass is gone
// and dev-login is off by default, but the embedded-OP local login cannot
// work until an admin provisions a credential (SetLocalCredential is
// admin-only). BootstrapLocalAdmin seeds the FIRST credential so a fresh
// plane is immediately usable — the operator reads the boot-log line (or
// pins the password via env) and signs in through the embedded OP.
//
// It is strictly local-mode, explicit-opt-out, and idempotent: it never
// runs outside local mode, never touches an existing admin, and never
// overwrites an existing credential.
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
	// Guard 3: explicit opt-out (ORCHICON_LOCAL_ADMIN_SEED=0). The
	// container prod dogfooding instance also boots in local mode, so the
	// opt-out is the documented escape hatch there.
	if os.Getenv(localAdminSeedEnv) == "0" {
		return nil
	}

	tenantID := op.DefaultTenantID
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
	// an admin, so the seed never touches the DB.
	exists, err := tenantAdminExists(ctx, pool, tenantID)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	// Credential from env, never a baked-in default: a generated
	// cryptographically random password is logged once at boot
	// (GitLab/Grafana first-admin pattern). It is never written to env or
	// config and is never repeated on subsequent boots (once an admin
	// exists the seed is a no-op).
	password := os.Getenv(localAdminPasswordEnv)
	generated := false
	if password == "" {
		pw, err := randomPassword()
		if err != nil {
			return err
		}
		password = pw
		generated = true
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
	if _, err := db.UpsertLocalCredential(ctx, ttx.Tx, tenantID, ident.ID, username, hash); err != nil {
		return err
	}
	if err := ttx.Commit(ctx); err != nil {
		return err
	}

	log.Warn("local-mode bootstrap admin created: username "+username+", password "+password,
		"hint", "sign in at /login via the embedded OP local login; pin ORCHICON_LOCAL_ADMIN_PASSWORD to make it deterministic, ORCHICON_LOCAL_ADMIN_SEED=0 to skip")
	if generated {
		log.Warn("local-mode bootstrap admin password was auto-generated — set ORCHICON_LOCAL_ADMIN_PASSWORD to pin it")
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

// randomPassword returns a 24-character URL-safe random string.
func randomPassword() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

const (
	// localAdminSeedEnv is the explicit opt-out for the bootstrap seed.
	// Any value other than "0" enables it (default on in local mode).
	localAdminSeedEnv = "ORCHICON_LOCAL_ADMIN_SEED"
	// localAdminUsernameEnv pins the bootstrap username.
	localAdminUsernameEnv = "ORCHICON_LOCAL_ADMIN_USERNAME"
	// localAdminPasswordEnv pins the bootstrap password. When unset a
	// random password is generated and logged once at boot.
	localAdminPasswordEnv = "ORCHICON_LOCAL_ADMIN_PASSWORD"
	// localAdminDefaultUsername is the default bootstrap username.
	localAdminDefaultUsername = "admin"
	// maxUsernameLen bounds the username at the boundary (mirrors the
	// SetLocalCredential validator).
	maxUsernameLen = 255
)
