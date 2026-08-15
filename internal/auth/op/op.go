package op

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	zitadelop "github.com/zitadel/oidc/v3/pkg/op"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/version"
)

// Config carries the provider construction options.
type Config struct {
	// SigningKey is the auth HMAC signing key the ES256 OP keys are
	// derived from (ORCHICON_AUTH_SIGNING_KEY).
	SigningKey string
	// ClientID is the built-in client id (defaults to DefaultClientID).
	ClientID string
	// RedirectURIs are the registered redirect URIs for the built-in
	// client. Empty falls back to [config.DefaultOPRedirectURIs] at the
	// wire layer.
	RedirectURIs []string
	// OPIssuer pins a static issuer; empty derives it from the request
	// Host on every request.
	OPIssuer string
	// TenantID is the deployment tenant id (ORCHICON_DEPLOYMENT_TENANT_ID)
	// identities resolve into when an auth/refresh request carries no
	// explicit tenant. Empty falls back to DefaultTenantID ("tnt_dev"),
	// which keeps standalone OP wiring and op-package tests unchanged.
	TenantID string
	// AllowInsecure permits an http:// issuer (local mode only).
	AllowInsecure bool
	// Identity resolves identity claims (sub = identity ULID).
	Identity IdentityResolver
	// Pool is the tenant-scoped DB pool used to resolve identities when
	// Identity is nil (falls back to db.GetIdentity in the deployment
	// tenant).
	Pool *db.Pool
	// Log is the structured logger.
	Log *slog.Logger
}

// Provider wraps the zitadel OpenID Provider with the plane's wiring:
// endpoint paths, the in-memory storage, and the login bridge. It serves
// only the six OP paths (see Register); every other path 404s so the
// plane's own /healthz etc. are never shadowed.
type Provider struct {
	*zitadelop.Provider
	storage *Storage
	client  *client
	cancel  context.CancelFunc
}

// NewProvider builds the embedded OP. The provider implements http.Handler;
// mount it with Register. Call Close on shutdown to stop the sweeper.
func NewProvider(cfg Config) (*Provider, error) {
	signingKey := cfg.SigningKey
	if signingKey == "" {
		signingKey = "orchicon-dev-signing-key-change-in-production"
	}
	key, err := DeriveES256Key(signingKey)
	if err != nil {
		return nil, err
	}
	encKey := DeriveCryptoKey(signingKey)

	resolve := cfg.Identity
	if resolve == nil {
		resolve = func(ctx context.Context, tenantID, identityID string) (IdentityClaims, error) {
			if cfg.Pool == nil {
				return IdentityClaims{}, context.DeadlineExceeded
			}
			ttx, err := cfg.Pool.BeginTenantTx(ctx, tenantID)
			if err != nil {
				return IdentityClaims{}, err
			}
			defer ttx.Rollback(ctx)
			row, err := db.GetIdentity(ctx, ttx.Tx, tenantID, identityID)
			if err != nil {
				return IdentityClaims{}, err
			}
			return claimsFromIdentity(row), nil
		}
	}

	clientID := cfg.ClientID
	if clientID == "" {
		clientID = DefaultClientID
	}
	redirectURIs := cfg.RedirectURIs
	if len(redirectURIs) == 0 {
		redirectURIs = []string{"http://localhost:5173/auth/callback"}
	}

	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}

	opCfg := &zitadelop.Config{
		CryptoKey:             encKey,
		CryptoKeyId:           kid,
		CodeMethodS256:        true,
		GrantTypeRefreshToken: false,
		SupportedScopes:       []string{"openid", "profile", "email"},
		SupportedClaims:       []string{"sub", "email", "email_verified", "name", "preferred_username"},
	}

	var issuer func(bool) (zitadelop.IssuerFromRequest, error)
	if cfg.OPIssuer != "" {
		issuer = zitadelop.StaticIssuer(cfg.OPIssuer)
	} else {
		issuer = zitadelop.IssuerFromHost("")
	}

	opts := []zitadelop.Option{
		zitadelop.WithCustomTokenEndpoint(zitadelop.NewEndpoint("token")),
		zitadelop.WithCustomKeysEndpoint(zitadelop.NewEndpoint("jwks")),
		zitadelop.WithCustomUserinfoEndpoint(zitadelop.NewEndpoint("userinfo")),
	}
	if cfg.AllowInsecure {
		opts = append(opts, zitadelop.WithAllowInsecure())
	}

	storage := NewStorage(key, resolve, log, cfg.TenantID)
	builtin := &client{
		id:           clientID,
		redirectURIs: redirectURIs,
		devMode:      cfg.AllowInsecure,
		idTokenTTL:   5 * time.Minute,
	}
	registerClient(builtin)

	p, err := zitadelop.NewProvider(opCfg, storage, issuer, opts...)
	if err != nil {
		return nil, err
	}
	sweepCtx, cancel := context.WithCancel(context.Background())
	storage.Start(sweepCtx)

	log.Info("embedded OpenID Provider enabled",
		"version", version.Current().Tag,
		"issuer", func() string {
			if cfg.OPIssuer != "" {
				return cfg.OPIssuer
			}
			return "(dynamic from request Host)"
		}(),
		"alg", "ES256", "kid", kid)

	return &Provider{Provider: p, storage: storage, client: builtin, cancel: cancel}, nil
}

// Close stops the storage sweeper (the library has no Provider.Close).
func (p *Provider) Close() {
	if p.cancel != nil {
		p.cancel()
	}
	p.storage.Stop()
}

// opPaths are the only endpoints the provider is allowed to serve. The
// zitadel router also mounts /healthz, /ready, /oauth/introspect, /revoke,
// /end_session and /device_authorization — mounting the router at "/" would
// shadow the plane's /healthz. The wrapper 404s everything outside this
// list so no dead surface is reachable.
var opPaths = map[string]bool{
	"/.well-known/openid-configuration": true,
	"/authorize":                        true,
	"/authorize/callback":               true,
	"/token":                            true,
	"/userinfo":                         true,
	"/jwks":                             true,
}

// ServeHTTP gates the zitadel router to the six OP paths.
func (p *Provider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !opPaths[r.URL.Path] {
		http.NotFound(w, r)
		return
	}
	p.Provider.ServeHTTP(w, r)
}

// Register mounts the six OP endpoints on the plane mux.
func (p *Provider) Register(mux *http.ServeMux) {
	for path := range opPaths {
		mux.Handle(path, p)
	}
}

// AuthRequest is the stored authorize request (op.AuthRequest). The login
// bridge reads it by id and marks it authenticated.
type AuthRequest = zitadelop.AuthRequest

// MarkAuthenticated authenticates a stored authorize request for the
// identity the caller resolved from the Orchicon session, recording
// auth_time + amr. The bridge redirects to /authorize/callback?id=<id>
// only after this succeeds.
func (p *Provider) MarkAuthenticated(ctx context.Context, id, identityID, tenantID string) error {
	req, err := p.storage.AuthRequestByID(ctx, id)
	if err != nil {
		return err
	}
	ar, ok := req.(*authRequest)
	if !ok {
		return errors.New("auth/op: unexpected auth request type")
	}
	ar.MarkAuthenticated(identityID, tenantID)
	return nil
}

// claimsFromIdentity maps a tenant-scoped identity row onto the OIDC
// claims the OP carries (sub = identity ULID — non-PII, stable across
// subject renames).
func claimsFromIdentity(row db.IdentityRow) IdentityClaims {
	c := IdentityClaims{Name: row.DisplayName, Username: row.DisplayName}
	if row.Subject != "" && containsAt(row.Subject) {
		c.Email = row.Subject
	}
	return c
}

func containsAt(s string) bool {
	for _, r := range s {
		if r == '@' {
			return true
		}
	}
	return false
}
