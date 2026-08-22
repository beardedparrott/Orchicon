package db_test

import (
	"context"
	"errors"
	"os"
	"testing"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
)

// TestLocalCredentialsCRUD exercises the local_credentials data-access
// surface: upsert keyed on (tenant, identity), username lookup, identity
// lookup, delete, and RLS cross-tenant isolation as the backstop. Guarded
// by ORCHICON_TEST_DSN like the seed tests:
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/db/ -run TestLocalCredentialsCRUD -v
func TestLocalCredentialsCRUD(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed credential test")
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

	const (
		tenant   = "tnt_dev"
		otherTen = "tnt_other"
		subject  = "local-user@orchicon.local"
		username = "local-user"
	)

	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	ident, _, err := db.GetOrCreateIdentity(ctx, ttx.Tx, tenant, subject, "Local User", "user")
	if err != nil {
		t.Fatalf("ensure identity: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit identity: %v", err)
	}
	t.Cleanup(func() {
		ct := context.Background()
		dttx, err := pool.BeginTenantTx(ct, tenant)
		if err == nil {
			_, _ = dttx.Exec(ct, `DELETE FROM identities WHERE id = $1`, ident.ID)
			_ = dttx.Commit(ct)
		}
	})

	hash := "$argon2id$v=19$m=65536,t=3,p=4$c2FsdHNhbHRzYWx0c2FsdA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	upsert := func(forceChange bool) {
		t.Helper()
		utx, err := pool.BeginTenantTx(ctx, tenant)
		if err != nil {
			t.Fatalf("begin upsert tx: %v", err)
		}
		defer utx.Rollback(ctx)
		if _, err := db.UpsertLocalCredential(ctx, utx.Tx, tenant, ident.ID, username, hash, forceChange); err != nil {
			t.Fatalf("UpsertLocalCredential: %v", err)
		}
		if err := utx.Commit(ctx); err != nil {
			t.Fatalf("commit upsert: %v", err)
		}
	}
	// The built-in-default bootstrap seeds the flag true; a default row is
	// false, so upsert with the flag set first to prove it round-trips.
	upsert(true)

	// Username lookup returns the stored hash (never a plaintext column).
	gtx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin get tx: %v", err)
	}
	row, err := db.GetLocalCredentialByUsername(ctx, gtx.Tx, tenant, username)
	if err != nil {
		t.Fatalf("GetLocalCredentialByUsername: %v", err)
	}
	if row.IdentityID != ident.ID || row.PasswordHash != hash || row.Status != "active" || row.Version != 1 {
		t.Fatalf("unexpected row: %+v", row)
	}
	if row.Username != username {
		t.Fatalf("username = %q, want %q", row.Username, username)
	}
	if !row.ForcePasswordChange {
		t.Fatal("force_password_change = false, want true (upsert with flag)")
	}

	// Identity lookup finds the same row.
	byIdent, err := db.GetLocalCredentialByIdentity(ctx, gtx.Tx, tenant, ident.ID)
	if err != nil {
		t.Fatalf("GetLocalCredentialByIdentity: %v", err)
	}
	if byIdent.ID != row.ID {
		t.Fatalf("identity lookup returned a different row: %+v vs %+v", byIdent, row)
	}
	if err := gtx.Rollback(ctx); err != nil {
		t.Fatalf("rollback get tx: %v", err)
	}

	// Upsert is idempotent per (tenant, identity) and bumps the version.
	// Replacing with the flag false must clear the flag (the ON CONFLICT
	// DO UPDATE path — this is how SetLocalCredential clears the state).
	upsert(false)
	gtx2, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin get2 tx: %v", err)
	}
	row2, err := db.GetLocalCredentialByIdentity(ctx, gtx2.Tx, tenant, ident.ID)
	if err != nil {
		t.Fatalf("GetLocalCredentialByIdentity after re-upsert: %v", err)
	}
	if row2.Version != 2 {
		t.Fatalf("version = %d, want 2 after re-upsert", row2.Version)
	}
	if row2.ForcePasswordChange {
		t.Fatal("force_password_change = true after re-upsert with false, want cleared")
	}
	if err := gtx2.Rollback(ctx); err != nil {
		t.Fatalf("rollback get2 tx: %v", err)
	}

	// RLS backstop: a different-tenant tx cannot see the row.
	otx, err := pool.BeginTenantTx(ctx, otherTen)
	if err != nil {
		t.Fatalf("begin other-tenant tx: %v", err)
	}
	defer otx.Rollback(ctx)
	if _, err := db.GetLocalCredentialByUsername(ctx, otx.Tx, otherTen, username); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("cross-tenant read = %v, want ErrNotFound (RLS leak)", err)
	}

	// Delete removes the credential; a second delete is NotFound.
	dtx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin delete tx: %v", err)
	}
	if err := db.DeleteLocalCredential(ctx, dtx.Tx, tenant, ident.ID); err != nil {
		t.Fatalf("DeleteLocalCredential: %v", err)
	}
	if err := dtx.Commit(ctx); err != nil {
		t.Fatalf("commit delete: %v", err)
	}
	dtx2, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin delete2 tx: %v", err)
	}
	defer dtx2.Rollback(ctx)
	if err := db.DeleteLocalCredential(ctx, dtx2.Tx, tenant, ident.ID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}

	// Re-insert so the cascade test below has a row to delete with the identity.
	upsert(false)

	// Identity deletion cascades to the credential (ON DELETE CASCADE).
	ctxTenant := context.Background()
	cttx, err := pool.BeginTenantTx(ctxTenant, tenant)
	if err != nil {
		t.Fatalf("begin cascade tx: %v", err)
	}
	if _, err := cttx.Exec(ctxTenant, `DELETE FROM identities WHERE id = $1`, ident.ID); err != nil {
		t.Fatalf("delete identity: %v", err)
	}
	if err := cttx.Commit(ctxTenant); err != nil {
		t.Fatalf("commit cascade: %v", err)
	}
	cttx2, err := pool.BeginTenantTx(ctxTenant, tenant)
	if err != nil {
		t.Fatalf("begin post-cascade tx: %v", err)
	}
	defer cttx2.Rollback(ctxTenant)
	if _, err := db.GetLocalCredentialByIdentity(ctxTenant, cttx2.Tx, tenant, ident.ID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("credential survived identity delete: %v", err)
	}
}
