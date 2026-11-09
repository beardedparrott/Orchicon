package auth

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/jackc/pgx/v5"
)

// BootstrapLocalAdmin is the OPT-IN alternative: it mints a credential ONLY when the operator
// explicitly pins BOTH ORCHICON_LOCAL_ADMIN_USERNAME and ORCHICON_LOCAL_ADMIN_PASSWORD.
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
	// Opt-in gate: the bootstrap mints a credential ONLY when the operator explicitly pins BOTH the
	// username and the password.
	username := os.Getenv(localAdminUsernameEnv)
	password := os.Getenv(localAdminPasswordEnv)
	if username == "" || password == "" {
		return nil
	}
	if len(username) > maxUsernameLen {
		log.Warn("local admin bootstrap: username exceeds length limit, skipping", "username", username)
		return nil
	}
	if len(password) > MaxPasswordLen {
		log.Warn("local admin bootstrap: password exceeds length limit, skipping", "username", username)
		return nil
	}
	// Reset override (ORCHICON_LOCAL_ADMIN_RESET=1): explicit, manual lockout recovery.
	reset := os.Getenv(localAdminResetEnv) == "1"

	tenantID := cfg.DeploymentTenantID
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}

	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return err
	}
	defer ttx.Rollback(ctx)

	adminRoleID, err := ensureAdminRole(ctx, ttx.Tx, tenantID)
	if err != nil {
		return err
	}

	var targetID string
	if reset {
		// Reset: the override targets the identity named by ORCHICON_LOCAL_ADMIN_USERNAME via
		// GetOrCreateIdentity and overwrites its credential.
		ident, _, err := db.GetOrCreateIdentity(ctx, ttx.Tx, tenantID, username, username, "user")
		if err != nil {
			return err
		}
		targetID = ident.ID
		if err := bindAdminRole(ctx, ttx.Tx, tenantID, ident.ID, adminRoleID); err != nil {
			return err
		}
	} else {
		// Don't-clobber (option B): skip when every admin-bound identity already has a local credential
		// (seed-once; never clobber a real credential).
		bound, err := adminBoundIdentities(ctx, ttx.Tx, tenantID, adminRoleID)
		if err != nil {
			return err
		}
		if len(bound) > 0 {
			// Deterministic order — role_bindings keyed by id — so the
			// seeded identity is stable across boots.
			for _, id := range bound {
				_, err := db.GetLocalCredentialByIdentity(ctx, ttx.Tx, tenantID, id)
				if err == nil {
					continue // already credentialed — seed-once
				}
				if !errors.Is(err, db.ErrNotFound) {
					return err
				}
				targetID = id
				break
			}
			if targetID == "" {
				// Every bound admin identity already has a credential: the
				// seed is a no-op. Only the explicit reset override re-arms.
				return nil
			}
		} else {
			// Fresh plane: create the admin identity (subject = username).
			ident, _, err := db.GetOrCreateIdentity(ctx, ttx.Tx, tenantID, username, username, "user")
			if err != nil {
				return err
			}
			targetID = ident.ID
			if err := bindAdminRole(ctx, ttx.Tx, tenantID, ident.ID, adminRoleID); err != nil {
				return err
			}
		}
	}

	// A pinned password is an explicit operator choice and is never flagged
	// for a forced change.
	if _, err := db.UpsertLocalCredential(ctx, ttx.Tx, tenantID, targetID, username, hash, false); err != nil {
		// The upsert can collide on the (tenant_id, username) unique index when another identity already
		// holds a credential literally named `username`.
		if isUniqueViolation(err) {
			log.Warn("local admin bootstrap: username already in use by another identity, skipping", "username", username)
			return nil
		}
		return err
	}
	if err := ttx.Commit(ctx); err != nil {
		return err
	}

	action := "created"
	if reset {
		action = "reset"
	}
	log.Warn("local-mode bootstrap admin "+action+": username "+username,
		"hint", "sign in at /login via the embedded OP local login")
	return nil
}

// adminBoundIdentities returns the ids of the identities bound to the
// tenant's admin role, in deterministic role_bindings (id ASC) order.
func adminBoundIdentities(ctx context.Context, tx pgx.Tx, tenantID, adminRoleID string) ([]string, error) {
	bindings, err := db.ListRoleBindings(ctx, tx, tenantID, "", 1000, "")
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, b := range bindings {
		if b.RoleID == adminRoleID {
			ids = append(ids, b.IdentityID)
		}
	}
	return ids, nil
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
	// localAdminUsernameEnv pins the bootstrap username.
	localAdminUsernameEnv = "ORCHICON_LOCAL_ADMIN_USERNAME"
	// localAdminPasswordEnv pins the bootstrap password.
	localAdminPasswordEnv = "ORCHICON_LOCAL_ADMIN_PASSWORD"
	// localAdminResetEnv is the explicit lockout-recovery override.
	localAdminResetEnv = "ORCHICON_LOCAL_ADMIN_RESET"
	// maxUsernameLen bounds the username at the boundary (mirrors the
	// SetLocalCredential validator).
	maxUsernameLen = 255
)
