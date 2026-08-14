package auth

import (
	"context"
	"net/http"
	"strings"

	authop "github.com/beardedparrott/orchicon/internal/auth/op"
	"github.com/beardedparrott/orchicon/internal/config"
)

// buildEmbeddedOP constructs the in-process OpenID Provider (zitadel/oidc
// v3 pkg/op) and wires it + its login bridge into the auth handler. It is
// called from NewHandler when ORCHICON_OP_ENABLED. The provider derives
// its ES256 signing key deterministically from the auth signing key (so
// the JWKS never leaks the HMAC secret and stays stable across restarts)
// and resolves identity claims live from Postgres via the tenant-scoped
// resolver.
func (h *Handler) buildEmbeddedOP() error {
	prov, err := authop.NewProvider(authop.Config{
		SigningKey:    h.cfg.SigningKey,
		ClientID:      h.cfg.ClientID,
		RedirectURIs:  opRedirectURIs(h.cfg),
		OPIssuer:      h.cfg.OPIssuer,
		AllowInsecure: h.mode == config.ModeLocal,
		Pool:          h.pool,
		Log:           h.log,
	})
	if err != nil {
		return err
	}
	h.op = prov
	return nil
}

// opRedirectURIs computes the built-in client's registered redirect URIs.
// An explicit ORCHICON_OP_REDIRECT_URIS wins; otherwise the default is the
// auth RedirectURL (the SPA dev callback) plus, when the OP issuer is
// pinned, the plane-origin /auth/callback if it differs. A dynamic issuer
// (no OPIssuer) has no static plane origin, so only the RedirectURL is
// registered.
func opRedirectURIs(cfg config.AuthConfig) []string {
	if cfg.OPRedirectURIs != "" {
		parts := strings.Split(cfg.OPRedirectURIs, ",")
		uris := make([]string, 0, len(parts))
		for _, p := range parts {
			if u := strings.TrimSpace(p); u != "" {
				uris = append(uris, u)
			}
		}
		return uris
	}
	uris := []string{cfg.RedirectURL}
	if cfg.OPIssuer != "" {
		plane := strings.TrimSuffix(cfg.OPIssuer, "/") + "/auth/callback"
		if plane != cfg.RedirectURL {
			uris = append(uris, plane)
		}
	}
	return uris
}

// registerEmbeddedOP mounts the six OP endpoints + the login bridge on the
// plane mux. Only called when the provider is built.
func (h *Handler) registerEmbeddedOP(mux *http.ServeMux) {
	if h.op == nil {
		return
	}
	h.op.Register(mux)
	authop.RegisterLoginBridge(mux, h.op, func(r *http.Request) (string, string, bool) {
		raw := readRefreshToken(r)
		if raw == "" {
			return "", "", false
		}
		claims, err := h.issuer.VerifyRefresh(raw)
		if err != nil {
			return "", "", false
		}
		// Re-resolve the identity so a deleted identity cannot log in.
		if _, _, err := h.resolver.ResolveIdentity(context.Background(), claims.TenantID, claims.Subject); err != nil {
			return "", "", false
		}
		return claims.TenantID, claims.Subject, true
	})
}

// CloseEmbeddedOP stops the OP's storage sweeper (shutdown hook).
func (h *Handler) CloseEmbeddedOP() {
	if h.op != nil {
		h.op.Close()
	}
}
