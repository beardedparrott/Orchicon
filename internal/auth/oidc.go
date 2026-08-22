package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// pkceVerifierTTL bounds how long an unused state→verifier mapping lives.
// A login that stalls past this window simply loses the verifier; the IdP
// rejects the code exchange and the user re-initiates.
const pkceVerifierTTL = 10 * time.Minute

// OIDCVerifier wraps a configured OIDC provider + verifier for the
// production authorization-code flow (docs/07 §6.1). It is lazily
// initialized on first use so the control plane boots even when the
// IdP is unreachable at startup (the outbox relay pattern — degrade
// gracefully, retry on demand).
//
// PKCE is capability-gated: only IdPs that advertise S256 in
// code_challenge_methods_supported receive a per-login code challenge.
// IdPs that do not advertise it get byte-for-byte the same flow as
// before this feature landed (no challenge params on the wire).
type OIDCVerifier struct {
	issuer       string
	clientID     string
	clientSecret string
	redirectURL  string

	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauthCfg *oauth2.Config

	// pkce is true when the IdP's discovery document advertises S256.
	pkce bool
	// verifiers maps the round-trip state to the PKCE code verifier it
	// was created with (single-use; consumed on exchange).
	verifiers   map[string]pkceEntry
	verifiersMu sync.Mutex
}

type pkceEntry struct {
	verifier  string
	expiresAt time.Time
}

// NewOIDCVerifier constructs a verifier. The provider is resolved lazily
// on first Verify/AuthorizeURL call so construction never blocks on the
// IdP being reachable.
func NewOIDCVerifier(issuer, clientID, clientSecret, redirectURL string) *OIDCVerifier {
	return &OIDCVerifier{
		issuer:       issuer,
		clientID:     clientID,
		clientSecret: clientSecret,
		redirectURL:  redirectURL,
	}
}

// ensureProvider resolves the OIDC provider + verifier on first use.
// Subsequent calls reuse the cached provider. It also reads
// code_challenge_methods_supported from discovery to decide whether PKCE
// is sent for this IdP.
func (o *OIDCVerifier) ensureProvider(ctx context.Context) error {
	if o.provider != nil {
		return nil
	}
	provider, err := oidc.NewProvider(ctx, o.issuer)
	if err != nil {
		return fmt.Errorf("auth: oidc provider %s: %w", o.issuer, err)
	}
	var claims struct {
		CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
	}
	_ = provider.Claims(&claims)
	for _, m := range claims.CodeChallengeMethodsSupported {
		if m == "S256" {
			o.pkce = true
			break
		}
	}
	o.provider = provider
	o.verifier = provider.Verifier(&oidc.Config{ClientID: o.clientID})
	o.oauthCfg = &oauth2.Config{
		ClientID:     o.clientID,
		ClientSecret: o.clientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  o.redirectURL,
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
	}
	return nil
}

// AuthCodeURL returns the IdP authorization URL to redirect the browser to.
func (o *OIDCVerifier) AuthCodeURL(ctx context.Context, state string) (string, error) {
	if err := o.ensureProvider(ctx); err != nil {
		return "", err
	}
	if !o.pkce {
		return o.oauthCfg.AuthCodeURL(state), nil
	}
	verifier := oauth2.GenerateVerifier()
	o.verifiersMu.Lock()
	if o.verifiers == nil {
		o.verifiers = make(map[string]pkceEntry)
	}
	o.purgeExpiredVerifiersLocked(time.Now())
	o.verifiers[state] = pkceEntry{verifier: verifier, expiresAt: time.Now().Add(pkceVerifierTTL)}
	o.verifiersMu.Unlock()
	return o.oauthCfg.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier)), nil
}

// purgeExpiredVerifiersLocked drops stale state→verifier entries. Callers
// hold verifiersMu.
func (o *OIDCVerifier) purgeExpiredVerifiersLocked(now time.Time) {
	for s, e := range o.verifiers {
		if e.expiresAt.Before(now) {
			delete(o.verifiers, s)
		}
	}
}

// ExchangeOutcome is the result of exchanging an authorization code for
// tokens and verifying the ID token.
type ExchangeOutcome struct {
	Subject     string
	Email       string
	DisplayName string
}

// Exchange exchanges an authorization code for tokens, verifies the ID
// token, and returns the identity claims (subject + profile). On the PKCE
// path, state carries the round-trip verifier (single-use); on the
// no-PKCE path it is ignored — the flow is unchanged from before.
func (o *OIDCVerifier) Exchange(ctx context.Context, state, code string) (ExchangeOutcome, error) {
	if err := o.ensureProvider(ctx); err != nil {
		return ExchangeOutcome{}, err
	}
	var opts []oauth2.AuthCodeOption
	if o.pkce {
		o.verifiersMu.Lock()
		entry, ok := o.verifiers[state]
		if ok {
			delete(o.verifiers, state)
		}
		o.verifiersMu.Unlock()
		if !ok || entry.expiresAt.Before(time.Now()) {
			return ExchangeOutcome{}, errors.New("auth: oidc: unknown or expired login state")
		}
		opts = append(opts, oauth2.VerifierOption(entry.verifier))
	}
	tok, err := o.oauthCfg.Exchange(ctx, code, opts...)
	if err != nil {
		return ExchangeOutcome{}, fmt.Errorf("auth: oidc exchange: %w", err)
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		return ExchangeOutcome{}, errors.New("auth: oidc: no id_token in exchange response")
	}
	idTok, err := o.verifier.Verify(ctx, rawID)
	if err != nil {
		return ExchangeOutcome{}, fmt.Errorf("auth: oidc verify id token: %w", err)
	}
	var claims struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	_ = idTok.Claims(&claims)
	return ExchangeOutcome{
		Subject:     idTok.Subject,
		Email:       claims.Email,
		DisplayName: claims.Name,
	}, nil
}
