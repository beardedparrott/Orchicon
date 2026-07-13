package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDCVerifier wraps a configured OIDC provider + verifier for the
// production authorization-code flow (docs/07 §6.1). It is lazily
// initialized on first use so the control plane boots even when the
// IdP is unreachable at startup (the outbox relay pattern — degrade
// gracefully, retry on demand).
type OIDCVerifier struct {
	issuer      string
	clientID    string
	clientSecret string
	redirectURL  string

	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauthCfg *oauth2.Config
}

// NewOIDCVerifier constructs a verifier. The provider is resolved lazily
// on first Verify/AuthorizeURL call so construction never blocks on the
// IdP being reachable.
func NewOIDCVerifier(issuer, clientID, clientSecret, redirectURL string) *OIDCVerifier {
	return &OIDCVerifier{
		issuer:       issuer,
		clientID:     clientID,
		clientSecret: clientSecret,
		redirectURL:   redirectURL,
	}
}

// ensureProvider resolves the OIDC provider + verifier on first use.
// Subsequent calls reuse the cached provider.
func (o *OIDCVerifier) ensureProvider(ctx context.Context) error {
	if o.provider != nil {
		return nil
	}
	provider, err := oidc.NewProvider(ctx, o.issuer)
	if err != nil {
		return fmt.Errorf("auth: oidc provider %s: %w", o.issuer, err)
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
	return o.oauthCfg.AuthCodeURL(state), nil
}

// ExchangeOutcome is the result of exchanging an authorization code for
// tokens and verifying the ID token.
type ExchangeOutcome struct {
	Subject     string
	Email       string
	DisplayName string
}

// Exchange exchanges an authorization code for tokens, verifies the ID
// token, and returns the identity claims (subject + profile).
func (o *OIDCVerifier) Exchange(ctx context.Context, code string) (ExchangeOutcome, error) {
	if err := o.ensureProvider(ctx); err != nil {
		return ExchangeOutcome{}, err
	}
	tok, err := o.oauthCfg.Exchange(ctx, code)
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
