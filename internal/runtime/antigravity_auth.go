package runtime

// antigravity_auth.go — resolving the tenant-side Antigravity settings into the create request.
//
// THE METHOD IS A TENANT SETTING, and the daemon cannot read it: the daemon is a separate process
// that answers POST /v1/runtimes and is given a CreateRequest. So the plane resolves the setting
// here and puts it ON the request, next to the boot profile it already sends.
//
// THE API KEY IS RESOLVED THE SAME WAY, and for the same reason. The gemini-api-key method names
// the secret it needs (GEMINI_API_KEY), and a work item only carries the secrets it explicitly
// selects — so without this a tenant-level method would do nothing on any item that had not been
// hand-wired. It is decrypted plane-side and travels as the container's environment like every
// other secret; it is never logged, and the spawn spec puts it on the CHILD's environment only.

import (
	"context"

	"github.com/beardedparrott/orchicon/internal/antigravity/authmethod"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/secretcrypto"
)

// antigravityAuthFor returns the tenant's Antigravity auth method (normalized, never "") and, for
// the gemini-api-key method, the decrypted GEMINI_API_KEY secret. Every failure path degrades to
// (method, "") rather than failing the run: the DAEMON is the authority on whether the method can
// work in a container, and it refuses actionably. Swallowing the error here would hide a missing
// secret behind a generic ensure-runtime failure.
func (l *Lifecycle) antigravityAuthFor(ctx context.Context, tenantID string) (string, string) {
	method := authmethod.Default
	if tenantID == "" {
		return method, ""
	}
	ttx, err := l.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		l.warn("antigravity: read tenant settings", err, tenantID)
		return method, ""
	}
	defer ttx.Rollback(ctx)
	row, err := db.GetTenantSettings(ctx, ttx.Tx, tenantID)
	if err != nil {
		l.warn("antigravity: read tenant settings row", err, tenantID)
		return method, ""
	}
	method = authmethod.Normalize(row.AntigravityAuthMethod)
	if !authmethod.NeedsAPIKey(method) {
		return method, ""
	}
	sec, err := db.GetSecretByName(ctx, ttx.Tx, tenantID, authmethod.APIKeySecretName)
	if err != nil {
		l.warn("antigravity: the gemini-api-key method is selected but the tenant has no "+authmethod.APIKeySecretName+" secret", err, tenantID)
		return method, ""
	}
	if len(l.secretsKEK) != 32 {
		l.warn("antigravity: the plane has no secrets KEK, so the "+authmethod.APIKeySecretName+" secret cannot be decrypted", nil, tenantID)
		return method, ""
	}
	plaintext, err := secretcrypto.Decrypt(sec.Ciphertext, l.secretsKEK)
	if err != nil {
		// NEVER include the ciphertext, the key or the plaintext in this message.
		l.warn("antigravity: decrypt the "+authmethod.APIKeySecretName+" secret", err, tenantID)
		return method, ""
	}
	return method, string(plaintext)
}

// warn is the lifecycle logger with nil-safety: a Lifecycle built without a logger (tests) must
// not panic while reporting a degraded antigravity setting.
func (l *Lifecycle) warn(msg string, err error, tenantID string) {
	if l.log == nil {
		return
	}
	if err != nil {
		l.log.Warn(msg, "tenant", tenantID, "error", err)
		return
	}
	l.log.Warn(msg, "tenant", tenantID)
}
