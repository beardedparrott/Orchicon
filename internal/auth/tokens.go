// Package auth implements Orchicon's authentication: token issuance
// (Orchicon access + refresh tokens), token verification (local dev
// HS256 + production OIDC), and identity resolution from a validated
// token into the request context.
//
// In local mode (Issuer="local") the control plane runs a built-in dev
// IdP that mints short-lived HS256 access tokens + refresh tokens with
// no external identity provider — the full auth flow is verifiable
// locally (AGENTS.md verification). In production mode the control
// plane validates OIDC ID tokens from the configured issuer via the
// authorization-code flow (docs/07 §6.1) and issues its own access
// tokens thereafter.
//
// API keys (hashed, scoped entitlements) are a separate machine
// credential path handled by the middleware + AuthService (docs/07 §6.1).
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// readRand fills b with cryptographically secure random bytes. Wrapped
// so apikeys.go and tokens.go share one entropy source.
func readRand(b []byte) (int, error) { return io.ReadFull(rand.Reader, b) }

// AccessClaims are the claims in an Orchicon access token. These are
// the canonical identity + tenant + entitlement context the middleware
// resolves per request (docs/07 §6.3).
type AccessClaims struct {
	Subject      string   `json:"sub"`        // identity id (ULID)
	TenantID     string   `json:"tid"`        // tenant id
	Entitlements []string `json:"ent"`        // granted entitlements
	IsAdmin      bool     `json:"adm,omitempty"`
	TokenType    string   `json:"typ"`        // "access"
	Issuer       string   `json:"iss"`
	Audience     string   `json:"aud"`
	IssuedAt     int64    `json:"iat"`
	ExpiresAt    int64    `json:"exp"`
	JTI          string   `json:"jti"`        // token id (for revocation tracking)
}

// RefreshClaims are the claims in an Orchicon refresh token. Refresh
// tokens are long-lived and stored HttpOnly; they exchange for a new
// access token without re-authenticating the user (docs/10 §7).
type RefreshClaims struct {
	Subject   string `json:"sub"`
	TenantID  string `json:"tid"`
	TokenType string `json:"typ"` // "refresh"
	Issuer    string `json:"iss"`
	Audience  string `json:"aud"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
	JTI       string `json:"jti"`
}

// TokenPair is the access + refresh token pair issued on login or
// refresh. The access token lives in memory (frontend); the refresh
// token lives in an HttpOnly secure cookie (docs/10 §7).
type TokenPair struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64 // seconds until the access token expires
}

// TokenIssuer mints and verifies Orchicon access + refresh tokens. It
// signs tokens with HMAC-SHA256 using the configured signing key. In
// production the signing key MUST be a strong random secret (enforced
// by config.Validate).
type TokenIssuer struct {
	signingKey []byte
	issuer     string
	audience   string
	accessTTL  time.Duration
	refreshTTL time.Duration
}

// NewTokenIssuer constructs a TokenIssuer from auth config.
func NewTokenIssuer(signingKey, issuer, audience string, accessTTL, refreshTTL time.Duration) *TokenIssuer {
	return &TokenIssuer{
		signingKey: []byte(signingKey),
		issuer:     issuer,
		audience:   audience,
		accessTTL:  accessTTL,
		refreshTTL:  refreshTTL,
	}
}

// IssueAccess mints a signed access token for the given identity context.
func (i *TokenIssuer) IssueAccess(subject, tenantID string, entitlements []string, isAdmin bool) (string, error) {
	now := time.Now().UTC()
	claims := AccessClaims{
		Subject:      subject,
		TenantID:     tenantID,
		Entitlements: entitlements,
		IsAdmin:      isAdmin,
		TokenType:    "access",
		Issuer:       i.issuer,
		Audience:     i.audience,
		IssuedAt:     now.Unix(),
		ExpiresAt:    now.Add(i.accessTTL).Unix(),
		JTI:          randID(16),
	}
	return signHS256(claims, i.signingKey)
}

// IssueRefresh mints a signed refresh token.
func (i *TokenIssuer) IssueRefresh(subject, tenantID string) (string, error) {
	now := time.Now().UTC()
	claims := RefreshClaims{
		Subject:   subject,
		TenantID:  tenantID,
		TokenType: "refresh",
		Issuer:    i.issuer,
		Audience:  i.audience,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(i.refreshTTL).Unix(),
		JTI:       randID(16),
	}
	return signHS256(claims, i.signingKey)
}

// IssuePair mints an access + refresh token pair.
func (i *TokenIssuer) IssuePair(subject, tenantID string, entitlements []string, isAdmin bool) (TokenPair, error) {
	access, err := i.IssueAccess(subject, tenantID, entitlements, isAdmin)
	if err != nil {
		return TokenPair{}, fmt.Errorf("auth: issue access: %w", err)
	}
	refresh, err := i.IssueRefresh(subject, tenantID)
	if err != nil {
		return TokenPair{}, fmt.Errorf("auth: issue refresh: %w", err)
	}
	return TokenPair{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresIn:    int64(i.accessTTL / time.Second),
	}, nil
}

// VerifyAccess validates an access token signature + expiry and returns
// the claims. A bad signature, wrong type, or expired token is an error.
func (i *TokenIssuer) VerifyAccess(token string) (AccessClaims, error) {
	var c AccessClaims
	if err := verifyHS256(token, i.signingKey, &c); err != nil {
		return AccessClaims{}, err
	}
	if c.TokenType != "access" {
		return AccessClaims{}, errors.New("auth: not an access token")
	}
	if c.Issuer != i.issuer {
		return AccessClaims{}, errors.New("auth: wrong issuer")
	}
	if c.ExpiresAt < time.Now().Unix() {
		return AccessClaims{}, errors.New("auth: token expired")
	}
	return c, nil
}

// VerifyRefresh validates a refresh token signature + expiry + type.
func (i *TokenIssuer) VerifyRefresh(token string) (RefreshClaims, error) {
	var c RefreshClaims
	if err := verifyHS256(token, i.signingKey, &c); err != nil {
		return RefreshClaims{}, err
	}
	if c.TokenType != "refresh" {
		return RefreshClaims{}, errors.New("auth: not a refresh token")
	}
	if c.Issuer != i.issuer {
		return RefreshClaims{}, errors.New("auth: wrong issuer")
	}
	if c.ExpiresAt < time.Now().Unix() {
		return RefreshClaims{}, errors.New("auth: refresh token expired")
	}
	return c, nil
}

// --- minimal HS256 JWT -----------------------------------------------------

// signHS256 builds a compact HS256 JWT (header.payload.signature) from
// the claims struct. No external JWT library keeps the binary small and
// the trust surface tight.
func signHS256(claims any, key []byte) (string, error) {
	header := map[string]string{"alg": "HS256", "typ": "JWT"}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("auth: marshal header: %w", err)
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("auth: marshal claims: %w", err)
	}
	enc := base64.RawURLEncoding
	h := enc.EncodeToString(hb)
	p := enc.EncodeToString(cb)
	signingInput := h + "." + p
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signingInput))
	sig := enc.EncodeToString(mac.Sum(nil))
	return signingInput + "." + sig, nil
}

// verifyHS256 validates the signature + decodes the payload into dst.
func verifyHS256(token string, key []byte, dst any) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("auth: malformed token")
	}
	signingInput := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signingInput))
	expected := mac.Sum(nil)
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("auth: decode signature: %w", err)
	}
	if !hmac.Equal(expected, sig) {
		return errors.New("auth: invalid signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("auth: decode payload: %w", err)
	}
	if err := json.Unmarshal(payload, dst); err != nil {
		return fmt.Errorf("auth: unmarshal claims: %w", err)
	}
	return nil
}

// randID returns a URL-safe random id of n bytes.
func randID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
