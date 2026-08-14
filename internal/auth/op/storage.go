package op

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"
)

// DefaultTenantID is the tenant an embedded-OP login binds identities to.
// The OP is served by the control plane itself, which is single-tenant for
// the auth flow; identities resolve within this tenant.
const DefaultTenantID = "tnt_dev"

// IdentityClaims are the OIDC user claims resolved live from Postgres for
// an identity (subject = identity ULID).
type IdentityClaims struct {
	Name     string
	Email    string
	Username string
}

// IdentityResolver resolves an identity by ULID within a tenant into the
// claims an ID token / userinfo response carries. Wired in internal/auth
// to the tenant-scoped DB access layer (db.GetIdentity); a failure makes
// the OP treat the subject as unknown.
type IdentityResolver func(ctx context.Context, tenantID, identityID string) (IdentityClaims, error)

// TTLs bound every piece of in-memory protocol state; the sweeper purges
// expired entries. Auth requests and one-time codes are single-use; opaque
// access tokens live 15 minutes (matching the plane's own AccessTTL).
const (
	authRequestTTL  = 10 * time.Minute
	codeTTL         = 5 * time.Minute
	accessTokenTTL  = 15 * time.Minute
	refreshTokenTTL = 24 * time.Hour
)

// Errors returned by the storage when a lookup misses. The library maps
// them to OIDC error responses.
var (
	ErrAuthRequestNotFound = errors.New("auth/op: auth request not found")
	ErrCodeNotFound        = errors.New("auth/op: code not found")
	ErrAccessTokenNotFound = errors.New("auth/op: access token not found")
	ErrRefreshTokenInvalid = op.ErrInvalidRefreshToken
)

// Storage implements op.Storage entirely in memory with short TTLs + a
// background sweeper. Identity claims resolve live from Postgres via the
// injected resolver — no new tables, no RLS surface.
type Storage struct {
	mu           sync.RWMutex
	authRequests map[string]*authRequest
	codes        map[string]*codeEntry
	accessTokens map[string]*accessToken
	refresh      map[string]*refreshToken

	key      *ecdsa.PrivateKey
	resolve  IdentityResolver
	log      *slog.Logger
	stop     chan struct{}
	stopOnce sync.Once
}

// codeEntry maps a one-time authorization code to the auth request it was
// issued for.
type codeEntry struct {
	authRequestID string
	expiresAt     time.Time
}

// accessToken is the opaque-bearer record the storage mints per flow.
// tokenID (a random id) is the value encrypted inside the opaque access
// token the library hands to the RP; the userinfo endpoint decrypts it
// back and looks the record up here.
type accessToken struct {
	ID        string
	Subject   string
	TenantID  string
	Scopes    []string
	ExpiresAt time.Time
}

// refreshToken is a rotation-free refresh-token record. The embedded OP
// only mints refresh tokens when a client asks for offline_access AND has
// the refresh_token grant registered; the built-in first-party client does
// neither, so this stays unused in practice.
type refreshToken struct {
	Token     string
	Subject   string
	TenantID  string
	Scopes    []string
	ExpiresAt time.Time
}

// NewStorage constructs the in-memory OP storage. resolve must be non-nil.
func NewStorage(key *ecdsa.PrivateKey, resolve IdentityResolver, log *slog.Logger) *Storage {
	if resolve == nil {
		resolve = func(context.Context, string, string) (IdentityClaims, error) {
			return IdentityClaims{}, errors.New("auth/op: identity resolver not configured")
		}
	}
	return &Storage{
		authRequests: make(map[string]*authRequest),
		codes:        make(map[string]*codeEntry),
		accessTokens: make(map[string]*accessToken),
		refresh:      make(map[string]*refreshToken),
		key:          key,
		resolve:      resolve,
		log:          log,
		stop:         make(chan struct{}),
	}
}

// Start launches the expiry sweeper. The library has no Provider.Close(),
// so the sweeper is keyed to a stop channel owned by the Provider wrapper.
func (s *Storage) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.stop:
				return
			case <-ticker.C:
				s.sweep(time.Now())
			}
		}
	}()
}

// Stop terminates the sweeper (idempotent).
func (s *Storage) Stop() { s.stopOnce.Do(func() { close(s.stop) }) }

// sweep purges expired auth requests, codes, and tokens.
func (s *Storage) sweep(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, req := range s.authRequests {
		if req.expiresAt.Before(now) {
			delete(s.authRequests, id)
		}
	}
	for code, e := range s.codes {
		if e.expiresAt.Before(now) {
			delete(s.codes, code)
		}
	}
	for id, t := range s.accessTokens {
		if t.ExpiresAt.Before(now) {
			delete(s.accessTokens, id)
		}
	}
	for id, t := range s.refresh {
		if t.ExpiresAt.Before(now) {
			delete(s.refresh, id)
		}
	}
}

// SigningKey returns the derived ES256 private key.
func (s *Storage) SigningKey(context.Context) (op.SigningKey, error) {
	return &signingKey{s.key}, nil
}

// SignatureAlgorithms advertises ES256 only.
func (s *Storage) SignatureAlgorithms(context.Context) ([]jose.SignatureAlgorithm, error) {
	return []jose.SignatureAlgorithm{jose.ES256}, nil
}

// KeySet returns ONLY the public ECDSA point for /jwks.
func (s *Storage) KeySet(context.Context) ([]op.Key, error) {
	return []op.Key{&publicKey{&s.key.PublicKey}}, nil
}

// --- AuthStorage -----------------------------------------------------------

// CreateAuthRequest stores a parsed + validated authorize request. prompt=none
// is always denied — there is no silent-auth path; every authorize must go
// through the interactive Orchicon-session bridge.
func (s *Storage) CreateAuthRequest(_ context.Context, req *oidc.AuthRequest, userID string) (op.AuthRequest, error) {
	if len(req.Prompt) == 1 && req.Prompt[0] == oidc.PromptNone {
		return nil, oidc.ErrLoginRequired().WithDescription("prompt=none is not supported")
	}
	ar := &authRequest{
		ID:        randToken(16),
		authReq:   req,
		userID:    userID,
		expiresAt: time.Now().Add(authRequestTTL),
	}
	s.mu.Lock()
	s.authRequests[ar.ID] = ar
	s.mu.Unlock()
	return ar, nil
}

// AuthRequestByID returns a stored auth request, or ErrAuthRequestNotFound.
func (s *Storage) AuthRequestByID(_ context.Context, id string) (op.AuthRequest, error) {
	s.mu.RLock()
	req, ok := s.authRequests[id]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrAuthRequestNotFound, id)
	}
	return req, nil
}

// AuthRequestByCode returns the auth request a one-time code was issued
// for, consuming the code on read (replay-safe: a second exchange of the
// same code fails with ErrCodeNotFound).
func (s *Storage) AuthRequestByCode(_ context.Context, code string) (op.AuthRequest, error) {
	s.mu.Lock()
	entry, ok := s.codes[code]
	if ok {
		delete(s.codes, code)
	}
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: code consumed or expired", ErrCodeNotFound)
	}
	return s.AuthRequestByID(context.Background(), entry.authRequestID)
}

// SaveAuthCode records the one-time code against the auth request id.
func (s *Storage) SaveAuthCode(_ context.Context, id, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.codes[code] = &codeEntry{authRequestID: id, expiresAt: time.Now().Add(codeTTL)}
	return nil
}

// DeleteAuthRequest removes the auth request and any of its outstanding
// codes.
func (s *Storage) DeleteAuthRequest(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.authRequests, id)
	for code, e := range s.codes {
		if e.authRequestID == id {
			delete(s.codes, code)
		}
	}
	return nil
}

// CreateAccessToken mints an opaque-bearer access-token record. The library
// encrypts the returned id inside the bearer token handed to the RP.
func (s *Storage) CreateAccessToken(_ context.Context, req op.TokenRequest) (string, time.Time, error) {
	exp := time.Now().Add(accessTokenTTL)
	t := &accessToken{
		ID:        randToken(16),
		Subject:   req.GetSubject(),
		TenantID:  tenantFor(req, DefaultTenantID),
		Scopes:    req.GetScopes(),
		ExpiresAt: exp,
	}
	s.mu.Lock()
	s.accessTokens[t.ID] = t
	s.mu.Unlock()
	return t.ID, exp, nil
}

// CreateAccessAndRefreshTokens is required by the interface but never
// triggered in practice: refresh tokens are only produced when a client
// registers the refresh_token grant AND the request asks for offline_access
// (op.needsRefreshToken). The built-in client does neither and
// GrantTypeRefreshToken is false, so the token endpoint refuses the grant.
// Implemented defensively: an offline_access request gets a refresh token,
// otherwise it degrades to an access-token-only response.
func (s *Storage) CreateAccessAndRefreshTokens(ctx context.Context, req op.TokenRequest, currentRefreshToken string) (accessTokenID, newRefreshToken string, expiration time.Time, err error) {
	accessTokenID, expiration, err = s.CreateAccessToken(ctx, req)
	if err != nil {
		return "", "", time.Time{}, err
	}
	if currentRefreshToken == "" {
		newRefreshToken = randToken(24)
		s.mu.Lock()
		s.refresh[newRefreshToken] = &refreshToken{
			Token:     newRefreshToken,
			Subject:   req.GetSubject(),
			TenantID:  tenantFor(req, DefaultTenantID),
			Scopes:    req.GetScopes(),
			ExpiresAt: time.Now().Add(refreshTokenTTL),
		}
		s.mu.Unlock()
	}
	return accessTokenID, newRefreshToken, expiration, nil
}

// TokenRequestByRefreshToken resolves a refresh token into a refresh request
// (only reachable for the refresh_token grant, which the config refuses).
func (s *Storage) TokenRequestByRefreshToken(_ context.Context, token string) (op.RefreshTokenRequest, error) {
	s.mu.RLock()
	rt, ok := s.refresh[token]
	s.mu.RUnlock()
	if !ok {
		return nil, ErrRefreshTokenInvalid
	}
	if rt.ExpiresAt.Before(time.Now()) {
		return nil, ErrRefreshTokenInvalid
	}
	return &refreshTokenRequest{refreshToken: rt}, nil
}

// TerminateSession revokes the subject's tokens for the client.
func (s *Storage) TerminateSession(_ context.Context, userID, clientID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, t := range s.accessTokens {
		if t.Subject == userID {
			delete(s.accessTokens, id)
		}
	}
	for id, t := range s.refresh {
		if t.Subject == userID {
			delete(s.refresh, id)
		}
	}
	return nil
}

// RevokeToken revokes an access (by tokenID) or refresh (by value) token.
func (s *Storage) RevokeToken(_ context.Context, tokenOrTokenID, userID, clientID string) *oidc.Error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.accessTokens[tokenOrTokenID]; ok {
		delete(s.accessTokens, tokenOrTokenID)
		_ = t
		return nil
	}
	if t, ok := s.refresh[tokenOrTokenID]; ok {
		delete(s.refresh, tokenOrTokenID)
		_ = t
		return nil
	}
	return nil
}

// GetRefreshTokenInfo returns the user/token id for a refresh token. Any
// value that is not a stored refresh token is reported as invalid.
func (s *Storage) GetRefreshTokenInfo(_ context.Context, clientID, token string) (userID, tokenID string, err error) {
	s.mu.RLock()
	rt, ok := s.refresh[token]
	s.mu.RUnlock()
	if !ok {
		return "", "", ErrRefreshTokenInvalid
	}
	return rt.Subject, rt.Token, nil
}

// --- OPStorage -------------------------------------------------------------

// GetClientByClientID returns the registered built-in client.
func (s *Storage) GetClientByClientID(_ context.Context, clientID string) (op.Client, error) {
	c, ok := clients[clientID]
	if !ok {
		return nil, fmt.Errorf("auth/op: unknown client %q", clientID)
	}
	return c, nil
}

// AuthorizeClientIDSecret is never called for the public (PKCE) client.
func (s *Storage) AuthorizeClientIDSecret(_ context.Context, clientID, clientSecret string) error {
	return errors.New("auth/op: client is public, no secret")
}

// SetUserinfoFromScopes is the deprecated path; SetUserinfoFromRequest is
// used instead.
func (s *Storage) SetUserinfoFromScopes(context.Context, *oidc.UserInfo, string, string, []string) error {
	return nil
}

// SetUserinfoFromRequest fills ID-token user claims from the identity,
// gated by the requested scopes.
func (s *Storage) SetUserinfoFromRequest(ctx context.Context, info *oidc.UserInfo, req op.IDTokenRequest, scopes []string) error {
	return s.setUserinfo(ctx, info, req.GetSubject(), tenantFor(req, DefaultTenantID), scopes)
}

// SetUserinfoFromToken fills userinfo-endpoint claims from the opaque
// access token's subject.
func (s *Storage) SetUserinfoFromToken(ctx context.Context, info *oidc.UserInfo, tokenID, subject, origin string) error {
	s.mu.RLock()
	t, ok := s.accessTokens[tokenID]
	s.mu.RUnlock()
	if !ok {
		return ErrAccessTokenNotFound
	}
	if t.ExpiresAt.Before(time.Now()) {
		return errors.New("auth/op: access token expired")
	}
	return s.setUserinfo(ctx, info, t.Subject, t.TenantID, t.Scopes)
}

// SetIntrospectionFromToken is only reachable if /oauth/introspect is
// mounted; it is not, so this returns an error.
func (s *Storage) SetIntrospectionFromToken(context.Context, *oidc.IntrospectionResponse, string, string, string) error {
	return errors.New("auth/op: introspection not supported")
}

// GetPrivateClaimsFromScopes returns no private claims (no custom scopes).
func (s *Storage) GetPrivateClaimsFromScopes(context.Context, string, string, []string) (map[string]any, error) {
	return nil, nil
}

// GetKeyByIDAndClientID is only reachable for JWT-profile client auth,
// which the provider does not support.
func (s *Storage) GetKeyByIDAndClientID(context.Context, string, string) (*jose.JSONWebKey, error) {
	return nil, errors.New("auth/op: private_key_jwt not supported")
}

// ValidateJWTProfileScopes is only reachable for JWT-profile grants, which
// the provider refuses at the client-auth layer.
func (s *Storage) ValidateJWTProfileScopes(_ context.Context, _ string, scopes []string) ([]string, error) {
	return scopes, nil
}

// Health reports the storage is ready.
func (s *Storage) Health(context.Context) error { return nil }

// setUserinfo maps a resolved identity onto the OIDC user claims, honoring
// the scope gates.
func (s *Storage) setUserinfo(ctx context.Context, info *oidc.UserInfo, subject, tenantID string, scopes []string) error {
	claims, err := s.resolve(ctx, tenantID, subject)
	if err != nil {
		return err
	}
	for _, scope := range scopes {
		switch scope {
		case oidc.ScopeOpenID:
			info.Subject = subject
		case oidc.ScopeProfile:
			if claims.Name != "" {
				info.Name = claims.Name
			}
			if claims.Username != "" {
				info.PreferredUsername = claims.Username
			}
		case oidc.ScopeEmail:
			info.Email = claims.Email
			info.EmailVerified = oidc.Bool(true)
		}
	}
	return nil
}

// tenantFor extracts the tenant an embedded auth request / refresh request
// carries; anything else falls back to the default tenant.
func tenantFor(req any, fallback string) string {
	switch r := req.(type) {
	case *authRequest:
		if r.tenantID != "" {
			return r.tenantID
		}
	case *refreshTokenRequest:
		if r.refreshToken != nil && r.refreshToken.TenantID != "" {
			return r.refreshToken.TenantID
		}
	}
	return fallback
}

// randToken returns a URL-safe random token of n bytes.
func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("auth/op: entropy unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
