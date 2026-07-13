package auth

import (
	"testing"
	"time"
)

func TestTokenRoundTrip(t *testing.T) {
	issuer := NewTokenIssuer("test-key", "orchicon", "orchicon-api", time.Minute, time.Hour)
	tok, err := issuer.IssueAccess("id-1", "tnt_dev", []string{"project:create"}, true)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	claims, err := issuer.VerifyAccess(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.Subject != "id-1" || claims.TenantID != "tnt_dev" || !claims.IsAdmin {
		t.Fatalf("claims mismatch: %+v", claims)
	}
	if len(claims.Entitlements) != 1 || claims.Entitlements[0] != "project:create" {
		t.Fatalf("entitlements mismatch: %v", claims.Entitlements)
	}
}

func TestTokenExpired(t *testing.T) {
	issuer := NewTokenIssuer("k", "orchicon", "a", -time.Minute, time.Hour)
	tok, err := issuer.IssueAccess("id", "tnt", nil, false)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := issuer.VerifyAccess(tok); err == nil {
		t.Fatal("expected expired error")
	}
}

func TestTokenBadSignature(t *testing.T) {
	good := NewTokenIssuer("good-key", "orchicon", "a", time.Minute, time.Hour)
	bad := NewTokenIssuer("bad-key", "orchicon", "a", time.Minute, time.Hour)
	tok, _ := good.IssueAccess("id", "tnt", nil, false)
	if _, err := bad.VerifyAccess(tok); err == nil {
		t.Fatal("expected signature error")
	}
}

func TestRefreshRoundTrip(t *testing.T) {
	issuer := NewTokenIssuer("k", "orchicon", "a", time.Minute, time.Hour)
	tok, _ := issuer.IssueRefresh("id", "tnt")
	claims, err := issuer.VerifyRefresh(tok)
	if err != nil {
		t.Fatalf("verify refresh: %v", err)
	}
	if claims.Subject != "id" || claims.TenantID != "tnt" {
		t.Fatalf("claims mismatch: %+v", claims)
	}
}

func TestApiKeyRoundTrip(t *testing.T) {
	plaintext, prefix, hash := GenerateApiKey()
	if plaintext == "" || hash == "" || prefix == "" {
		t.Fatal("empty key material")
	}
	if !VerifyApiKeyHash(plaintext, hash) {
		t.Fatal("verify failed")
	}
	if VerifyApiKeyHash("wrong", hash) {
		t.Fatal("expected mismatch")
	}
}

func TestResolvedIdentityEntitlements(t *testing.T) {
	r := ResolvedIdentity{Entitlements: []string{"project:create", "worker:*"}, IsAdmin: false}
	if !r.HasEntitlement("project:create") {
		t.Fatal("missing project:create")
	}
	if !r.HasEntitlement("worker:publish") {
		t.Fatal("worker:* should match worker:publish")
	}
	if r.HasEntitlement("policy:supersede") {
		t.Fatal("should not have policy:supersede")
	}
	r.IsAdmin = true
	if !r.HasEntitlement("anything") {
		t.Fatal("admin should pass all")
	}
}
