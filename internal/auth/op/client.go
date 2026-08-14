package op

import (
	"time"

	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"
)

// DefaultClientID is the built-in first-party client of the embedded OP.
// It is a PUBLIC (user-agent / SPA) client — no secret — so PKCE is the
// proof of possession at the token endpoint.
const DefaultClientID = "orchicon"

// clients is the registry the storage resolves GetClientByClientID from.
// The provider adds the configured built-in client at construction.
var clients = map[string]op.Client{}

// registerClient stores a client under its id.
func registerClient(c op.Client) { clients[c.GetID()] = c }

// authRequest is the concrete op.AuthRequest held in the in-memory storage.
// The login bridge (handlers.go) marks it done via MarkAuthenticated,
// filling subject (identity ULID), tenant, auth_time and amr.
type authRequest struct {
	ID        string
	authReq   *oidc.AuthRequest
	userID    string
	tenantID  string
	authTime  time.Time
	amr       []string
	done      bool
	expiresAt time.Time
}

func (a *authRequest) GetID() string          { return a.ID }
func (a *authRequest) GetACR() string         { return "" }
func (a *authRequest) GetAMR() []string       { return a.amr }
func (a *authRequest) GetAudience() []string  { return []string{a.authReq.ClientID} }
func (a *authRequest) GetAuthTime() time.Time { return a.authTime }
func (a *authRequest) GetClientID() string    { return a.authReq.ClientID }
func (a *authRequest) GetCodeChallenge() *oidc.CodeChallenge {
	if a.authReq.CodeChallenge == "" {
		return nil
	}
	method := oidc.CodeChallengeMethodPlain
	if a.authReq.CodeChallengeMethod == oidc.CodeChallengeMethodS256 {
		method = oidc.CodeChallengeMethodS256
	}
	return &oidc.CodeChallenge{Challenge: a.authReq.CodeChallenge, Method: method}
}
func (a *authRequest) GetNonce() string                   { return a.authReq.Nonce }
func (a *authRequest) GetRedirectURI() string             { return a.authReq.RedirectURI }
func (a *authRequest) GetResponseType() oidc.ResponseType { return a.authReq.ResponseType }
func (a *authRequest) GetResponseMode() oidc.ResponseMode { return a.authReq.ResponseMode }
func (a *authRequest) GetScopes() []string                { return a.authReq.Scopes }
func (a *authRequest) GetState() string                   { return a.authReq.State }
func (a *authRequest) GetSubject() string                 { return a.userID }
func (a *authRequest) Done() bool                         { return a.done }

// MarkAuthenticated authenticates an auth request for the given identity,
// recording auth_time + amr so the resulting ID token carries them.
func (a *authRequest) MarkAuthenticated(identityID, tenantID string) {
	a.userID = identityID
	a.tenantID = tenantID
	a.authTime = time.Now()
	a.amr = []string{"pwd"}
	a.done = true
}

// refreshTokenRequest implements op.RefreshTokenRequest over a stored
// refresh-token record (only reachable for the refresh_token grant, which
// the provider config refuses).
type refreshTokenRequest struct {
	refreshToken *refreshToken
}

func (r *refreshTokenRequest) GetAMR() []string       { return []string{"pwd"} }
func (r *refreshTokenRequest) GetAudience() []string  { return []string{DefaultClientID} }
func (r *refreshTokenRequest) GetAuthTime() time.Time { return time.Now() }
func (r *refreshTokenRequest) GetClientID() string    { return DefaultClientID }
func (r *refreshTokenRequest) GetScopes() []string    { return r.refreshToken.Scopes }
func (r *refreshTokenRequest) GetSubject() string     { return r.refreshToken.Subject }
func (r *refreshTokenRequest) SetCurrentScopes(scopes []string) {
	r.refreshToken.Scopes = scopes
}

// client is the built-in public (PKCE) SPA client of the embedded OP.
type client struct {
	id           string
	redirectURIs []string
	devMode      bool
	// idTokenTTL bounds how long ID tokens minted for this client stay
	// valid; short, matching the plane's AccessTTL philosophy.
	idTokenTTL time.Duration
}

func (c *client) GetID() string                       { return c.id }
func (c *client) RedirectURIs() []string              { return c.redirectURIs }
func (c *client) PostLogoutRedirectURIs() []string    { return nil }
func (c *client) ApplicationType() op.ApplicationType { return op.ApplicationTypeUserAgent }
func (c *client) AuthMethod() oidc.AuthMethod         { return oidc.AuthMethodNone }
func (c *client) ResponseTypes() []oidc.ResponseType {
	return []oidc.ResponseType{oidc.ResponseTypeCode}
}
func (c *client) GrantTypes() []oidc.GrantType        { return []oidc.GrantType{oidc.GrantTypeCode} }
func (c *client) LoginURL(id string) string           { return "/auth/op/login?id=" + id }
func (c *client) AccessTokenType() op.AccessTokenType { return op.AccessTokenTypeBearer }
func (c *client) IDTokenLifetime() time.Duration      { return c.idTokenTTL }
func (c *client) DevMode() bool                       { return c.devMode }
func (c *client) RestrictAdditionalIdTokenScopes() func(scopes []string) []string {
	return func(scopes []string) []string { return scopes }
}
func (c *client) RestrictAdditionalAccessTokenScopes() func(scopes []string) []string {
	return func(scopes []string) []string { return scopes }
}
func (c *client) IsScopeAllowed(scope string) bool     { return false }
func (c *client) IDTokenUserinfoClaimsAssertion() bool { return true }
func (c *client) ClockSkew() time.Duration             { return 5 * time.Second }
