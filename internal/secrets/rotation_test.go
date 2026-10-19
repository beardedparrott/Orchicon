package secrets_test

// rotation_test.go — A ROTATED PROVIDER KEY MUST REACH THE NEXT REQUEST.
//
// THE BUG THIS PINS, from the operator's report:
//
//	"provider status 401 … api_key_expired … This API key has expired."
//	"I deleted the key resaved it under providers with the new key … yet it still errors."
//
// Provider clients are cached by internal/orchicon's registry with the resolved
// credential BAKED INTO them, and CredentialResolver caches the decrypted
// plaintext for the life of the process. Both caches are dropped by
// Registry.Invalidate — which internal/providers calls on every one of ITS OWN
// token mutations. The SECRETS service called nothing at all, so rotating a
// provider key from Settings → Secrets (the page that owns the same rows) left
// the plane authenticating with the OLD value until the process restarted.
//
// The fix is a post-write hook. What is asserted here is the CONTRACT the hook
// depends on: it fires once per write, AFTER the commit, carrying the tenant and
// the secret's NAME (a cache is keyed by what it was asked for) — and it does NOT
// fire when the write failed.

import (
	"context"
	"os"
	"testing"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/secrets"
)

type change struct{ tenant, name string }

// recorder holds the hook's calls.
//
// A POINTER, DELIBERATELY: returning a bare []change would copy the slice header
// at return time, so the closure's later appends would update the function's
// local and leave the caller's copy nil — the test would read "no notifications"
// while they were firing correctly.
type recorder struct{ seen []change }

func (r *recorder) hook(_ context.Context, tenantID, name string) {
	r.seen = append(r.seen, change{tenantID, name})
}

func (r *recorder) reset() { r.seen = nil }

func rotationService(t *testing.T) (*secrets.Service, *recorder) {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed secrets rotation test")
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
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i)
	}
	// A tenant row first: tenant_secrets has an FK to tenants, so the fixture has
	// to exist before a secret can be written.
	ttx, err := pool.BeginTenantTx(ctx, "tnt_secret_rotation")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	for _, id := range []string{"tnt_secret_rotation", "tnt_secret_rotation_fail"} {
		if _, err := ttx.Tx.Exec(ctx,
			`INSERT INTO tenants (id, slug, name, status) VALUES ($1,$1,$1,'active') ON CONFLICT DO NOTHING`,
			id); err != nil {
			t.Fatalf("seed tenant %s: %v", id, err)
		}
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit tenant seed: %v", err)
	}

	svc := secrets.New(pool, kek)
	rec := &recorder{}
	svc.SetOnChanged(rec.hook)
	return svc, rec
}

// TestRotatedSecretFiresTheInvalidationHook is the regression: create then update
// must each notify with the NAME, so the provider cache for OPENCODE_API_KEY is
// dropped and the next turn uses the new key.
func TestRotatedSecretFiresTheInvalidationHook(t *testing.T) {
	svc, rec := rotationService(t)
	ctx := context.Background()
	const tenantID = "tnt_secret_rotation"

	created, err := svc.Create(ctx, tenantID, "OPENCODE_API_KEY", "expired-key", "provider token")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_ = svc.Delete(context.Background(), tenantID, created.ID)
	})

	if len(rec.seen) != 1 || rec.seen[0].name != "OPENCODE_API_KEY" || rec.seen[0].tenant != tenantID {
		t.Fatalf("create notified %v, want one change (%s, OPENCODE_API_KEY) — a creation the "+
			"resolver never hears about can leave a cached nil credential in place", rec.seen, tenantID)
	}

	rec.reset()
	newValue := "fresh-key"
	if _, err := svc.Update(ctx, tenantID, created.ID, &newValue, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(rec.seen) != 1 || rec.seen[0].name != "OPENCODE_API_KEY" {
		t.Fatalf("update notified %v, want one change naming OPENCODE_API_KEY — without it the plane "+
			"keeps sending the OLD key until restart, which is exactly the reported "+
			"\"I replaced the key and it still 401s\"", rec.seen)
	}

	rec.reset()
	if err := svc.Delete(ctx, tenantID, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(rec.seen) != 1 || rec.seen[0].name != "OPENCODE_API_KEY" {
		t.Fatalf("delete notified %v, want one change naming OPENCODE_API_KEY — a DELETED credential "+
			"must stop being served from cache too", rec.seen)
	}
}

// TestFailedWriteDoesNotNotify — the hook runs AFTER the commit, so a rolled-back
// write must not clear a cache for a value the database does not hold.
func TestFailedWriteDoesNotNotify(t *testing.T) {
	svc, rec := rotationService(t)
	ctx := context.Background()

	// An empty value is rejected after the KEK/name checks but before any write.
	if _, err := svc.Create(ctx, "tnt_secret_rotation_fail", "OPENCODE_API_KEY", "", ""); err == nil {
		t.Fatal("created a secret with an empty value")
	}
	if len(rec.seen) != 0 {
		t.Errorf("a rejected (empty-value) write notified %v — the hook must run only AFTER a commit, "+
			"or a cache is cleared for a value the database never held", rec.seen)
	}

	// An invalid name is rejected before any transaction begins.
	if _, err := svc.Create(ctx, "tnt_secret_rotation_fail", "lower-case", "x", ""); err == nil {
		t.Fatal("created a secret with an invalid name")
	}
	if len(rec.seen) != 0 {
		t.Errorf("a rejected write notified %v", rec.seen)
	}

	// And an update against an absent id fails without notifying.
	missing := "not-a-secret"
	if _, err := svc.Update(ctx, "tnt_secret_rotation_fail", "00000000-0000-0000-0000-000000000000", &missing, nil); err == nil {
		t.Fatal("updated a secret that does not exist")
	}
	if len(rec.seen) != 0 {
		t.Errorf("a failed update notified %v", rec.seen)
	}
}
