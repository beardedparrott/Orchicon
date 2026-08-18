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

// Local-mode first-admin bootstrap. A fresh local plane has no way to
// authenticate: the anonymous dev bypass is gone, but the embedded-OP local
// login cannot work until an admin provisions a credential
// (SetLocalCredential is admin-only). BootstrapLocalAdmin seeds the FIRST
// credential so a fresh plane is immediately usable — the operator signs in
// with the built-in default admin/admin (or a username/password pinned via
// env) through the embedded OP. When the built-in default is seeded, the
// credential is flagged for a forced password change on first login, so the
// default never persists.
//
// It is strictly local-mode and idempotent (seed-once): it never runs
// outside local mode and never clobbers a credential that already exists.
// The one exception is the explicit reset override
// (ORCHICON_LOCAL_ADMIN_RESET=1), a manual maintenance action that re-arms
// the seed on a plane that already has an admin — the operator lost the
// credential and would otherwise be locked out. It keeps the same
// local-mode + embedded-OP guards and is never a default.
//
// Don't-clobber (option B): on a plane where the tenant admin role already
// binds an identity, the seed provisions a credential for that EXISTING
// identity when it has no local credential (e.g. a volume that only ever
// used the old dev-login surface) — identity + role binding preserved, and
// admin/admin works after upgrade with no reset env. When every bound admin
// identity already has a credential, the seed is a no-op.
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
	// a credential already exists, overwriting it (identity + role binding
	// preserved). It can only fire under guards 1+2, so a production plane
	// is unaffected, and it requires a deliberate env change + restart —
	// never a default.
	reset := os.Getenv(localAdminResetEnv) == "1"

	tenantID := cfg.DeploymentTenantID
	username := os.Getenv(localAdminUsernameEnv)
	if username == "" {
		username = localAdminDefaultUsername
	}
	if len(username) > maxUsernameLen {
		log.Warn("local admin bootstrap: username exceeds length limit, skipping", "username", username)
		return nil
	}

	// Credential: env pin wins; otherwise the built-in default admin/admin
	// (GitLab/Grafana first-admin pattern). The default is discoverable —
	// no password to hunt for in a boot log — and self-expiring: seeding
	// it flags the credential for a forced password change on first login,
	// so the default stops working as soon as a new password is set. A
	// pinned password is an explicit operator choice and is never flagged.
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

	adminRoleID, err := ensureAdminRole(ctx, ttx.Tx, tenantID)
	if err != nil {
		return err
	}

	var targetID string
	if reset {
		// Reset: the override targets the identity named by
		// ORCHICON_LOCAL_ADMIN_USERNAME (or the default "admin") via
		// GetOrCreateIdentity and overwrites its credential. This is
		// deliberately distinct from the option-B path below, which targets
		// the EXISTING admin-bound identity whatever its subject.
		ident, _, err := db.GetOrCreateIdentity(ctx, ttx.Tx, tenantID, username, username, "user")
		if err != nil {
			return err
		}
		targetID = ident.ID
		if err := bindAdminRole(ctx, ttx.Tx, tenantID, ident.ID, adminRoleID); err != nil {
			return err
		}
	} else {
		// Don't-clobber (option B): skip when every admin-bound identity
		// already has a local credential (seed-once; never clobber a real
		// credential). When an admin binding exists but the bound identity
		// has NO credential — the dev-login-era volume, or an OIDC-first-
		// login volume — provision one for that existing identity (identity
		// + role binding preserved). No admin binding at all → fresh-plane
		// path: create the admin identity, bind the role, seed the
		// credential.
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

	if _, err := db.UpsertLocalCredential(ctx, ttx.Tx, tenantID, targetID, username, hash, forceChange); err != nil {
		// The upsert can collide on the (tenant_id, username) unique index
		// when another identity already holds a credential literally named
		// `username`. Never fail boot or clobber that credential: log and
		// skip — the operator resolves via ORCHICON_LOCAL_ADMIN_RESET or
		// the Admin → Identities flow.
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
	log.Warn("local-mode bootstrap admin "+action+": username "+username+", password "+password,
		"hint", "sign in at /login via the embedded OP local login; pin ORCHICON_LOCAL_ADMIN_PASSWORD to skip the forced password change")
	if forceChange {
		log.Warn("local-mode bootstrap admin was seeded with the default credential admin/admin — it MUST be changed on first login (the SPA gates the plane until then)")
	}
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
	// localAdminPasswordEnv pins the bootstrap password. When unset the
	// built-in default admin/admin is seeded with the forced-change flag.
	localAdminPasswordEnv = "ORCHICON_LOCAL_ADMIN_PASSWORD"
	// localAdminResetEnv is the explicit lockout-recovery override. When
	// set to "1", the seed re-arms on a plane that already has a
	// credential and overwrites the admin credential (identity + role
	// binding kept). Local mode + embedded OP only, same guards as the
	// seed; it is never a default.
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