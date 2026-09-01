package providers_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/orchicon"
	"github.com/beardedparrott/orchicon/internal/providers"
)

// Integration tests for the Providers settings service (ADR-0006): custom
// provider CRUD, token auto-write, the deletion guard, and end-to-end
// credential resolution. Guarded by ORCHICON_TEST_DSN like the db tests:
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/providers/ -run TestProviders -v

const testKEK = "0123456789abcdef0123456789abcdef" // 32 bytes

func newProvidersTestService(t *testing.T) (*providers.Service, *db.Pool) {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed providers test")
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
	return providers.New(pool, []byte(testKEK), nil), pool
}

func ensureTenant(t *testing.T, pool *db.Pool, tenantID string) {
	t.Helper()
	if err := db.SeedDevTenant(context.Background(), pool, tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
}

const testTenant = "tnt_providers_test"

// cleanupCustom removes leftover custom-provider rows (and their auto-written
// secrets) from earlier runs so the test is idempotent across a shared DB.
func cleanupCustom(t *testing.T, pool *db.Pool) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.BeginTenantTx(ctx, testTenant)
	if err != nil {
		t.Fatalf("cleanup begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Tx.Query(ctx, `DELETE FROM provider_settings WHERE tenant_id=$1 AND is_custom RETURNING provider_id`, testTenant)
	if err != nil {
		t.Fatalf("cleanup delete settings: %v", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("cleanup scan: %v", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if _, err := tx.Tx.Exec(ctx, `DELETE FROM tenant_secrets WHERE tenant_id=$1 AND name=$2`, testTenant, providers.CustomSecretName(id)); err != nil {
			t.Fatalf("cleanup delete secret: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("cleanup commit: %v", err)
	}
}

func TestProvidersCustomCRUD(t *testing.T) {
	svc, pool := newProvidersTestService(t)
	ensureTenant(t, pool, testTenant)
	ctx := context.Background()
	cleanupCustom(t, pool)

	// Create (auth mode defaults to none).
	in := providers.CreateCustomInput{RefID: "local-models", BaseURL: "http://localhost:11434/v1", AuthMode: providers.AuthModeToken}
	e, err := svc.CreateCustom(ctx, testTenant, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if e.ID != "local-models" || !e.IsCustom || e.ReadOnly || e.AuthMode != providers.AuthModeToken {
		t.Fatalf("unexpected entry: %+v", e)
	}
	if e.TokenSecretName != "CUSTOM_LOCAL_MODELS_API_KEY" {
		t.Fatalf("derived secret name: %q", e.TokenSecretName)
	}

	// Duplicate ref rejected.
	if _, err := svc.CreateCustom(ctx, testTenant, in); err == nil {
		t.Fatal("duplicate ref accepted")
	}

	// Built-in ref collision rejected.
	if _, err := svc.CreateCustom(ctx, testTenant, providers.CreateCustomInput{RefID: "openai", BaseURL: "http://x/v1"}); err == nil {
		t.Fatal("built-in ref accepted")
	}

	// List: built-ins are read-only rows, custom is first-class next to them.
	list, err := svc.ListForTenant(ctx, testTenant)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	sawBuiltin, sawCustom := 0, 0
	for _, en := range list {
		if en.IsCustom && en.ID == "local-models" {
			sawCustom++
		}
		if !en.IsCustom && en.ReadOnly {
			sawBuiltin++
		}
	}
	if sawCustom != 1 || sawBuiltin == 0 {
		t.Fatalf("merged list wrong: customs=%d builtins=%d", sawCustom, sawBuiltin)
	}

	// Multiple custom entries are first-class.
	e2, err := svc.CreateCustom(ctx, testTenant, providers.CreateCustomInput{RefID: "local-models-fast", BaseURL: "http://localhost:11435/v1", DisplayName: "Local Fast"})
	if err != nil {
		t.Fatalf("create second custom: %v", err)
	}
	if e2.DisplayName != "Local Fast" || e2.AuthMode != providers.AuthModeNone {
		t.Fatalf("second custom wrong: %+v", e2)
	}

	// Edit: display name + auth mode flip; ref id immutable.
	name := "Local Models"
	auth := providers.AuthModeNone
	if _, err := svc.UpdateCustom(ctx, testTenant, providers.UpdateCustomInput{RefID: "local-models", DisplayName: &name, AuthMode: &auth}); err != nil {
		t.Fatalf("update: %v", err)
	}
	list, _ = svc.ListForTenant(ctx, testTenant)
	for _, en := range list {
		if en.ID == "local-models" {
			if en.DisplayName != "Local Models" || en.AuthMode != providers.AuthModeNone || en.TokenSecretName != "" {
				t.Fatalf("post-update entry: %+v", en)
			}
		}
	}

	// Token auto-write after auth-mode flip back to token.
	auth = providers.AuthModeToken
	if _, err := svc.UpdateCustom(ctx, testTenant, providers.UpdateCustomInput{RefID: "local-models", AuthMode: &auth}); err != nil {
		t.Fatalf("re-enable token: %v", err)
	}
	if _, err := svc.SetProviderToken(ctx, testTenant, "local-models", "sk-test-123"); err != nil {
		t.Fatalf("set token: %v", err)
	}

	// Deletion guard: create a worker version referencing the provider.
	wtx, err := pool.BeginTenantTx(ctx, testTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := wtx.Exec(ctx, `INSERT INTO workers (id, tenant_id, name, slug) VALUES ($1,$2,$3,$4)
		ON CONFLICT (id) DO NOTHING`, "wkr_guard1", testTenant, "Guard Worker", "guard-worker"); err != nil {
		t.Fatalf("insert worker: %v", err)
	}
	if _, err := wtx.Exec(ctx, `INSERT INTO worker_versions (id, tenant_id, worker_id, version, model_ref, status)
		VALUES ($1,$2,$3,1,$4,'published') ON CONFLICT (worker_id, version) DO UPDATE SET model_ref = EXCLUDED.model_ref`, uuid.NewString(), testTenant, "wkr_guard1", "orchicon/local-models/Qwen3.6-35B"); err != nil {
		t.Fatalf("insert worker version: %v", err)
	}
	if err := wtx.Commit(ctx); err != nil {
		t.Fatalf("commit guard fixture: %v", err)
	}
	t.Cleanup(func() {
		ct := context.Background()
		dtx, err := pool.BeginTenantTx(ct, testTenant)
		if err == nil {
			_, _ = dtx.Exec(ct, `DELETE FROM worker_versions WHERE worker_id = 'wkr_guard1' AND tenant_id = $1`, testTenant)
			_, _ = dtx.Exec(ct, `DELETE FROM workers WHERE id = 'wkr_guard1' AND tenant_id = $1`, testTenant)
			_, _ = dtx.Exec(ct, `DELETE FROM provider_settings WHERE tenant_id = $1`, testTenant)
			_, _ = dtx.Exec(ct, `DELETE FROM tenant_secrets WHERE tenant_id = $1 AND name LIKE 'CUSTOM\_%' ESCAPE '\'`, testTenant)
			_ = dtx.Commit(ct)
		}
	})

	err = svc.DeleteCustom(ctx, testTenant, "local-models")
	var refErr *providers.ReferencedError
	if !errors.As(err, &refErr) {
		t.Fatalf("deletion not guarded: %v", err)
	}
	found := false
	for _, w := range refErr.Workers {
		if w.WorkerName == "Guard Worker" && w.ModelRef == "orchicon/local-models/Qwen3.6-35B" {
			found = true
		}
	}
	if !found {
		t.Fatalf("guard did not list referencing worker: %+v", refErr.Workers)
	}

	// The other custom (unreferenced) deletes fine.
	if err := svc.DeleteCustom(ctx, testTenant, "local-models-fast"); err != nil {
		t.Fatalf("delete unreferenced custom: %v", err)
	}
}

func TestProvidersTokenAutoWriteAndResolve(t *testing.T) {
	svc, pool := newProvidersTestService(t)
	ensureTenant(t, pool, testTenant)
	ctx := context.Background()

	// Built-in auto-write under the standard name (commandcode).
	name, err := svc.SetProviderToken(ctx, testTenant, "commandcode", "cc-token-1")
	if err != nil {
		t.Fatalf("set builtin token: %v", err)
	}
	if name != "COMMANDCODE_API_KEY" {
		t.Fatalf("builtin secret name: %q", name)
	}
	// Idempotent re-save: no error, same name.
	name2, err := svc.SetProviderToken(ctx, testTenant, "commandcode", "cc-token-1")
	if err != nil || name2 != name {
		t.Fatalf("idempotent re-save: %v %q", err, name2)
	}
	// Rotate.
	if _, err := svc.SetProviderToken(ctx, testTenant, "commandcode", "cc-token-2"); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	// Ollama rejects token saves (no auth needed).
	if _, err := svc.SetProviderToken(ctx, testTenant, "ollama", "x"); err == nil {
		t.Fatal("ollama token save accepted")
	}

	// End-to-end: the provider layer resolves the tab-written secret.
	profile, ok, err := svc.EffectiveProfile(ctx, testTenant, "commandcode")
	if err != nil || !ok {
		t.Fatalf("effective profile: %v %v", ok, err)
	}
	resolver := orchicon.NewCredentialResolver(pool, []byte(testKEK))
	tok, err := resolver.Resolve(ctx, testTenant, profile)
	if err != nil {
		t.Fatalf("resolve after auto-write: %v", err)
	}
	if tok != "cc-token-2" {
		t.Fatalf("resolved wrong token: %q", tok)
	}

	// Clear: resolution falls back to env; missing both yields the
	// actionable ErrAuthMissing naming the exact secret key.
	if err := svc.ClearProviderToken(ctx, testTenant, "commandcode"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	// The resolver caches by (tenant, name); production drops this cache via
	// registry invalidation when a token changes. Mirror it here.
	resolver.Invalidate()
	if _, err := resolver.Resolve(ctx, testTenant, profile); err == nil {
		t.Fatal("resolve succeeded without secret or env")
	} else if !errors.Is(err, orchicon.ErrAuthMissing) || !contains(err.Error(), "COMMANDCODE_API_KEY") {
		t.Fatalf("error not actionable: %v", err)
	}

	// The resolved secret must never leak into the merged view.
	if _, err := svc.SetProviderToken(ctx, testTenant, "commandcode", "cc-token-3"); err != nil {
		t.Fatalf("re-set: %v", err)
	}
	list, err := svc.ListForTenant(ctx, testTenant)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, en := range list {
		if en.ID == "commandcode" {
			if !en.HasTokenStored || en.TokenSecretName != "COMMANDCODE_API_KEY" {
				t.Fatalf("commandcode entry: %+v", en)
			}
			// Structural leak-guard: the merged view carries only
			// has_token_stored + the secret NAME — the token value itself
			// has no field to ride on (built-in BaseURL is the public
			// catalog URL, not a secret).
		}
	}

	t.Cleanup(func() {
		ct := context.Background()
		dtx, err := pool.BeginTenantTx(ct, testTenant)
		if err == nil {
			_, _ = dtx.Exec(ct, `DELETE FROM tenant_secrets WHERE tenant_id = $1 AND name = 'COMMANDCODE_API_KEY'`, testTenant)
			_ = dtx.Commit(ct)
		}
	})
}

func TestProvidersBuiltInOverrides(t *testing.T) {
	svc, pool := newProvidersTestService(t)
	ensureTenant(t, pool, testTenant)
	ctx := context.Background()

	enabled := false
	base := "http://proxy.internal:8080"
	numCtx := int64(8192)
	e, err := svc.UpdateSettings(ctx, testTenant, providers.UpdateSettingsInput{
		ProviderID: "ollama", Enabled: &enabled, BaseURLOverride: &base, NumCtxDefault: &numCtx,
	})
	if err != nil {
		t.Fatalf("update settings: %v", err)
	}
	if e.Enabled || e.BaseURLOverride != base || e.NumCtxDefault != numCtx {
		t.Fatalf("ollama settings: %+v", e)
	}

	// Partial merge: re-enable keeps the override.
	yes := true
	e, err = svc.UpdateSettings(ctx, testTenant, providers.UpdateSettingsInput{ProviderID: "ollama", Enabled: &yes})
	if err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if !e.Enabled || e.BaseURLOverride != base {
		t.Fatalf("override lost on re-enable: %+v", e)
	}

	// EffectiveProfile carries the override into the substrate profile.
	prof, ok, err := svc.EffectiveProfile(ctx, testTenant, "ollama")
	if err != nil || !ok || prof.BaseURL != base || prof.NumCtxDefault != numCtx {
		t.Fatalf("effective profile: %+v ok=%v err=%v", prof, ok, err)
	}

	// Disable hides the provider from EnabledCustomProviderIDs semantics
	// for customs only; built-ins list still shows the row (read-only).
	list, err := svc.ListForTenant(ctx, testTenant)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, en := range list {
		if en.ID == "ollama" && en.ReadOnly {
			return // built-in rows are read-only entries in the same list
		}
	}
	t.Fatal("ollama not listed as read-only built-in")
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
