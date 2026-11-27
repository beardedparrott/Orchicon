package server

// antigravity_settings.go — the ONE resolution of a tenant's Antigravity spawn settings.
//
// WHAT IT DOES, AND WHY IT IS NOT INLINE IN server.go. The bridge needs three things per launch: the auth
// METHOD (an operator setting that decides which `authenticate {methodId}` agy is sent AND whether an API key
// is required at all), the telemetry preference (which decides `--disable-telemetry`), and — only for the
// gemini-api-key method — the tenant's decrypted GEMINI_API_KEY secret.
//
// internal/runtime/antigravity_auth.go resolves the SAME settings for the CONTAINER create request, and it
// deliberately DEGRADES on every failure because the daemon is the authority for a container. This one must
// NOT degrade, and that is the difference worth spelling out: an in-process worker launched with a method it
// cannot authenticate, or with an empty API key, spends its whole budget reaching the first model call and then
// fails with agy's own text. Failing here instead puts the actionable message in front of the operator before
// the child is spawned, which is what adapter.AntigravityAuthRequiredMessage exists for.
//
// THE PLAINTEXT KEY NEVER LEAVES THIS CALL. It is returned to the bridge, which puts it on the CHILD's
// environment and nowhere else: not on argv (procspec.go), not in a log line (SpawnSpec.LogValue redacts it),
// and not in the session transcript.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/beardedparrott/orchicon/internal/antigravity"
	"github.com/beardedparrott/orchicon/internal/antigravity/authmethod"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/secretcrypto"
)

// resolveAntigravitySpawn reads the tenant's Antigravity settings and, for the API-key method, decrypts the
// GEMINI_API_KEY secret.
func resolveAntigravitySpawn(ctx context.Context, pool *db.Pool, secretsKEK []byte, tenantID string) (antigravity.SpawnSettings, string, error) {
	// No tenant is a WIRING fault rather than a settings one, and it is worth its own message: without a tenant
	// there is no settings row to read and no secret to decrypt.
	if strings.TrimSpace(tenantID) == "" {
		return antigravity.SpawnSettings{}, "", fmt.Errorf("antigravity: the execution has no tenant, so its " +
			"Antigravity settings cannot be read")
	}
	if pool == nil {
		return antigravity.SpawnSettings{}, "", fmt.Errorf("antigravity: no database pool is wired, so the " +
			"tenant's Antigravity settings cannot be read")
	}

	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return antigravity.SpawnSettings{}, "", fmt.Errorf("antigravity: open the tenant transaction: %w", err)
	}
	defer ttx.Rollback(ctx)

	row, err := db.GetTenantSettings(ctx, ttx.Tx, tenantID)
	if err != nil {
		return antigravity.SpawnSettings{}, "", fmt.Errorf("antigravity: read the tenant's settings: %w", err)
	}

	method := authmethod.Normalize(row.AntigravityAuthMethod)
	settings := antigravity.SpawnSettings{
		AuthMethod:     method,
		ShareTelemetry: row.AntigravityShareTelemetry,
	}
	if !authmethod.NeedsAPIKey(method) {
		return settings, "", nil
	}

	// THE API-KEY METHOD NEEDS THE SECRET, and every way it can be missing is a refusal rather than a launch
	// that will fail at the first model call.
	sec, err := db.GetSecretByName(ctx, ttx.Tx, tenantID, authmethod.APIKeySecretName)
	if err != nil {
		return antigravity.SpawnSettings{}, "", fmt.Errorf(
			"antigravity: the %q auth method is selected but the tenant has no %s secret — create a tenant "+
				"secret named %s, or switch the method to %q",
			method, authmethod.APIKeySecretName, authmethod.APIKeySecretName, authmethod.MethodOAuthPersonal)
	}
	if len(secretsKEK) != 32 {
		return antigravity.SpawnSettings{}, "", fmt.Errorf(
			"antigravity: the plane has no secrets KEK, so the %s secret cannot be decrypted — set "+
				"ORCHICON_SECRETS_KEK on the plane", authmethod.APIKeySecretName)
	}
	plaintext, err := secretcrypto.Decrypt(sec.Ciphertext, secretsKEK)
	if err != nil {
		// NEVER include the ciphertext, the key or the plaintext in this message.
		return antigravity.SpawnSettings{}, "", fmt.Errorf(
			"antigravity: the %s secret could not be decrypted (the plane's secrets KEK does not match the key "+
				"it was sealed with)", authmethod.APIKeySecretName)
	}
	key := strings.TrimSpace(string(plaintext))
	if key == "" {
		return antigravity.SpawnSettings{}, "", fmt.Errorf(
			"antigravity: the %s secret is set but empty — the %q method needs a real Gemini API key",
			authmethod.APIKeySecretName, method)
	}
	return settings, key, nil
}

// resolveAntigravityAskProjectDir resolves an Ask conversation's project to its directory.
//
// IT IS THE ASK PATH'S CWD, and therefore the boundary the mode gate and the platform's permission rules are
// judged against — so a wrong answer here is a conversation that appears to work while writing into the
// wrong tree. That is why it is a resolver the SERVER owns (the adapter has no pool) and why it FAILS LOUD
// rather than degrading to a temp directory: antigravity.Bridge falls back to the per-conversation ask
// directory only when the resolver returns "" with no error, i.e. when the conversation genuinely has no
// project.
func resolveAntigravityAskProjectDir(ctx context.Context, pool *db.Pool, tenantID, projectID string) (string, error) {
	if strings.TrimSpace(projectID) == "" {
		return "", nil // an unassigned conversation: the adapter uses its own ask directory
	}
	if pool == nil {
		return "", errors.New("antigravity ask: the plane has no database pool, so a project directory cannot be resolved")
	}
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return "", fmt.Errorf("antigravity ask: project %s: %w", projectID, err)
	}
	defer ttx.Rollback(ctx)

	proj, err := db.GetProject(ctx, ttx.Tx, tenantID, projectID)
	if err != nil {
		return "", fmt.Errorf("antigravity ask: project %s: %w", projectID, err)
	}
	dir := strings.TrimSpace(proj.ProjectDir)
	if dir == "" {
		// A PROJECT WITH NO DIRECTORY IS NOT AN ERROR THE OPERATOR CANNOT SEE: the adapter logs it and runs in
		// the ask directory, which is the honest outcome for a project that has not been provisioned yet.
		return "", nil
	}
	return dir, nil
}
