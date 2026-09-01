package orchicon

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/secretcrypto"
)

// CredentialResolver resolves provider credentials (D7), in order:
//
//  1. tenant secret (profile's AuthSecretRef secret NAME, looked up by name
//     and KEK-decrypted),
//  2. host environment (profile's AuthEnv var, e.g. ANTHROPIC_API_KEY),
//  3. actionable failure naming the exact secret/env expected.
//
// No credential is ever baked into images, seed data, or defaults.
type CredentialResolver struct {
	pool *db.Pool
	kek  []byte
	// Env is the env lookup (nil = os.Getenv); test hook.
	Env func(string) string

	mu    sync.Mutex
	cache map[string]string // tenantID|name → plaintext (process-lifetime; secrets rotate via instance invalidation)
}

// NewCredentialResolver wires the resolver to the DB pool and KEK. kek may
// be nil — env-only resolution still works (fail-closed on secret path,
// mirroring the secrets service).
func NewCredentialResolver(pool *db.Pool, kek []byte) *CredentialResolver {
	return &CredentialResolver{pool: pool, kek: kek, cache: map[string]string{}}
}

// Resolve returns the credential for the profile. A profile with no auth
// requirement (AuthEnv == "" and AuthSecretRef == "") resolves to "" with
// no error (e.g. local Ollama).
func (r *CredentialResolver) Resolve(ctx context.Context, tenantID string, p Profile) (string, error) {
	if p.AuthSecretRef == "" && p.AuthEnv == "" {
		return "", nil
	}
	if p.AuthSecretRef != "" {
		v, err := r.resolveSecret(ctx, tenantID, p.AuthSecretRef)
		if err == nil && v != "" {
			return v, nil
		}
		// fall through to env on not-found/decrypt failure; a KEK failure is
		// surfaced as a wrapping error but still falls back (env may be set).
	}
	if p.AuthEnv != "" {
		if v := r.getenv(p.AuthEnv); v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("%w: no credential resolved for provider %q: set tenant secret %q or environment variable %q",
		ErrAuthMissing, p.ID, expectedSecretName(p), expectedEnvName(p))
}

// expectedSecretName names what the operator should store (the AuthSecretRef
// if set, else the canonical secret name derived from the env var).
func expectedSecretName(p Profile) string {
	if p.AuthSecretRef != "" {
		return p.AuthSecretRef
	}
	if p.AuthEnv != "" {
		return p.AuthEnv // canonical secret names match env var names
	}
	return "(none)"
}

// expectedEnvName names the env var that would satisfy the profile.
func expectedEnvName(p Profile) string {
	if p.AuthEnv != "" {
		return p.AuthEnv
	}
	return "(none)"
}

func (r *CredentialResolver) getenv(k string) string {
	if r.Env != nil {
		return r.Env(k)
	}
	return os.Getenv(k)
}

// resolveSecret looks a tenant secret up BY NAME and decrypts it. The db
// layer gains GetSecretByName for this; decryption rides secretcrypto with
// the tenant KEK, fail-closed (mirrors internal/secrets.Service).
func (r *CredentialResolver) resolveSecret(ctx context.Context, tenantID, name string) (string, error) {
	key := tenantID + "|" + name
	r.mu.Lock()
	v, ok := r.cache[key]
	r.mu.Unlock()
	if ok {
		return v, nil
	}
	if r.pool == nil || len(r.kek) != 32 {
		return "", fmt.Errorf("secrets store unavailable: KEK not configured")
	}
	tx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	row, err := db.GetSecretByName(ctx, tx.Tx, tenantID, name)
	if err != nil {
		return "", err
	}
	pt, err := secretcrypto.Decrypt(row.Ciphertext, r.kek)
	if err != nil {
		return "", fmt.Errorf("decrypt secret %s: %w", name, err)
	}
	_ = tx.Commit(ctx)
	val := string(pt)
	r.mu.Lock()
	r.cache[key] = val
	r.mu.Unlock()
	return val, nil
}

// Invalidate drops cached resolutions (called on settings change / secret
// rotation; the registry calls it through instance invalidation).
func (r *CredentialResolver) Invalidate() {
	r.mu.Lock()
	r.cache = map[string]string{}
	r.mu.Unlock()
}
