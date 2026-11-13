package mcpsettings_test

// embedded_secret_refs_test.go — BUG 1: a ${SECRET_NAME} embedded in a larger value.
//
// `Authorization: Bearer ${TOKEN}` is the canonical shape in essentially every remote MCP server's
// documentation, and the platform used to recognise a reference only when it was the ENTIRE value. The
// literal string went to the server (401 "Invalid API token" — a bad-credential looking failure for a
// correct config) and has_secret_stored reported false for a secret that existed.
//
// These tests pin the four surfaces that read the reference, all of which shared the one predicate:
//
//	ResolveSecretRefs     the expansion the client actually sends
//	RequiredSecretsFor    what the entry DECLARES it needs
//	anySecretStored       the has_secret_stored view (through Service.Get)
//	validateSecretRefs    the write gate (refused when the secret does not exist)
//
// Guarded by ORCHICON_TEST_DSN like the rest of the package.

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/mcpsettings"
	"github.com/beardedparrott/orchicon/internal/secretcrypto"
)

// TestMCPEmbeddedSecretRefInAHeader is the operator's own case, end to end through the service: a
// streamable-http entry whose Authorization header embeds the reference.
func TestMCPEmbeddedSecretRefInAHeader(t *testing.T) {
	svc, pool := newTestService(t)
	ctx := context.Background()
	cleanupMCP(t, pool)
	projID, _ := seedOwner(t, pool)

	// The secret is stored FIRST under the name the entry will reference, the way an operator sets one
	// up (the tenant secrets store), so the write gate has something to find.
	if _, err := storeTestSecret(t, pool, "PIXELLAB_API_KEY", "sk-live-token"); err != nil {
		t.Fatalf("store secret: %v", err)
	}

	e, err := svc.Create(ctx, testTenant, mcpsettings.CreateInput{
		Name:      "pixellab",
		ProjectID: projID,
		Transport: mcpsettings.TransportStreamable,
		URL:       "https://api.pixellab.ai/mcp",
		Headers:   map[string]string{"Authorization": "Bearer ${PIXELLAB_API_KEY}"},
		Enabled:   true,
	})
	if err != nil {
		t.Fatalf("create with an embedded reference was REFUSED: %v — this is the documented shape every remote server uses", err)
	}

	// REQUIRED SECRETS: the entry must say it needs PIXELLAB_API_KEY.
	req := e.RequiredSecrets
	if !containsStr(req, "PIXELLAB_API_KEY") {
		t.Fatalf("required_secrets = %v, want PIXELLAB_API_KEY — collecting names from an embedded reference is the same scan", req)
	}

	// HAS SECRET STORED: true, because the referenced secret exists.
	got, err := svc.Get(ctx, testTenant, e.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.HasSecretStored {
		t.Fatalf("has_secret_stored = false for an entry whose referenced secret EXISTS (the operator's exact symptom)")
	}

	// THE EXPANSION: "Bearer ${PIXELLAB_API_KEY}" → "Bearer sk-live-token", surrounding text intact.
	env, headers, err := mcpsettings.ResolveSecretRefs(ctx, pool, []byte(testKEK), testTenant, e.Env, e.Headers)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	_ = env
	if want := "Bearer sk-live-token"; headers["Authorization"] != want {
		t.Fatalf("Authorization = %q, want %q — an embedded reference must be expanded in place, not passed through",
			headers["Authorization"], want)
	}
	if strings.Contains(headers["Authorization"], "${") {
		t.Fatalf("the literal ${...} reached the header, which is exactly what produces a 401 from the far end: %q",
			headers["Authorization"])
	}
}

// TestMCPEmbeddedSecretRefExpansionCases pins the expansion rule itself: several references in one
// value, mixed with literal text, and the value that carries none.
func TestMCPEmbeddedSecretRefExpansionCases(t *testing.T) {
	svc, pool := newTestService(t)
	_ = svc
	ctx := context.Background()
	cleanupMCP(t, pool)
	projID, _ := seedOwner(t, pool)

	if _, err := storeTestSecret(t, pool, "A_TOKEN", "aaa"); err != nil {
		t.Fatalf("store A_TOKEN: %v", err)
	}
	if _, err := storeTestSecret(t, pool, "B_TOKEN", "bbb"); err != nil {
		t.Fatalf("store B_TOKEN: %v", err)
	}
	e, err := svc.Create(ctx, testTenant, mcpsettings.CreateInput{
		Name:      "multi",
		ProjectID: projID,
		Transport: mcpsettings.TransportStreamable,
		URL:       "https://mcp.example.test/sse",
		Headers: map[string]string{
			"Authorization": "Bearer ${A_TOKEN}",
			"X-Pair":        "${A_TOKEN}:${B_TOKEN}",
			"X-Whole":       "${B_TOKEN}",
			"X-Literal":     "no-reference-here",
			"X-Braces":      "cost=${5}", // an identifier-shaped name is required: ordinary text stays
		},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	sortStrings(e.RequiredSecrets)
	for _, want := range []string{"A_TOKEN", "B_TOKEN"} {
		if !containsStr(e.RequiredSecrets, want) {
			t.Fatalf("required_secrets = %v, want %q from an embedded reference", e.RequiredSecrets, want)
		}
	}
	if len(e.RequiredSecrets) != 2 {
		t.Fatalf("required_secrets = %v, want exactly A_TOKEN and B_TOKEN — `${5}` is not a reference", e.RequiredSecrets)
	}

	_, headers, err := mcpsettings.ResolveSecretRefs(ctx, pool, []byte(testKEK), testTenant, nil, e.Headers)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for k, want := range map[string]string{
		"Authorization": "Bearer aaa",
		"X-Pair":        "aaa:bbb",
		"X-Whole":       "bbb",
		"X-Literal":     "no-reference-here",
		"X-Braces":      "cost=${5}", // untouched: not an identifier-shaped name
	} {
		if headers[k] != want {
			t.Errorf("%s = %q, want %q", k, headers[k], want)
		}
	}
}

// TestMCPEmbeddedSecretRefMustExist is the write gate: an embedded reference to a secret nobody stored
// is refused when the definition is saved, exactly as the whole-value form always was. Without this the
// failure would move to the far end (a 401) instead of the save.
func TestMCPEmbeddedSecretRefMustExist(t *testing.T) {
	svc, pool := newTestService(t)
	ctx := context.Background()
	cleanupMCP(t, pool)
	projID, _ := seedOwner(t, pool)

	_, err := svc.Create(ctx, testTenant, mcpsettings.CreateInput{
		Name:      "bad-ref",
		ProjectID: projID,
		Transport: mcpsettings.TransportStreamable,
		URL:       "https://mcp.example.test/sse",
		Headers:   map[string]string{"Authorization": "Bearer ${NEVER_STORED_ANYWHERE}"},
		Enabled:   true,
	})
	if err == nil {
		t.Fatal("a create whose header embeds a reference to an unstored secret was accepted — " +
			"the definition would save and then fail every session that resolved it")
	}
	for _, want := range []string{"NEVER_STORED_ANYWHERE", "header"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name %q: %v", want, err)
		}
	}
}

// TestMCPEnvEmbeddedSecretRefResolves covers the stdio half: env values embed references too (a
// connection string is the common case).
func TestMCPEnvEmbeddedSecretRefResolves(t *testing.T) {
	svc, pool := newTestService(t)
	ctx := context.Background()
	cleanupMCP(t, pool)
	projID, _ := seedOwner(t, pool)

	if _, err := storeTestSecret(t, pool, "PG_PASSWORD", "hunter2"); err != nil {
		t.Fatalf("store secret: %v", err)
	}
	e, err := svc.Create(ctx, testTenant, mcpsettings.CreateInput{
		Name:      "postgres",
		ProjectID: projID,
		Transport: mcpsettings.TransportStdio,
		Command:   "npx",
		Args:      []string{"-y", "@modelcontextprotocol/server-postgres"},
		Env:       map[string]string{"DATABASE_URL": "postgres://app:${PG_PASSWORD}@db.internal:5432/app"},
		Enabled:   true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !e.HasSecretStored {
		// Create does not compute the stored view; Get does. Assert through Get, which is the read path.
		got, gerr := svc.Get(ctx, testTenant, e.ID)
		if gerr != nil {
			t.Fatalf("get: %v", gerr)
		}
		if !got.HasSecretStored {
			t.Fatal("has_secret_stored = false for an env value embedding an existing secret")
		}
	}
	env, _, err := mcpsettings.ResolveSecretRefs(ctx, pool, []byte(testKEK), testTenant, e.Env, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if want := "postgres://app:hunter2@db.internal:5432/app"; env["DATABASE_URL"] != want {
		t.Fatalf("DATABASE_URL = %q, want %q", env["DATABASE_URL"], want)
	}
}

// --- helpers -----------------------------------------------------------------

// storeTestSecret writes a tenant secret through the store the expansion actually decrypts from — a
// REAL ciphertext under the suite's KEK, because these tests assert on the expanded plaintext rather
// than on existence alone.
func storeTestSecret(t *testing.T, pool *db.Pool, name, value string) (string, error) {
	t.Helper()
	ct, err := secretcrypto.Encrypt([]byte(value), []byte(testKEK))
	if err != nil {
		return "", err
	}
	ctx := context.Background()
	tx, err := pool.BeginTenantTx(ctx, testTenant)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := db.CreateSecret(ctx, tx.Tx, db.SecretRow{
		ID: db.NewID(), TenantID: testTenant, Name: name,
		Description: "seeded for the embedded-reference tests", Ciphertext: ct, KeyVersion: 1,
	}); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return name, nil
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func sortStrings(s []string) { sort.Strings(s) }
