package op

import (
	"context"
	"testing"

	"github.com/zitadel/oidc/v3/pkg/oidc"
)

// storageWithTenant builds an in-memory storage wired with the given
// deployment tenant id.
func storageWithTenant(t *testing.T, tenantID string) *Storage {
	t.Helper()
	key, err := DeriveES256Key(testSigningKey)
	if err != nil {
		t.Fatalf("DeriveES256Key: %v", err)
	}
	return NewStorage(key, nil, testLogger(), tenantID)
}

// testAuthRequest builds a stored authorize request with a valid authReq
// payload (GetScopes reads authReq.Scopes) but no tenant yet.
func testAuthRequest(id string) *authRequest {
	return &authRequest{
		ID:      id,
		userID:  testIdentityID,
		authReq: &oidc.AuthRequest{Scopes: []string{oidc.ScopeOpenID}},
	}
}

// TestStorageTenantFallbackUsesDeploymentTenant pins the decision-#178
// wiring: an embedded-OP token issued for an auth request that carries no
// explicit tenant resolves into the configured deployment tenant id, not
// the hardcoded DefaultTenantID.
func TestStorageTenantFallbackUsesDeploymentTenant(t *testing.T) {
	s := storageWithTenant(t, "acme")
	req := testAuthRequest("req-1")
	// The storage's CreateAccessToken records the tenant on the minted
	// access-token record via tenantFor(req, s.tenantID).
	id, exp, err := s.CreateAccessToken(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateAccessToken: %v", err)
	}
	s.mu.RLock()
	tok, ok := s.accessTokens[id]
	s.mu.RUnlock()
	if !ok {
		t.Fatal("access token not stored")
	}
	if tok.TenantID != "acme" {
		t.Fatalf("access token tenant = %q, want %q (deployment tenant, not %q)",
			tok.TenantID, "acme", DefaultTenantID)
	}
	if exp.IsZero() {
		t.Fatal("no expiration on the access token")
	}
}

// TestStorageTenantExplicitRequestWins pins that an auth request carrying
// an explicit tenant (e.g. marked by the login bridge) beats the storage
// default — the mapping is a fallback, not an override.
func TestStorageTenantExplicitRequestWins(t *testing.T) {
	s := storageWithTenant(t, "acme")
	req := testAuthRequest("req-2")
	req.MarkAuthenticated(testIdentityID, "other-tnt")
	id, _, err := s.CreateAccessToken(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateAccessToken: %v", err)
	}
	s.mu.RLock()
	tok, ok := s.accessTokens[id]
	s.mu.RUnlock()
	if !ok {
		t.Fatal("access token not stored")
	}
	if tok.TenantID != "other-tnt" {
		t.Fatalf("access token tenant = %q, want %q (explicit request tenant)", tok.TenantID, "other-tnt")
	}
}

// TestStorageTenantEmptyFallsBackToDefault pins the standalone-wiring
// contract: a provider constructed without a deployment tenant id keeps
// the DefaultTenantID behavior (op-package tests / standalone wiring).
func TestStorageTenantEmptyFallsBackToDefault(t *testing.T) {
	s := storageWithTenant(t, "")
	req := testAuthRequest("req-3")
	id, _, err := s.CreateAccessToken(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateAccessToken: %v", err)
	}
	s.mu.RLock()
	tok, ok := s.accessTokens[id]
	s.mu.RUnlock()
	if !ok {
		t.Fatal("access token not stored")
	}
	if tok.TenantID != DefaultTenantID {
		t.Fatalf("access token tenant = %q, want %q", tok.TenantID, DefaultTenantID)
	}
}
